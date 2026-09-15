package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	return t.base.RoundTrip(clone)
}
