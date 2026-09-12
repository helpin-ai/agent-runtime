package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agenticopenai "github.com/cloudwego/eino-ext/components/model/agenticopenai"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestChatGPTResponsesStreamAndSummary(t *testing.T) {
	requests := 0
	complete := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["store"] != false || body["stream"] != true || body["instructions"] == "" {
			t.Errorf("invalid request: %v", body)
		}
		if _, ok := body["max_output_tokens"]; ok {
			t.Error("unsupported limit sent")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"summary\"}\n\n")
		fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\",\"annotations\":[]}]}}\n\n")
		if complete {
			fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"test\",\"output\":[],\"usage\":{\"input_tokens\":31,\"output_tokens\":7,\"total_tokens\":38}}}\n\n")
		}
	}))
	defer server.Close()
	retries := 0
	provider, err := agenticopenai.NewResponsesModel(context.Background(), &agenticopenai.ResponsesConfig{APIKey: "test", Model: "test", BaseURL: server.URL, MaxRetries: &retries, HTTPClient: &http.Client{Transport: chatGPTTransport{base: http.DefaultTransport}}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := (EinoAgenticModelFactory{Model: provider, Provider: "openai_chatgpt"}).ResolveNativeModel(context.Background(), contextTestExec(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Generate(context.Background(), NativeModelRequest{SystemPrompt: "Follow instructions", Messages: []NativeMessage{{Role: "user", Content: "hello"}}})
	if err != nil || response.Message.Content != "summary" {
		t.Fatalf("generate: %+v %v", response, err)
	}
	summary, err := model.(NativeSummaryModel).Summarize(context.Background(), "summarize", 100)
	if err != nil || summary.Incomplete || summary.Usage.InputTokens != 31 {
		t.Fatalf("summary: %+v %v", summary, err)
	}
	complete = false
	if _, err = model.Generate(context.Background(), NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "hello"}}}); err == nil {
		t.Fatal("truncated stream accepted")
	}
	if requests != 3 {
		t.Fatalf("unexpected retry count: %d", requests)
	}
}

func TestNativeRunKeyOverridesGlobalAndAlsoFundsSummaries(t *testing.T) {
	ctx := context.Background()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer personal" {
			t.Error("wrong key")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","model":"test","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}]}`)
	}))
	defer server.Close()
	x := contextTestExec(t)
	db := store.NewMemory()
	x.Store = db
	x.Run.Input.CredentialSource = "app"
	x.Run.Input.Model = &sdk.RunModel{Provider: "openai", Model: "test"}
	x.ModelCredentials = &modelauth.Manager{Store: db, Key: []byte(strings.Repeat("k", 32))}
	record, err := x.ModelCredentials.Prepare(x.Run.AppID, x.Run.ID, "openai", sdk.ModelCredential{Type: "api_key", APIKey: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateRunWithModelCredential(ctx, x.Run, nil, record); err != nil {
		t.Fatal(err)
	}
	// No global key is configured.
	model, err := (EinoProviderFactory{OpenAIBaseURL: server.URL}).ResolveNativeModel(ctx, x, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = model.Generate(ctx, NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = model.(NativeSummaryModel).Summarize(ctx, "summarize", 100); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls: %d", calls)
	}
}

func TestRunCredentialAuthenticationDoesNotFallBackOrRetryRepeatedly(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(401) }))
			defer server.Close()
			x := contextTestExec(t)
			db := store.NewMemory()
			x.Store = db
			x.Run.Input.CredentialSource = "app"
			x.Run.Input.Model = &sdk.RunModel{Provider: provider, Model: "test"}
			x.ModelCredentials = &modelauth.Manager{Store: db, Key: []byte(strings.Repeat("k", 32))}
			record, err := x.ModelCredentials.Prepare(x.Run.AppID, x.Run.ID, provider, sdk.ModelCredential{Type: "api_key", APIKey: "invalid-personal"})
			if err != nil {
				t.Fatal(err)
			}
			if err = db.CreateRunWithModelCredential(ctx, x.Run, nil, record); err != nil {
				t.Fatal(err)
			}
			m, err := (EinoProviderFactory{OpenAIAPIKey: "global", AnthropicAPIKey: "global", OpenRouterAPIKey: "global", OpenAIBaseURL: server.URL, AnthropicBaseURL: server.URL, OpenRouterBaseURL: server.URL}).ResolveNativeModel(ctx, x, []tools.Definition{{Name: "probe", Description: "Fixture tool", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.Generate(ctx, NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "hello"}}})
			var authErr *modelauth.AuthenticationError
			if !errors.As(err, &authErr) {
				t.Fatalf("authentication error lost: %v", err)
			}
			if requests != 1 {
				t.Fatalf("unauthorized request count: %d", requests)
			}
			if err = x.ModelCredentials.Replace(ctx, x.Run.AppID, x.Run.ID, sdk.ModelCredential{Type: "api_key", APIKey: "replacement"}); err != nil {
				t.Fatal(err)
			}
			_, err = m.Generate(ctx, NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "hello"}}})
			if !errors.As(err, &authErr) || requests != 2 {
				t.Fatalf("new credential was not tried exactly once: %d %v", requests, err)
			}

		})
	}
}
