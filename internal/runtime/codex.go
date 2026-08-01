package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	runtimeworkspace "github.com/helpin-ai/agent-runtime/internal/workspace"
)

type CodexConfig struct {
	CommandPath string
	Args        []string
	Env         []string
	WorkDir     string
	Timeout     time.Duration
	AppServer   bool

	ModelProvider         string
	Model                 string
	Sandbox               string
	ApprovalPolicy        string
	ApprovalsReviewer     string
	DeveloperInstructions string
	OpenAIAuthMode        string
	AuthStore             CodexAuthStore
	RuntimeRoot           string

	// UseLegacyLandlock swaps Codex's bubblewrap sandbox for Landlock+seccomp.
	// See codexHomeConfig.UseLegacyLandlock.
	UseLegacyLandlock    bool
	PendingReplayTimeout time.Duration
}

type CodexAdapter struct {
	cfg CodexConfig
}

const codexRuntimeSkillNamespace = "agent-runtime"

const defaultCodexPendingReplayTimeout = 15 * time.Second

var errCodexPendingReplayTimeout = errors.New("codex pending request replay timed out")

type codexAppServerRPC interface {
	Next(context.Context) (codexRPCMessage, error)
	Respond(context.Context, json.RawMessage, any) error
	Request(context.Context, string, any) (json.RawMessage, error)
}

type codexPendingResumeResult struct {
	Response       any
	Followup       string
	FallbackPrompt string
	Replayed       bool
}

func NewCodexAdapter() *CodexAdapter {
	return NewCodexAdapterWithConfig(DefaultCodexConfigFromEnv())
}

func DefaultCodexConfigFromEnv() CodexConfig {
	cfg := CodexConfig{
		CommandPath:       strings.TrimSpace(os.Getenv("CODEX_PATH")),
		OpenAIAuthMode:    strings.TrimSpace(os.Getenv("CODEX_OPENAI_AUTH_MODE")),
		RuntimeRoot:       strings.TrimSpace(os.Getenv("AGENT_RUNTIME_CODEX_ROOT")),
		Sandbox:           firstNonEmpty(os.Getenv("CODEX_SANDBOX_MODE"), os.Getenv("CODEX_SANDBOX")),
		ApprovalPolicy:    firstNonEmpty(os.Getenv("CODEX_APPROVAL_POLICY"), os.Getenv("CODEX_ASK_FOR_APPROVAL")),
		ApprovalsReviewer: strings.TrimSpace(os.Getenv("CODEX_APPROVALS_REVIEWER")),
		UseLegacyLandlock: envFlagEnabled("CODEX_USE_LEGACY_LANDLOCK"),
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_APP_SERVER")), "true") || strings.TrimSpace(os.Getenv("CODEX_APP_SERVER")) == "1" {
		cfg.AppServer = true
	}
	if root := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_CODEX_AUTH_DIR")); root != "" {
		keyValue := firstNonEmpty(os.Getenv("AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY"), os.Getenv("CODEX_AUTH_ENCRYPTION_KEY"))
		if key, err := ParseCodexAuthEncryptionKey(keyValue); err == nil && len(key) == 32 {
			cfg.AuthStore = NewEncryptedFileCodexAuthStore(root, key)
		}
	}
	return cfg
}

func NewCodexAdapterWithConfig(cfg CodexConfig) *CodexAdapter {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	return &CodexAdapter{cfg: cfg}
}

func (a *CodexAdapter) Kind() string {
	return agentcore.RuntimeCodex
}

func (a *CodexAdapter) Execute(execCtx *ExecutionContext) (*Result, error) {
	if execCtx == nil || execCtx.Run == nil || execCtx.Agent == nil {
		return nil, fmt.Errorf("execution context is incomplete")
	}
	// Runtime interactions require the bidirectional app-server protocol. Do
	// not let an omitted CODEX_APP_SERVER setting silently route an approval-
	// gated skill through the legacy one-shot command adapter, which cannot
	// pause and resume the run.
	if a.cfg.AppServer || codexCompletionRequiresInteraction(execCtx) {
		return a.executeAppServer(execCtx)
	}
	if strings.TrimSpace(a.cfg.CommandPath) != "" {
		return a.executeCommand(execCtx)
	}
	if !deterministicFallbackAllowed() {
		return nil, fmt.Errorf("codex runtime is not configured")
	}
	contextSummary := ""
	if execCtx.TargetContext != nil {
		contextSummary = strings.TrimSpace(execCtx.TargetContext.Summary)
	}
	if contextSummary == "" {
		contextSummary = fmt.Sprintf("%s %s", execCtx.Run.Target.Type, execCtx.Run.Target.ID)
	}
	summary, _ := json.Marshal(map[string]interface{}{
		"runtime_kind": agentcore.RuntimeCodex,
		"target_type":  execCtx.Run.Target.Type,
		"target_id":    execCtx.Run.Target.ID,
	})
	return &Result{
		AssistantMessage: fmt.Sprintf("Codex run prepared for %s/%s.\nContext: %s\nInstructions: %s", execCtx.Run.Target.Type, execCtx.Run.Target.ID, contextSummary, execCtx.Run.Input.Instructions),
		OutputSummary:    summary,
	}, nil
}

