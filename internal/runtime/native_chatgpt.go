package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// chatGPTTransport adapts the subscription request shape while retaining the
// Responses client's parser. Authentication remains in the inner run transport.
type chatGPTTransport struct{ base http.RoundTripper }

func (t chatGPTTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/responses") {
		return nil, fmt.Errorf("unsupported ChatGPT request")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, (32<<20)+1))
	req.Body.Close()
	if err != nil || len(raw) > 32<<20 {
		return nil, fmt.Errorf("cannot encode ChatGPT request")
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return nil, fmt.Errorf("invalid ChatGPT request")
	}
	var instructions []string
	var input []any
	if items, ok := body["input"].([]any); ok {
		for _, item := range items {
			message, ok := item.(map[string]any)
			if ok && (message["role"] == "system" || message["role"] == "developer") {
				if content, ok := message["content"].(string); ok {
					instructions = append(instructions, content)
				}
				if content, ok := message["content"].([]any); ok {
					for _, part := range content {
						if obj, ok := part.(map[string]any); ok {
							if text, ok := obj["text"].(string); ok {
								instructions = append(instructions, text)
							}
						}
					}
				}
			} else {
				input = append(input, item)
			}
		}
	}
	body["input"] = input
	if len(instructions) == 0 {
		instructions = []string{"You are a helpful assistant."}
	}
	body["instructions"] = strings.Join(instructions, "\n\n")
	body["store"] = false
	body["stream"] = true
	delete(body, "max_output_tokens")
	delete(body, "previous_response_id")
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cannot encode ChatGPT request")
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Accept", "text/event-stream")
	clone.Body = io.NopCloser(bytes.NewReader(encoded))
	clone.ContentLength = int64(len(encoded))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		resp, err = t.base.RoundTrip(clone)
		if attempt == 2 || req.Context().Err() != nil || (err != nil && !retryableChatGPTConnectionError(err)) || (err == nil && (resp == nil || !retryableChatGPTStatus(resp.StatusCode))) {
			break
		}
		retryAfter, status := "", 0
		if resp != nil {
			retryAfter, status = resp.Header.Get("Retry-After"), resp.StatusCode
			if resp.Body != nil {
				resp.Body.Close()
			}
		}
		delay := chatGPTRetryDelay(retryAfter, attempt, time.Now())
		slog.Warn("ChatGPT stream start retry", "status", status, "retry", attempt+1, "delay", delay)
		timer := time.NewTimer(delay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
		clone = clone.Clone(req.Context())
		clone.Body = io.NopCloser(bytes.NewReader(encoded))
	}
	if err != nil || resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, err
	}
	// This endpoint is forced to stream; proxies can omit or change its MIME type.
	resp.Body = &sseDataOnly{body: resp.Body, scanner: newSSEScanner(resp.Body)}
	return resp, nil
}

func chatGPTRetryDelay(retryAfter string, attempt int, now time.Time) time.Duration {
	delay := time.Second << attempt
	if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds >= 0 {
		// Clamp before converting seconds to avoid duration overflow.
		delay = time.Duration(min(seconds, 10)) * time.Second
	} else if date, err := http.ParseTime(retryAfter); err == nil {
		delay = date.Sub(now)
	}
	return max(0, min(delay, 10*time.Second))
}

func retryableChatGPTStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		status == http.StatusInternalServerError || status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func retryableChatGPTConnectionError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var networkError net.Error
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || (errors.As(err, &networkError) && networkError.Timeout())
}

// sseDataOnly passes on only the server-sent events that carry data. The
// subscription stream can include events without a data line (keep-alives,
// stray blank lines); the Responses client parses every event's data as
// JSON, and an empty one fails the whole run with "unexpected end of JSON
// input".
type sseDataOnly struct {
	body          io.ReadCloser
	scanner       *bufio.Scanner
	pending       []byte
	done          bool
	err           error
	toolArguments map[int64]chatGPTToolArguments
}

type chatGPTToolArguments struct {
	itemID    string
	received  string
	completed string
}

func newSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 32<<20)
	return scanner
}

func (s *sseDataOnly) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.done {
			if s.err != nil {
				return 0, s.err
			}
			if err := s.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		s.pending = s.nextEvent()
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// nextEvent returns the next event with data, ending in a blank line, or
// nil when an event without data (or with only empty data lines) was
// dropped.
func (s *sseDataOnly) nextEvent() []byte {
	var event, data bytes.Buffer
	hasData := false
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 {
			if !hasData {
				return nil
			}
			event.WriteByte('\n')
			return s.completeEvent(event.Bytes(), data.Bytes())
		}
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok && len(bytes.TrimSpace(value)) > 0 {
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimSpace(value))
			hasData = true
		}
		event.Write(line)
		event.WriteByte('\n')
	}
	s.done = true
	if hasData {
		event.WriteByte('\n')
		return s.completeEvent(event.Bytes(), data.Bytes())
	}
	return nil
}

