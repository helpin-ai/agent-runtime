package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
)

func TestOAuthHostRunAndSync(t *testing.T) {
	var server *httptest.Server
	var challenge, redirect string
	var model codingModel
	var synced bool
	mux := http.NewServeMux()
	mux.HandleFunc("/agent-runtime/cli.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Descriptor{ProtocolVersion: protocolVersion, AppID: "independent-app", Name: "Independent test host", APIBaseURL: server.URL + "/cli", Issuer: server.URL, ClientID: "public-cli", Resource: server.URL + "/cli", Scopes: []string{"runs"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oauthMetadata{Issuer: server.URL, AuthorizationEndpoint: server.URL + "/authorize", TokenEndpoint: server.URL + "/token", CodeChallengeMethodsSupported: []string{"S256"}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "valid-code" || r.Form.Get("redirect_uri") != redirect || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("client_id") != "public-cli" || r.Form.Get("resource") != server.URL+"/cli" {
			http.Error(w, "invalid grant", 400)
			return
		}
		json.NewEncoder(w).Encode(token{AccessToken: "user-token", TokenType: "Bearer", ExpiresIn: 3600})
	})
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer user-token" {
				http.Error(w, "unauthorized", 401)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/cli/agents", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"agents":[{"id":"coder","name":"Coder"}]}`)
	}))
	mux.HandleFunc("/cli/runs", auth(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]interface{}
		json.NewDecoder(r.Body).Decode(&input)
		if input["execution_location"] != "local" || input["target"] != "opaque:123" {
			http.Error(w, "bad admission", 400)
			return
		}
		json.NewEncoder(w).Encode(Admission{RunID: "host-run-1", Agent: agentcore.Agent{ID: "coder", Name: "Coder", SystemPrompt: "Fix the requested local code", ApprovalMode: "never"}, AllowedTools: []string{"read_files", "write_file", "run_command", "forbidden_host_tool"}})
	}))
	mux.HandleFunc("/cli/runs/host-run-1/model", auth(func(w http.ResponseWriter, r *http.Request) {
		var req runtime.NativeModelRequest
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		for _, d := range req.Tools {
			if d.Name == "forbidden_host_tool" {
				t.Error("host expanded CLI tool policy")
			}
		}
		res, e := model.Generate(r.Context(), req)
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		json.NewEncoder(w).Encode(res)
	}))
	mux.HandleFunc("/cli/runs/host-run-1/events", auth(func(w http.ResponseWriter, r *http.Request) {
		var data struct {
			Events []json.RawMessage `json:"events"`
			Status string            `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&data)
		if len(data.Events) == 0 || data.Status != "completed" {
			http.Error(w, "missing events", 400)
			return
		}
		synced = true
		w.WriteHeader(204)
	}))
	server = httptest.NewServer(mux)
	defer server.Close()
	home := t.TempDir()
	var out bytes.Buffer
	if e := connectionCommand(context.Background(), home, []string{"connect", server.URL, "--name", "test", "--credential-store", "file"}, &out); e != nil {
		t.Fatal(e)
	}
	original := browserOpener
	defer func() { browserOpener = original }()
	browserOpener = func(raw string) {
		u, e := url.Parse(raw)
		if e != nil {
			t.Error(e)
			return
		}
		q := u.Query()
		challenge = q.Get("code_challenge")
		redirect = q.Get("redirect_uri")
		bad, e := http.Get(redirect + "?state=wrong&code=valid-code")
		if e != nil {
			t.Error(e)
			return
		}
		bad.Body.Close()
		if bad.StatusCode != 400 {
			t.Error("accepted invalid state")
		}
		callback := redirect + "?" + url.Values{"state": {q.Get("state")}, "code": {"valid-code"}, "iss": {server.URL}}.Encode()
		res, e := http.Get(callback)
		if e != nil {
			t.Error(e)
			return
		}
		res.Body.Close()
	}
	if e := connectionCommand(context.Background(), home, []string{"login", "test"}, &out); e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(filepath.Join(home, "credentials", "test.json"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file permissions")
	}
	if e := connectionCommand(context.Background(), home, []string{"agents", "test"}, &out); e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "calc.py"), []byte("def add(a,b): return a-b\n"), 0600)
	s, e := OpenSession(home, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := s.Execute(context.Background(), Options{Directory: dir, Connection: "test", Agent: "coder", Target: "opaque:123", Prompt: "Fix addition", Yes: true})
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "completed" {
		t.Fatal(r.Status)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "calc.py"))
	if !strings.Contains(string(data), "a + b") {
		t.Fatal("host-backed coding did not edit fixture")
	}
	if e = s.Sync(context.Background(), r.ID); e != nil {
		t.Fatal(e)
	}
	if !synced {
		t.Fatal("run not synced")
	}
	events, _ := s.Store.ListEvents(context.Background(), localApp, r.ID)
	b, _ := json.Marshal(events)
	if bytes.Contains(b, []byte("user-token")) {
		t.Fatal("token leaked into events")
	}
}
func TestRejectInsecureEndpoints(t *testing.T) {
	for _, raw := range []string{"http://example.com", "https://user:pass@example.com", "file:///tmp/credentials", "https://example.com/#fragment"} {
		if secureURL(raw) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"https://example.com", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if e := secureURL(raw); e != nil {
			t.Error(e)
		}
	}
}
func TestConnectionCredentialsAreIsolated(t *testing.T) {
	home := t.TempDir()
	c := Connection{CredentialStore: "file"}
	if e := saveToken(home, "first", c, token{AccessToken: "first-token", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}); e != nil {
		t.Fatal(e)
	}
	if _, e := loadToken(home, "second", c); e == nil {
		t.Fatal("loaded another connection token")
	}
	if _, e := connectionPath(home, "../escape"); e == nil {
		t.Fatal("allowed path traversal")
	}
}

func TestConcurrentTokenRefreshRotatesOnce(t *testing.T) {
	var server *httptest.Server
	var refreshes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(oauthMetadata{Issuer: server.URL, AuthorizationEndpoint: server.URL + "/authorize", TokenEndpoint: server.URL + "/token", CodeChallengeMethodsSupported: []string{"S256"}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
			http.Error(w, "invalid grant", 400)
			return
		}
		refreshes.Add(1)
		json.NewEncoder(w).Encode(token{AccessToken: "new-access", RefreshToken: "new-refresh", TokenType: "Bearer", ExpiresIn: 3600})
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	home := t.TempDir()
	c := Connection{CredentialStore: "file", Descriptor: Descriptor{Issuer: server.URL, ClientID: "cli", Resource: server.URL}}
	if e := saveToken(home, "profile", c, token{AccessToken: "old-access", RefreshToken: "old-refresh", TokenType: "Bearer", ExpiresAt: time.Now().Add(-time.Minute)}); e != nil {
		t.Fatal(e)
	}
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			value, e := accessToken(context.Background(), home, "profile", c)
			if e == nil && value != "new-access" {
				e = fmt.Errorf("unexpected token")
			}
			results <- e
		}()
	}
	for i := 0; i < 8; i++ {
		if e := <-results; e != nil {
			t.Error(e)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh token rotated %d times", refreshes.Load())
	}
}