func (a *CodexAdapter) executeAppServer(execCtx *ExecutionContext) (*Result, error) {
	ctx := execCtx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if a.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.cfg.Timeout)
		defer cancel()
	}
	workDir, cleanupWorkDir, err := resolveRuntimeWorkDir(execCtx, a.cfg.WorkDir, agentcore.RuntimeCodex)
	if err != nil {
		return nil, err
	}
	defer cleanupWorkDir()
	slog.InfoContext(ctx, "codex app-server execute starting",
		"run_id", execCtx.Run.ID,
		"app_id", execCtx.Run.AppID,
		"invocation_mode", execCtx.Run.InvocationMode,
		"timeout_ms", a.cfg.Timeout.Milliseconds(),
		"has_auth_store", a.cfg.AuthStore != nil,
	)
	sessionStore := newCodexSessionStore(execCtx.Store)
	state, err := sessionStore.Load(ctx, execCtx.Run.AppID, execCtx.Run.ID)
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = &codexSessionState{}
	}
	slog.InfoContext(ctx, "codex session state loaded",
		"run_id", execCtx.Run.ID,
		"has_thread", strings.TrimSpace(state.ThreadID) != "",
		"has_pending_request", state.PendingRequest != nil,
		"pending_kind", codexPendingKind(state),
	)
	if err := a.prepareCodexHome(ctx, execCtx, state); err != nil {
		return nil, err
	}
	provider := firstNonEmpty(a.cfg.ModelProvider, execCtx.Agent.Provider, "openai")
	authMode := a.authModeForProvider(provider)
	if authMode != "" {
		state.Provider = provider
		state.AuthMode = authMode
	}
	slog.InfoContext(ctx, "codex auth restore starting", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode, "has_auth_store", a.cfg.AuthStore != nil)
	if err := a.restoreCodexAuth(ctx, execCtx, state); err != nil {
		slog.WarnContext(ctx, "codex auth restore failed", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode, "error", err)
		return nil, err
	}
	slog.InfoContext(ctx, "codex auth restore complete", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode)
	repoSkillMask, err := maskCodexRepoSkillRoots(execCtx, workDir)
	if err != nil {
		return nil, err
	}
	if repoSkillMask != nil {
		defer func() {
			_ = repoSkillMask.Restore()
		}()
	}
	client := newCodexAppServerClient(a.cfg.CommandPath, workDir, a.codexEnv(state))
	slog.InfoContext(ctx, "codex app-server process starting", "run_id", execCtx.Run.ID)
	if err := client.Start(ctx); err != nil {
		slog.WarnContext(ctx, "codex app-server process failed to start", "run_id", execCtx.Run.ID, "error", err)
		return nil, err
	}
	defer client.Close()
	slog.InfoContext(ctx, "codex app-server process started", "run_id", execCtx.Run.ID)
	slog.InfoContext(ctx, "codex app-server initialize starting", "run_id", execCtx.Run.ID)
	if err := client.Initialize(ctx); err != nil {
		slog.WarnContext(ctx, "codex app-server initialize failed", "run_id", execCtx.Run.ID, "error", err)
		return nil, err
	}
	slog.InfoContext(ctx, "codex app-server initialize complete", "run_id", execCtx.Run.ID)
	authResult, err := a.ensureCodexAuthenticated(ctx, client, execCtx, state)
	if err != nil {
		if a.codexShouldReauthForError(state, err) {
			slog.WarnContext(ctx, "codex auth requires refresh after reusable-token error", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode)
			_ = a.clearCodexAuth(ctx, execCtx, state)
			return a.codexAuthRequiredResult(ctx, execCtx, state, "ChatGPT authentication needs to be refreshed."), nil
		}
		slog.WarnContext(ctx, "codex auth check failed", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode, "error", err)
		return nil, err
	}
	if authResult != nil {
		slog.InfoContext(ctx, "codex auth interaction required", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode)
		if err := sessionStore.Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
			return nil, err
		}
		return authResult, nil
	}
	slog.InfoContext(ctx, "codex auth ready", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode)
	threadID, err := a.startOrResumeCodexThread(ctx, client, execCtx, workDir, state)
	if err != nil {
		slog.WarnContext(ctx, "codex thread lifecycle returned error", "run_id", execCtx.Run.ID, "error", err)
		return nil, err
	}
	writeCodexConfigArtifact(ctx, execCtx, workDir, a.cfg, state)
	if state.PendingRequest != nil {
		pendingKind := strings.TrimSpace(state.PendingRequest.Kind)
		slog.InfoContext(ctx, "codex resume pending request", "run_id", execCtx.Run.ID, "kind", pendingKind, "thread_id", threadID)
		resume, err := a.respondToPendingCodexRequest(ctx, client, execCtx, state)
		if err != nil {
			slog.WarnContext(ctx, "codex resume pending request failed", "run_id", execCtx.Run.ID, "kind", pendingKind, "error", err)
			return nil, err
		}
		if strings.TrimSpace(resume.FallbackPrompt) != "" {
			slog.InfoContext(ctx, "codex resume starting fallback turn", "run_id", execCtx.Run.ID, "kind", pendingKind)
			if err := a.startCodexTurn(ctx, client, threadID, resume.FallbackPrompt); err != nil {
				slog.WarnContext(ctx, "codex resume fallback turn failed to start", "run_id", execCtx.Run.ID, "kind", pendingKind, "error", err)
				return nil, err
			}
			slog.InfoContext(ctx, "codex resume fallback turn started", "run_id", execCtx.Run.ID, "kind", pendingKind)
			state.PendingRequest = nil
			if err := sessionStore.Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
				return nil, err
			}
			return a.collectCodexTurn(ctx, client, workDir, execCtx, state)
		}
		state.PendingRequest = nil
		if err := sessionStore.Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
			return nil, err
		}
		result, err := a.collectCodexTurn(ctx, client, workDir, execCtx, state)
		if err != nil || strings.TrimSpace(resume.Followup) == "" {
			_ = a.promoteCodexAuth(ctx, execCtx, state)
			return result, err
		}
		slog.InfoContext(ctx, "codex resume starting reviewer-feedback followup turn", "run_id", execCtx.Run.ID, "kind", pendingKind)
		if err := a.startCodexTurn(ctx, client, threadID, resume.Followup); err != nil {
			slog.WarnContext(ctx, "codex resume reviewer-feedback followup turn failed to start", "run_id", execCtx.Run.ID, "kind", pendingKind, "error", err)
			return result, err
		}
		slog.InfoContext(ctx, "codex resume reviewer-feedback followup turn started", "run_id", execCtx.Run.ID, "kind", pendingKind)
		_ = resume.Response
		return a.collectCodexTurn(ctx, client, workDir, execCtx, state)
	}
	if state.PendingInteraction != nil {
		followup := codexInteractionResumePrompt(execCtx, state.PendingInteraction)
		if err := a.startCodexTurn(ctx, client, threadID, followup); err != nil {
			return nil, err
		}
		state.PendingInteraction = nil
		if err := sessionStore.Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
			return nil, err
		}
		return a.collectCodexTurn(ctx, client, workDir, execCtx, state)
	}
	input := strings.TrimSpace(execCtx.Run.Input.Instructions)
	if input == "" && execCtx.TargetContext != nil {
		input = strings.TrimSpace(execCtx.TargetContext.Summary)
	}
	if input == "" {
		input = "Run the agent task for this target."
	}
	slog.InfoContext(ctx, "codex starting initial turn", "run_id", execCtx.Run.ID, "thread_id", threadID)
	if err := a.startCodexTurn(ctx, client, threadID, input); err != nil {
		slog.WarnContext(ctx, "codex initial turn failed to start", "run_id", execCtx.Run.ID, "thread_id", threadID, "error", err)
		return nil, err
	}
	slog.InfoContext(ctx, "codex initial turn started", "run_id", execCtx.Run.ID, "thread_id", threadID)
	state.LastSubmittedMessageSeqNo = 0
	if err := sessionStore.Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
		return nil, err
	}
	return a.collectCodexTurn(ctx, client, workDir, execCtx, state)
}

