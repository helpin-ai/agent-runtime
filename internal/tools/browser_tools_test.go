package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type fakeBrowserRunner struct {
	mu       sync.Mutex
	calls    [][]string
	envs     [][]string
	pageURLs map[string]string
}

func (r *fakeBrowserRunner) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
	r.envs = append(r.envs, slices.Clone(env))
	if r.pageURLs == nil {
		r.pageURLs = map[string]string{}
	}
	sessionName := envValue(env, "AGENT_BROWSER_SESSION")
	for i, arg := range args {
		switch arg {
		case "open":
			if i+1 < len(args) {
				r.pageURLs[sessionName] = args[i+1]
				return []byte(`{"success":true,"data":{"url":"` + args[i+1] + `","title":"Fixture page"}}`), nil
			}
		case "get":
			if i+1 < len(args) && args[i+1] == "url" {
				return []byte(`{"success":true,"data":{"url":"` + r.pageURLs[sessionName] + `"}}`), nil
			}
			if i+1 < len(args) && args[i+1] == "title" {
				return []byte(`{"success":true,"data":{"title":"Fixture page"}}`), nil
			}
		case "screenshot":
			if err := os.WriteFile(args[len(args)-1], []byte("\x89PNG\r\n\x1a\nfixture"), 0600); err != nil {
				return nil, err
			}
		case "close":
			delete(r.pageURLs, sessionName)
		}
	}
	return []byte(`{"success":true,"data":{"snapshot":"button Submit [ref=e1]"}}`), nil
}

func browserTestCallContext(runID string) CallContext {
	run := &agentcore.AgentRun{ID: runID, AppID: "helpin"}
	return CallContext{AppID: run.AppID, RunID: run.ID, Run: run}
}

func browserTestCallContextForApp(appID, runID string) CallContext {
	run := &agentcore.AgentRun{ID: runID, AppID: appID}
	return CallContext{AppID: appID, RunID: run.ID, Run: run}
}

func TestBrowserActIsARoutineMutation(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, KernelAPIKey: "key", AllowedDomains: []string{"*"}, Runner: &fakeBrowserRunner{},
	})
	def, ok := registry.Definition("browser_act")
	if !ok {
		t.Fatal("browser_act is not registered")
	}
	if !def.Mutating {
		t.Fatal("browser_act must remain classified as mutating")
	}
	if got := def.EffectiveRiskLevel(); got != RiskLevelRoutine {
		t.Fatalf("browser_act risk level = %q, want %q", got, RiskLevelRoutine)
	}
}

func TestBrowserOpenUsesEphemeralRunSessionAndBoundedSnapshot(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://must-not-leak")
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "kernel-secret",
		AllowedDomains: []string{"stage.example.com"}, Runner: runner,
	})
	out, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://stage.example.com/app"}`))
	if err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if !strings.Contains(string(out), "snapshot") || !strings.Contains(string(out), `"url":"https://stage.example.com/app"`) || !strings.Contains(string(out), `"title":"Fixture page"`) {
		t.Fatalf("unexpected output: %s", out)
	}
	if len(runner.calls) != 6 {
		t.Fatalf("calls=%d, want viewport, open, settle wait, snapshot, URL, and title", len(runner.calls))
	}
	if got := runner.calls[0][len(runner.calls[0])-4:]; !slices.Equal(got, []string{"set", "viewport", "1440", "900"}) {
		t.Fatalf("default browser viewport call = %#v", got)
	}
	if got := runner.calls[2][len(runner.calls[2])-2:]; !slices.Equal(got, []string{"wait", "1500"}) {
		t.Fatalf("default browser settle call = %#v", got)
	}
	joinedEnv := strings.Join(runner.envs[0], "\n")
	if !strings.Contains(joinedEnv, "KERNEL_API_KEY=kernel-secret") || !strings.Contains(joinedEnv, "AGENT_BROWSER_SESSION=ar-") {
		t.Fatalf("missing Kernel run env: %s", joinedEnv)
	}
	if !strings.Contains(joinedEnv, "AGENT_BROWSER_ALLOWED_DOMAINS=stage.example.com") {
		t.Fatalf("explicit app domain policy was not forwarded: %s", joinedEnv)
	}
	if strings.Contains(joinedEnv, "KERNEL_PROFILE_NAME=") {
		t.Fatalf("persistent Kernel profile unexpectedly configured: %s", joinedEnv)
	}
	if strings.Contains(joinedEnv, "must-not-leak") {
		t.Fatalf("runtime secret leaked to browser subprocess: %s", joinedEnv)
	}
	if err := registry.CloseRun(context.Background(), "helpin", "run-1"); err != nil {
		t.Fatalf("close run: %v", err)
	}
	if got := runner.calls[len(runner.calls)-1][len(runner.calls[len(runner.calls)-1])-1]; got != "close" {
		t.Fatalf("last command=%q, want close", got)
	}
}

