package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Runs under the existing native extension fixture on macOS and Windows.
func testExtensionDialogs(t *testing.T, executable string) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<title>Dialog fixture</title>
<div role="dialog" hidden><button>Hidden dialog button</button></div>
<button id="connect" onclick="setTimeout(()=>document.getElementById('modal').hidden=false,75)">Connect</button>
<main id="background"></main>
<div role="dialog" id="modal" hidden><h2>Add a note to your invitation?</h2><note-controls></note-controls></div>
<p id="result"></p><p id="clicks">0</p>
<div id="captcha"><guarded-control></guarded-control></div>
<div inert><inert-control></inert-control></div>
<div role="dialog" style="display:contents"><button>Contents wrapper button</button></div>
<script>
document.getElementById('background').innerHTML='<button>Background action</button>'.repeat(350);
document.documentElement.appendChild(document.getElementById('modal'));
const controls=document.querySelector('note-controls').attachShadow({mode:'open'});
controls.innerHTML='<button id="note">Add a note</button><button>Send without a note</button><nested-note hidden></nested-note>';
const nested=controls.querySelector('nested-note');
nested.attachShadow({mode:'open'}).innerHTML='<label id="label">Invitation note</label><textarea aria-labelledby="label"></textarea><button id="save">Save draft</button>';
controls.getElementById('note').onclick=()=>{document.getElementById('clicks').textContent=String(Number(document.getElementById('clicks').textContent)+1);nested.hidden=false};
nested.shadowRoot.getElementById('save').onclick=()=>document.getElementById('result').textContent=nested.shadowRoot.querySelector('textarea').value;
document.querySelector('guarded-control').attachShadow({mode:'open'}).innerHTML='<button>Verify shadow</button>';
document.querySelector('inert-control').attachShadow({mode:'open'}).innerHTML='<button>Inert shadow</button>';
</script>`)
	}))
	defer fixture.Close()
	m := newBrowserManager(BrowserToolsConfig{ProfileRoot: t.TempDir(), ExtensionExecutable: executable})
	t.Cleanup(func() { _ = registryForBrowser(m).CloseBrowsers(context.Background()) })
	session := &browserRunSession{appID: "test", agentID: "dialogs", runID: "dialog-run", sessionName: "dialog-run", policy: BrowserPolicy{Backend: "extension"}}
	invoke := func(args ...string) (map[string]any, string) {
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
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if !out.Success && out.Error == "" {
			t.Fatalf("missing error: %s", raw)
		}
		return out.Data, out.Error
	}
	call := func(args ...string) map[string]any {
		t.Helper()
		data, err := invoke(args...)
		if err != "" {
			t.Fatalf("%v: %s", args, err)
		}
		return data
	}
	var snap string
	find := func(label string) string {
		t.Helper()
		for _, line := range strings.Split(snap, "\n") {
			if strings.Contains(line, `"`+label+`"`) {
				_, ref, _ := strings.Cut(line, "[ref=")
				ref, _, _ = strings.Cut(ref, "]")
				return "@" + ref
			}
		}
		t.Fatalf("missing %s: %s", label, snap)
		return ""
	}
	call("open", fixture.URL)
	snap = call("snapshot")["snapshot"].(string)
	click := call("click", find("Connect"))
	snap = call("snapshot")["snapshot"].(string)
	find("Add a note") // Must precede the 300 background controls.
	if got := click["effect"]; got != "click_dispatched" {
		t.Fatalf("click outcome overclaimed: %v", got)
	}
	if strings.Contains(snap, "Hidden dialog button") {
		t.Fatal("hidden dialog exposed")
	}
	snap = call("snapshot", "-s", "[role=dialog],dialog")["snapshot"].(string)
	find("Contents wrapper button")
	call("click", find("Add a note"))
	snap = call("snapshot", "-s", "[role=dialog],dialog")["snapshot"].(string)
	call("fill", find("Invitation note"), "Local test draft")
	call("click", find("Save draft"))
	if got := call("get", "text", "#result")["text"]; got != "Local test draft" {
		t.Fatalf("shadow form did not save: %v", got)
	}
	if got := call("get", "text", "#clicks")["text"]; got != "1" {
		t.Fatalf("click was repeated: %v", got)
	}
	if got := call("get", "text", "[role=dialog],dialog")["text"].(string); !strings.Contains(got, "Add a note") || strings.Contains(got, "Hidden dialog button") {
		t.Fatalf("dialog read: %s", got)
	}
	for _, tc := range []struct{ scope, label, want string }{
		{"guarded-control", "Verify shadow", "human takeover"},
		{"inert-control", "Inert shadow", "disabled"},
	} {
		snap = call("snapshot", "-s", tc.scope)["snapshot"].(string)
		if _, err := invoke("click", find(tc.label)); !strings.Contains(err, tc.want) {
			t.Fatalf("shadow host restriction lost: %s", err)
		}
	}
}