func (a *CodexAdapter) prepareCodexHome(_ context.Context, execCtx *ExecutionContext, state *codexSessionState) error {
	if state == nil || execCtx == nil || execCtx.Run == nil {
		return nil
	}
	runRoot := strings.TrimSpace(state.HomeRoot)
	if runRoot == "" {
		root := strings.TrimSpace(a.cfg.RuntimeRoot)
		if root == "" {
			root = filepath.Join(os.TempDir(), "agent-runtime-codex")
		}
		runRoot = filepath.Join(root, sanitizeCodexPathComponent(execCtx.Run.AppID), sanitizeCodexPathComponent(execCtx.Run.ID), "home")
	}
	codexHome := strings.TrimSpace(state.CodexHome)
	if codexHome == "" {
		codexHome = filepath.Join(runRoot, ".codex")
	}
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		return fmt.Errorf("create codex home: %w", err)
	}
	if stagedRoot := strings.TrimSpace(execCtx.StagedSkillRoot); stagedRoot != "" {
		if err := skills.SyncRuntimeSkillRoot(stagedRoot, filepath.Join(codexHome, "skills", codexRuntimeSkillNamespace)); err != nil {
			return fmt.Errorf("sync staged codex skills: %w", err)
		}
	}
	if _, err := installCodexCommandGuards(runRoot); err != nil {
		return err
	}
	// Carry the session's settings through: this runs on every execute, and a
	// resumed run must not lose what the device-code auth flow wrote here.
	if err := writeCodexHomeConfig(codexHome, a.codexHomeConfig(state.Model, state.Provider, state.AuthMode)); err != nil {
		return fmt.Errorf("write codex config: %w", err)
	}
	state.HomeRoot = runRoot
	state.CodexHome = codexHome
	return nil
}

// codexHomeConfig collects the runtime-owned config.toml settings for a run.
// A fresh execute has no model or provider in session state yet, which is
// correct: Codex receives those over the app-server protocol instead.
func (a *CodexAdapter) codexHomeConfig(model, provider, authMode string) codexHomeConfig {
	cfg := codexHomeConfig{
		Model:             strings.TrimSpace(model),
		ModelProvider:     strings.TrimSpace(provider),
		UseLegacyLandlock: a.cfg.UseLegacyLandlock,
	}
	// Only device-code auth forces the ChatGPT login method. Setting it for an
	// api_key run would send Codex down the wrong auth path.
	if strings.TrimSpace(authMode) == codexOpenAIAuthModeDevice {
		cfg.ForcedLoginMethod = "chatgpt"
	}
	return cfg
}

func (a *CodexAdapter) codexEnv(state *codexSessionState) []string {
	env := append([]string(nil), a.cfg.Env...)
	if state != nil {
		if strings.TrimSpace(state.HomeRoot) != "" {
			env = upsertEnv(env, "HOME", strings.TrimSpace(state.HomeRoot))
		}
		if strings.TrimSpace(state.CodexHome) != "" {
			env = upsertEnv(env, "CODEX_HOME", strings.TrimSpace(state.CodexHome))
		}
		if strings.TrimSpace(state.HomeRoot) != "" {
			env = prependPathEnv(env, filepath.Join(strings.TrimSpace(state.HomeRoot), ".agent-runtime-command-guards"))
		}
	}
	return env
}

func (a *CodexAdapter) authModeForProvider(provider string) string {
	if strings.TrimSpace(provider) != "openai" {
		return ""
	}
	return firstNonEmpty(a.cfg.OpenAIAuthMode, codexOpenAIAuthModeAPIKey)
}

func (a *CodexAdapter) ensureCodexAuthenticated(ctx context.Context, client *codexAppServerClient, execCtx *ExecutionContext, state *codexSessionState) (*Result, error) {
	if state == nil || strings.TrimSpace(state.Provider) != "openai" || strings.TrimSpace(state.AuthMode) != codexOpenAIAuthModeDevice {
		if execCtx != nil && execCtx.Run != nil && state != nil {
			slog.InfoContext(ctx, "codex auth check skipped", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode)
		}
		return nil, nil
	}
	slog.InfoContext(ctx, "codex auth check starting", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode)
	raw, err := client.Request(ctx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return nil, err
	}
	var response codexAccountReadResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode codex account/read response: %w", err)
	}
	if !response.RequiresOpenAIAuth || response.Account != nil {
		authState := codexAuthState(state.Provider, state.AuthMode, codexAuthStateConnected)
		if response.Account != nil && response.Account.PlanType != nil && strings.TrimSpace(*response.Account.PlanType) != "" {
			value := strings.TrimSpace(*response.Account.PlanType)
			authState.PlanType = &value
		}
		persistCodexAuthState(ctx, execCtx, authState)
		_ = a.promoteCodexAuth(ctx, execCtx, state)
		slog.InfoContext(ctx, "codex auth check connected", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode, "has_account", response.Account != nil)
		return nil, nil
	}
	slog.InfoContext(ctx, "codex auth check requires sign-in", "run_id", execCtx.Run.ID, "provider", state.Provider, "auth_mode", state.AuthMode)
	return a.codexAuthRequiredResult(ctx, execCtx, state, ""), nil
}

func (a *CodexAdapter) codexAuthRequiredResult(ctx context.Context, execCtx *ExecutionContext, state *codexSessionState, message string) *Result {
	authState := codexAuthState(firstNonEmpty(state.Provider, "openai"), firstNonEmpty(state.AuthMode, codexOpenAIAuthModeDevice), codexAuthStateRequired)
	if strings.TrimSpace(message) != "" {
		value := strings.TrimSpace(message)
		authState.Error = &value
	}
	persistCodexAuthState(ctx, execCtx, authState)
	requestCodexAuthInteraction(ctx, execCtx, authState)
	return &Result{
		AssistantMessage: "Sign in with ChatGPT to continue this Codex run.",
		AwaitingAuth:     true,
	}
}

func (a *CodexAdapter) restoreCodexAuth(ctx context.Context, execCtx *ExecutionContext, state *codexSessionState) error {
	if a.cfg.AuthStore == nil || state == nil {
		return nil
	}
	return a.cfg.AuthStore.Restore(ctx, codexAuthScope(execCtx, state.Provider, state.AuthMode), state.CodexHome)
}