// The Responses library ignores completed arguments; supply only missing deltas.
func (s *sseDataOnly) completeEvent(event, data []byte) []byte {
	var chunk struct {
		Type        string `json:"type"`
		OutputIndex int64  `json:"output_index"`
		ItemID      string `json:"item_id"`
		Delta       string `json:"delta"`
		Arguments   string `json:"arguments"`
		Item        struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			Arguments string `json:"arguments"`
			Name      string `json:"name"`
		} `json:"item"`
	}
	if json.Unmarshal(data, &chunk) != nil {
		return event
	}
	switch chunk.Type {
	case "response.output_item.added", "response.output_item.done":
		if chunk.Item.Type != "function_call" {
			return event
		}
		chunk.ItemID = chunk.Item.ID
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
	default:
		return event
	}
	if s.toolArguments == nil {
		s.toolArguments = map[int64]chatGPTToolArguments{}
	}
	state := s.toolArguments[chunk.OutputIndex]
	if state.itemID != "" && chunk.ItemID != "" && state.itemID != chunk.ItemID {
		s.err = fmt.Errorf("ChatGPT tool argument stream changed item identity")
		s.done = true
		return nil
	}
	if chunk.ItemID != "" {
		state.itemID = chunk.ItemID
	}
	switch chunk.Type {
	case "response.output_item.added":
		state.received = chunk.Item.Arguments
	case "response.function_call_arguments.delta":
		state.received += chunk.Delta
	case "response.function_call_arguments.done":
		state.completed = chunk.Arguments
	case "response.output_item.done":
		completed := chunk.Item.Arguments
		if completed == "" {
			completed = state.completed
		}
		slog.Info("ChatGPT tool argument completion", "tool_name", chunk.Item.Name, "output_index", chunk.OutputIndex, "streamed_bytes", len(state.received), "completed_bytes", len(completed))
		if completed != "" {
			if !json.Valid([]byte(completed)) || !strings.HasPrefix(completed, state.received) {
				s.err = fmt.Errorf("ChatGPT completed tool arguments conflict with streamed arguments")
				s.done = true
				return nil
			}
			missing := completed[len(state.received):]
			if missing != "" {
				delta, _ := json.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "output_index": chunk.OutputIndex, "item_id": state.itemID, "delta": missing})
				event = append(event, []byte("data: ")...)
				event = append(event, delta...)
				event = append(event, '\n', '\n')
				slog.Warn("ChatGPT recovered completed tool arguments", "output_index", chunk.OutputIndex, "recovered_bytes", len(missing))
			}
			state.received = completed
		}
	}
	s.toolArguments[chunk.OutputIndex] = state
	return event
}

func (s *sseDataOnly) Close() error { return s.body.Close() }

// Buffer tool-only output until the response succeeds; tools execute only after
// collection. Never replay text or reasoning already exposed to the caller.
type chatGPTStreamRetry struct {
	ctx            context.Context
	stream         NativeModelStream
	reopen         func() (NativeModelStream, error)
	retried        bool
	emitted        bool
	pending        []*NativeModelResponse
	finalErr       error
	argumentGuards map[string]*nativeToolArgumentGuard
}

func (s *chatGPTStreamRetry) Recv() (*NativeModelResponse, error) {
	for {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		if s.emitted && len(s.pending) > 0 {
			chunk := s.pending[0]
			s.pending[0] = nil
			s.pending = s.pending[1:]
			return chunk, nil
		}
		if s.finalErr != nil {
			return nil, s.finalErr
		}
		chunk, err := s.stream.Recv()
		if err == nil {
			if s.emitted || chunk == nil {
				return chunk, nil
			}
			// Keep the shared stall guard active while arguments are buffered.
			for _, call := range nativeRawToolCallBlocks(chunk.Message) {
				if s.argumentGuards == nil {
					s.argumentGuards = map[string]*nativeToolArgumentGuard{}
				}
				guard := s.argumentGuards[call.ToolCallID]
				if guard == nil {
					guard = &nativeToolArgumentGuard{}
					s.argumentGuards[call.ToolCallID] = guard
				}
				if !guard.accept(string(call.Input)) {
					return nil, fmt.Errorf("model stream stalled: excessive whitespace outside JSON strings in %s tool arguments", call.ToolName)
				}
			}
			s.pending = append(s.pending, chunk)
			if chunk.Message.Content != "" || chunk.Message.ReasoningContent != "" || chunk.Usage != (NativeUsage{}) {
				s.emitted = true
			}
			for _, block := range chunk.Message.Blocks {
				if block.Type != nativeBlockTypeToolCall {
					s.emitted = true
				}
			}
			continue
		}
		if err == io.EOF {
			s.finalErr = err
			s.emitted = true
			continue
		}
		if s.emitted || s.retried || !retryableChatGPTStreamError(err) {
			return nil, err
		}
		s.retried = true
		s.stream.Close()
		s.pending = nil
		s.argumentGuards = nil
		slog.Warn("ChatGPT stream retry before output")
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return nil, s.ctx.Err()
		case <-timer.C:
		}
		stream, err := s.reopen()
		if err != nil {
			return nil, err
		}
		if stream == nil {
			return nil, fmt.Errorf("empty ChatGPT retry stream")
		}
		s.stream = stream
	}
}

func (s *chatGPTStreamRetry) Close() { s.stream.Close() }

func retryableChatGPTStreamError(err error) bool {
	if retryableChatGPTConnectionError(err) {
		return true
	}
	text := strings.ToLower(err.Error())
	if text == "chatgpt stream ended before completion" {
		return true
	}
	if !strings.Contains(text, "failed to read stream:") && !strings.HasPrefix(text, "received error event:") {
		return false
	}
	for _, transient := range []string{"unexpected end of json input", "unexpected eof", "connection reset by peer", "server_is_overloaded", "overloaded_error", "service_unavailable_error", "rate_limit_exceeded", "rate_limit_error", "internal_server_error", "code=server_error", `"code":"server_error"`} {
		if strings.Contains(text, transient) {
			return true
		}
	}
	if strings.Contains(text, "stream error:") && strings.Contains(text, "internal_error; received from peer") {
		return true
	}
	return false
}
