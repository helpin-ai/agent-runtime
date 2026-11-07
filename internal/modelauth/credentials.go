// Package modelauth owns encrypted run credentials, never user login state.
package modelauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/credentials"
)

type Callback struct {
	URL   string
	Token string
}
type Manager struct {
	Store          agentcore.ModelCredentialStore
	Key            []byte
	ChatGPTEnabled bool
	Callbacks      map[string]Callback
	HTTPClient     *http.Client
}

type AuthenticationError struct{ Provider, ConnectionID, Reason string }

func (e *AuthenticationError) Error() string {
	return "model authentication required: reconnect the selected AI connection"
}

func Validate(provider string, c sdk.ModelCredential) error {
	if len(c.APIKey)+len(c.AccessToken) > 65536 || len(c.ConnectionID) > 512 || len(c.AccountID) > 512 {
		return errors.New("model credential exceeds size limit")
	}
	switch provider {
	case "openai_compatible":
		if c.AccessToken != "" || c.AccountID != "" || c.ExpiresAt != nil ||
			(c.Type != "api_key" && c.Type != "none") || (c.Type == "api_key" && strings.TrimSpace(c.APIKey) == "") || (c.Type == "none" && c.APIKey != "") {
			return errors.New("compatible provider requires an API key or explicit no-auth credential")
		}
	case "openai", "anthropic", "openrouter", "openrouter_responses":
		if c.Type != "api_key" || strings.TrimSpace(c.APIKey) == "" || c.AccessToken != "" || c.AccountID != "" {
			return errors.New("provider requires an API-key credential")
		}
	case "openai_chatgpt":
		if c.Type != "oauth" || c.AccessToken == "" || c.APIKey != "" || c.ConnectionID == "" || c.AccountID == "" || c.ExpiresAt == nil {
			return errors.New("ChatGPT requires an access token, expiry, account_id, and connection_id")
		}
	default:
		return errors.New("unsupported model credential provider")
	}
	if strings.ContainsAny(c.APIKey+c.AccessToken+c.AccountID+c.ConnectionID, "\r\n\x00") {
		return errors.New("invalid model credential")
	}
	if c.ExpiresAt != nil && !c.ExpiresAt.After(time.Now()) {
		return errors.New("model credential has expired")
	}
	return nil
}

func aad(c *agentcore.RunModelCredential) []byte {
	return []byte("model\x00" + c.AppID + "\x00" + c.RunID + "\x00" + c.Provider + "\x00" + c.Kind + "\x00" + c.ConnectionID + "\x00" + c.AccountID)
}
func (m *Manager) Prepare(appID, runID, provider string, c sdk.ModelCredential) (*agentcore.RunModelCredential, error) {
	if m == nil || m.Store == nil {
		return nil, errors.New("run model credentials are not configured")
	}
	if provider == "openai_chatgpt" && !m.ChatGPTEnabled {
		return nil, errors.New("ChatGPT subscription access is not enabled")
	}
	if err := Validate(provider, c); err != nil {
		return nil, err
	}
	if c.Type == "oauth" && m.Callbacks[appID].URL == "" {
		return nil, errors.New("ChatGPT requires an app credential refresh callback")
	}
	record := &agentcore.RunModelCredential{AppID: appID, RunID: runID, Provider: provider, Kind: c.Type, ConnectionID: c.ConnectionID, AccountID: c.AccountID, Version: 1}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	record.EncryptedCredential, err = credentials.Seal(m.Key, aad(record), raw)
	return record, err
}

func (m *Manager) Replace(ctx context.Context, appID, runID string, c sdk.ModelCredential) error {
	old, err := m.Store.GetRunModelCredential(ctx, appID, runID)
	if err != nil {
		return err
	}
	if old == nil {
		return errors.New("run model credential not found")
	}
	if c.Type != old.Kind || c.ConnectionID != old.ConnectionID || c.AccountID != old.AccountID {
		return errors.New("credential replacement cannot change connection or account")
	}
	next, err := m.Prepare(appID, runID, old.Provider, c)
	if err != nil {
		return err
	}
	ok, err := m.Store.ReplaceRunModelCredential(ctx, next, old.Version)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("credential changed concurrently or run is terminal; retry")
	}
	return nil
}

