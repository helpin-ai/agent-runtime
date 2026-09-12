package modelauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestRunCredentialsIsolationRotationAndRevocation(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	m := &Manager{Store: db, Key: []byte(strings.Repeat("k", 32))}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Header.Get("Authorization")) }))
	defer server.Close()
	for _, app := range []string{"app-a", "app-b"} {
		run := &agentcore.AgentRun{AppID: app, ID: "same-run", Status: agentcore.RunStatusPaused}
		credential, err := m.Prepare(app, run.ID, "openai", sdk.ModelCredential{Type: "api_key", APIKey: app + "-secret"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(credential.EncryptedCredential), "secret") {
			t.Fatal("plaintext credential stored")
		}
		if err := db.CreateRunWithModelCredential(ctx, run, nil, credential); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, app := range []string{"app-a", "app-b"} {
		wg.Add(1)
		go func(app string) {
			defer wg.Done()
			client := &http.Client{Transport: m.Transport(nil, &agentcore.AgentRun{AppID: app, ID: "same-run"}, "openai")}
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if string(raw) != "Bearer "+app+"-secret" {
				t.Error("cross-app credential")
			}
		}(app)
	}
	wg.Wait()
	c := sdk.ModelCredential{Type: "api_key", APIKey: "rotated"}
	if err := m.Replace(ctx, "app-a", "same-run", c); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearRunModelCredential(ctx, "app-a", "same-run"); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.resolve(ctx, &agentcore.AgentRun{AppID: "app-a", ID: "same-run"}, false, 0)
	var authErr *AuthenticationError
	if !errors.As(err, &authErr) {
		t.Fatalf("revoked credential: %v", err)
	}
}

func TestRefreshRetriesOnlyAuthenticationAndPinsAccount(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	expiry := time.Now().Add(time.Hour)
	c := sdk.ModelCredential{Type: "oauth", AccessToken: "old", ExpiresAt: &expiry, ConnectionID: "connection", AccountID: "account"}
	callbackCalls := 0
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callbackCalls++
		if r.Header.Get("Authorization") != "Bearer host-secret" {
			t.Error("missing callback authentication")
		}
		var req sdk.ModelCredentialRefreshRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.ConnectionID != "connection" || req.AccountID != "account" || req.Reason != "unauthorized" {
			t.Error("incorrect refresh scope")
		}
		next := c
		next.AccessToken = "new"
		json.NewEncoder(w).Encode(sdk.UpdateRunModelCredentialRequest{Credential: next})
	}))
	defer callback.Close()
	m := &Manager{Store: db, Key: []byte(strings.Repeat("k", 32)), ChatGPTEnabled: true, Callbacks: map[string]Callback{"app": {URL: callback.URL, Token: "host-secret"}}}
	run := &agentcore.AgentRun{AppID: "app", ID: "run", Status: agentcore.RunStatusRunning}
	record, err := m.Prepare(run.AppID, run.ID, "openai_chatgpt", c)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRunWithModelCredential(ctx, run, nil, record); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("ChatGPT-Account-ID") != "account" {
			t.Error("missing account")
		}
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer server.Close()
	client := &http.Client{Transport: m.Transport(nil, run, "openai_chatgpt")}
	resp, err := client.Post(server.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 2 || callbackCalls != 1 {
		t.Fatalf("requests=%d refresh=%d", calls, callbackCalls)
	}
	c.AccountID = "other"
	if m.Replace(ctx, "app", "run", c) == nil {
		t.Fatal("account switch accepted")
	}
}
