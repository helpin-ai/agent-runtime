package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/helpin-ai/agent-runtime-go"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func newRunGroupTestServer(t *testing.T, enabledApps ...string) (http.Handler, *store.Memory) {
	t.Helper()
	t.Setenv("AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK", "true")
	enabled := map[string]bool{}
	for _, app := range enabledApps {
		enabled[app] = true
	}
	mem := store.NewMemory()
	eng := engine.New(engine.Config{
		DefaultExecutionMode: engine.ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(runtime.NewNativeAdapter()),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		RunGroupsEnabled:     func(appID string) bool { return enabled[appID] },
	})
	return NewServer(Config{Engine: eng, Store: mem, AllowAnonymous: true}), mem
}

func createTestAgent(t *testing.T, handler http.Handler, appID string) agentcore.Agent {
	t.Helper()
	return postJSON[agentcore.Agent](t, handler, "/v1/agents", map[string]interface{}{
		"app_id": appID, "name": "Agent", "runtime_kind": "native_sdk",
		"allowed_tools": []string{"get_context"}, "allowed_targets": []string{"ticket"},
		"approval_mode": "never", "default_invocation_mode": "autonomous",
	}, http.StatusCreated)
}

func doRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRunGroupsAPIEndToEnd(t *testing.T) {
	handler, mem := newRunGroupTestServer(t, "app-a")
	agent := createTestAgent(t, handler, "app-a")
	group := postJSON[agentcore.RunGroup](t, handler, "/v1/run-groups", map[string]interface{}{"app_id": "app-a", "creation_key": "plan-1"}, http.StatusCreated)
	again := postJSON[agentcore.RunGroup](t, handler, "/v1/run-groups", map[string]interface{}{"app_id": "app-a", "creation_key": "plan-1"}, http.StatusCreated)
	if again.ID != group.ID || group.OwnerKind != agentcore.RunGroupOwnerHost {
		t.Fatalf("idempotent create = %#v then %#v", group, again)
	}
	runIDs := map[string]bool{}
	for _, target := range []string{"T-1", "T-2", "T-3"} {
		run := postJSON[agentcore.AgentRun](t, handler, "/v1/runs", map[string]interface{}{
			"app_id": "app-a", "agent_id": agent.ID, "group_id": group.ID,
			"target": map[string]interface{}{"type": "ticket", "id": target},
		}, http.StatusAccepted)
		runIDs[run.ID] = true
	}
	for runID := range runIDs {
		waitForAPIStatus(t, mem, "app-a", runID, agentcore.RunStatusCompleted)
	}

	rec := doRequest(t, handler, http.MethodGet, "/v1/run-groups/"+group.ID+"/completions?app_id=app-a&after=0&limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("completions status = %d body = %s", rec.Code, rec.Body.String())
	}
	var first agentcore.RunGroupCompletionPage
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if len(first.Items) != 2 || first.NextAfter != 2 || first.Items[0].Seq != 1 {
		t.Fatalf("first page = %#v", first)
	}
	rec = doRequest(t, handler, http.MethodGet, "/v1/run-groups/"+group.ID+"/completions?app_id=app-a&after=2", "")
	var second agentcore.RunGroupCompletionPage
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if len(second.Items) != 1 || second.NextAfter != 3 || !runIDs[second.Items[0].RunID] {
		t.Fatalf("second page = %#v", second)
	}

	rec = doRequest(t, handler, http.MethodGet, "/v1/run-groups/"+group.ID+"/members?app_id=app-a", "")
	var members []agentcore.RunGroupMember
	_ = json.Unmarshal(rec.Body.Bytes(), &members)
	if rec.Code != http.StatusOK || len(members) != 3 || members[0].Status != agentcore.RunStatusCompleted || members[0].Hidden {
		t.Fatalf("members = %d %#v", rec.Code, members)
	}
	// Host-owned members stay in default listings.
	rec = doRequest(t, handler, http.MethodGet, "/v1/runs?app_id=app-a", "")
	var listed []agentcore.AgentRun
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed) != 3 {
		t.Fatalf("host-owned members must stay listed, got %d", len(listed))
	}

	cancelled := postJSON[agentcore.RunGroup](t, handler, "/v1/run-groups/"+group.ID+"/cancel?app_id=app-a", nil, http.StatusOK)
	if cancelled.State != agentcore.RunGroupStateCancelled {
		t.Fatalf("cancel = %#v", cancelled)
	}
	rec = doRequest(t, handler, http.MethodPost, "/v1/runs", `{"app_id":"app-a","agent_id":"`+agent.ID+`","group_id":"`+group.ID+`","target":{"type":"ticket","id":"T-9"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("start into cancelled group = %d %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, handler, http.MethodPost, "/v1/runs", `{"app_id":"app-a","agent_id":"`+agent.ID+`","group_id":"missing","target":{"type":"ticket","id":"T-9"}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("start into missing group = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, handler, http.MethodGet, "/v1/run-groups/"+group.ID+"/completions?app_id=app-a&limit=501", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized limit = %d", rec.Code)
	}
	if rec := doRequest(t, handler, http.MethodPost, "/v1/run-groups", `{"app_id":"app-a","owner_kind":"work_job"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown fields must be rejected strictly, got %d", rec.Code)
	}
}

func TestRunGroupsCapabilityIsPerApp(t *testing.T) {
	handler, _ := newRunGroupTestServer(t, "app-a")
	read := func(query string) *sdk.FeatureCapability {
		t.Helper()
		rec := doRequest(t, handler, http.MethodGet, "/v1/capabilities"+query, "")
		var caps struct {
			RunGroups *sdk.FeatureCapability `json:"run_groups"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
			t.Fatal(err)
		}
		return caps.RunGroups
	}
	if got := read("?app_id=app-a"); got == nil || !got.Supported {
		t.Fatalf("app-a capability = %#v", got)
	}
	if got := read("?app_id=app-b"); got == nil || got.Supported || got.Reason != engine.RunGroupsReasonAppDisabled {
		t.Fatalf("app-b capability = %#v", got)
	}
	if got := read(""); got != nil {
		t.Fatalf("capability without app_id must be omitted, got %#v", got)
	}
}