func TestBrowserOpenRejectsUnknownAndDisallowedDomain(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, KernelAPIKey: "key", AllowedDomains: []string{"example.com"}, Runner: &fakeBrowserRunner{}})
	callCtx := browserTestCallContext("run-1")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","extra":true}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected strict decode error, got %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://evil.example.net"}`)); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected domain error, got %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":10001}`)); err == nil || !strings.Contains(err.Error(), "wait_ms") {
		t.Fatalf("expected wait_ms validation error, got %v", err)
	}
}

func TestBrowserOpenSetsDefaultViewportOncePerSession(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key",
		AllowedDomains: []string{"example.com"}, Runner: runner,
	})
	callCtx := browserTestCallContext("run-1")
	for _, target := range []string{"https://example.com/one", "https://example.com/two"} {
		input := json.RawMessage(`{"url":"` + target + `","wait_ms":0}`)
		if _, err := registry.Execute(context.Background(), callCtx, "browser_open", input); err != nil {
			t.Fatalf("browser_open %s: %v", target, err)
		}
	}
	viewportCalls := 0
	for _, call := range runner.calls {
		if len(call) >= 4 && slices.Equal(call[len(call)-4:], []string{"set", "viewport", "1440", "900"}) {
			viewportCalls++
		}
	}
	if viewportCalls != 1 {
		t.Fatalf("default viewport calls=%d, want 1 per session", viewportCalls)
	}
}

func TestBrowserOpenAllowsAllDomainsWhenAppPolicyUsesWildcard(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"*"}, Runner: runner})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://arbitrary.example.net/login"}`)); err != nil {
		t.Fatalf("wildcard browser policy rejected URL: %v", err)
	}
	if allowedDomains := envValue(runner.envs[0], "AGENT_BROWSER_ALLOWED_DOMAINS"); allowedDomains != "" {
		t.Fatalf("wildcard must omit agent-browser's literal allowlist, got %q", allowedDomains)
	}
}

func TestBrowserSessionsAreIsolatedPerRun(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, KernelAPIKey: "key", AllowedDomains: []string{"example.com"}, Runner: runner})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://example.com/one","wait_ms":0}`)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-2"), "browser_open", json.RawMessage(`{"url":"https://example.com/two","wait_ms":0}`)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	sessionOne := envValue(runner.envs[0], "AGENT_BROWSER_SESSION")
	sessionTwo := envValue(runner.envs[5], "AGENT_BROWSER_SESSION")
	if sessionOne == "" || sessionTwo == "" || sessionOne == sessionTwo {
		t.Fatalf("run sessions must be non-empty and isolated: run-1=%q run-2=%q", sessionOne, sessionTwo)
	}
}

func TestBrowserSessionClosesAfterIdleTimeout(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"example.com"},
		SessionTimeoutSeconds: 1, CommandTimeout: time.Second, Runner: runner,
	})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("open: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		closed := len(runner.calls) > 1 && runner.calls[len(runner.calls)-1][len(runner.calls[len(runner.calls)-1])-1] == "close"
		runner.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("browser session was not closed after idle timeout")
}

