package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestNativeModelControlsReachResponsesRequest(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","object":"response","status":"completed","model":"gpt-test","output":[{"type":"message","id":"msg_test","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()
	x := &ExecutionContext{Agent: &agentcore.Agent{Provider: "openai", Model: "gpt-test", ExecutionConfig: json.RawMessage(`{"reasoning_effort":"high","service_tier":"fast"}`)}}
	m, err := (EinoProviderFactory{OpenAIAPIKey: "test", OpenAIBaseURL: server.URL}).ResolveNativeModel(context.Background(), x, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Generate(context.Background(), NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" || body["service_tier"] != "priority" {
		t.Fatalf("controls missing from HTTP request: %#v", body)
	}
}
