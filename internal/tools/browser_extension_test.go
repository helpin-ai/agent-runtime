package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostSelectedExtensionUsesExistingToolsWithoutAgentBrowser(t *testing.T) {
	var selected, commands int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-host-token" {
			t.Error("missing host authentication")
			w.WriteHeader(401)
			return
		}
		var request struct {
			AppID   string `json:"app_id"`
			RunID   string `json:"run_id"`
			Session string `json:"session"`
			Action  string `json:"action"`
			Command []string
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.AppID != "helpin" || request.RunID != "extension-run" || request.Session == "" {
			t.Errorf("missing scope: %+v", request)
		}
		if request.Action == "resolve" {
			selected++
			_, _ = w.Write([]byte(`{"backend":"extension"}`))
			return
		}
		commands++
		data := map[string]any{}
		switch request.Command[0] {
		case "open", "get":
			if len(request.Command) > 1 && request.Command[1] == "text" {
				data["text"] = "Fixture contents"
			} else {
				data["url"] = "https://example.com/"
				data["title"] = "Fixture"
			}
		case "snapshot":
			data["snapshot"] = `- button "Save" [ref=e1]`
		case "click":
		default:
			t.Errorf("unexpected command: %v", request.Command)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}))
	defer server.Close()
	registry := NewRegistry()
	runner := &fakeBrowserRunner{}
	kernel := &fakeKernelProvider{}
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, HostURL: server.URL, HostToken: "private-host-token", Runner: runner, Kernel: kernel, AllowedDomains: []string{"example.com"}})
	call := browserTestCallContext("extension-run")
	for _, tool := range []struct{ name, input string }{
		{"browser_open", `{"url":"https://example.com/","wait_ms":0}`},
		{"browser_act", `{"action":"click","ref":"@e1"}`},
		{"browser_read", `{}`},
	} {
		out, err := registry.Execute(context.Background(), call, tool.name, json.RawMessage(tool.input))
		if err != nil {
			t.Fatalf("%s: %v", tool.name, err)
		}
		if tool.name == "browser_read" && (!strings.Contains(string(out), "Fixture contents") || !strings.Contains(string(out), "_boundary")) {
			t.Fatalf("lost text or boundary: %s", out)
		}
	}
	if selected != 1 || commands == 0 || len(runner.calls) != 0 || len(kernel.creates) != 0 {
		t.Fatalf("driver calls: select=%d commands=%d runner=%d kernel=%d", selected, commands, len(runner.calls), len(kernel.creates))
	}
}

func TestExtensionScreenshotsStayInRuntimeAndRejectUnsupportedModes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"image":"data:image/png;base64,aW1hZ2U="}}`))
	}))
	defer server.Close()
	manager := newBrowserManager(BrowserToolsConfig{HostURL: server.URL, HostToken: "token"})
	session := &browserRunSession{backend: browserBackendExtension, appID: "app", runID: "run", sessionName: "session"}
	path := filepath.Join(t.TempDir(), "screen.png")
	if _, err := manager.extensionCommand(t.Context(), session, 1000, []string{"--screenshot-format", "png"}, []string{"screenshot", path}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "image" {
		t.Fatal("screenshot not saved in runtime")
	}
	if _, err := manager.extensionCommand(t.Context(), session, 1000, []string{"--screenshot-format", "png"}, []string{"screenshot", "--full", path}); err == nil {
		t.Fatal("unsupported full-page capture accepted")
	}
}

func TestExplicitCloudWithoutKernelDoesNotFallBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"backend":"cloud"}`)) }))
	defer server.Close()
	runner := &fakeBrowserRunner{}
	manager := newBrowserManager(BrowserToolsConfig{HostURL: server.URL, Runner: runner})
	session := &browserRunSession{appID: "app", runID: "run", sessionName: "session"}
	if err := manager.ensureConnectedLocked(t.Context(), session, false, "session"); err == nil || session.connected || len(runner.calls) != 0 {
		t.Fatalf("cloud silently fell back: %v", err)
	}
}