func (m *Manager) resolve(ctx context.Context, run *agentcore.AgentRun, force bool, previousVersion int64) (sdk.ModelCredential, int64, error) {
	return m.resolveAttempt(ctx, run, force, previousVersion, 0)
}
func (m *Manager) resolveAttempt(ctx context.Context, run *agentcore.AgentRun, force bool, previousVersion int64, attempt int) (sdk.ModelCredential, int64, error) {
	if attempt > 1 {
		return sdk.ModelCredential{}, 0, &AuthenticationError{Reason: "credential_changed"}
	}
	old, err := m.Store.GetRunModelCredential(ctx, run.AppID, run.ID)
	if err != nil {
		return sdk.ModelCredential{}, 0, errors.New("cannot load model credential")
	}
	authErr := &AuthenticationError{Reason: "reconnect"}
	if old == nil {
		return sdk.ModelCredential{}, 0, authErr
	}
	authErr.Provider = old.Provider
	authErr.ConnectionID = old.ConnectionID
	if old.Revoked || len(old.EncryptedCredential) == 0 {
		return sdk.ModelCredential{}, old.Version, authErr
	}
	raw, err := credentials.Open(m.Key, aad(old), old.EncryptedCredential)
	if err != nil {
		return sdk.ModelCredential{}, 0, authErr
	}
	var c sdk.ModelCredential
	if json.Unmarshal(raw, &c) != nil {
		return c, old.Version, authErr
	}
	expired := c.ExpiresAt != nil && !c.ExpiresAt.After(time.Now().Add(30*time.Second))
	if !expired && (!force || old.Version != previousVersion) {
		return c, old.Version, nil
	}
	reason := "expired"
	if force {
		reason = "unauthorized"
	}
	next, err := m.refreshFromHost(ctx, run.AppID, run.ID, run.HostRunID, old.Provider, c, reason)
	if err != nil {
		return c, old.Version, authErr
	}
	prepared, err := m.Prepare(run.AppID, run.ID, old.Provider, next)
	if err != nil {
		return c, old.Version, authErr
	}
	ok, err := m.Store.ReplaceRunModelCredential(ctx, prepared, old.Version)
	if err != nil {
		return c, old.Version, authErr
	}
	if !ok {
		return m.resolveAttempt(ctx, run, false, 0, attempt+1)
	}
	return next, old.Version + 1, nil
}

// refreshFromHost asks the app's credential callback for a replacement of c.
// The replacement must keep the credential kind, account and connection.
func (m *Manager) refreshFromHost(ctx context.Context, appID, runID, hostRunID, provider string, c sdk.ModelCredential, reason string) (sdk.ModelCredential, error) {
	callback := m.Callbacks[appID]
	if callback.URL == "" || c.ConnectionID == "" {
		return c, errors.New("no credential refresh callback")
	}
	secret := c.APIKey
	if c.Type == "oauth" {
		secret = c.AccessToken
	}
	body, _ := json.Marshal(sdk.ModelCredentialRefreshRequest{CredentialFingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(secret))), AppID: appID, RunID: runID, HostRunID: hostRunID, ConnectionID: c.ConnectionID, Provider: provider, AccountID: c.AccountID, Reason: reason})
	refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(refreshCtx, http.MethodPost, callback.URL, bytes.NewReader(body))
	if err != nil {
		return c, err
	}
	req.Header.Set("Authorization", "Bearer "+callback.Token)
	req.Header.Set("Content-Type", "application/json")
	client := m.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safeClient.Do(req)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("credential refresh returned %d", resp.StatusCode)
	}
	var update sdk.UpdateRunModelCredentialRequest
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 65537))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&update) != nil {
		return c, errors.New("invalid credential refresh response")
	}
	next := update.Credential
	if next.Type != c.Type || next.AccountID != c.AccountID || next.ConnectionID != c.ConnectionID {
		return c, errors.New("credential refresh changed the connection")
	}
	return next, nil
}

// CredentialForRun returns a run's current model credential and provider,
// for a child the run spawns. ok is false when the run has none.
func (m *Manager) CredentialForRun(ctx context.Context, run *agentcore.AgentRun) (sdk.ModelCredential, string, bool, error) {
	if m == nil || m.Store == nil {
		return sdk.ModelCredential{}, "", false, nil
	}
	record, err := m.Store.GetRunModelCredential(ctx, run.AppID, run.ID)
	if err != nil || record == nil {
		return sdk.ModelCredential{}, "", false, err
	}
	credential, _, err := m.resolve(ctx, run, false, 0)
	if err != nil {
		return sdk.ModelCredential{}, "", false, err
	}
	return credential, record.Provider, true, nil
}

// FreshCredential returns c, or a replacement from the app's refresh
// callback when c expires within minValidity. Work jobs use it before
// copying their stored credential to a new worker run, since a run cannot
// be created with an expired credential. changed reports a replacement.
func (m *Manager) FreshCredential(ctx context.Context, appID, provider, hostRunID string, c sdk.ModelCredential, minValidity time.Duration) (sdk.ModelCredential, bool, error) {
	if m == nil || c.ExpiresAt == nil || c.ExpiresAt.After(time.Now().Add(minValidity)) {
		return c, false, nil
	}
	next, err := m.refreshFromHost(ctx, appID, "", hostRunID, provider, c, "expired")
	if err != nil {
		return c, false, &AuthenticationError{Provider: provider, ConnectionID: c.ConnectionID, Reason: "reconnect"}
	}
	if err := Validate(provider, next); err != nil {
		return c, false, err
	}
	return next, true, nil
}

