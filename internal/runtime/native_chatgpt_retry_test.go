package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	agenticopenai "github.com/cloudwego/eino-ext/components/model/agenticopenai"
)

func TestChatGPTStreamRetryBoundaries(t *testing.T) {
	delta := "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"
	complete := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n"
	for _, tc := range []struct {
		name, first string
		repeat      bool
		requests    int
		success     bool
	}{
		{"truncated JSON recovers", "data: {\"type\":", false, 2, true},
		{"EOF before output recovers", "", false, 2, true},
		{"repeated truncation stops", "data: {\"type\":", true, 2, false},
		{"partial text never replays", delta + "data: {\"type\":", false, 1, false},
		{"partial tool never replays", "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"probe\",\"arguments\":\"\"}}\n\ndata: {\"type\":", false, 1, false},
		{"explicit incomplete never retries", "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_1\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", false, 1, false},
		{"explicit error never retries", "data: {\"type\":\"error\",\"code\":\"usage_limit\",\"message\":\"Limit reached\"}\n\n", false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if requests.Add(1) == 1 || tc.repeat {
					fmt.Fprint(w, tc.first)
				} else {
					fmt.Fprint(w, delta+complete)
				}
			}))
			defer server.Close()
			retries := 0
			provider, err := agenticopenai.NewResponsesModel(t.Context(), &agenticopenai.ResponsesConfig{APIKey: "test", Model: "test", BaseURL: server.URL, MaxRetries: &retries, HTTPClient: &http.Client{Transport: chatGPTTransport{base: http.DefaultTransport}}})
			if err != nil {
				t.Fatal(err)
			}
			model, err := (EinoAgenticModelFactory{Model: provider, Provider: "openai_chatgpt"}).ResolveNativeModel(t.Context(), contextTestExec(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := model.(NativeStreamingModel).Stream(t.Context(), NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "hello"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			text := ""
			for {
				var chunk *NativeModelResponse
				chunk, err = stream.Recv()
				if err != nil {
					break
				}
				if chunk != nil {
					text += chunk.Message.Content
				}
			}
			if (err == io.EOF) != tc.success {
				t.Fatalf("success=%v err=%v", tc.success, err)
			}
			if tc.success && text != "hello" {
				t.Fatalf("duplicated/missing text %q", text)
			}
			if int(requests.Load()) != tc.requests {
				t.Fatalf("requests=%d want=%d", requests.Load(), tc.requests)
			}
		})
	}
}

type brokenChatGPTStream struct{ cancel context.CancelFunc }

func (s brokenChatGPTStream) Recv() (*NativeModelResponse, error) {
	s.cancel()
	return nil, fmt.Errorf("failed to read stream: unexpected end of JSON input")
}
func (brokenChatGPTStream) Close() {}
func TestChatGPTStreamRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	retried := false
	s := &chatGPTStreamRetry{ctx: ctx, stream: brokenChatGPTStream{cancel: cancel}, reopen: func() (NativeModelStream, error) { retried = true; return nil, nil }}
	started := time.Now()
	_, err := s.Recv()
	if !errors.Is(err, context.Canceled) || retried || time.Since(started) > time.Second {
		t.Fatalf("err=%v retry=%v", err, retried)
	}
}