// TestRunGroupsDisabledPreservesLegacyRunContract checks that an app without
// run groups sees 404 on group routes while existing run behavior is
// unchanged.
func TestRunGroupsDisabledPreservesLegacyRunContract(t *testing.T) {
	handler, mem := newRunGroupTestServer(t, "app-a")
	agent := createTestAgent(t, handler, "app-b")
	for _, path := range []string{"/v1/run-groups/any?app_id=app-b", "/v1/run-groups/any/completions?app_id=app-b"} {
		if rec := doRequest(t, handler, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
	}
	if rec := doRequest(t, handler, http.MethodPost, "/v1/run-groups", `{"app_id":"app-b"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("create group for disabled app = %d", rec.Code)
	}
	rec := doRequest(t, handler, http.MethodPost, "/v1/runs", `{"app_id":"app-b","agent_id":"`+agent.ID+`","group_id":"g","target":{"type":"ticket","id":"T-1"}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("grouped start for disabled app = %d %s", rec.Code, rec.Body.String())
	}

	run := postJSON[agentcore.AgentRun](t, handler, "/v1/runs", map[string]interface{}{
		"app_id": "app-b", "agent_id": agent.ID, "target": map[string]interface{}{"type": "ticket", "id": "T-1"},
	}, http.StatusAccepted)
	waitForAPIStatus(t, mem, "app-b", run.ID, agentcore.RunStatusCompleted)
	rec = doRequest(t, handler, http.MethodGet, "/v1/runs?app_id=app-b", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "group_id") {
		t.Fatalf("legacy listing changed: %d %s", rec.Code, rec.Body.String())
	}
	var listed []agentcore.AgentRun
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].ID != run.ID {
		t.Fatalf("legacy listing = %#v", listed)
	}
	rec = doRequest(t, handler, http.MethodGet, "/v1/runs/search?app_id=app-b", "")
	var page agentcore.RunPage
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != http.StatusOK || page.Total != 1 {
		t.Fatalf("legacy search = %d %#v", rec.Code, page)
	}
}