func (a *CodexAdapter) promoteCodexAuth(ctx context.Context, execCtx *ExecutionContext, state *codexSessionState) error {
	if a.cfg.AuthStore == nil || state == nil {
		return nil
	}
	return a.cfg.AuthStore.Promote(ctx, codexAuthScope(execCtx, state.Provider, state.AuthMode), state.CodexHome)
}

func (a *CodexAdapter) clearCodexAuth(ctx context.Context, execCtx *ExecutionContext, state *codexSessionState) error {
	if state != nil && strings.TrimSpace(state.CodexHome) != "" {
		_ = os.Remove(filepath.Join(strings.TrimSpace(state.CodexHome), codexAuthFileName))
	}
	if a.cfg.AuthStore == nil || state == nil {
		return nil
	}
	return a.cfg.AuthStore.Clear(ctx, codexAuthScope(execCtx, state.Provider, state.AuthMode))
}

func (a *CodexAdapter) codexShouldReauthForError(state *codexSessionState, err error) bool {
	if err == nil || state == nil || !codexShouldPersistAuth(state.Provider, state.AuthMode) {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(normalized, "refresh token") && strings.Contains(normalized, "already used")
}

func (a *CodexAdapter) startOrResumeCodexThread(ctx context.Context, client *codexAppServerClient, execCtx *ExecutionContext, workDir string, state *codexSessionState) (string, error) {
	sandbox := codexSandboxMode(a.cfg, execCtx)
	codexConfig := map[string]any{
		"features.default_mode_request_user_input": true,
	}
	params := map[string]any{
		"cwd":                   workDir,
		"modelProvider":         firstNonEmpty(a.cfg.ModelProvider, execCtx.Agent.Provider, "openai"),
		"approvalPolicy":        firstNonEmpty(a.cfg.ApprovalPolicy, "on-request"),
		"approvalsReviewer":     firstNonEmpty(a.cfg.ApprovalsReviewer, "user"),
		"sandbox":               sandbox,
		"serviceName":           "Agent Runtime",
		"developerInstructions": a.codexDeveloperInstructions(execCtx, state),
		"config":                codexConfig,
	}
	if codexWebSearchEnabled(execCtx) {
		// Helpin exposes provider-specific search permissions. Codex owns its
		// search implementation, so translate either permission into the live
		// built-in web-search capability for both new and resumed threads.
		codexConfig["web_search"] = "live"
	}
	if model := firstNonEmpty(a.cfg.Model, execCtx.Agent.Model); model != "" {
		params["model"] = model
	}
	method := "thread/start"
	existingThreadID := ""
	if state != nil && strings.TrimSpace(state.ThreadID) != "" {
		method = "thread/resume"
		existingThreadID = strings.TrimSpace(state.ThreadID)
		params["threadId"] = existingThreadID
	} else {
		dynamicTools, err := codexDynamicToolSpecs(ctx, execCtx)
		if err != nil {
			return "", fmt.Errorf("prepare codex dynamic tools: %w", err)
		}
		if len(dynamicTools) > 0 {
			params["dynamicTools"] = dynamicTools
		}
	}
	startedAt := time.Now()
	slog.InfoContext(ctx, "codex thread lifecycle starting",
		"run_id", execCtx.Run.ID,
		"method", method,
		"existing_thread_id", existingThreadID,
		"provider", params["modelProvider"],
		"model", params["model"],
		"sandbox", params["sandbox"],
		"approval_policy", params["approvalPolicy"],
	)
	raw, err := client.Request(ctx, method, params)
	if err != nil {
		slog.WarnContext(ctx, "codex thread lifecycle failed", "run_id", execCtx.Run.ID, "method", method, "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
		return "", err
	}
	var response codexThreadLifecycleResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("decode codex thread response: %w", err)
	}
	if strings.TrimSpace(response.Thread.ID) == "" {
		return "", fmt.Errorf("codex %s returned an empty thread id", method)
	}
	slog.InfoContext(ctx, "codex thread lifecycle complete", "run_id", execCtx.Run.ID, "method", method, "thread_id", strings.TrimSpace(response.Thread.ID), "elapsed_ms", time.Since(startedAt).Milliseconds())
	if state != nil {
		state.ThreadID = strings.TrimSpace(response.Thread.ID)
		if response.Thread.Path != nil {
			state.ThreadPath = strings.TrimSpace(*response.Thread.Path)
		}
		state.Provider = firstNonEmpty(strings.TrimSpace(response.ModelProvider), state.Provider, firstNonEmpty(a.cfg.ModelProvider, execCtx.Agent.Provider))
		state.Model = firstNonEmpty(strings.TrimSpace(response.Model), state.Model, firstNonEmpty(a.cfg.Model, execCtx.Agent.Model))
		state.Sandbox = sandbox
		state.InvocationMode = execCtx.Run.InvocationMode
	}
	return strings.TrimSpace(response.Thread.ID), nil
}

func codexSandboxMode(cfg CodexConfig, execCtx *ExecutionContext) string {
	if execCtx != nil && runtimeworkspace.AccessMode(execCtx.Agent) == runtimeworkspace.AccessReadOnly {
		return "read-only"
	}
	return firstNonEmpty(cfg.Sandbox, "workspace-write")
}

func (a *CodexAdapter) startCodexTurn(ctx context.Context, client codexAppServerRPC, threadID string, input string) error {
	if input == "" {
		input = "Run the agent task for this target."
	}
	raw, err := client.Request(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input": []map[string]any{{
			"type":          "text",
			"text":          input,
			"text_elements": []any{},
		}},
	})
	if err != nil {
		return err
	}
	var response codexTurnResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode codex turn response: %w", err)
	}
	if strings.TrimSpace(response.Turn.ID) == "" {
		return fmt.Errorf("codex turn/start returned an empty turn id")
	}
	return nil
}

