package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
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
		{"overload before output recovers", "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\",\"message\":\"Overloaded\"}\n\n", false, 2, true},
		{"service unavailable recovers", "data: {\"type\":\"error\",\"code\":\"service_unavailable_error\",\"message\":\"Unavailable\"}\n\n", false, 2, true},
		{"server error recovers", "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"Unavailable\"}\n\n", false, 2, true},
		{"rate limit before output recovers", "data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"Try later\"}\n\n", false, 2, true},
		{"overload after output never replays", delta + "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\",\"message\":\"Overloaded\"}\n\n", false, 1, false},
		{"repeated overload stops", "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\",\"message\":\"Overloaded\"}\n\n", true, 2, false},
		{"quota exhausted never retries", "data: {\"type\":\"error\",\"code\":\"insufficient_quota\",\"message\":\"No credits\"}\n\n", false, 1, false},
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

func TestChatGPTConnectionRetry(t *testing.T) {
	for _, tc := range []struct {
		err   error
		retry bool
	}{
		{syscall.ECONNRESET, true}, {syscall.EPIPE, true}, {io.ErrUnexpectedEOF, true}, {os.ErrDeadlineExceeded, true},
		{context.Canceled, false}, {context.DeadlineExceeded, false}, {errors.New("invalid credential"), false},
	} {
		if got := retryableChatGPTConnectionError(tc.err); got != tc.retry {
			t.Errorf("err=%v retry=%v", tc.err, got)
		}
	}
	requests := 0
	transport := chatGPTTransport{base: reviewTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return nil, syscall.ECONNRESET
		}
		body, _ := io.ReadAll(r.Body)
		if !json.Valid(body) {
			t.Error("retry body is missing or invalid")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}, nil
	})}
	request, _ := http.NewRequestWithContext(t.Context(), "POST", "https://example.com/responses", strings.NewReader(`{"input":[]}`))
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestChatGPTStreamStartRetries(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504, 400, 401, 403, 404, 402} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			for _, recover := range []bool{true, false} {
				requests := 0
				var firstBody string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if requests == 0 {
						firstBody = string(body)
					}
					if string(body) != firstBody || r.Header.Get("Authorization") != "Bearer test" || r.Header.Get("Accept") != "text/event-stream" {
						t.Error("retry changed the request")
					}
					requests++
					w.Header().Set("Retry-After", "0")
					if requests == 1 || !recover {
						w.WriteHeader(status)
						return
					}
					fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
				}))
				request, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/responses", strings.NewReader(`{"input":[]}`))
				request.Header.Set("Authorization", "Bearer test")
				response, err := (chatGPTTransport{base: http.DefaultTransport}).RoundTrip(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				server.Close()
				wantRequests, wantStatus := 1, status
				if retryableChatGPTStatus(status) {
					wantRequests = 3
					if recover {
						wantRequests, wantStatus = 2, 200
					}
				}
				if requests != wantRequests || response.StatusCode != wantStatus {
					t.Fatalf("recover=%v requests=%d status=%d", recover, requests, response.StatusCode)
				}
			}
		})
	}
}

func TestChatGPTStreamStartRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(503)
		cancel()
	}))
	defer server.Close()
	request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/responses", strings.NewReader(`{"input":[]}`))
	started := time.Now()
	_, err := (chatGPTTransport{base: http.DefaultTransport}).RoundTrip(request)
	if !errors.Is(err, context.Canceled) || requests != 1 || time.Since(started) > time.Second {
		t.Fatalf("err=%v requests=%d", err, requests)
	}
}

func TestChatGPTRetryDelay(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		header  string
		attempt int
		want    time.Duration
	}{
		{"", 0, time.Second}, {"invalid", 1, 2 * time.Second}, {"3", 0, 3 * time.Second},
		{"999999999", 0, 10 * time.Second}, {"-1", 0, time.Second},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 0, 4 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0, 0},
		{now.Add(time.Hour).Format(http.TimeFormat), 0, 10 * time.Second},
	} {
		if got := chatGPTRetryDelay(tc.header, tc.attempt, now); got != tc.want {
			t.Errorf("header=%q got=%v want=%v", tc.header, got, tc.want)
		}
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
