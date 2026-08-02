package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const (
	codexAuthRequestTimeout         = 30 * time.Second
	codexLoginTypeChatGPT           = "chatgpt"
	codexLoginTypeChatGPTDeviceCode = "chatgptDeviceCode"
)

type CodexAuthManager struct {
	store  agentcore.Store
	cfg    CodexConfig
	events EventSink

	mu       sync.Mutex
	sessions map[string]*codexManagedAuthSession
}

type codexManagedAuthSession struct {
	appID     string
	runID     string
	hostRunID string
	loginID   string
	codexHome string
	scope     CodexAuthScope
	client    *codexAppServerClient
	cancel    context.CancelFunc
	store     agentcore.Store
	authStore CodexAuthStore

	mu    sync.Mutex
	state CodexAuthState
}

func NewCodexAuthManager(store agentcore.Store, cfg CodexConfig) *CodexAuthManager {
	if store == nil {
		return nil
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	return &CodexAuthManager{
		store:    store,
		cfg:      cfg,
		sessions: map[string]*codexManagedAuthSession{},
	}
}

func (m *CodexAuthManager) SetEventSink(sink EventSink) *CodexAuthManager {
	if m != nil {
		m.events = sink
	}
	return m
}

func (m *CodexAuthManager) StartDeviceCode(ctx context.Context, appID, runID string) (*CodexAuthState, error) {
	if m == nil || m.store == nil {
		return nil, fmt.Errorf("codex auth manager is not configured")
	}
	run, err := m.store.GetRun(ctx, strings.TrimSpace(appID), strings.TrimSpace(runID))
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("run not found")
	}
	agent, err := m.store.GetAgent(ctx, run.AppID, run.AgentID)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, fmt.Errorf("agent not found")
	}
	key := codexAuthSessionKey(run.AppID, run.ID)
	m.mu.Lock()
	if session := m.sessions[key]; session != nil {
		current := session.snapshot()
		m.mu.Unlock()
		return &current, nil
	}
	m.mu.Unlock()

	adapter := NewCodexAdapterWithConfig(m.cfg)
	execCtx := &ExecutionContext{Context: ctx, AppID: run.AppID, Agent: agent, Run: run, Store: m.store}
	sessionStore := newCodexSessionStore(m.store)
	state, err := sessionStore.Load(ctx, run.AppID, run.ID)
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = &codexSessionState{}
	}
	if err := adapter.prepareCodexHome(ctx, execCtx, state); err != nil {
		return nil, err
	}
	provider, modelName := adapter.resolveCodexProviderAndModel(agent)
	authMode := firstNonEmpty(m.cfg.OpenAIAuthMode, codexOpenAIAuthModeDevice)
	if provider != "openai" || authMode != codexOpenAIAuthModeDevice {
		return nil, fmt.Errorf("codex device-code auth requires provider openai with auth mode %q", codexOpenAIAuthModeDevice)
	}
	state.Provider = provider
	state.AuthMode = authMode
	state.Model = modelName
	state.InvocationMode = run.InvocationMode
	if err := writeCodexHomeConfig(state.CodexHome, adapter.codexHomeConfig(state.Model, provider, state.AuthMode)); err != nil {
		return nil, err
	}
	if err := adapter.restoreCodexAuth(ctx, execCtx, state); err != nil {
		return nil, err
	}
	if err := sessionStore.Save(ctx, run.AppID, run.ID, state); err != nil {
		return nil, err
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	client := newCodexAppServerClient(m.cfg.CommandPath, firstNonEmpty(m.cfg.WorkDir, "."), adapter.codexEnv(state))
	requestCtx, cancelRequest := context.WithTimeout(sessionCtx, codexAuthRequestTimeout)
	defer cancelRequest()
	if err := client.Start(sessionCtx); err != nil {
		cancel()
		return nil, err
	}
	if err := client.Initialize(requestCtx); err != nil {
		_ = client.Close()
		cancel()
		return nil, err
	}
	authState, authenticated, err := codexReadManagedAuthState(requestCtx, client, provider, authMode)
	if err != nil {
		_ = client.Close()
		cancel()
		return nil, err
	}
	if authenticated {
		_ = client.Close()
		cancel()
		if authState == nil {
			connected := codexAuthState(provider, authMode, codexAuthStateConnected)
			authState = &connected
		}
		_ = appendCodexAuthState(ctx, m.store, run.AppID, run.ID, *authState)
		m.emitAuthState(ctx, run.AppID, run.ID, run.HostRunID, *authState)
		_ = adapter.promoteCodexAuth(ctx, execCtx, state)
		return authState, nil
	}

	raw, err := startManagedChatGPTLogin(requestCtx, client)
	if err != nil {
		_ = client.Close()
		cancel()
		return nil, err
	}
	var response codexLoginAccountResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		_ = client.Close()
		cancel()
		return nil, fmt.Errorf("decode codex device-code login response: %w", err)
	}
	if responseType := strings.TrimSpace(response.Type); responseType != codexLoginTypeChatGPTDeviceCode && responseType != codexLoginTypeChatGPT {
		_ = client.Close()
		cancel()
		return nil, fmt.Errorf("unexpected codex managed login response type %q", responseType)
	}
	loginID := optionalStringValue(response.LoginID)
	if loginID == "" {
		_ = client.Close()
		cancel()
		return nil, fmt.Errorf("codex device-code login response did not include loginId")
	}

	current := codexAuthState(provider, authMode, codexAuthStatePending)
	current.LoginID = optionalStringPtr(loginID)
	current.AuthURL = optionalStringClone(response.AuthURL)
	current.VerificationURL = optionalStringClone(response.VerificationURL)
	current.UserCode = optionalStringClone(response.UserCode)
	_ = appendCodexAuthState(ctx, m.store, run.AppID, run.ID, current)
	m.emitAuthState(ctx, run.AppID, run.ID, run.HostRunID, current)

	session := &codexManagedAuthSession{
		appID:     run.AppID,
		runID:     run.ID,
		hostRunID: run.HostRunID,
		loginID:   loginID,
		codexHome: state.CodexHome,
		scope:     codexAuthScope(execCtx, provider, authMode),
		client:    client,
		cancel:    cancel,
		store:     m.store,
		authStore: m.cfg.AuthStore,
		state:     current,
	}
	m.mu.Lock()
	m.sessions[key] = session
	m.mu.Unlock()

	go m.watchSession(sessionCtx, session)
	return &current, nil
}