func (a *CodexAdapter) collectCodexTurn(ctx context.Context, client *codexAppServerClient, workDir string, execCtx *ExecutionContext, state *codexSessionState) (*Result, error) {
	mapper := newCodexEventMapper(execCtx, workDir)
	policyRetryAttempted := false
	completionToolRetryAttempted := false
	runID := ""
	if execCtx != nil && execCtx.Run != nil {
		runID = execCtx.Run.ID
	}
	startedAt := time.Now()
	defer func() {
		// A transport error or cancellation can arrive after Codex has already
		// emitted useful assistant/tool items. Persist those items with a short
		// detached context so a failed run does not erase its visible timeline.
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := mapper.PersistMessages(persistCtx); err != nil {
			slog.ErrorContext(persistCtx, "persist codex timeline after turn exit", "run_id", runID, "error", err)
		}
	}()
	slog.InfoContext(ctx, "codex turn collecting", "run_id", runID)
	for {
		msg, err := client.Next(ctx)
		if err != nil {
			slog.InfoContext(ctx, "codex turn collect failed", "run_id", runID, "elapsed_ms", time.Since(startedAt).Milliseconds(), "error", err)
			mapper.FlushArtifacts(ctx)
			return nil, err
		}
		switch strings.TrimSpace(msg.Method) {
		case "item/tool/call":
			if err := a.handleCodexDynamicToolCall(ctx, client, execCtx, msg); err != nil {
				mapper.FlushArtifacts(ctx)
				return nil, err
			}
			continue
		case "item/tool/requestUserInput", "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
			if handled, err := a.maybeDeclineForbiddenCodexCommand(ctx, client, msg, state); handled || err != nil {
				if err != nil {
					mapper.FlushArtifacts(ctx)
					return nil, err
				}
				continue
			}
			pending, interactionKind, summary, err := codexPendingFromRequest(msg.Method, msg.ID, msg.Params)
			if err != nil {
				mapper.FlushArtifacts(ctx)
				return nil, err
			}
			if state != nil {
				state.PendingRequest = pending
				_ = a.promoteCodexAuth(ctx, execCtx, state)
				if err := newCodexSessionStore(execCtx.Store).Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
					mapper.FlushArtifacts(ctx)
					return nil, err
				}
			}
			a.requestCodexInteraction(ctx, execCtx, pending, interactionKind, summary, msg.Params)
			slog.InfoContext(ctx, "codex turn paused",
				"run_id", runID,
				"kind", pending.Kind,
				"interaction_kind", interactionKind,
				"request_id", pending.RequestID,
				"turn_id", pending.TurnID,
				"item_id", pending.ItemID,
				"elapsed_ms", time.Since(startedAt).Milliseconds(),
			)
			mapper.FlushArtifacts(ctx)
			messagesPersisted, persistErr := mapper.PersistMessages(ctx)
			if persistErr != nil {
				return nil, persistErr
			}
			result := &Result{
				AssistantMessage:   firstNonEmpty(mapper.AssistantText(), summary),
				AssistantMessageID: mapper.AssistantMessageID(),
				ToolInvocations:    mapper.ToolInvocations(),
				OutputSummary:      mapper.OutputSummary(),
				MessagesPersisted:  messagesPersisted,
			}
			if interactionKind == "human_input" {
				result.AwaitingInput = true
			} else {
				result.WaitForApproval = true
			}
			return result, nil
		case "turn/completed":
			if err := mapper.HandleNotification(ctx, msg.Method, msg.Params); err != nil {
				mapper.FlushArtifacts(ctx)
				return nil, err
			}
			mapper.FlushArtifacts(ctx)
			completed := mapper.CompletedTurn()
			if completed != nil && strings.EqualFold(strings.TrimSpace(completed.Status), "interrupted") {
				return nil, fmt.Errorf("codex turn was interrupted")
			}
			if completed != nil && completed.Error != nil && strings.TrimSpace(completed.Error.Message) != "" {
				return nil, fmt.Errorf("codex turn failed: %s", strings.TrimSpace(completed.Error.Message))
			}
			pendingInteraction, waitForApproval, awaitingInput, err := latestPendingRuntimeInteraction(ctx, execCtx)
			if err != nil {
				return nil, err
			}
			if pendingInteraction != nil && awaitingInput {
				if state != nil {
					state.PendingRequest = nil
					state.PendingInteraction = pendingInteraction
					_ = a.promoteCodexAuth(ctx, execCtx, state)
					if err := newCodexSessionStore(execCtx.Store).Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
						return nil, err
					}
				}
				messagesPersisted, persistErr := mapper.PersistMessages(ctx)
				if persistErr != nil {
					return nil, persistErr
				}
				return &Result{
					AssistantMessage:   mapper.AssistantText(),
					AssistantMessageID: mapper.AssistantMessageID(),
					ToolInvocations:    mapper.ToolInvocations(),
					OutputSummary:      mapper.OutputSummary(),
					AwaitingInput:      true,
					MessagesPersisted:  messagesPersisted,
				}, nil
			}
			missingCompletionTools, err := missingCodexCompletionTools(ctx, execCtx)
			if err != nil {
				return nil, err
			}
			if len(missingCompletionTools) > 0 {
				if completionToolRetryAttempted {
					return nil, fmt.Errorf("codex turn completed without successful required tool calls: %s", strings.Join(missingCompletionTools, ", "))
				}
				threadID := ""
				if state != nil {
					threadID = strings.TrimSpace(state.ThreadID)
				}
				if threadID == "" {
					return nil, fmt.Errorf("codex turn completed without required tool calls and no resumable thread is available: %s", strings.Join(missingCompletionTools, ", "))
				}
				if _, err := mapper.PersistMessages(ctx); err != nil {
					return nil, err
				}
				if err := a.startCodexTurn(ctx, client, threadID, codexCompletionToolRetryPrompt(missingCompletionTools)); err != nil {
					return nil, err
				}
				completionToolRetryAttempted = true
				mapper = newCodexEventMapper(execCtx, workDir)
				continue
			}
			if pendingInteraction != nil {
				if state != nil {
					state.PendingRequest = nil
					state.PendingInteraction = pendingInteraction
					_ = a.promoteCodexAuth(ctx, execCtx, state)
					if err := newCodexSessionStore(execCtx.Store).Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
						return nil, err
					}
				}
				messagesPersisted, persistErr := mapper.PersistMessages(ctx)
				if persistErr != nil {
					return nil, persistErr
				}
				return &Result{
					AssistantMessage:   mapper.AssistantText(),
					AssistantMessageID: mapper.AssistantMessageID(),
					ToolInvocations:    mapper.ToolInvocations(),
					OutputSummary:      mapper.OutputSummary(),
					WaitForApproval:    waitForApproval,
					MessagesPersisted:  messagesPersisted,
				}, nil
			}
			if codexCompletionRequiresInteraction(execCtx) && !codexCompletionAllowedAfterApproval(execCtx) {
				synthesized, ok, err := synthesizeCodexCompletionApproval(ctx, execCtx, mapper.AssistantText())
				if err != nil {
					return nil, err
				}
				if ok {
					if state != nil {
						state.PendingRequest = nil
						state.PendingInteraction = synthesized
						_ = a.promoteCodexAuth(ctx, execCtx, state)
						if err := newCodexSessionStore(execCtx.Store).Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
							return nil, err
						}
					}
					messagesPersisted, persistErr := mapper.PersistMessages(ctx)
					if persistErr != nil {
						return nil, persistErr
					}
					return &Result{
						AssistantMessage:   mapper.AssistantText(),
						AssistantMessageID: mapper.AssistantMessageID(),
						ToolInvocations:    mapper.ToolInvocations(),
						OutputSummary:      mapper.OutputSummary(),
						WaitForApproval:    true,
						MessagesPersisted:  messagesPersisted,
					}, nil
				}
				if policyRetryAttempted {
					return nil, fmt.Errorf("codex turn completed without an interaction required by the active skills")
				}
				threadID := ""
				if state != nil {
					threadID = strings.TrimSpace(state.ThreadID)
				}
				if threadID == "" {
					return nil, fmt.Errorf("codex turn completed without a required interaction and no resumable thread is available")
				}
				if _, err := mapper.PersistMessages(ctx); err != nil {
					return nil, err
				}
				if err := a.startCodexTurn(ctx, client, threadID, codexCompletionInteractionRetryPrompt(execCtx)); err != nil {
					return nil, err
				}
				policyRetryAttempted = true
				mapper = newCodexEventMapper(execCtx, workDir)
				continue
			}
			if state != nil {
				state.PendingRequest = nil
				state.PendingInteraction = nil
				_ = a.promoteCodexAuth(ctx, execCtx, state)
				if err := newCodexSessionStore(execCtx.Store).Clear(ctx, execCtx.Run.AppID, execCtx.Run.ID); err != nil {
					return nil, err
				}
			}
			status := ""
			if completed != nil {
				status = strings.TrimSpace(completed.Status)
			}
			slog.InfoContext(ctx, "codex turn complete", "run_id", runID, "status", status, "elapsed_ms", time.Since(startedAt).Milliseconds(), "assistant_message_id", mapper.AssistantMessageID())
			messagesPersisted, persistErr := mapper.PersistMessages(ctx)
			if persistErr != nil {
				return nil, persistErr
			}
			return &Result{
				AssistantMessage:   mapper.AssistantText(),
				AssistantMessageID: mapper.AssistantMessageID(),
				ToolInvocations:    mapper.ToolInvocations(),
				OutputSummary:      mapper.OutputSummary(),
				MessagesPersisted:  messagesPersisted,
			}, nil
		default:
			if err := mapper.HandleNotification(ctx, msg.Method, msg.Params); err != nil {
				mapper.FlushArtifacts(ctx)
				return nil, err
			}
		}
	}
}

