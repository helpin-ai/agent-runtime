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

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type fakeBrowserRunner struct {
	mu    sync.Mutex
	calls [][]string
	envs  [][]string
}

func (r *fakeBrowserRunner) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
	r.envs = append(r.envs, slices.Clone(env))
	for i, arg := range args {
		if arg == "screenshot" && i+1 < len(args) {
			if err := os.WriteFile(args[i+1], []byte("\x89PNG\r\n\x1a\nfixture"), 0600); err != nil {
				return nil, err
			}
		}
	}
	return []byte(`{"success":true,"data":{"snapshot":"button Submit [ref=e1]"}}`), nil
}

func browserTestCallContext(runID, workspaceID string) CallContext {
	run := &agentcore.AgentRun{ID: runID, AppID: "helpin", Input: agentcore.RunInput{Metadata: map[string]interface{}{"workspace_id": workspaceID}}}
	return CallContext{AppID: run.AppID, RunID: run.ID, Run: run}
}

func TestBrowserOpenUsesWorkspaceProfileAndBoundedSnapshot(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://must-not-leak")
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, KernelAPIKey: "kernel-secret", ProfileNameSalt: "salt",
		AllowedDomains: []string{"stage.example.com"}, Runner: runner,
	})
	out, err := registry.Execute(context.Background(), browserTestCallContext("run-1", "ws-1"), "browser_open", json.RawMessage(`{"url":"https://stage.example.com/app"}`))
	if err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if !strings.Contains(string(out), "snapshot") {
		t.Fatalf("unexpected output: %s", out)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%d, want open and snapshot", len(runner.calls))
	}
	joinedEnv := strings.Join(runner.envs[0], "\n")
	if !strings.Contains(joinedEnv, "KERNEL_API_KEY=kernel-secret") || !strings.Contains(joinedEnv, "KERNEL_PROFILE_NAME=ar-") {
		t.Fatalf("missing Kernel run env: %s", joinedEnv)
	}
	if strings.Contains(joinedEnv, "ws-1") {
		t.Fatalf("workspace id leaked in profile env: %s", joinedEnv)
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
	callCtx := browserTestCallContext("run-1", "ws-1")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","extra":true}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected strict decode error, got %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://evil.example.net"}`)); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected domain error, got %v", err)
	}
}

func TestBrowserProfileAllowsOnlyOneRunAtATime(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, KernelAPIKey: "key", AllowedDomains: []string{"example.com"}, Runner: &fakeBrowserRunner{}})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-1", "ws-1"), "browser_snapshot", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-2", "ws-1"), "browser_snapshot", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("expected profile contention, got %v", err)
	}
	if err := registry.CloseRun(context.Background(), "helpin", "run-1"); err != nil {
		t.Fatalf("close first run: %v", err)
	}
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-2", "ws-1"), "browser_snapshot", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("second run after close: %v", err)
	}
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"asset_id":"asset-1","url":"https://assets.example.com/shot.png","object_key":"shots/shot.png"}`))
	}))
	defer uploader.Close()
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, KernelAPIKey: "key", AllowedDomains: []string{"example.com"},
		Runner: &fakeBrowserRunner{}, AssetUploadURL: uploader.URL,
	})
	out, err := registry.Execute(context.Background(), browserTestCallContext("run-1", "ws-1"), "browser_screenshot", json.RawMessage(`{"name":"Settings page","annotate":true}`))
	if err != nil {
		t.Fatalf("browser_screenshot: %v", err)
	}
	if len(uploaded) == 0 {
		t.Fatal("screenshot was not uploaded")
	}
	if strings.Contains(string(out), "iVBOR") || !strings.Contains(string(out), "asset-1") {
		t.Fatalf("unexpected screenshot output: %s", out)
	}
}
