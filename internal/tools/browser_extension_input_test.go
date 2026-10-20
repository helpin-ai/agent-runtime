package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestExtensionInputAndFrames(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_EXTENSION_E2E") != "1" {
		t.Skip("set AGENT_RUNTIME_EXTENSION_E2E=1 for the native Chrome fixture")
	}
	executable := firstNonEmptyString(os.Getenv("AGENT_RUNTIME_BROWSER_EXTENSION_EXECUTABLE"), BundledExtensionBrowser())
	if executable == "" {
		t.Fatal("Chrome for Testing is not installed")
	}
	t.Run("Dialogs", func(t *testing.T) { testExtensionDialogs(t, executable) })
	cross := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<title>Cross</title><button>Cross secret</button>`)
	}))
	defer cross.Close()
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/many":
			fmt.Fprint(w, `<title>Many frames</title>`+strings.Repeat(`<iframe src="/child"></iframe>`, 20))
		case "/child":
			fmt.Fprint(w, `<title>Child</title><label>Child input<input oninput="parent.document.getElementById('frame-result').textContent=window.frameElement.id+':'+this.value"></label><button onclick="parent.document.getElementById('frame-result').textContent=window.frameElement.id+':saved'">Child save</button><p>Child visible text</p><iframe title="Nested" src="/nested"></iframe>`)
		case "/nested":
			fmt.Fprint(w, `<title>Nested</title><label>Nested input<input oninput="top.document.getElementById('frame-result').textContent='nested:'+this.value"></label>`)
		default:
			fmt.Fprintf(w, `<title>Input fixture</title>
