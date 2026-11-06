package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
)

// Host contract fixtures preserve the start-run payloads that Usermaven
// (Python SDK v0.4.0), Helpin (Go SDK v0.7.0) and Rex (Go SDK v0.9.0) send
// today. Start-run decoding is strict, so a removed or renamed field breaks
// those hosts; see docs/plans/2026-11-04-phase0-contracts.md.
func TestHostStartRunFixturesDecodeStrictly(t *testing.T) {
	paths, err := filepath.Glob("testdata/host_contracts/*_start_run.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("fixtures: %v %v", paths, err)
	}
	handler, _ := newRunGroupTestServer(t)
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var req engine.StartRunRequest
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&req); err != nil {
				t.Fatalf("strict decode: %v", err)
			}
			// Membership is only ever the explicit top-level field; Helpin's
			// metadata.parent_run_id is its own Dock parent and must stay inert.
			if req.GroupID != "" || req.ParentRunID != "" {
				t.Fatalf("legacy payload set group fields: %q %q", req.GroupID, req.ParentRunID)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body)))
			if strings.Contains(rec.Body.String(), "invalid request body") {
				t.Fatalf("handler rejected the payload shape: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRunResponseKeepsPythonSDKRequiredFields guards Usermaven's pydantic
// AgentRun model, which requires these keys to be present (pause_reason is a
// non-optional string, so it must be serialized even when empty).
func TestRunResponseKeepsPythonSDKRequiredFields(t *testing.T) {
	handler, mem := newRunGroupTestServer(t)
	agent := createTestAgent(t, handler, "usermaven")
	run := postJSON[agentcore.AgentRun](t, handler, "/v1/runs", map[string]interface{}{
		"app_id": "usermaven", "agent_id": agent.ID, "target": map[string]interface{}{"type": "ticket", "id": "T-1"},
	}, http.StatusAccepted)
	waitForAPIStatus(t, mem, "usermaven", run.ID, agentcore.RunStatusCompleted)
	rec := doRequest(t, handler, http.MethodGet, "/v1/runs/"+run.ID+"?app_id=usermaven", "")
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "app_id", "agent_id", "target", "runtime_kind", "execution_mode", "invocation_mode", "status", "pause_reason", "approval_state", "input"} {
		if raw, ok := body[key]; !ok || string(raw) == "null" {
			t.Fatalf("run response missing %q: %s", key, rec.Body.String())
		}
	}
}