func TestBrowserScreenshotUploadsWithoutReturningImageBytes(t *testing.T) {
	var uploaded []byte
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxBrowserScreenshotBytes); err != nil {
			t.Fatalf("parse upload: %v", err)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer file.Close()
		uploaded, _ = io.ReadAll(file)
		if r.FormValue("app_id") != "helpin" || r.FormValue("run_id") != "run-1" || r.FormValue("artifact_type") != "browser_screenshot" {
			t.Fatalf("unexpected artifact envelope: app=%q run=%q type=%q", r.FormValue("app_id"), r.FormValue("run_id"), r.FormValue("artifact_type"))
		}
		if r.FormValue("workspace_id") != "" {
			t.Fatalf("host-specific workspace_id leaked into generic artifact contract")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"asset-1","artifact_ref":"helpin://artifacts/asset-1","visibility":"private","file_name":"settings-page.png","content_type":"image/png","size_bytes":16}`))
	}))
	defer uploader.Close()
	registry := NewRegistry()
	runner := &fakeBrowserRunner{}
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"example.com"},
		Runner: runner, ArtifactUploadURL: uploader.URL,
	})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://example.com/settings","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	out, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_screenshot", json.RawMessage(`{"name":"Settings page","annotate":true,"full_page":true}`))
	if err != nil {
		t.Fatalf("browser_screenshot: %v", err)
	}
	if len(uploaded) == 0 {
		t.Fatal("screenshot was not uploaded")
	}
	if strings.Contains(string(out), "iVBOR") || !strings.Contains(string(out), "helpin://artifacts/asset-1") || strings.Contains(string(out), "object_key") || !strings.Contains(string(out), `"url":"https://example.com/settings"`) || !strings.Contains(string(out), `"full_page":true`) {
		t.Fatalf("unexpected screenshot output: %s", out)
	}
}

func TestBrowserScreenshotRejectsFreshBlankSession(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"*"},
		Runner: runner, ArtifactUploadURL: "https://host.test/artifacts",
	})
	_, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_screenshot", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "call browser_open") {
		t.Fatalf("expected unopened-page repair error, got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("blank session executed browser commands: %#v", runner.calls)
	}
}

func TestBrowserScreenshotOnlyRegisteredWithAppArtifactProvider(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry.ForApp("helpin"), BrowserToolsConfig{Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"*"}, Runner: &fakeBrowserRunner{}})
	defs := registry.CloneForApp("helpin").Definitions()
	for _, def := range defs {
		if def.Name == "browser_screenshot" {
			t.Fatal("browser_screenshot registered without an app artifact provider")
		}
	}
}

func TestBrowserToolsRejectAnotherAppContext(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry.ForApp("helpin"), BrowserToolsConfig{Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"*"}, Runner: &fakeBrowserRunner{}})
	run := &agentcore.AgentRun{ID: "run-1", AppID: "usermaven"}
	_, err := registry.CloneForApp("helpin").Execute(context.Background(), CallContext{AppID: "usermaven", RunID: run.ID, Run: run}, "browser_snapshot", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "not configured for app") {
		t.Fatalf("expected cross-app rejection, got %v", err)
	}
}

func TestBrowserSessionsAndPoliciesAreNamespacedPerApp(t *testing.T) {
	registry := NewRegistry()
	runnerA := &fakeBrowserRunner{}
	runnerB := &fakeBrowserRunner{}
	RegisterBrowserTools(registry.ForApp("app-a"), BrowserToolsConfig{
		Enabled: true, AppID: "app-a", KernelAPIKey: "key",
		AllowedDomains: []string{"a.example.com"}, Runner: runnerA,
	})
	RegisterBrowserTools(registry.ForApp("app-b"), BrowserToolsConfig{
		Enabled: true, AppID: "app-b", KernelAPIKey: "key",
		AllowedDomains: []string{"b.example.com"}, Runner: runnerB,
	})
	if _, err := registry.CloneForApp("app-a").Execute(context.Background(), browserTestCallContextForApp("app-a", "shared-run"), "browser_open", json.RawMessage(`{"url":"https://a.example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("app-a snapshot: %v", err)
	}
	if _, err := registry.CloneForApp("app-b").Execute(context.Background(), browserTestCallContextForApp("app-b", "shared-run"), "browser_open", json.RawMessage(`{"url":"https://b.example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("app-b snapshot: %v", err)
	}
	sessionA := envValue(runnerA.envs[0], "AGENT_BROWSER_SESSION")
	sessionB := envValue(runnerB.envs[0], "AGENT_BROWSER_SESSION")
	if sessionA == "" || sessionB == "" || sessionA == sessionB {
		t.Fatalf("sessions must be non-empty and app-isolated: app-a=%q app-b=%q", sessionA, sessionB)
	}
	if _, err := registry.CloneForApp("app-a").Execute(context.Background(), browserTestCallContextForApp("app-a", "run-c"), "browser_open", json.RawMessage(`{"url":"https://b.example.com"}`)); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("app-a accepted app-b domain policy: %v", err)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, value := range env {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return ""
}
