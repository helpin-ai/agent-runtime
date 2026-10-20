package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func registryForBrowser(m *BrowserManager) *Registry {
	r := NewRegistry()
	r.state.browsers = map[string]*BrowserManager{"": m}
	return r
}
func TestExtensionLaunchHasNoDebuggerOrAutomationFlags(t *testing.T) {
	args := extensionBrowserArgs("/tmp/profile", "https://example.com", "/tmp/extension")
	for _, arg := range args {
		if strings.Contains(arg, "debug") || strings.Contains(arg, "Automation") || strings.Contains(arg, "enable-automation") {
			t.Fatalf("automation flag: %s", arg)
		}
	}
	raw, _ := browserExtensionFiles.ReadFile("browser_extension/manifest.json")
	var manifest struct {
		Permissions           []string `json:"permissions"`
		ExternallyConnectable any      `json:"externally_connectable"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(manifest.Permissions, "debugger") || manifest.ExternallyConnectable != nil {
		t.Fatal("extension must have no debugger or web-page control channel")
	}
}

func TestExtensionBridgeScopesResponsesAndRejectsPageOrigins(t *testing.T) {
	m := newBrowserManager(BrowserToolsConfig{ProfileRoot: t.TempDir()})
	t.Cleanup(func() { _ = registryForBrowser(m).CloseBrowsers(context.Background()) })
	first, err := m.nativeExtension("test", "first", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.nativeExtension("test", "second", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.token == second.token {
		t.Fatal("shared bridge credential")
	}
	handler := http.HandlerFunc(m.extensionBridge)
	post := func(token, origin, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/extension/browser", strings.NewReader(`{"id":"`+id+`","result":{"success":true,"data":{"text":"ok"}}}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", origin)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	if got := post(first.token, "https://attacker.example", "x").Code; got != 403 {
		t.Fatalf("page origin: %d", got)
	}
	if got := post("wrong", browserExtensionOrigin, "x").Code; got != 401 {
		t.Fatalf("bad token: %d", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { _, err := first.execute(ctx, "run-first", []string{"get", "text"}); completed <- err }()
	command := <-first.queue
	if got := post(second.token, browserExtensionOrigin, command.ID).Code; got != 409 {
		t.Fatalf("other bot response: %d", got)
	}
	if got := post(first.token, browserExtensionOrigin, command.ID).Code; got != 204 {
		t.Fatalf("owner response: %d", got)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if got := post(first.token, browserExtensionOrigin, command.ID).Code; got != 409 {
		t.Fatalf("replayed response: %d", got)
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if _, err := first.execute(ctx, "expired", []string{"click", "@e1"}); err == nil {
		t.Fatal("cancelled command accepted")
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	if len(first.pending) != 0 {
		t.Fatal("expired command retained")
	}
}

// Exercise the production launcher with automatic extension loading, never CDP.
func TestExtensionBrowserWithoutDevTools(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_EXTENSION_E2E") != "1" {
		t.Skip("set AGENT_RUNTIME_EXTENSION_E2E=1 to run Chrome for Testing without DevTools")
	}
	executable := firstNonEmptyString(os.Getenv("AGENT_RUNTIME_BROWSER_EXTENSION_EXECUTABLE"), BundledExtensionBrowser())
	if executable == "" {
		t.Fatal("Chrome for Testing is not installed")
	}
	m := newBrowserManager(BrowserToolsConfig{ProfileRoot: t.TempDir(), ExtensionExecutable: executable})
	r := registryForBrowser(m)
	t.Cleanup(func() { _ = r.CloseBrowsers(context.Background()) })
	agent := &agentcore.Agent{ID: "first", AppID: "test", AllowedTools: []string{"browser_open"}, ExecutionConfig: json.RawMessage(`{"browser":{"backend":"extension"}}`)}
	session := &browserRunSession{appID: "test", agentID: "first", runID: "test-session", sessionName: "test-session", policy: BrowserPolicy{Backend: "extension"}}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			cookie, _ := r.Cookie("rex")
			value := ""
			if cookie != nil {
				value = cookie.Value
			}
			_, _ = w.Write([]byte(`<title>Next</title><p>Done</p><p id="cookie">` + value + `</p>`))
			return
		}
		_, _ = w.Write([]byte(`<title>Fixture</title>
<label>Email<input id="email" oninput="document.getElementById('trusted').textContent=String(event.isTrusted)"></label><p id="trusted"></p>
<label>Note<textarea oninput="document.getElementById('note-value').textContent=this.value">seed</textarea></label><pre id="note-value"></pre>
<label>Choice<select onchange="document.getElementById('selected').textContent=this.value"><option value="">None</option><option value="b">Beta</option></select></label><p id="selected"></p>
<label>Agree<input type="checkbox" onchange="document.getElementById('checked').textContent=String(this.checked)"></label><p id="checked"></p>
<label>Cancelled<input type="checkbox" onclick="event.preventDefault()"></label>
<label>Radio<input type="radio" checked></label>
<button disabled>Disabled</button>
<input type="file" aria-label="Upload"><div id="captcha"><button>Verify</button></div>
<button onclick="document.getElementById('result').textContent=document.getElementById('email').value">Save</button><p id="result"></p><p id="webdriver"></p><a href="/next">Next</a>
<script>document.cookie='rex=kept; path=/; max-age=86400';document.getElementById('webdriver').textContent=String(navigator.webdriver)</script>`))
	}))
	defer fixture.Close()
	ext, err := m.nativeExtension("test", "first", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.launchNativeExtension(ext); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if ext.process != nil {
			_ = ext.process.Process.Kill()
			<-ext.exited
		}
	}()
	call := func(args ...string) map[string]any {
		t.Helper()
		raw, err := m.nativeExtensionCommand(t.Context(), session, args)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Success bool           `json:"success"`
			Error   string         `json:"error"`
			Data    map[string]any `json:"data"`
		}
		if json.Unmarshal(raw, &out) != nil || !out.Success {
			t.Fatalf("%v: %s", args, raw)
		}
		return out.Data
	}
	call("open", fixture.URL)
	if got := call("get", "text", "#webdriver")["text"]; got != "false" {
		t.Fatalf("browser reports automation: %v", got)
	}
	snap := call("snapshot", "-i")["snapshot"].(string)
	find := func(label string) string {
		for _, line := range strings.Split(snap, "\n") {
			if strings.Contains(line, label) {
				_, rest, _ := strings.Cut(line, "[ref=")
				ref, _, _ := strings.Cut(rest, "]")
				return "@" + ref
			}
		}
		t.Fatalf("missing %s: %s", label, snap)
		return ""
	}
	email, save, next := find("Email"), find("Save"), find("Next")
	for _, test := range []struct{ ref, want string }{{find("Verify"), "human takeover"}, {find("Upload"), "File uploads"}} {
		raw, err := m.nativeExtensionCommand(t.Context(), session, []string{"click", test.ref})
		if err != nil || !bytes.Contains(raw, []byte(test.want)) {
			t.Fatalf("human-required control: %s %v", raw, err)
		}
	}

	call("fill", email, "extension works")
	if got := call("get", "text", "#trusted")["text"]; got != "false" {
		t.Fatalf("input event trust=%v", got)
	}
	call("click", save)
	if text := call("get", "text", "#result")["text"]; text != "extension works" {
		t.Fatalf("input result: %v", text)
	}
	t.Run("form_controls", func(t *testing.T) {
		for _, ref := range []string{save, strings.TrimPrefix(save, "@"), find("Disabled")} {
			if got := call("get", "text", ref)["text"]; got != "Save" && got != "Disabled" {
				t.Fatalf("reference text: %v", got)
			}
		}
		note := find("Note")
		call("fill", note, "")
		call("type", note, "  new note  ")
		if got := call("get", "text", "#note-value")["text"]; got != "  new note  " {
			t.Fatalf("type restored default text or lost whitespace: %q", got)
		}
		call("fill", note, "replacement")
		call("type", note, " appended")
		if got := call("get", "text", "#note-value")["text"]; got != "replacement appended" {
			t.Fatalf("textarea result: %v", got)
		}
		call("select", find("Choice"), "b")
		if got := call("get", "text", "#selected")["text"]; got != "b" {
			t.Fatalf("select result: %v", got)
		}
		call("select", find("Choice"), "")
		if got := call("get", "text", "#selected")["text"]; got != "" {
			t.Fatalf("empty select result: %v", got)
		}
		call("check", find("Agree"))
		if got := call("get", "text", "#checked")["text"]; got != "true" {
			t.Fatalf("check result: %v", got)
		}
		call("uncheck", find("Agree"))
		if got := call("get", "text", "#checked")["text"]; got != "false" {
			t.Fatalf("uncheck result: %v", got)
		}
		for _, test := range []struct{ action, ref, want string }{
			{"uncheck", find("Radio"), "Radio buttons cannot be unchecked"},
			{"check", find("Cancelled"), "did not change"},
			{"click", find("Disabled"), "disabled"},
		} {
			raw, err := m.nativeExtensionCommand(t.Context(), session, []string{test.action, test.ref})
			if err != nil || !bytes.Contains(raw, []byte(test.want)) {
				t.Fatalf("control must fail explicitly: %s %v", raw, err)
			}
		}
	})
	raw, _ := m.nativeExtensionCommand(t.Context(), session, []string{"get", "text", "body"})
	if !bytes.Contains(raw, []byte("extension works")) {
		t.Fatal("read lost page text")
	}
	image := call("screenshot", "png")["image"].(string)
	if !strings.HasPrefix(image, "data:image/png;base64,") {
		t.Fatal("no visible-tab screenshot")
	}
	call("click", next)
	call("wait", "500")
	if got := call("get", "url")["url"]; got != fixture.URL+"/next" {
		t.Fatalf("navigation: %v", got)
	}
	call("snapshot", "-i")
	raw, err = m.nativeExtensionCommand(t.Context(), session, []string{"click", save})
	if err != nil || !bytes.Contains(raw, []byte("Stale element reference")) {
		t.Fatalf("stale ref: %s %v", raw, err)
	}
	for _, ref := range []string{save, strings.TrimPrefix(save, "@")} {
		raw, err = m.nativeExtensionCommand(t.Context(), session, []string{"get", "text", ref})
		if err != nil || !bytes.Contains(raw, []byte("Stale element reference")) {
			t.Fatalf("stale read ref: %s %v", raw, err)
		}
	}
	call("open", fixture.URL)
	snap = call("snapshot", "-i")["snapshot"].(string)
	email = find("Email")
	if _, err := r.BrowserControl(t.Context(), agent, "takeover"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BrowserControl(t.Context(), agent, "handback"); err != nil {
		t.Fatal(err)
	}
	raw, err = m.nativeExtensionCommand(t.Context(), session, []string{"fill", email, "stale after takeover"})
	if err != nil || !bytes.Contains(raw, []byte("Stale element reference")) {
		t.Fatalf("takeover ref survived: %s %v", raw, err)
	}
	call("close")
	if err := r.CloseBrowsers(t.Context()); err != nil {
		t.Fatal(err)
	}
	m = newBrowserManager(m.cfg)
	r = registryForBrowser(m)
	ext, err = m.nativeExtension("test", "first", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.launchNativeExtension(ext); err != nil {
		t.Fatal(err)
	}
	call("wait", "500")
	if got := call("get", "url")["url"]; got != fixture.URL+"/" {
		t.Fatalf("page lost across runtime restart: %v", got)
	}
	call("open", fixture.URL+"/next")
	if got := call("get", "text", "#cookie")["text"]; got != "kept" {
		t.Fatalf("profile lost cookie across restart: %v", got)
	}
}

func TestExtensionNavigationScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is needed for service worker unit tests")
	}
	out, err := exec.Command(node, "--test", "browser_extension_navigation.test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("navigation tests: %v\n%s", err, out)
	}
}

func TestBundledExtensionBrowserNativeLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	layout := []string{"chrome-linux64", "chrome"}
	switch runtime.GOOS {
	case "windows":
		layout = []string{"chrome-win64", "chrome.exe"}
	case "darwin":
		layout = []string{"Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"}
	}
	var newest string
	for _, version := range []string{"149.0.1", "99.0.1", "148.0.2"} {
		path := filepath.Join(append([]string{home, ".agent-browser", "browsers", "chrome-" + version}, layout...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0700); err != nil {
			t.Fatal(err)
		}
		if version == "149.0.1" {
			newest = path
		}
	}
	if got := BundledExtensionBrowser(); got != newest {
		t.Fatalf("browser = %q, want %q", got, newest)
	}
}
