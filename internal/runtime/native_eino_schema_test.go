package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestNativeProviderReceivesOperationRequirements(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"operations":{"type":"array","minItems":1,"maxItems":20,"items":{"anyOf":[{"type":"object","additionalProperties":false,"required":["type","block_id","old_text","new_text"],"properties":{"type":{"type":"string","enum":["replace_text"]},"block_id":{"type":"string","minLength":1},"old_text":{"type":"string","minLength":1},"new_text":{"type":"string"}}}]}}},"required":["operations"]}`)
	var expected map[string]any
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"anthropic", "openai", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			requests := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "application/json")
				if provider == "anthropic" {
					_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"test","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
				}
			}))
			defer server.Close()
			x := contextTestExec(t)
			x.Agent.Provider, x.Agent.Model = provider, "test"
			factory := EinoProviderFactory{AnthropicAPIKey: "test", OpenAIAPIKey: "test", OpenRouterAPIKey: "test", AnthropicBaseURL: server.URL, OpenAIBaseURL: server.URL, OpenRouterBaseURL: server.URL}
			defs := []tools.Definition{{Name: "edit_document", InputSchema: raw}}
			model, err := factory.ResolveNativeModel(context.Background(), x, defs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := model.Generate(context.Background(), NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "edit"}}, Tools: defs}); err != nil {
				t.Fatal(err)
			}
			body := <-requests
			wireTools, _ := body["tools"].([]any)
			if len(wireTools) != 1 {
				t.Fatalf("missing tools: %v", body)
			}
			tool := wireTools[0].(map[string]any)
			key := "parameters"
			if provider == "anthropic" {
				key = "input_schema"
			}
			got := tool[key].(map[string]any)
			if !reflect.DeepEqual(expected["properties"], got["properties"]) || !reflect.DeepEqual(expected["required"], got["required"]) {
				t.Fatalf("provider lost operation requirements: %v", got)
			}
		})
	}
}

func TestEinoPreservesFullToolContract(t *testing.T) {
	// Exercise the model boundary, including fields lost by ParameterInfo.
	raw := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["operations"],"$defs":{"id":{"type":"string","minLength":1}},"properties":{"operations":{"type":"array","minItems":1,"maxItems":20,"items":{"anyOf":[{"type":"object","additionalProperties":false,"required":["type","block_id","old_text","new_text"],"properties":{"type":{"enum":["replace_text"]},"block_id":{"$ref":"#/$defs/id"},"old_text":{"type":"string","minLength":1},"new_text":{"type":"string"}}},{"type":"object","additionalProperties":false,"required":["type","content"],"properties":{"type":{"enum":["insert"]},"content":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"object"}}]}}}]}}}}`)
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	for _, input := range []any{raw, []byte(raw), object} {
		infos, err := toEinoToolInfos([]tools.Definition{{Name: "edit_document", InputSchema: input}})
		if err != nil {
			t.Fatal(err)
		}
		converted, err := infos[0].ParamsOneOf.ToJSONSchema()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(converted)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(object, got) {
			t.Fatalf("model contract changed: %s", encoded)
		}
	}
}

func TestEinoRejectsMalformedToolSchema(t *testing.T) {
	_, err := toEinoToolInfos([]tools.Definition{{Name: "broken", InputSchema: json.RawMessage(`{"type":`)}})
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("expected named schema error, got %v", err)
	}
}
