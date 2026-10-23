package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type authResumeModel struct{ requests []runtime.NativeModelRequest }

func (m *authResumeModel) ResolveNativeModel(context.Context, *runtime.ExecutionContext, []tools.Definition) (runtime.NativeModel, error) {
	return m, nil
}

func (m *authResumeModel) Generate(_ context.Context, request runtime.NativeModelRequest) (*runtime.NativeModelResponse, error) {
	m.requests = append(m.requests, request)
	return &runtime.NativeModelResponse{Message: runtime.NativeMessage{Role: "assistant", Content: "Done."}}, nil
}

func TestMCPAuthenticationBeforeModelPreservesUserReply(t *testing.T) {
	remote := protocol.NewServer(&protocol.Implementation{Name: "test-mcp", Version: "1.0"}, nil)
	remote.AddTool(&protocol.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		return &protocol.CallToolResult{}, nil
	})
	handler := protocol.NewStreamableHTTPHandler(func(*http.Request) *protocol.Server { return remote }, &protocol.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	var expired atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expired.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.ExecutionConfig = nil
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{ID: "auth-reply", AppID: agent.AppID, AgentID: agent.ID, RuntimeKind: agentcore.RuntimeNativeSDK,
		Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"}, Status: agentcore.RunStatusQueued,
		Input: agentcore.RunInput{Instructions: "Research demo candidates.", TurnPolicy: agentcore.TurnPolicy{Mode: agentcore.TurnPolicyPauseAfterAssist}},
	}
	mcpConfig := mcp.RunConfig{CredentialKey: []byte("0123456789abcdef0123456789abcdef"), AllowHTTP: true, AllowPrivateNetwork: true}
	servers, err := mcp.PrepareStoredServers(run.AppID, run.ID, []mcp.RunServerRequest{{ServerID: "helpin", ServerName: "Helpin", Transport: agentcore.MCPTransportStreamableHTTP, URL: server.URL,
		Tools: []mcp.RunTool{{Name: "read", Access: agentcore.MCPToolAccessRead}}, Credential: &mcp.RunCredential{Type: mcp.CredentialBearerToken, AccessToken: "test-token"},
	}}, mcpConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.CreateRunWithMCP(ctx, run, servers); err != nil {
		t.Fatal(err)
	}
	model := &authResumeModel{}
	eng := New(Config{Store: mem, Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider(), RunMCP: mcpConfig,
		ManualLightweightExecution: true, Runtimes: runtime.NewRegistry(runtime.NewNativeAdapterWithConfig(runtime.NativeConfig{ModelFactory: model})),
	})
	if _, err := eng.ExecuteRunOnce(ctx, run.AppID, run.ID); err != nil {
		t.Fatal(err)
	}
	question := "Which other English ones are in the list?"
	if _, err := eng.ResumeRun(ctx, run.AppID, run.ID, ResumePayload{Intent: "reply", Content: question, ResumeID: "human-reply", MessageProvenance: "human", ExternalActorID: "user-1"}); err != nil {
		t.Fatal(err)
	}
	requests := len(model.requests)
	expired.Store(true)
	for refresh := 0; refresh < 2; refresh++ {
		result, err := eng.ExecuteRunOnce(ctx, run.AppID, run.ID)
		if err != nil || result == nil || !result.AwaitingAuth || len(model.requests) != requests {
			t.Fatalf("expected authentication pause before model execution: result=%+v error=%v requests=%d", result, err, len(model.requests))
		}
		interactions, err := mem.ListInteractions(ctx, run.AppID, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		var pending string
		for _, interaction := range interactions {
			if interaction.Status == "pending" && interaction.InteractionKind == "authentication" {
				pending = interaction.ID
			}
		}
		if pending == "" {
			t.Fatal("missing authentication interaction")
		}
		if _, err := eng.ResumeRun(ctx, run.AppID, run.ID, ResumePayload{Intent: "auth_completed", InteractionID: pending, ResumeID: "auth-" + pending}); err != nil {
			t.Fatal(err)
		}
	}
	expired.Store(false)
	saved, err := mem.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := saved.Input.Metadata["last_resume"].(map[string]interface{})
	interrupted := last["interrupted_resume"].(map[string]interface{})
	if interrupted["content"] != question || interrupted["resume_id"] != "human-reply" || interrupted["interrupted_resume"] != nil {
		t.Fatalf("authentication retries lost or nested the original request: %+v", interrupted)
	}
	if _, err := eng.ExecuteRunOnce(ctx, run.AppID, run.ID); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, message := range model.requests[len(model.requests)-1].Messages {
		if message.Role == "user" && strings.Contains(message.Content, question) {
			count++
			if message.Provenance != "human" {
				t.Fatalf("user reply lost its provenance: %+v", message)
			}
		}
	}
	if count != 1 {
		t.Fatalf("after credential refresh, model received the user question %d times", count)
	}
	messages, err := mem.ListMessages(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	count = 0
	for _, message := range messages {
		if message.Role == "user" && message.Content == question {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("saved user message count = %d", count)
	}
	if _, err := eng.ResumeRun(ctx, run.AppID, run.ID, ResumePayload{Intent: "reply", Content: "Only include accounting companies.", ResumeID: "human-next", MessageProvenance: "human", ExternalActorID: "user-1"}); err != nil {
		t.Fatal(err)
	}
	saved, err = mem.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Input.Metadata["last_resume"].(map[string]interface{})["interrupted_resume"] != nil {
		t.Fatal("the next human request retained stale authentication context")
	}
}