func (m *CodexAuthManager) CancelDeviceCode(ctx context.Context, appID, runID string) (*CodexAuthState, error) {
	if m == nil {
		return nil, fmt.Errorf("codex auth manager is not configured")
	}
	key := codexAuthSessionKey(appID, runID)
	m.mu.Lock()
	session := m.sessions[key]
	m.mu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("no pending codex device-code login for this run")
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), codexAuthRequestTimeout)
	defer cancel()
	raw, err := session.client.Request(requestCtx, "account/login/cancel", map[string]any{"loginId": session.loginID})
	if err != nil {
		return nil, err
	}
	var response codexCancelLoginAccountResponse
	if err := json.Unmarshal(raw, &response); err == nil && strings.EqualFold(strings.TrimSpace(response.Status), "notFound") {
		return nil, fmt.Errorf("pending codex device-code login was not found")
	}
	cancelled := session.apply(codexAuthState(session.scope.Provider, session.scope.AuthMode, codexAuthStateCancelled))
	_ = appendCodexAuthState(ctx, session.store, session.appID, session.runID, cancelled)
	m.emitAuthState(ctx, session.appID, session.runID, session.hostRunID, cancelled)
	session.cancel()
	return &cancelled, nil
}

func (m *CodexAuthManager) watchSession(ctx context.Context, session *codexManagedAuthSession) {
	defer func() {
		session.cancel()
		_ = session.client.Close()
		m.mu.Lock()
		delete(m.sessions, codexAuthSessionKey(session.appID, session.runID))
		m.mu.Unlock()
	}()
	loginSucceeded := false
	for {
		msg, err := session.client.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if loginSucceeded {
				current := session.snapshot()
				if strings.TrimSpace(current.State) == codexAuthStateConnected {
					return
				}
			}
			failed := session.apply(codexAuthState(session.scope.Provider, session.scope.AuthMode, codexAuthStateFailed))
			failed.Error = optionalStringPtr(err.Error())
			_ = appendCodexAuthState(context.Background(), session.store, session.appID, session.runID, failed)
			m.emitAuthState(context.Background(), session.appID, session.runID, session.hostRunID, failed)
			return
		}
		switch strings.TrimSpace(msg.Method) {
		case "account/login/completed":
			var payload codexAccountLoginCompletedNotification
			if len(msg.Params) > 0 {
				if err := json.Unmarshal(msg.Params, &payload); err != nil {
					failed := session.apply(codexAuthState(session.scope.Provider, session.scope.AuthMode, codexAuthStateFailed))
					failed.Error = optionalStringPtr("decode account/login/completed: " + err.Error())
					_ = appendCodexAuthState(context.Background(), session.store, session.appID, session.runID, failed)
					m.emitAuthState(context.Background(), session.appID, session.runID, session.hostRunID, failed)
					return
				}
			}
			if !payload.Success {
				stateKind := codexAuthStateFailed
				if payload.Error != nil && strings.Contains(strings.ToLower(strings.TrimSpace(*payload.Error)), "cancel") {
					stateKind = codexAuthStateCancelled
				}
				next := codexAuthState(session.scope.Provider, session.scope.AuthMode, stateKind)
				next.LoginID = optionalStringClone(payload.LoginID)
				next.Error = optionalStringClone(payload.Error)
				final := session.apply(next)
				_ = appendCodexAuthState(context.Background(), session.store, session.appID, session.runID, final)
				m.emitAuthState(context.Background(), session.appID, session.runID, session.hostRunID, final)
				return
			}
			loginSucceeded = true
		case "account/updated":
			var payload codexAccountUpdatedNotification
			if len(msg.Params) > 0 {
				if err := json.Unmarshal(msg.Params, &payload); err != nil {
					failed := session.apply(codexAuthState(session.scope.Provider, session.scope.AuthMode, codexAuthStateFailed))
					failed.Error = optionalStringPtr("decode account/updated: " + err.Error())
					_ = appendCodexAuthState(context.Background(), session.store, session.appID, session.runID, failed)
					m.emitAuthState(context.Background(), session.appID, session.runID, session.hostRunID, failed)
					return
				}
			}
			if payload.AuthMode != nil && strings.TrimSpace(*payload.AuthMode) == "chatgpt" {
				connected := codexAuthState(session.scope.Provider, session.scope.AuthMode, codexAuthStateConnected)
				connected.PlanType = optionalStringClone(payload.PlanType)
				final := session.apply(connected)
				if session.authStore != nil {
					if err := session.authStore.Promote(context.Background(), session.scope, session.codexHome); err != nil {
						final = session.apply(codexAuthState(session.scope.Provider, session.scope.AuthMode, codexAuthStateFailed))
						final.Error = optionalStringPtr("persist codex auth: " + err.Error())
					}
				}
				_ = appendCodexAuthState(context.Background(), session.store, session.appID, session.runID, final)
				m.emitAuthState(context.Background(), session.appID, session.runID, session.hostRunID, final)
				return
			}
		}
	}
}