func (a *CodexAdapter) maybeDeclineForbiddenCodexCommand(ctx context.Context, client *codexAppServerClient, msg codexRPCMessage, state *codexSessionState) (bool, error) {
	if strings.TrimSpace(msg.Method) != "item/commandExecution/requestApproval" {
		return false, nil
	}
	var payload codexCommandExecutionRequestApprovalParams
	if err := json.Unmarshal(msg.Params, &payload); err != nil {
		return false, err
	}
	command := ""
	if payload.Command != nil {
		command = strings.TrimSpace(*payload.Command)
	}
	if !isForbiddenCodexDeliveryCommand(command) {
		return false, nil
	}
	if err := client.Respond(ctx, msg.ID, map[string]any{"decision": "decline"}); err != nil {
		return true, err
	}
	threadID := strings.TrimSpace(payload.ThreadID)
	if threadID == "" && state != nil {
		threadID = strings.TrimSpace(state.ThreadID)
	}
	if threadID == "" {
		return true, nil
	}
	return true, a.startCodexTurn(ctx, client, threadID, "Do not push branches, open pull requests, or run remote delivery commands from inside this run. The platform will fetch, merge, push, and open delivery records after the run completes. Continue by making a local commit only, then summarize the completed work.")
}

func isForbiddenCodexDeliveryCommand(command string) bool {
	normalized := strings.ToLower(strings.TrimSpace(command))
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return false
	}
	for _, prefix := range []string{
		"git push",
		"gh pr",
		"hub pr",
		"hub pull-request",
		"glab mr",
	} {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+" ") {
			return true
		}
	}
	return false
}

func (a *CodexAdapter) respondToPendingCodexRequest(ctx context.Context, client codexAppServerRPC, execCtx *ExecutionContext, state *codexSessionState) (codexPendingResumeResult, error) {
	if state == nil || state.PendingRequest == nil {
		return codexPendingResumeResult{}, fmt.Errorf("missing pending codex request")
	}
	runID := ""
	if execCtx != nil && execCtx.Run != nil {
		runID = execCtx.Run.ID
	}
	intent, content, responsePayload := lastResumePayload(execCtx)
	startedAt := time.Now()
	slog.InfoContext(ctx, "codex awaiting pending request replay",
		"run_id", runID,
		"kind", state.PendingRequest.Kind,
		"request_id", state.PendingRequest.RequestID,
		"turn_id", state.PendingRequest.TurnID,
		"item_id", state.PendingRequest.ItemID,
		"resume_intent", intent,
		"has_resume_content", strings.TrimSpace(content) != "",
		"timeout_ms", a.pendingReplayTimeout().Milliseconds(),
	)
	msg, err := a.awaitPendingCodexRequestReplay(ctx, client)
	if err != nil {
		if errors.Is(err, errCodexPendingReplayTimeout) {
			prompt, promptErr := codexResumeFallbackPrompt(state.PendingRequest, intent, content, responsePayload)
			if promptErr != nil {
				slog.WarnContext(ctx, "codex pending request fallback prompt failed",
					"run_id", runID,
					"kind", state.PendingRequest.Kind,
					"request_id", state.PendingRequest.RequestID,
					"elapsed_ms", time.Since(startedAt).Milliseconds(),
					"error", promptErr,
				)
				return codexPendingResumeResult{}, promptErr
			}
			slog.InfoContext(ctx, "codex pending request replay timed out; using fallback turn",
				"run_id", runID,
				"kind", state.PendingRequest.Kind,
				"request_id", state.PendingRequest.RequestID,
				"elapsed_ms", time.Since(startedAt).Milliseconds(),
			)
			return codexPendingResumeResult{FallbackPrompt: prompt}, nil
		}
		slog.WarnContext(ctx, "codex pending request replay failed",
			"run_id", runID,
			"kind", state.PendingRequest.Kind,
			"request_id", state.PendingRequest.RequestID,
			"elapsed_ms", time.Since(startedAt).Milliseconds(),
			"error", err,
		)
		return codexPendingResumeResult{}, err
	}
	slog.InfoContext(ctx, "codex pending request replay received",
		"run_id", runID,
		"kind", state.PendingRequest.Kind,
		"request_id", state.PendingRequest.RequestID,
		"replayed_method", msg.Method,
		"elapsed_ms", time.Since(startedAt).Milliseconds(),
	)
	pendingID := msg.ID
	if len(pendingID) == 0 {
		pendingID = codexPendingRequestResponseID(state.PendingRequest)
	}
	response, followup, err := codexResumeResponse(state.PendingRequest, intent, content, responsePayload)
	if err != nil {
		return codexPendingResumeResult{}, err
	}
	if err := client.Respond(ctx, pendingID, response); err != nil {
		return codexPendingResumeResult{}, err
	}
	slog.InfoContext(ctx, "codex pending request response sent",
		"run_id", runID,
		"kind", state.PendingRequest.Kind,
		"request_id", state.PendingRequest.RequestID,
		"has_followup", strings.TrimSpace(followup) != "",
	)
	return codexPendingResumeResult{Response: response, Followup: followup, Replayed: true}, nil
}

