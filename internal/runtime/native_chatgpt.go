package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	resp, err := t.base.RoundTrip(clone)
	if err != nil || resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, err
	}
	// This endpoint is forced to stream; proxies can omit or change its MIME type.
	resp.Body = &sseDataOnly{body: resp.Body, scanner: newSSEScanner(resp.Body)}
	return resp, nil
}

// sseDataOnly passes on only the server-sent events that carry data. The
// subscription stream can include events without a data line (keep-alives,
// stray blank lines); the Responses client parses every event's data as
// JSON, and an empty one fails the whole run with "unexpected end of JSON
// input".
type sseDataOnly struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	pending []byte
	done    bool
}

func newSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 32<<20)
	return scanner
}

func (s *sseDataOnly) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.done {
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
	var event bytes.Buffer
	hasData := false
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 {
			if !hasData {
				return nil
			}
			event.WriteByte('\n')
			return event.Bytes()
		}
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok && len(bytes.TrimSpace(value)) > 0 {
			hasData = true
		}
		event.Write(line)
		event.WriteByte('\n')
	}
	s.done = true
	if hasData {
		event.WriteByte('\n')
		return event.Bytes()
	}
	return nil
}

func (s *sseDataOnly) Close() error { return s.body.Close() }

// Retry only before observable output: a restarted generation cannot duplicate
// streamed text or tool calls, and the rest of the run is never replayed.
type chatGPTStreamRetry struct {
	ctx     context.Context
	stream  NativeModelStream
	reopen  func() (NativeModelStream, error)
	retried bool
	emitted bool
}

func (s *chatGPTStreamRetry) Recv() (*NativeModelResponse, error) {
	for {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		chunk, err := s.stream.Recv()
		if err == nil {
			if chunk != nil && (chunk.Message.Content != "" || chunk.Message.ReasoningContent != "" || len(chunk.Message.Blocks) != 0 || chunk.Usage != (NativeUsage{})) {
				s.emitted = true
			}
			return chunk, nil
		}
		if s.emitted || s.retried || !retryableChatGPTStreamError(err) {
			return nil, err
		}
		s.retried = true
		s.stream.Close()
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
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	text := strings.ToLower(err.Error())
	return text == "chatgpt stream ended before completion" ||
		(strings.Contains(text, "failed to read stream:") && (strings.Contains(text, "unexpected end of json input") || strings.Contains(text, "unexpected eof")))
}
