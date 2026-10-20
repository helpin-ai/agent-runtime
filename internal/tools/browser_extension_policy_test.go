package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func nativeTestAgent() *agentcore.Agent {
	return &agentcore.Agent{ID: "agent", AppID: "app", AllowedTools: []string{"browser_open"}, ExecutionConfig: json.RawMessage(`{"browser":{"backend":"extension","profile":"saved-profile"}}`)}
}

func TestNativeBrowserPolicyIsValidatedFrozenAndScoped(t *testing.T) {
	for _, raw := range []string{`{"browser":{"backend":"auto"}}`, `{"browser":{"profile":"../other"}}`, `{"browser":{"profile":"/private"}}`, `{"browser":{"profile":"a\\b"}}`} {
		if _, err := BrowserPolicyFromConfig(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	r := NewRegistry()
	RegisterBrowserTools(r, BrowserToolsConfig{Enabled: true, ProfileRoot: t.TempDir(), ProfileAppID: "app"})
	t.Cleanup(func() { _ = r.CloseBrowsers(context.Background()) })
	agent := nativeTestAgent()
	metadata := map[string]interface{}{BrowserPolicyMetadataKey: map[string]string{"backend": "standard"}}
	if err := r.FreezeBrowserPolicy(agent, metadata); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{AppID: "app", ID: "run", AgentID: agent.ID, Input: agentcore.RunInput{Metadata: metadata}}
	agent.ExecutionConfig = json.RawMessage(`{"browser":{"backend":"standard"}}`)
	session, err := r.browserManager("app").session(CallContext{AppID: "app", RunID: "run", Agent: agent, Run: run})
	if err != nil || session.policy.Backend != "extension" {
		t.Fatalf("session=%+v err=%v", session, err)
	}
	m := r.browserManager("app")
	e, err := m.nativeExtension("app", "agent", "saved-profile")
	if err != nil {
		t.Fatal(err)
	}
	if e.profile != filepath.Join(m.cfg.ProfileRoot, "saved-profile") {
		t.Fatalf("profile changed: %s", e.profile)
	}
	if _, err := m.nativeExtension("app", "other-agent", "saved-profile"); err == nil {
		t.Fatal("another agent claimed profile")
	}
	other, err := m.nativeExtension("another-app", "agent", "saved-profile")
	if err != nil {
		t.Fatal(err)
	}
	if other.profile == e.profile || other.token == e.token {
		t.Fatal("app scope reused")
	}
	if err := e.setHeld(true); err != nil {
		t.Fatal(err)
	}
	// Restart recreates the manager and rotates credentials without releasing takeover.
	m2 := newBrowserManager(m.cfg)
	r2 := registryForBrowser(m2)
	t.Cleanup(func() { _ = r2.CloseBrowsers(context.Background()) })
	restored, err := m2.nativeExtension("app", "agent", "saved-profile")
	if err != nil {
		t.Fatal(err)
	}
	if !restored.status().Held || restored.token == e.token {
		t.Fatal("restart released takeover or retained credential")
	}
}

func TestNativeTakeoverGateAndRunLease(t *testing.T) {
	m := newBrowserManager(BrowserToolsConfig{ProfileRoot: t.TempDir()})
	r := registryForBrowser(m)
	t.Cleanup(func() { _ = r.CloseBrowsers(context.Background()) })
	e, err := m.nativeExtension("app", "agent", "saved-profile")
	if err != nil {
		t.Fatal(err)
	}
	session := &browserRunSession{appID: "app", agentID: "agent", runID: "run", sessionName: "run", policy: BrowserPolicy{Backend: "extension", Profile: "saved-profile"}}
	e.runID = "another-run"
	if _, err := m.nativeExtensionCommand(t.Context(), session, []string{"snapshot"}); err == nil || !strings.Contains(err.Error(), "different run") {
		t.Fatalf("lease error=%v", err)
	}
	e.runID = session.runID
	if err := e.setHeld(true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.nativeExtensionCommand(t.Context(), session, []string{"click", "@e1"}); err == nil || !strings.Contains(err.Error(), "user holds") {
		t.Fatalf("takeover error=%v", err)
	}
	if _, err := m.nativeExtensionCommand(t.Context(), session, []string{"close"}); err != nil {
		t.Fatal(err)
	}
	if e.status().RunID != "" || !e.status().Held {
		t.Fatal("run cleanup failed or released takeover")
	}
	oldEpoch := e.epoch
	// Handback is serialized behind the current operation.
	e.gate.Lock()
	handedBack := make(chan error, 1)
	go func() {
		_, err := r.BrowserControl(context.Background(), nativeTestAgent(), "handback")
		handedBack <- err
	}()
	select {
	case <-handedBack:
		t.Fatal("handback passed active operation")
	case <-time.After(30 * time.Millisecond):
	}
	e.gate.Unlock()
	if err := <-handedBack; err != nil {
		t.Fatal(err)
	}
	if e.status().Held || e.epoch == oldEpoch {
		t.Fatal("handback failed to invalidate references")
	}
}

func TestNativeExtensionBusyProfileNeverRewritesConfiguration(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "saved-profile")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("existing-browser", filepath.Join(profile, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	m := newBrowserManager(BrowserToolsConfig{ProfileRoot: root, ProfileAppID: "app"})
	r := registryForBrowser(m)
	t.Cleanup(func() { _ = r.CloseBrowsers(context.Background()) })
	if _, err := m.nativeExtension("app", "agent", "saved-profile"); err == nil {
		t.Fatal("busy profile accepted")
	}
	matches, _ := filepath.Glob(filepath.Join(root, ".runtime-extensions", "*", "bridge.json"))
	if len(matches) != 0 {
		t.Fatal("configuration written while another browser owns profile")
	}
}

func TestNativeExtensionToolsUseRuntimeBridgeWithoutHost(t *testing.T) {
	r := NewRegistry()
	runner := &fakeBrowserRunner{}
	RegisterBrowserTools(r, BrowserToolsConfig{Enabled: true, ProfileRoot: t.TempDir(), Runner: runner, AllowedDomains: []string{"example.com"}})
	t.Cleanup(func() { _ = r.CloseBrowsers(context.Background()) })
	agent := nativeTestAgent()
	m := r.browserManager("app")
	e, err := m.nativeExtension("app", "agent", "saved-profile")
	if err != nil {
		t.Fatal(err)
	}
	// An already-owned process avoids launching Chrome in this protocol test.
	e.process = &execCommandFixture
	e.exited = make(chan struct{})
	e.seen = time.Now()
	defer func() { e.mu.Lock(); e.process = nil; e.mu.Unlock() }()
	run := &agentcore.AgentRun{AppID: "app", AgentID: agent.ID, ID: "run"}
	call := CallContext{AppID: "app", RunID: "run", Run: run, Agent: agent}
	serveCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			req, _ := http.NewRequestWithContext(serveCtx, "GET", m.bridgeURL, nil)
			req.Header.Set("Authorization", "Bearer "+e.token)
			req.Header.Set("Origin", browserExtensionOrigin)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			if resp.StatusCode != 200 {
				resp.Body.Close()
				continue
			}
			var command extensionCommand
			err = json.NewDecoder(resp.Body).Decode(&command)
			resp.Body.Close()
			if err != nil {
				return
			}
			data := map[string]any{}
			switch command.Args[0] {
			case "open":
				data = map[string]any{"url": "https://example.com/", "title": "Fixture"}
			case "snapshot":
				data["snapshot"] = `- button "Save" [ref=e9ba8f9718052380a_3_9]`
			case "click", "fill":
				if command.Args[1] != "@e9ba8f9718052380a_3_9" {
					t.Errorf("snapshot reference not normalized: %v", command.Args)
				}
			case "get":
				switch command.Args[1] {
				case "url":
					data["url"] = "https://example.com/"
				case "title":
					data["title"] = "Fixture"
				case "text":
					data["text"] = "Native runtime fixture"
				}
			}
			raw, _ := json.Marshal(map[string]any{"id": command.ID, "result": map[string]any{"success": true, "data": data}})
			req, _ = http.NewRequestWithContext(serveCtx, "POST", m.bridgeURL, strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer "+e.token)
			req.Header.Set("Origin", browserExtensionOrigin)
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
		}
	}()
	for _, tool := range []struct{ name, input string }{
		{"browser_open", `{"url":"https://example.com/","wait_ms":0}`},
		{"browser_read", `{}`},
		{"browser_act", `{"action":"click","ref":"e9ba8f9718052380a_3_9"}`},
		{"browser_act", `{"action":"click","ref":"@e9ba8f9718052380a_3_9"}`},
		{"browser_act", `{"action":"fill","ref":"e9ba8f9718052380a_3_9","value":"Invitation note"}`},
		{"browser_act", `{"action":"fill","ref":"@e9ba8f9718052380a_3_9","value":"Invitation note"}`},
	} {
		raw, err := r.Execute(t.Context(), call, tool.name, json.RawMessage(tool.input))
		if err != nil {
			t.Fatalf("%s: %v", tool.name, err)
		}
		if tool.name == "browser_read" && (!strings.Contains(string(raw), "Native runtime fixture") || !strings.Contains(string(raw), "_boundary")) {
			t.Fatalf("lost boundary: %s", raw)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatal("native extension invoked agent-browser")
	}
	if err := e.setHeld(true); err != nil {
		t.Fatal(err)
	}
	oldEpoch := e.epoch
	if err := r.ReleasePausedBrowser(t.Context(), "app", "run"); err != nil {
		t.Fatal(err)
	}
	if status := e.status(); status.RunID != "" || !status.Held || !status.Running || e.epoch == oldEpoch {
		t.Fatal("pause did not release ownership while preserving browser and takeover")
	}
	if m.sessions["app/run"] == nil {
		t.Fatal("pause erased resumable session")
	}
	if _, err := r.BrowserControl(t.Context(), agent, "handback"); err != nil {
		t.Fatal(err)
	}
	nextRun := &agentcore.AgentRun{AppID: "app", AgentID: agent.ID, ID: "next-run"}
	nextCall := CallContext{AppID: "app", RunID: nextRun.ID, Run: nextRun, Agent: agent}
	if _, err := r.Execute(t.Context(), nextCall, "browser_open", json.RawMessage(`{"url":"https://example.com/","wait_ms":0}`)); err != nil {
		t.Fatalf("new run blocked by paused owner: %v", err)
	}
	if err := r.CloseRun(t.Context(), "app", "run"); err != nil {
		t.Fatal(err)
	}
	if e.status().RunID != nextRun.ID {
		t.Fatal("old cleanup released the new owner's lease")
	}
	if err := r.CloseRun(t.Context(), "app", nextRun.ID); err != nil {
		t.Fatal(err)
	}
	if e.status().RunID != "" || e.process == nil {
		t.Fatal("run cleanup closed the persistent browser")
	}
}

// A process marker is sufficient because protocol tests never shut it down.
var execCommandFixture = exec.Cmd{}

func TestNativeBridgeRejectsUntrustedOrigin(t *testing.T) {
	m := newBrowserManager(BrowserToolsConfig{ProfileRoot: t.TempDir()})
	r := registryForBrowser(m)
	t.Cleanup(func() { _ = r.CloseBrowsers(context.Background()) })
	e, err := m.nativeExtension("app", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/extension/browser", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Origin", "https://page.example")
	out := httptest.NewRecorder()
	m.extensionBridge(out, req)
	if out.Code != 403 {
		t.Fatal(out.Code)
	}
}

func TestNativeHandbackSchedulesIdleCleanup(t *testing.T) {
	m := newBrowserManager(BrowserToolsConfig{ProfileRoot: t.TempDir()})
	r := registryForBrowser(m)
	t.Cleanup(func() { _ = r.CloseBrowsers(context.Background()) })
	e, err := m.nativeExtension("app", "agent", "saved-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.setHeld(true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BrowserControl(t.Context(), nativeTestAgent(), "handback"); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	scheduled := e.idleTimer != nil
	e.mu.Unlock()
	if !scheduled || e.status().Held {
		t.Fatal("idle browser was not released for cleanup")
	}
}