func (m *CodexAuthManager) emitAuthState(ctx context.Context, appID, runID, hostRunID string, authState CodexAuthState) {
	if m == nil || m.events == nil {
		return
	}
	emitCodexAuthStateEvent(ctx, m.events, appID, runID, hostRunID, authState)
}

func codexReadManagedAuthState(ctx context.Context, client *codexAppServerClient, provider, authMode string) (*CodexAuthState, bool, error) {
	raw, err := client.Request(ctx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return nil, false, err
	}
	var response codexAccountReadResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, false, fmt.Errorf("decode codex account/read response: %w", err)
	}
	if !response.RequiresOpenAIAuth {
		return nil, true, nil
	}
	if response.Account != nil && strings.TrimSpace(response.Account.Type) != "" {
		state := codexAuthState(provider, authMode, codexAuthStateConnected)
		state.PlanType = optionalStringClone(response.Account.PlanType)
		return &state, true, nil
	}
	state := codexAuthState(provider, authMode, codexAuthStateRequired)
	return &state, false, nil
}

func startManagedChatGPTLogin(ctx context.Context, client *codexAppServerClient) (json.RawMessage, error) {
	raw, err := client.Request(ctx, "account/login/start", map[string]any{"type": codexLoginTypeChatGPTDeviceCode})
	if err != nil && codexNeedsManagedChatGPTFallback(err) {
		return nil, fmt.Errorf("the configured Codex binary does not support ChatGPT device-code login; point CODEX_PATH at a newer Codex build with chatgptDeviceCode support")
	}
	return raw, err
}

func codexNeedsManagedChatGPTFallback(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(message, "unknown variant") && strings.Contains(message, "chatgpt") && strings.Contains(message, "chatgptauthtokens")
}

func (s *codexManagedAuthSession) snapshot() CodexAuthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *codexManagedAuthSession) apply(next CodexAuthState) CodexAuthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next.LoginID == nil {
		next.LoginID = optionalStringClone(s.state.LoginID)
	}
	if next.AuthURL == nil {
		next.AuthURL = optionalStringClone(s.state.AuthURL)
	}
	if next.VerificationURL == nil {
		next.VerificationURL = optionalStringClone(s.state.VerificationURL)
	}
	if next.UserCode == nil {
		next.UserCode = optionalStringClone(s.state.UserCode)
	}
	if next.PlanType == nil {
		next.PlanType = optionalStringClone(s.state.PlanType)
	}
	s.state = next
	return s.state
}

func codexAuthSessionKey(appID, runID string) string {
	return strings.TrimSpace(appID) + "/" + strings.TrimSpace(runID)
}

func optionalStringPtr(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func optionalStringClone(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return optionalStringPtr(*value)
}

func optionalStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
