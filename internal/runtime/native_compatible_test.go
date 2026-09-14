package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func compatibleTestModel(t *testing.T, baseURL, mode string) (NativeModel, *ExecutionContext) {
	t.Helper()
	x := contextTestExec(t)
	db := store.NewMemory()
	x.Store, x.Agent.ExecutionConfig = db, nil
	x.Run.Input.Model = &sdk.RunModel{Provider: "openai_compatible", Model: "test-model", Controls: &sdk.ModelControls{}, Endpoint: &sdk.ModelEndpoint{ID: "test", BaseURL: baseURL, AuthMode: mode}}
	x.Run.Input.CredentialSource = "app"
	x.ModelCredentials = &modelauth.Manager{Store: db, Key: []byte(strings.Repeat("k", 32))}
	credential := sdk.ModelCredential{Type: mode}
	if mode == "api_key" {
		credential.APIKey = "selected-key"
	}
	record, err := x.ModelCredentials.Prepare(x.Run.AppID, x.Run.ID, "openai_compatible", credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRunWithModelCredential(context.Background(), x.Run, nil, record); err != nil {
		t.Fatal(err)
	}
	model, err := (EinoProviderFactory{OpenAIAPIKey: "ambient-key", MaxTokens: 128}).ResolveNativeModel(context.Background(), x, []tools.Definition{{Name: "alpha"}, {Name: "beta"}})
	if err != nil {
		t.Fatal(err)
	}
	return model, x
}

func readCompatibleStream(t *testing.T, ctx context.Context, model NativeModel, request NativeModelRequest) *NativeModelResponse {
	t.Helper()
	stream, err := model.(NativeStreamingModel).Stream(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var chunks []NativeModelResponse
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, *chunk)
	}
	return concatNativeModelStreamResponses(chunks)
}

func TestCompatibleStreamInterleavedCallsUsageAndReplay(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer selected-key" {
			t.Error("wrong route or credential")
		}
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if !body.Stream {
			t.Error("expected streaming")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			for _, delta := range []string{
				`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"alpha","arguments":""}},{"index":1,"id":"call_b","type":"function","function":{"name":"beta","arguments":""}}]}`,
				`{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":"}},{"index":1,"function":{"arguments":"{\"y\":"}}]}`,
				`{"tool_calls":[{"index":1,"function":{"arguments":"2}"}},{"index":0,"function":{"arguments":"1}"}}]}`,
			} {
				fmt.Fprintf(w, "data: {\"id\":\"response\",\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", delta)
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			var ids []string
			for _, m := range body.Messages {
				if m.Role == "tool" {
					ids = append(ids, m.ToolCallID)
				}
			}
			if strings.Join(ids, ",") != "call_a,call_b" {
				t.Errorf("tool IDs lost on replay: %v", ids)
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":8,\"total_tokens\":28,\"prompt_tokens_details\":{\"cached_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model, _ := compatibleTestModel(t, server.URL+"/v1", "api_key")
	request := NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "call tools"}}}
	response := readCompatibleStream(t, context.Background(), model, request)
	if response.Usage != (NativeUsage{InputTokens: 20, CachedInputTokens: 4, OutputTokens: 8, ReasoningOutputTokens: 3}) {
		t.Fatalf("usage lost: %+v", response.Usage)
	}
	blocks := nativeRawToolCallBlocks(response.Message)
	if len(blocks) != 2 || blocks[0].ToolCallID != "call_a" || string(blocks[0].Input) != `{"x":1}` || blocks[1].ToolCallID != "call_b" || string(blocks[1].Input) != `{"y":2}` {
		t.Fatalf("interleaved calls corrupted: %+v", blocks)
	}
	request.Messages = append(request.Messages, response.Message, NativeMessage{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: "call_a", ToolName: "alpha", Output: "ok"}, {Type: nativeBlockTypeToolResult, ToolCallID: "call_b", ToolName: "beta", Output: "ok"}}})
	if result := readCompatibleStream(t, context.Background(), model, request); result.Message.Content != "done" || result.Continuation != nil {
		t.Fatalf("replay: %+v", result)
	}
}

func TestCompatibleNoAuthRevocationCancellationAndRedirect(t *testing.T) {
	var calls, redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			t.Error("no-auth sent an ambient credential")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	model, x := compatibleTestModel(t, server.URL+"/v1", "none")
	request := NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "hi"}}}
	if _, err := model.Generate(context.Background(), request); err == nil {
		t.Fatal("redirect succeeded")
	}
	if redirected.Load() != 0 {
		t.Fatal("followed redirect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := model.Generate(ctx, request); err == nil {
		t.Fatal("cancelled request succeeded")
	}
	before := calls.Load()
	if err := x.ModelCredentials.Store.ClearRunModelCredential(context.Background(), x.Run.AppID, x.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := model.Generate(context.Background(), request); err == nil {
		t.Fatal("revoked no-auth run succeeded")
	}
	if calls.Load() != before {
		t.Fatal("revoked request reached provider")
	}
}

func TestCompatibleTruncationAndSummaryUseAcceptedRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer selected-key" {
			t.Error("summary lost accepted credential")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		if definitions, ok := body["tools"].([]any); ok && len(definitions) != 0 {
			t.Error("summary included tools")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"summary","choices":[{"index":0,"message":{"role":"assistant","content":"summary"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer server.Close()
	model, _ := compatibleTestModel(t, server.URL+"/v1", "api_key")
	result := readCompatibleStream(t, context.Background(), model, NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "hi"}}})
	if !result.Incomplete || result.Message.Content != "partial" {
		t.Fatalf("truncation lost: %+v", result)
	}
	summary, err := model.(NativeSummaryModel).Summarize(context.Background(), "Summarize the transcript", 64)
	if err != nil || summary == nil || summary.Incomplete || summary.Message.Content != "summary" {
		t.Fatalf("summary failed: %+v, %v", summary, err)
	}
}
