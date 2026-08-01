package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
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
}

type CodexAdapter struct {
	cfg CodexConfig
}

const codexRuntimeSkillNamespace = "agent-runtime"

func NewCodexAdapter() *CodexAdapter {
	return NewCodexAdapterWithConfig(DefaultCodexConfigFromEnv())
}

func DefaultCodexConfigFromEnv() CodexConfig {
	cfg := CodexConfig{
		CommandPath:    strings.TrimSpace(os.Getenv("CODEX_PATH")),
		OpenAIAuthMode: strings.TrimSpace(os.Getenv("CODEX_OPENAI_AUTH_MODE")),
		RuntimeRoot:    strings.TrimSpace(os.Getenv("AGENT_RUNTIME_CODEX_ROOT")),
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
	if a.cfg.AppServer {
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
	workDir := firstNonEmpty(workspaceRoot(execCtx), a.cfg.WorkDir, ".")
	sessionStore := newCodexSessionStore(execCtx.Store)
	state, err := sessionStore.Load(ctx, execCtx.Run.AppID, execCtx.Run.ID)
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = &codexSessionState{}
	}
	if err := a.prepareCodexHome(ctx, execCtx, state); err != nil {
		return nil, err
	}
	provider := firstNonEmpty(a.cfg.ModelProvider, execCtx.Agent.Provider, "openai")
	authMode := a.authModeForProvider(provider)
	if authMode != "" {
		state.Provider = provider
		state.AuthMode = authMode
	}
	if err := a.restoreCodexAuth(ctx, execCtx, state); err != nil {
		return nil, err
	}
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
	if err := client.Start(ctx); err != nil {
		return nil, err
	}
	defer client.Close()
	if err := client.Initialize(ctx); err != nil {
		return nil, err
	}
	authResult, err := a.ensureCodexAuthenticated(ctx, client, execCtx, state)
	if err != nil {
		if a.codexShouldReauthForError(state, err) {
			_ = a.clearCodexAuth(ctx, execCtx, state)
			return a.codexAuthRequiredResult(ctx, execCtx, state, "ChatGPT authentication needs to be refreshed."), nil
		}
		return nil, err
	}
	if authResult != nil {
		if err := sessionStore.Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
			return nil, err
		}
		return authResult, nil
	}
	threadID, err := a.startOrResumeCodexThread(ctx, client, execCtx, workDir, state)
	if err != nil {
		return nil, err
	}
	writeCodexConfigArtifact(ctx, execCtx, workDir, a.cfg, state)
	if state.PendingRequest != nil {
		response, followup, err := a.respondToPendingCodexRequest(ctx, client, execCtx, state)
		if err != nil {
			return nil, err
		}
		state.PendingRequest = nil
		if err := sessionStore.Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
			return nil, err
		}
		result, err := a.collectCodexTurn(ctx, client, workDir, execCtx, state)
		if err != nil || strings.TrimSpace(followup) == "" {
			_ = a.promoteCodexAuth(ctx, execCtx, state)
			return result, err
		}
		if err := a.startCodexTurn(ctx, client, threadID, followup); err != nil {
			return result, err
		}
		_ = response
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
	if err := a.startCodexTurn(ctx, client, threadID, input); err != nil {
		return nil, err
	}
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
	state.HomeRoot = runRoot
	state.CodexHome = codexHome
	return nil
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
		return nil, nil
	}
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
		return nil, nil
	}
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
	params := map[string]any{
		"cwd":                   workDir,
		"modelProvider":         firstNonEmpty(a.cfg.ModelProvider, execCtx.Agent.Provider, "openai"),
		"approvalPolicy":        firstNonEmpty(a.cfg.ApprovalPolicy, "on-request"),
		"approvalsReviewer":     firstNonEmpty(a.cfg.ApprovalsReviewer, "user"),
		"sandbox":               firstNonEmpty(a.cfg.Sandbox, "workspace-write"),
		"serviceName":           "Agent Runtime",
		"developerInstructions": a.codexDeveloperInstructions(execCtx, state),
	}
	if model := firstNonEmpty(a.cfg.Model, execCtx.Agent.Model); model != "" {
		params["model"] = model
	}
	method := "thread/start"
	if state != nil && strings.TrimSpace(state.ThreadID) != "" {
		method = "thread/resume"
		params["threadId"] = strings.TrimSpace(state.ThreadID)
	}
	raw, err := client.Request(ctx, method, params)
	if err != nil {
		return "", err
	}
	var response codexThreadLifecycleResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("decode codex thread response: %w", err)
	}
	if strings.TrimSpace(response.Thread.ID) == "" {
		return "", fmt.Errorf("codex %s returned an empty thread id", method)
	}
	if state != nil {
		state.ThreadID = strings.TrimSpace(response.Thread.ID)
		if response.Thread.Path != nil {
			state.ThreadPath = strings.TrimSpace(*response.Thread.Path)
		}
		state.Provider = firstNonEmpty(strings.TrimSpace(response.ModelProvider), state.Provider, firstNonEmpty(a.cfg.ModelProvider, execCtx.Agent.Provider))
		state.Model = firstNonEmpty(strings.TrimSpace(response.Model), state.Model, firstNonEmpty(a.cfg.Model, execCtx.Agent.Model))
		state.Sandbox = firstNonEmpty(a.cfg.Sandbox, "workspace-write")
		state.InvocationMode = execCtx.Run.InvocationMode
	}
	return strings.TrimSpace(response.Thread.ID), nil
}

