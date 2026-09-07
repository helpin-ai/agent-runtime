package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Exercise the actual Eino serializers and response metadata, without paid calls.
func TestNativeSummaryProviderWireContract(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			var requests []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				requests = append(requests, body)
				w.Header().Set("Content-Type", "application/json")
				if provider == "anthropic" {
					_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"summary"}],"stop_reason":"end_turn","usage":{"input_tokens":31,"output_tokens":7}}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"test","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"summary","annotations":[]}]}],"usage":{"input_tokens":31,"output_tokens":7,"total_tokens":38}}`))
				}
			}))
			defer server.Close()
			x := contextTestExec(t)
			x.Agent.Provider = provider
			x.Agent.Model = "test"
			factory := EinoProviderFactory{AnthropicAPIKey: "test", OpenAIAPIKey: "test", OpenRouterAPIKey: "test", AnthropicBaseURL: server.URL, OpenAIBaseURL: server.URL, OpenRouterBaseURL: server.URL}
			definitions := []tools.Definition{{Name: "write_probe", Description: "write", Mutating: true, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}}
			model, err := factory.ResolveNativeModel(context.Background(), x, definitions)
			if err != nil {
				t.Fatal(err)
			}
			response, err := model.(NativeSummaryModel).Summarize(context.Background(), "Summarize historical work", 500)
			if err != nil || response.Incomplete || response.Usage.InputTokens != 31 || response.Usage.OutputTokens != 7 {
				t.Fatalf("summary response: %+v %v", response, err)
			}
			if len(requests) != 1 {
				t.Fatalf("requests: %d", len(requests))
			}
			body := requests[0]
			if wireTools, ok := body["tools"].([]any); ok && len(wireTools) > 0 {
				t.Fatalf("tools exposed to summarizer: %v", wireTools)
			}
			maxKey := "max_output_tokens"
			if provider == "anthropic" {
				maxKey = "max_tokens"
			}
			if body[maxKey] != float64(500) {
				t.Fatalf("summary output not bounded: %v", body[maxKey])
			}
			if _, err := model.Generate(context.Background(), NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "continue"}}, Tools: definitions}); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 {
				t.Fatal("missing normal request")
			}
			wireTools, _ := requests[1]["tools"].([]any)
			if len(wireTools) != 1 {
				t.Fatalf("summary removed normal agent tools: %v", requests[1])
			}
		})
	}
}