<label>Keyboard<input id="keyboard" oninput="document.getElementById('value').textContent=this.value"></label><input aria-label="Next field"><pre id="value"></pre>
<label>Cancelled key<input value="keep" onkeydown="if(event.key==='Backspace')event.preventDefault()"></label>
<label>Cancelled input<input value="keep" onbeforeinput="event.preventDefault()"></label>
<label>Changed input<input value="original" onbeforeinput="this.value='handler value'"></label>
<label>Lines<textarea oninput="document.getElementById('lines-result').textContent=JSON.stringify(this.value)" onkeyup="document.getElementById('selection').textContent=this.selectionStart+':'+this.selectionEnd"></textarea></label><pre id="lines-result"></pre><p id="selection"></p>
<label>Readonly<input readonly value="keep"></label><button disabled>Disabled key</button>
<label>Limited<input maxlength="1" value="a"></label><input aria-label="Positive first" tabindex="1"><input aria-label="Positive second" tabindex="2"><p id="focused"></p>
<form onsubmit="event.preventDefault();document.getElementById('count').textContent=++window.submits"><label>Submit field<input required></label><button>Submit form</button></form><p id="count">0</p>
<form onsubmit="event.preventDefault();document.getElementById('cancel-count').textContent='double submit'"><label>Cancelled submit<input onkeydown="if(event.key==='Enter'){event.preventDefault();document.getElementById('cancel-count').textContent='one'}"></label><button>Cancelled submit button</button></form><p id="cancel-count">zero</p>
<div tabindex="0" aria-label="JS menu" onmouseenter="document.getElementById('hover-focus').textContent=document.activeElement.getAttribute('aria-label')||document.activeElement.labels?.[0]?.innerText||'';setTimeout(()=>document.getElementById('menu-item').hidden=false,25)" onmouseleave="document.getElementById('menu-item').hidden=true">Menu<button id="menu-item" hidden onclick="document.getElementById('menu-result').textContent='clicked'">Menu item</button></div><p id="menu-result"></p><p id="hover-focus"></p><button>Outside menu</button>
<style>#css-item{display:none}#css-menu:hover #css-item{display:block}</style><div tabindex="0" aria-label="CSS menu" id="css-menu">CSS<button id="css-item">CSS item</button></div>
<label>Controlled<input id="controlled"></label><p id="controlled-result"></p>
<iframe id="first" title="First" src="/child"></iframe><iframe id="second" title="Second" src="/child"></iframe><pre id="frame-result"></pre>
<iframe hidden src="/child"></iframe><div style="opacity:0"><iframe src="/child"></iframe></div><div id="captcha"><iframe src="/child"></iframe></div><iframe src=%q></iframe>
<button onclick="document.getElementById('first').src='/nested'">Replace child</button>
<button onclick="document.getElementById('first').hidden=true">Hide child</button><button onclick="document.getElementById('first').hidden=false">Show child</button>
<button onclick="document.body.appendChild(document.getElementById('second').contentDocument.querySelector('input'))">Adopt child</button>
<button onclick="document.getElementById('first').remove()">Remove child</button>
<script>document.addEventListener('keyup',()=>{const target=document.activeElement;document.getElementById('focused').textContent=target.getAttribute('aria-label')||target.labels?.[0]?.innerText||''});window.submits=0;const input=document.getElementById('controlled');const descriptor=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value');let tracked='';Object.defineProperty(input,'value',{get(){return descriptor.get.call(this)},set(value){tracked=value;descriptor.set.call(this,value)}});input.addEventListener('input',()=>{if(input.value!==tracked){tracked=input.value;document.getElementById('controlled-result').textContent=tracked}});</script>`, cross.URL)
		}
	}))
	defer fixture.Close()
	r := NewRegistry()
	RegisterBrowserTools(r, BrowserToolsConfig{Enabled: true, AppID: "test", ProfileRoot: t.TempDir(), ExtensionExecutable: executable, AllowedDomains: []string{"127.0.0.1"}, MaxOutputChars: 20000})
	t.Cleanup(func() { _ = r.CloseBrowsers(context.Background()) })
	agent := &agentcore.Agent{ID: "input", AppID: "test", AllowedTools: []string{"browser_open", "browser_snapshot", "browser_act", "browser_read"}, ExecutionConfig: json.RawMessage(`{"browser":{"backend":"extension"}}`)}
	run := &agentcore.AgentRun{ID: "input-run", AppID: "test", AgentID: agent.ID}
	callCtx := CallContext{AppID: "test", RunID: run.ID, Run: run, Agent: agent}
	var snapshot string
	tool := func(name string, input map[string]any) (map[string]json.RawMessage, error) {
		raw, _ := json.Marshal(input)
		result, err := r.Execute(t.Context(), callCtx, name, raw)
		if err != nil {
			return nil, err
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(result, &out); err != nil {
			return nil, err
		}
		if data, ok := out["snapshot"]; ok {
			snapshot = browserResultString(data, "snapshot")
		}
		return out, nil
	}
	must := func(name string, input map[string]any) map[string]json.RawMessage {
		t.Helper()
		out, err := tool(name, input)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	find := func(label string, index int) string {
		t.Helper()
		for _, line := range strings.Split(snapshot, "\n") {
			if strings.Contains(line, label) {
				if index == 0 {
					_, ref, _ := strings.Cut(line, "[ref=")
					ref, _, _ = strings.Cut(ref, "]")
					return ref
				}
				index--
			}
		}
		t.Fatalf("missing %s: %s", label, snapshot)
		return ""
	}
	act := func(action, ref, value string) string {
		t.Helper()
		out := must("browser_act", map[string]any{"action": action, "ref": ref, "value": value})
		return browserResultString(out["action_result"], "effect")
	}
	read := func(selector string) string {
		t.Helper()
		out := must("browser_read", map[string]any{"selector": selector})
		var text string
		if err := json.Unmarshal(out["text"], &text); err != nil {
			t.Fatal(err)
		}
		return text
	}
	must("browser_open", map[string]any{"url": fixture.URL, "wait_ms": 100})
	if !strings.Contains(snapshot, "Cross-origin or inaccessible frame") || strings.Contains(snapshot, "Cross secret") {
		t.Fatalf("cross-origin boundary missing: %s", snapshot)
	}
	if strings.Count(snapshot, `input "Child input"`) != 2 {
		t.Fatalf("hidden/verification frame exposed or valid duplicate lost: %s", snapshot)
	}
	// Every public action refreshes references; find again before the next action.
	act("fill", find(`input "Child input"`, 1), "second value")
	if got := read("#frame-result"); got != "second:second value" {
		t.Fatalf("duplicate frame routing: %s", got)
	}
	act("click", find(`button "Child save"`, 0), "")
	if got := read("#frame-result"); got != "first:saved" {
		t.Fatalf("frame click: %s", got)
	}
	act("fill", find(`input "Nested input"`, 0), "nested value")
	if got := read("#frame-result"); got != "nested:nested value" {
		t.Fatalf("nested frame: %s", got)
	}
	frame := find(`frame "Child"`, 0)
	if got := read(frame); !strings.Contains(got, "Child visible text") {
		t.Fatalf("frame read: %s", got)
	}
	must("browser_snapshot", map[string]any{"selector": frame})
	if strings.Contains(snapshot, `input "Keyboard"`) || !strings.Contains(snapshot, `input "Child input"`) {
		t.Fatalf("scoped frame snapshot: %s", snapshot)
	}
	must("browser_snapshot", map[string]any{})
	act("scroll", find(`frame "Child"`, 0), "down")
	act("fill", find(`input "Keyboard"`, 0), "ab😀c")
	for _, key := range []string{"End", "ArrowLeft", "Backspace", "Shift+Home", "z"} {
		act("press", find(`input "Keyboard"`, 0), key)
	}
	if got := read("#value"); got != "zc" {
		t.Fatalf("keyboard selection/grapheme editing: %q", got)
	}
	for _, key := range []string{"Control+a", "Backspace"} {
		act("press", find(`input "Keyboard"`, 0), key)
	}
	if got := read("#value"); got != "" {
		t.Fatalf("select all/delete: %q", got)
	}
	if effect := act("press", find(`input "Keyboard"`, 0), "Tab"); effect != "focus_changed" {
		t.Fatalf("tab: %s", effect)
	}
	if effect := act("press", find(`input "Next field"`, 0), "Shift+Tab"); effect != "focus_changed" {
		t.Fatalf("shift tab: %s", effect)
	}
	act("press", find(`input "Positive first"`, 0), "Tab")
	if got := read("#focused"); got != "Positive second" {
		t.Fatalf("positive tab order: %s", got)
	}
	for _, label := range []string{`input "Cancelled key"`, `input "Cancelled input"`} {
		if effect := act("press", find(label, 0), "Backspace"); effect != "cancelled" {
			t.Fatalf("cancel ignored: %s", effect)
		}
	}
	if _, err := tool("browser_act", map[string]any{"action": "press", "ref": find(`input "Changed input"`, 0), "value": "x"}); err == nil || !strings.Contains(err.Error(), "Input changed") {
		t.Fatalf("beforeinput handler was overwritten: %v", err)
	}
	for _, key := range []string{"Meta+a", "Backspace", "a", "Enter", "b", "Home", "Delete"} {
		act("press", find(`textarea "Lines"`, 0), key)
	}
	if got := read("#lines-result"); got != `"a\n"` {
		t.Fatalf("multiline keyboard result: %q", got)
	}
	if got := read("#selection"); got != "2:2" {
		t.Fatalf("keyboard selection: %s", got)
	}
	act("fill", find(`input "Controlled"`, 0), "hello")
	act("press", find(`input "Controlled"`, 0), "!")
	if got := read("#controlled-result"); got != "hello!" {
		t.Fatalf("controlled value: %q", got)
	}
	for _, test := range []struct{ ref, key string }{{find(`input "Readonly"`, 0), "Backspace"}, {find(`button "Disabled key"`, 0), "Enter"}, {find(`input "Keyboard"`, 0), "Control+w"}, {"", "Enter"}} {
		if _, err := tool("browser_act", map[string]any{"action": "press", "ref": test.ref, "value": test.key}); err == nil {
			t.Fatalf("unsupported key accepted: %+v", test)
		}
	}
	if _, err := tool("browser_act", map[string]any{"action": "press", "ref": find(`input "Limited"`, 0), "value": "b"}); err == nil || !strings.Contains(err.Error(), "maxlength") {
		t.Fatalf("maxlength ignored: %v", err)
	}
	act("press", find(`input "Submit field"`, 0), "Enter")
	if got := read("#count"); got != "0" {
		t.Fatalf("form validation bypassed: %s", got)
	}
	act("fill", find(`input "Submit field"`, 0), "valid")
	act("press", find(`input "Submit field"`, 0), "Enter")
	if got := read("#count"); got != "1" {
		t.Fatalf("implicit submission: %s", got)
	}
	act("press", find(`button "Submit form"`, 0), "Space")
	if got := read("#count"); got != "2" {
		t.Fatalf("button activation count: %s", got)
	}
	act("press", find(`input "Cancelled submit"`, 0), "Enter")
	if got := read("#cancel-count"); got != "one" {
		t.Fatalf("cancelled submission replay: %s", got)
	}
	focus := read("#focused")
	if effect := act("hover", find(`div "JS menu"`, 0), ""); effect != "events_dispatched" {
		t.Fatalf("hover outcome: %s", effect)
	}
	if got := read("#hover-focus"); got != focus {
		t.Fatalf("hover unexpectedly changed focus: %s", got)
	}
	act("click", find(`button "Menu item"`, 0), "")
	if got := read("#menu-result"); got != "clicked" {
		t.Fatalf("hover menu click: %s", got)
	}
	act("hover", find(`button "Outside menu"`, 0), "")
	if strings.Contains(snapshot, `button "Menu item"`) {
		t.Fatal("hover leave did not close menu")
	}
	out := must("browser_act", map[string]any{"action": "hover", "ref": find(`div "CSS menu"`, 0)})
	if !strings.Contains(string(out["action_result"]), "CSS hover") {
		t.Fatal("CSS hover limitation lost")
	}
	// Navigate only a child, without refreshing the snapshot: document identity must still reject its old references.
	m := r.browserManager("test")
	session, err := m.session(callCtx)
	if err != nil {
		t.Fatal(err)
	}
	stale := find(`input "Child input"`, 0)
	_, err = m.nativeExtensionCommand(t.Context(), session, []string{"click", "@" + find(`button "Replace child"`, 0)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.nativeExtensionCommand(t.Context(), session, []string{"wait", "500"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := m.nativeExtensionCommand(t.Context(), session, []string{"fill", "@" + stale, "must not land elsewhere"})
	if err != nil || (!strings.Contains(string(raw), "Stale") && !strings.Contains(string(raw), "hidden frame")) {
		t.Fatalf("child navigation reference survived: %s %v", raw, err)
	}
	must("browser_snapshot", map[string]any{})
	stale = find(`input "Nested input"`, 0)
	for _, label := range []string{`button "Hide child"`, `button "Show child"`} {
		_, err = m.nativeExtensionCommand(t.Context(), session, []string{"click", "@" + find(label, 0)})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(label, "Hide") {
			raw, err = m.nativeExtensionCommand(t.Context(), session, []string{"fill", "@" + stale, "hidden"})
			if err != nil || (!strings.Contains(string(raw), "hidden frame") && !strings.Contains(string(raw), "Stale")) {
				t.Fatalf("hidden ancestor accepted: %s %v", raw, err)
			}
		}
	}
	adopted := find(`input "Child input"`, 0)
	_, err = m.nativeExtensionCommand(t.Context(), session, []string{"click", "@" + find(`button "Adopt child"`, 0)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = m.nativeExtensionCommand(t.Context(), session, []string{"fill", "@" + adopted, "wrong document"})
	if err != nil || !strings.Contains(string(raw), "Stale element") {
		t.Fatalf("adopted node reference accepted: %s %v", raw, err)
	}
	_, err = m.nativeExtensionCommand(t.Context(), session, []string{"click", "@" + find(`button "Remove child"`, 0)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err = m.nativeExtensionCommand(t.Context(), session, []string{"fill", "@" + stale, "removed"})
	if err != nil || !strings.Contains(string(raw), "Stale") {
		t.Fatalf("removed frame reference accepted: %s %v", raw, err)
	}
	must("browser_open", map[string]any{"url": fixture.URL + "/many", "wait_ms": 100})
	if !strings.Contains(snapshot, "Frame limit reached") || strings.Count(snapshot, "- frame ") > 16 {
		t.Fatalf("unbounded frame discovery: %s", snapshot)
	}
}
