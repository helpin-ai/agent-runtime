package cli

import (
	"bytes"
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

func TestIndependentHostAdmissionCommands(t *testing.T) {
	var calls int
	var received map[string]any
	grant := ExecutionGrant{ID: "grant-independent", RunID: "run-independent", Epoch: 1, PolicyHash: "policy-1", LeaseExpiresAt: time.Now().Add(time.Minute)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-access" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/local/runs":
			calls++
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Error(err)
				return
			}
			if received["target"] != "project:example" || received["execution_location"] != "local" {
				http.Error(w, "bad target", 400)
				return
			}
			json.NewEncoder(w).Encode(Admission{RunID: grant.RunID, Agent: agentcore.Agent{ID: "fixture-agent", RuntimeKind: "native_sdk"}, AllowedTools: []string{"read_files"}, Execution: &grant})
		case "/local/runs/run-independent/execution":
			json.NewEncoder(w).Encode(grant)
		case "/local/runs/run-independent/bind":
			var req struct {
				Epoch      int64  `json:"epoch"`
				LocalRunID string `json:"local_run_id"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Epoch != grant.Epoch {
				http.Error(w, "stale", 409)
				return
			}
			grant.LocalRunID = req.LocalRunID
			json.NewEncoder(w).Encode(grant)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	c := Connection{URL: server.URL, CredentialStore: "file", Descriptor: Descriptor{ProtocolVersion: protocolVersion, AppID: "independent", Name: "Independent", APIBaseURL: server.URL + "/local", Issuer: server.URL, ClientID: "other-cli", Resource: server.URL + "/local", Scopes: []string{"projects"}, Capabilities: []string{"admission", "execution_leases"}}}
	if err := saveJSON(filepath.Join(home, "connections", "other.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := saveToken(home, "other", c, token{AccessToken: "fixture-access", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) ([]byte, error) {
		if binary := os.Getenv("AGENT_RUNTIME_CLI_TEST_BINARY"); binary != "" {
			cmd := exec.Command(binary, args...)
			cmd.Env = append(os.Environ(), "AGENT_RUNTIME_CLI_HOME="+home)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &bytes.Buffer{}
			err := cmd.Run()
			return stdout.Bytes(), err
		}
		var stdout, stderr bytes.Buffer
		err := admissionCommand(context.Background(), home, args, &stdout, &stderr)
		return stdout.Bytes(), err
	}
	args := []string{"admit", "--connection", "other", "--agent", "fixture-agent", "--target", "project:example", "--request-id", "independent-123", "--review", "Inspect project"}
	out, err := invoke(args...)
	if err != nil {
		t.Fatal(err)
	}
	var a Admission
	if err = json.Unmarshal(out, &a); err != nil {
		t.Fatal(err)
	}
	if a.Execution == nil || a.Execution.ID != grant.ID || received["review"] != true {
		t.Fatalf("missing grant or review policy: %s", out)
	}
	changed := append([]string(nil), args...)
	changed[len(changed)-1] = "Different request"
	if _, err = invoke(changed...); err == nil {
		t.Fatal("local idempotency conflict accepted")
	}
	if calls != 1 {
		t.Fatal("conflicting request reached host")
	}
	if _, err = invoke("executions", "bind", "other", grant.RunID, "--epoch", "1", "--local-run-id", "local-1"); err != nil {
		t.Fatal(err)
	}
	out, err = invoke("executions", "show", "other", grant.RunID)
	if err != nil || !strings.Contains(string(out), "local-1") {
		t.Fatalf("binding lost: %s %v", out, err)
	}
	if _, err = invoke("executions", "bind", "other", grant.RunID, "--epoch", "2", "--local-run-id", "local-2"); err == nil {
		t.Fatal("stale epoch accepted")
	}
	session, err := OpenSession(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_, err = session.Execute(context.Background(), Options{Directory: t.TempDir(), Connection: "other", Agent: "fixture-agent", Target: "project:example", Prompt: "Inspect"})
	if err == nil || !strings.Contains(err.Error(), "model gateway") {
		t.Fatalf("missing capability not explained: %v", err)
	}
	if calls != 1 {
		t.Fatal("unsupported connected execution admitted a run")
	}
	if supports(c, "model_gateway") {
		t.Fatal("admission-only host advertised model capability")
	}
}