// Transport resolves the current credential for every request, including summaries.
// It retries only an HTTP authentication rejection before a response stream starts.
func (m *Manager) Transport(base http.RoundTripper, run *agentcore.AgentRun, provider string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &credentialTransport{m: m, base: base, run: run, provider: provider}
}

type credentialAuthenticationFailure struct {
	version int64
	err     *AuthenticationError
}

type credentialTransport struct {
	failure  atomic.Pointer[credentialAuthenticationFailure]
	m        *Manager
	base     http.RoundTripper
	run      *agentcore.AgentRun
	provider string
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.provider == "openai_compatible" {
		model := t.run.Input.Model
		if model == nil || model.Provider != "openai_compatible" || model.Endpoint == nil || sdk.ValidateRunModel(model) != nil || req.URL.String() != model.Endpoint.BaseURL+"/chat/completions" {
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, errors.New("model request destination does not match the accepted endpoint")
		}
	}
	// Provider SDKs may retry transport errors. Reuse a final authentication
	// failure until the app replaces this credential, without contacting the
	// provider or refreshing the same token again.
	if failure := t.failure.Load(); failure != nil {
		current, err := t.m.Store.GetRunModelCredential(req.Context(), t.run.AppID, t.run.ID)
		if err == nil && (current == nil || current.Version == failure.version) {
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, failure.err
		}
	}
	remember := func(err error, version int64) {
		var authErr *AuthenticationError
		if errors.As(err, &authErr) {
			t.failure.Store(&credentialAuthenticationFailure{version: version, err: authErr})
		}
	}

	c, version, err := t.m.resolve(req.Context(), t.run, false, 0)
	if err != nil {
		remember(err, version)
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	if t.provider == "openai_compatible" && c.Type != t.run.Input.Model.Endpoint.AuthMode {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, errors.New("model credential mode does not match the accepted endpoint")
	}
	send := func(c sdk.ModelCredential, retry bool) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.Header = req.Header.Clone()
		if retry && req.Body != nil {
			if req.GetBody == nil {
				return nil, errors.New("model request cannot be retried")
			}
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			clone.Body = body
		}
		if c.Type == "none" {
			clone.Header.Del("Authorization")
			clone.Header.Del("x-api-key")
		} else if t.provider == "anthropic" {
			clone.Header.Set("x-api-key", c.APIKey)
			clone.Header.Del("Authorization")
		} else {
			token := c.APIKey
			if c.Type == "oauth" {
				token = c.AccessToken
			}
			clone.Header.Set("Authorization", "Bearer "+token)
		}
		if t.provider == "openai_chatgpt" {
			clone.Header.Set("ChatGPT-Account-ID", c.AccountID)
		}
		response, err := t.base.RoundTrip(clone)
		if err != nil {
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			return nil, errors.New("model provider request failed")
		}
		return response, nil
	}
	resp, err := send(c, false)
	if err != nil || resp.StatusCode != 401 {
		return resp, err
	}
	resp.Body.Close()
	c, version, err = t.m.resolve(req.Context(), t.run, true, version)
	if err != nil {
		remember(err, version)
		return nil, err
	}
	resp, err = send(c, true)
	if err == nil && resp.StatusCode == 401 {
		resp.Body.Close()
		authErr := &AuthenticationError{Provider: t.provider, ConnectionID: c.ConnectionID, Reason: "unauthorized"}
		remember(authErr, version)
		return nil, authErr
	}
	return resp, err
}

func ValidateModel(model *sdk.RunModel) error {
	return sdk.ValidateRunModel(model)
}

// RejectedFingerprint identifies the current secret without refreshing it or
// exposing it to a paused run's interaction payload.
func (m *Manager) RejectedFingerprint(ctx context.Context, appID, runID string) string {
	if m == nil || m.Store == nil {
		return ""
	}
	record, err := m.Store.GetRunModelCredential(ctx, appID, runID)
	if err != nil || record == nil {
		return ""
	}
	raw, err := credentials.Open(m.Key, aad(record), record.EncryptedCredential)
	if err != nil {
		return ""
	}
	var credential sdk.ModelCredential
	if json.Unmarshal(raw, &credential) != nil {
		return ""
	}
	token := credential.AccessToken
	if token == "" {
		token = credential.APIKey
	}
	if token == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}
