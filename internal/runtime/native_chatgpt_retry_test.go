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
	"github.com/helpin-ai/agent-runtime/internal/tools"
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
		{"partial tool is discarded before retry", "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"probe\",\"arguments\":\"\"}}\n\ndata: {\"type\":", false, 2, true},
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
					if tc.name == "partial tool is discarded before retry" && len(nativeRawToolCallBlocks(chunk.Message)) != 0 {
						t.Fatal("discarded tool call leaked into the recovered response")
					}
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

type interruptedToolStream struct {
	fakeNativeModelStream
	err    error
	closed bool
}

func (s *interruptedToolStream) Recv() (*NativeModelResponse, error) {
	if len(s.chunks) == 0 {
		return nil, s.err
	}
	return s.fakeNativeModelStream.Recv()
}
func (s *interruptedToolStream) Close() { s.closed = true }

func TestChatGPTInterruptedToolArguments(t *testing.T) {
	interruption := errors.New("failed to read stream: stream error: stream ID 37; INTERNAL_ERROR; received from peer")
	tool := func(id, args string) NativeModelResponse {
		return NativeModelResponse{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: id, ToolName: "run_command", Input: json.RawMessage(args)}}}}
	}
	for _, tc := range []struct {
		name    string
		prefix  []NativeModelResponse
		repeat  bool
		retries int
	}{
		{"pending tool recovers", nil, false, 1},
		{"repeated interruption stops", nil, true, 1},
		{"visible text prevents retry", []NativeModelResponse{{Message: NativeMessage{Content: "Already visible"}}}, false, 0},
		{"visible reasoning prevents retry", []NativeModelResponse{{Message: NativeMessage{ReasoningContent: "Already visible"}}}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &interruptedToolStream{fakeNativeModelStream: fakeNativeModelStream{chunks: append(tc.prefix, tool("discarded", `{"program":"python3","args":[`))}, err: interruption}
			second := &interruptedToolStream{fakeNativeModelStream: fakeNativeModelStream{chunks: []NativeModelResponse{tool("accepted", `{"program":"python3","args":[]}`)}}, err: io.EOF}
			if tc.repeat {
				second.err = interruption
			}
			retries := 0
			stream := &chatGPTStreamRetry{ctx: t.Context(), stream: first, reopen: func() (NativeModelStream, error) { retries++; return second, nil }}
			x := contextTestExec(t)
			executions := 0
			x.Tools.Register(tools.Definition{Name: "run_command", InputSchema: map[string]any{"type": "object"}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
				executions++
				return json.RawMessage(`{"saved":true}`), nil
			})
			x.AllowedTools["run_command"] = true
			executeNativeToolCallsForRound(t.Context(), x, nativeRawToolCallBlocks(tool("previous", `{}`).Message), "previous-message")
			if executions != 1 {
				t.Fatal("previous checkpoint did not execute")
			}
			sink := &testEventSink{}
			x.EventSink = sink
			response, _, err := collectNativeModelStream(t.Context(), x, stream)
			if executions != 1 {
				t.Fatal("stream retry executed a tool")
			}
			if retries != tc.retries || !first.closed {
				t.Fatalf("retries=%d closed=%v", retries, first.closed)
			}
			if tc.retries == 1 && !tc.repeat {
				if err != nil {
					t.Fatal(err)
				}
				calls := nativeRawToolCallBlocks(response.Message)
				if len(calls) != 1 || calls[0].ToolCallID != "accepted" || !json.Valid(calls[0].Input) {
					t.Fatalf("calls=%+v", calls)
				}
				executeNativeToolCallsForRound(t.Context(), x, calls, "accepted-message")
				if executions != 2 {
					t.Fatalf("executions=%d; earlier action must not replay", executions)
				}
				for _, event := range sink.events {
					if event.Data["tool_call_id"] == "discarded" {
						t.Fatal("discarded call was exposed")
					}
				}
			} else if err == nil {
				t.Fatal("expected interruption")
			}
		})
	}
}

func TestChatGPTBufferedToolWhitespaceGuard(t *testing.T) {
	first := &interruptedToolStream{fakeNativeModelStream: fakeNativeModelStream{chunks: []NativeModelResponse{
		{Message: NativeMessage{Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "a", ToolName: "read_files", Input: json.RawMessage(`{"x":`)}}}},
		{Message: NativeMessage{Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "a", ToolName: "read_files", Input: json.RawMessage(strings.Repeat(" ", 8193))}}}},
	}}, err: io.EOF}
	stream := &chatGPTStreamRetry{ctx: t.Context(), stream: first, reopen: func() (NativeModelStream, error) { t.Fatal("stall must not retry"); return nil, nil }}
	_, _, err := collectNativeModelStream(t.Context(), contextTestExec(t), stream)
	if err == nil || !strings.Contains(err.Error(), "excessive whitespace") || !first.closed {
		t.Fatalf("err=%v closed=%v", err, first.closed)
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

type retryCancellationBody struct {
	io.Reader
	cancel context.CancelFunc
	closed bool
}

func (b *retryCancellationBody) Close() error {
	b.closed = true
	b.cancel()
	return nil
}

func TestChatGPTStreamStartRetryCancellation(t *testing.T) {
	for _, when := range []string{"before request", "with response", "during backoff"} {
		t.Run(when, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			requests := 0
			body := &retryCancellationBody{Reader: strings.NewReader(""), cancel: cancel}
			transport := chatGPTTransport{base: reviewTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				r.Body.Close()
				if when == "with response" {
					cancel()
				}
				return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"10"}}, Body: body}, nil
			})}
			if when == "before request" {
				cancel()
			}
			request, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com/responses", strings.NewReader(`{"input":[]}`))
			started := time.Now()
			response, err := transport.RoundTrip(request)
			wantRequests := 1
			if when == "before request" {
				wantRequests = 0
			}
			if !errors.Is(err, context.Canceled) || response != nil || requests != wantRequests || body.closed != (wantRequests == 1) || time.Since(started) > time.Second {
				t.Fatalf("response=%v err=%v requests=%d bodyClosed=%v", response, err, requests, body.closed)
			}
		})
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