func (a *CodexAdapter) startCodexTurn(ctx context.Context, client *codexAppServerClient, threadID string, input string) error {
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
	for {
		msg, err := client.Next(ctx)
		if err != nil {
			mapper.FlushArtifacts(ctx)
			return nil, err
		}
		switch strings.TrimSpace(msg.Method) {
		case "item/tool/requestUserInput", "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
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
			mapper.FlushArtifacts(ctx)
			result := &Result{
				AssistantMessage:   firstNonEmpty(mapper.AssistantText(), summary),
				AssistantMessageID: mapper.AssistantMessageID(),
				ToolInvocations:    mapper.ToolInvocations(),
				OutputSummary:      mapper.OutputSummary(),
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
			if pendingInteraction != nil {
				if state != nil {
					state.PendingRequest = nil
					state.PendingInteraction = pendingInteraction
					_ = a.promoteCodexAuth(ctx, execCtx, state)
					if err := newCodexSessionStore(execCtx.Store).Save(ctx, execCtx.Run.AppID, execCtx.Run.ID, state); err != nil {
						return nil, err
					}
				}
				return &Result{
					AssistantMessage:   mapper.AssistantText(),
					AssistantMessageID: mapper.AssistantMessageID(),
					ToolInvocations:    mapper.ToolInvocations(),
					OutputSummary:      mapper.OutputSummary(),
					WaitForApproval:    waitForApproval,
					AwaitingInput:      awaitingInput,
				}, nil
			}
			if codexCompletionRequiresInteraction(execCtx) && !codexCompletionAllowedAfterApproval(execCtx) {
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
				if err := persistCodexMapperMessage(ctx, execCtx, mapper); err != nil {
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
			return &Result{
				AssistantMessage:   mapper.AssistantText(),
				AssistantMessageID: mapper.AssistantMessageID(),
				ToolInvocations:    mapper.ToolInvocations(),
				OutputSummary:      mapper.OutputSummary(),
			}, nil
		default:
			if err := mapper.HandleNotification(ctx, msg.Method, msg.Params); err != nil {
				mapper.FlushArtifacts(ctx)
				return nil, err
			}
		}
	}
}

func (a *CodexAdapter) respondToPendingCodexRequest(ctx context.Context, client *codexAppServerClient, execCtx *ExecutionContext, state *codexSessionState) (any, string, error) {
	if state == nil || state.PendingRequest == nil {
		return nil, "", fmt.Errorf("missing pending codex request")
	}
	msg, err := a.awaitPendingCodexRequestReplay(ctx, client)
	if err != nil {
		return nil, "", err
	}
	pendingID := msg.ID
	if len(pendingID) == 0 {
		pendingID = codexPendingRequestResponseID(state.PendingRequest)
	}
	intent, content, responsePayload := lastResumePayload(execCtx)
	response, followup, err := codexResumeResponse(state.PendingRequest, intent, content, responsePayload)
	if err != nil {
		return nil, "", err
	}
	if err := client.Respond(ctx, pendingID, response); err != nil {
		return nil, "", err
	}
	return response, followup, nil
}

func (a *CodexAdapter) awaitPendingCodexRequestReplay(ctx context.Context, client *codexAppServerClient) (codexRPCMessage, error) {
	for {
		msg, err := client.Next(ctx)
		if err != nil {
			return codexRPCMessage{}, err
		}
		if isCodexPauseRequestMethod(msg.Method) {
			return msg, nil
		}
		if strings.TrimSpace(msg.Method) != "" && len(msg.ID) > 0 {
			return codexRPCMessage{}, fmt.Errorf("unsupported codex server request while awaiting pending replay: %s", strings.TrimSpace(msg.Method))
		}
	}
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
		strings.TrimSpace(execCtx.Agent.SystemPrompt),
	}
	if execCtx.TargetContext != nil && strings.TrimSpace(execCtx.TargetContext.Summary) != "" {
		parts = append(parts, "Target context:\n"+strings.TrimSpace(execCtx.TargetContext.Summary))
	}
	if strings.TrimSpace(execCtx.StagedSkillRoot) != "" && state != nil && strings.TrimSpace(state.CodexHome) != "" {
		codexSkillRoot := filepath.Join(strings.TrimSpace(state.CodexHome), "skills", codexRuntimeSkillNamespace)
		parts = append(parts, "Active runtime skills are installed for Codex discovery at:\n"+codexSkillRoot+"\nUse the absolute skill paths supplied by Codex. Do not construct repository-relative paths under .agent-runtime/skills.")
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n\n")
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
	bin, prefixArgs, workDir, err := ResolveCodexLaunch(a.cfg.CommandPath, firstNonEmpty(workspaceRoot(execCtx), a.cfg.WorkDir, "."))
	if err != nil {
		return nil, err
	}
	args := append(prefixArgs, a.cfg.Args...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workDir
	env := append(os.Environ(), a.cfg.Env...)
	if strings.TrimSpace(execCtx.StagedSkillRoot) != "" {
		codexHome, err := prepareCommandCodexHome(execCtx, a.cfg.RuntimeRoot)
		if err != nil {
			return nil, err
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
