package workjobs

import (
	"bytes"
	"context"
	"encoding/json"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"testing"
)

func TestJobCredentialRotationIsScopedEncryptedAndPreservesPolicy(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, tempDB(t), &scriptedWorker{script: submitAll})
	h.stop()
	key := bytes.Repeat([]byte{42}, 32)
	h.service.cfg.Credentials = &modelauth.Manager{Store: h.store, Key: key}
	h.service.cfg.RunMCP = mcp.RunConfig{CredentialKey: key}
	job := h.create(testSpec(), 5)
	worker := agentcore.WorkJobWorker{AgentID: h.agent(), Target: testTarget, MCPServers: []sdk.RunMCPServer{{ServerID: "crm", URL: "https://crm.example/mcp", Tools: []sdk.RunMCPTool{{Name: "read", Access: "read"}}, Credential: &sdk.RunMCPCredential{Type: "bearer_token", AccessToken: "old-secret"}}}}
	raw, err := sealWorker(key, testApp, job.ID, worker)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetWorkJobWorker(ctx, testApp, job.ID, raw); err != nil {
		t.Fatal(err)
	}
	state, err := h.service.CredentialState(ctx, testApp, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := sdk.UpdateWorkJobCredentialsRequest{ExpectedVersion: state.Version, MCP: map[string]sdk.RunMCPCredential{"crm": {Type: "bearer_token", AccessToken: "fresh-secret"}}}
	if _, err := h.service.UpdateCredentials(ctx, "foreign", job.ID, request); err == nil {
		t.Fatal("foreign rotation accepted")
	}
	next, err := h.service.UpdateCredentials(ctx, testApp, job.ID, request)
	if err != nil || next.Version == state.Version {
		t.Fatalf("rotation: %+v %v", next, err)
	}
	if _, err := h.service.UpdateCredentials(ctx, testApp, job.ID, request); err == nil {
		t.Fatal("stale overwrite accepted")
	}
	stored, err := h.store.GetWorkJobWorker(ctx, testApp, job.ID)
	if err != nil || bytes.Contains(stored, []byte("fresh-secret")) {
		t.Fatal("secret persisted in plaintext")
	}
	updated, err := openWorker(key, testApp, job.ID, stored)
	if err != nil || updated.MCPServers[0].Credential.AccessToken != "fresh-secret" || updated.MCPServers[0].Tools[0].Access != "read" {
		t.Fatalf("worker policy/credential changed incorrectly: %v", err)
	}
	response, _ := json.Marshal(next)
	if bytes.Contains(response, []byte("secret")) {
		t.Fatal("secret returned by credential state")
	}
	request.ExpectedVersion = next.Version
	request.MCP = map[string]sdk.RunMCPCredential{"unattached": {Type: "bearer_token", AccessToken: "new-secret"}}
	if _, err := h.service.UpdateCredentials(ctx, testApp, job.ID, request); err == nil {
		t.Fatal("new connection smuggled into job")
	}
	paused, err := h.service.Pause(ctx, testApp, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	request.MCP = map[string]sdk.RunMCPCredential{"crm": {Type: "bearer_token", AccessToken: "new-secret"}}
	if _, err := h.service.UpdateCredentials(ctx, testApp, job.ID, request); err != nil {
		t.Fatal(err)
	}
	current, err := h.service.Get(ctx, testApp, job.ID)
	if err != nil || current.State != paused.State {
		t.Fatal("rotation resumed explicitly paused job")
	}
}