func (a *CodexAdapter) awaitPendingCodexRequestReplay(ctx context.Context, client codexAppServerRPC) (codexRPCMessage, error) {
	parentCtx := ctx
	timeout := a.pendingReplayTimeout()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for {
		msg, err := client.Next(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				if parentCtx.Err() != nil {
					return codexRPCMessage{}, err
				}
				return codexRPCMessage{}, errCodexPendingReplayTimeout
			}
			return codexRPCMessage{}, err
		}
		if isCodexPauseRequestMethod(msg.Method) {
			return msg, nil
		}
		if strings.TrimSpace(msg.Method) != "" && len(msg.ID) > 0 {
			slog.WarnContext(ctx, "codex unsupported server request while awaiting pending replay", "method", strings.TrimSpace(msg.Method))
			return codexRPCMessage{}, fmt.Errorf("unsupported codex server request while awaiting pending replay: %s", strings.TrimSpace(msg.Method))
		}
	}
}

func (a *CodexAdapter) pendingReplayTimeout() time.Duration {
	if a != nil && a.cfg.PendingReplayTimeout > 0 {
		return a.cfg.PendingReplayTimeout
	}
	return defaultCodexPendingReplayTimeout
}

func codexPendingKind(state *codexSessionState) string {
	if state == nil || state.PendingRequest == nil {
		return ""
	}
	return strings.TrimSpace(state.PendingRequest.Kind)
}

func (a *CodexAdapter) requestCodexInteraction(ctx context.Context, execCtx *ExecutionContext, pending *codexPendingRequest, kind string, summary string, payload json.RawMessage) {
	if execCtx == nil || execCtx.InteractionBroker == nil || pending == nil {
		return
	}
	title := "Codex needs input"
	if kind != "human_input" {
		title = "Codex needs approval"
	}
	metadata, _ := json.Marshal(map[string]interface{}{
		"runtime_kind":       agentcore.RuntimeCodex,
		"codex_request_kind": pending.Kind,
		"codex_request_id":   pending.RequestID,
		"codex_turn_id":      pending.TurnID,
		"codex_item_id":      pending.ItemID,
	})
	_ = execCtx.InteractionBroker.RequestInteraction(ctx, agentcore.AgentRunInteraction{
		InteractionKind: kind,
		Status:          "pending",
		Title:           title,
		Summary:         strings.TrimSpace(summary),
		RequestPayload:  append(json.RawMessage(nil), payload...),
		ResponsePayload: metadata,
	})
}

