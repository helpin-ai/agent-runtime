package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestBrowserControlsAreAuthenticatedScopedAndHostOnly(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	tools.RegisterBrowserTools(registry.ForApp("app-a"), tools.BrowserToolsConfig{Enabled: true, AppID: "app-a", ProfileRoot: t.TempDir()})
	t.Cleanup(func() { _ = registry.CloseBrowsers(context.Background()) })
	agent := agentcore.Agent{ID: "bot", AppID: "app-a", Name: "Bot", RuntimeKind: "native_sdk", AllowedTools: []string{"browser_open"}, ExecutionConfig: json.RawMessage(`{"browser":{"backend":"extension"}}`)}
	if err := mem.CreateAgent(t.Context(), &agent); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(Config{Store: mem, Tools: registry, ServiceToken: "service-secret"})
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	for _, token := range []string{"", "incorrect"} {
		if got := request("GET", "/v1/browser/bot?app_id=app-a", token, "").Code; got != 401 {
			t.Fatal(got)
		}
	}
	if got := request("GET", "/v1/browser/bot?app_id=other-app", "service-secret", "").Code; got != 404 {
		t.Fatal(got)
	}
	if got := request("GET", "/v1/browser/bot", "service-secret", "").Code; got != 400 {
		t.Fatal(got)
	}
	out := request("GET", "/v1/browser/bot?app_id=app-a", "service-secret", "")
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"backend":"extension"`) || strings.Contains(out.Body.String(), "token") {
		t.Fatalf("status: %d %s", out.Code, out.Body)
	}
	out = request("POST", "/v1/browser/bot?app_id=app-a", "service-secret", `{"action":"handback"}`)
	if out.Code != 200 {
		t.Fatalf("handback: %d %s", out.Code, out.Body)
	}
	if got := request("POST", "/v1/browser/bot?app_id=app-a", "service-secret", `{"action":"click"}`).Code; got != 409 {
		t.Fatal(got)
	}
	if _, ok := registry.DefinitionForApp("app-a", "browser_takeover"); ok {
		t.Fatal("host takeover exposed as a model tool")
	}
	for _, raw := range []string{`{"browser":{"backend":"auto"}}`, `{"browser":{"profile":"../another"}}`} {
		body := `{"name":"Invalid","runtime_kind":"native_sdk","execution_config":` + raw + `}`
		out = request(http.MethodPut, "/v1/agents/invalid?app_id=app-a", "service-secret", body)
		if out.Code != 400 {
			t.Fatalf("invalid policy admitted: %d %s", out.Code, out.Body)
		}
	}
}