func lastResumePayload(execCtx *ExecutionContext) (intent string, content string, responsePayload json.RawMessage) {
	if execCtx == nil || execCtx.Run == nil || execCtx.Run.Input.Metadata == nil {
		return "", "", nil
	}
	raw, ok := execCtx.Run.Input.Metadata["last_resume"]
	if !ok {
		return "", "", nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return "", "", nil
	}
	var payload struct {
		Intent          string          `json:"intent"`
		Content         string          `json:"content"`
		ResponsePayload json.RawMessage `json:"response_payload"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", nil
	}
	return strings.TrimSpace(payload.Intent), strings.TrimSpace(payload.Content), payload.ResponsePayload
}

func (a *CodexAdapter) codexDeveloperInstructions(execCtx *ExecutionContext, state *codexSessionState) string {
	parts := []string{
		strings.TrimSpace(a.cfg.DeveloperInstructions),
		strings.TrimSpace(skills.RenderRuntimeToolNamesInInstructionsForRuntime(execCtx.Agent.SystemPrompt, agentcore.RuntimeCodex)),
	}
	if kinds := codexCompletionInteractionKinds(execCtx); len(kinds) > 0 {
		parts = append(parts, "Runtime interaction contract:\nThis run must pause for one of these interactions before completion: "+strings.Join(kinds, ", ")+". When approval is required, do not ask for it only in prose: call `request_approval` as the final action after publishing the complete review artifact. The Agent Runtime will preserve the thread and resume it after the human approves or requests changes.")
	}
	if codexWebSearchEnabled(execCtx) {
		parts = append(parts, "Web research is enabled through Codex's built-in web search. If task instructions name web_search_exa or web_search_brave but that dynamic tool is not present, use the built-in web search instead. Do not report web search as unavailable without attempting the built-in capability.")
	}
	if execCtx.TargetContext != nil && strings.TrimSpace(execCtx.TargetContext.Summary) != "" {
		parts = append(parts, "Target context:\n"+strings.TrimSpace(execCtx.TargetContext.Summary))
	}
	if strings.TrimSpace(execCtx.StagedSkillRoot) != "" && state != nil && strings.TrimSpace(state.CodexHome) != "" {
		codexSkillRoot := filepath.Join(strings.TrimSpace(state.CodexHome), "skills", codexRuntimeSkillNamespace)
		parts = append(parts, "Active runtime skills are installed for Codex discovery at:\n"+codexSkillRoot+"\nUse the absolute skill paths supplied by Codex. Do not construct repository-relative paths under .agent-runtime/skills.")
	}
	if branchInstructions := repositoryBranchSyncInstructions(execCtx); branchInstructions != "" {
		parts = append(parts, branchInstructions)
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n\n")
}

func codexWebSearchEnabled(execCtx *ExecutionContext) bool {
	if execCtx == nil {
		return false
	}
	return execCtx.AllowedTools["web_search_exa"] || execCtx.AllowedTools["web_search_brave"] || execCtx.AllowedTools["web_search"]
}

func repositoryBranchSyncInstructions(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.WorkspaceLease == nil || execCtx.WorkspaceLease.Metadata == nil {
		return ""
	}
	status := strings.TrimSpace(firstMapStringAny(execCtx.WorkspaceLease.Metadata, "branch_sync_status"))
	if status != "conflicted" {
		return ""
	}
	parts := []string{
		"Repository branch sync produced merge conflicts before this run.",
		"Before continuing the task, resolve the current git merge conflict that came from syncing the base branch into the working branch.",
		"Preserve the task's intended changes while incorporating the incoming base-branch changes. Remove all conflict markers, stage the resolved files, and complete the merge commit before doing additional implementation work.",
	}
	if files := stringSliceFromAny(execCtx.WorkspaceLease.Metadata["branch_sync_conflict_files"]); len(files) > 0 {
		parts = append(parts, "Conflicted files: "+strings.Join(files, ", ")+".")
	}
	return strings.Join(parts, "\n")
}

func stringSliceFromAny(value interface{}) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		parts := strings.Split(typed, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if text := strings.TrimSpace(part); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func (a *CodexAdapter) executeCommand(execCtx *ExecutionContext) (*Result, error) {
	ctx := execCtx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if a.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.cfg.Timeout)
		defer cancel()
	}
	resolvedWorkDir, cleanupWorkDir, err := resolveRuntimeWorkDir(execCtx, a.cfg.WorkDir, agentcore.RuntimeCodex)
	if err != nil {
		return nil, err
	}
	defer cleanupWorkDir()
	bin, prefixArgs, workDir, err := ResolveCodexLaunch(a.cfg.CommandPath, resolvedWorkDir)
	if err != nil {
		return nil, err
	}
	args := append(prefixArgs, a.cfg.Args...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workDir
	env := procenv.Sanitized(a.cfg.Env...)
	if strings.TrimSpace(execCtx.StagedSkillRoot) != "" {
		codexHome, err := prepareCommandCodexHome(execCtx, a.cfg.RuntimeRoot)
		if err != nil {
			return nil, err
		}
		// The command path has no session state; only the sandbox setting is
		// runtime-owned here.
		if err := writeCodexHomeConfig(codexHome, a.codexHomeConfig("", "", "")); err != nil {
			return nil, fmt.Errorf("write codex config: %w", err)
		}
		env = upsertEnv(env, "CODEX_HOME", codexHome)
	}
	cmd.Env = env
	payload, _ := json.Marshal(commandExecutionPayload{
		AppID:           execCtx.AppID,
		Agent:           execCtx.Agent,
		Run:             execCtx.Run,
		TargetContext:   execCtx.TargetContext,
		WorkspaceLease:  execCtx.WorkspaceLease,
		AllowedTools:    execCtx.AllowedTools,
		StagedSkillRoot: execCtx.StagedSkillRoot,
	})
	cmd.Stdin = bytes.NewReader(payload)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	repoSkillMask, err := maskCodexRepoSkillRoots(execCtx, workDir)
	if err != nil {
		return nil, err
	}
	if repoSkillMask != nil {
		defer func() {
			_ = repoSkillMask.Restore()
		}()
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("codex command failed: %s", msg)
	}
	return decodeCommandResult(stdout.Bytes())
}

func maskCodexRepoSkillRoots(execCtx *ExecutionContext, workDir string) (*runtimeworkspace.RepoSkillMask, error) {
	runID := ""
	if execCtx != nil && execCtx.Run != nil {
		runID = execCtx.Run.ID
	}
	return runtimeworkspace.MaskRepoSkillRoots(workDir, runID)
}

func prepareCommandCodexHome(execCtx *ExecutionContext, runtimeRoot string) (string, error) {
	if execCtx == nil || execCtx.Run == nil {
		return "", fmt.Errorf("execution context is incomplete")
	}
	root := strings.TrimSpace(runtimeRoot)
	if root == "" {
		root = filepath.Join(os.TempDir(), "agent-runtime-codex")
	}
	codexHome := filepath.Join(root, sanitizeCodexPathComponent(execCtx.Run.AppID), sanitizeCodexPathComponent(execCtx.Run.ID), "home", ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		return "", fmt.Errorf("create command codex home: %w", err)
	}
	if err := skills.SyncRuntimeSkillRoot(execCtx.StagedSkillRoot, filepath.Join(codexHome, "skills", codexRuntimeSkillNamespace)); err != nil {
		return "", fmt.Errorf("sync staged codex skills: %w", err)
	}
	return codexHome, nil
}

type commandExecutionPayload struct {
	AppID           string                    `json:"app_id"`
	Agent           *agentcore.Agent          `json:"agent"`
	Run             *agentcore.AgentRun       `json:"run"`
	TargetContext   interface{}               `json:"target_context,omitempty"`
	WorkspaceLease  *agentcore.WorkspaceLease `json:"workspace_lease,omitempty"`
	AllowedTools    map[string]bool           `json:"allowed_tools,omitempty"`
	StagedSkillRoot string                    `json:"staged_skill_root,omitempty"`
	Metadata        map[string]interface{}    `json:"metadata,omitempty"`
}

func workspaceRoot(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.WorkspaceLease == nil {
		return ""
	}
	return strings.TrimSpace(execCtx.WorkspaceLease.RootPath)
}

func decodeCommandResult(output []byte) (*Result, error) {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return &Result{}, nil
	}
	var result struct {
		AssistantMessage   string          `json:"assistant_message"`
		AssistantMessageID string          `json:"assistant_message_id"`
		OutputSummary      json.RawMessage `json:"output_summary"`
		WaitForApproval    bool            `json:"wait_for_approval"`
		AwaitingInput      bool            `json:"awaiting_input"`
		AwaitingAuth       bool            `json:"awaiting_auth"`
	}
	if err := json.Unmarshal(output, &result); err == nil {
		return &Result{
			AssistantMessage:   result.AssistantMessage,
			AssistantMessageID: result.AssistantMessageID,
			OutputSummary:      result.OutputSummary,
			WaitForApproval:    result.WaitForApproval,
			AwaitingInput:      result.AwaitingInput,
			AwaitingAuth:       result.AwaitingAuth,
		}, nil
	}
	return &Result{AssistantMessage: text}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func upsertEnv(env []string, key, value string) []string {
	key = strings.TrimSpace(key)
	if key == "" {
		return env
	}
	prefix := key + "="
	for idx, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[idx] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func sanitizeCodexPathComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "run"
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	value = strings.Trim(builder.String(), "-")
	if value == "" {
		return "run"
	}
	return value
}
