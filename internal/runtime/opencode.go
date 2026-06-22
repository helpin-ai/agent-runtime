package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	runtimeworkspace "github.com/helpin-ai/agent-runtime/internal/workspace"
)

const (
	openCodeChunkFlushInterval    = 2 * time.Second
	openCodeChunkFlushBytes       = 4 * 1024
	openCodeScannerBufferSize     = 1024 * 1024
	openCodeGracefulShutdownDelay = 10 * time.Second
	openCodeRepoChangeWaitTimeout = 60 * time.Second
	openCodeRepoChangePollEvery   = 2 * time.Second
)

type OpenCodeConfig struct {
	CommandPath       string
	Args              []string
	Env               []string
	WorkDir           string
	Timeout           time.Duration
	RuntimeRoot       string
	AnthropicAPIKey   string
	AnthropicBaseURL  string
	OpenAIAPIKey      string
	OpenAIBaseURL     string
	OpenRouterAPIKey  string
	OpenRouterBaseURL string
}

type OpenCodeAdapter struct {
	cfg OpenCodeConfig
}

func NewOpenCodeAdapter() *OpenCodeAdapter {
	return NewOpenCodeAdapterWithConfig(DefaultOpenCodeConfigFromEnv())
}

func DefaultOpenCodeConfigFromEnv() OpenCodeConfig {
	commandPath := strings.TrimSpace(os.Getenv("OPENCODE_PATH"))
	if commandPath == "" {
		commandPath = "opencode"
	}
	return OpenCodeConfig{
		CommandPath:       commandPath,
		RuntimeRoot:       strings.TrimSpace(os.Getenv("AGENT_RUNTIME_OPENCODE_ROOT")),
		AnthropicAPIKey:   strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		AnthropicBaseURL:  strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
		OpenAIAPIKey:      strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIBaseURL:     strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
		OpenRouterAPIKey:  strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")),
		OpenRouterBaseURL: strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL")),
	}
}

func NewOpenCodeAdapterWithConfig(cfg OpenCodeConfig) *OpenCodeAdapter {
	if strings.TrimSpace(cfg.CommandPath) == "" {
		cfg.CommandPath = "opencode"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	return &OpenCodeAdapter{cfg: cfg}
}

func (a *OpenCodeAdapter) Kind() string {
	return agentcore.RuntimeOpenCode
}

func (a *OpenCodeAdapter) Execute(execCtx *ExecutionContext) (*Result, error) {
	if execCtx == nil || execCtx.Run == nil || execCtx.Agent == nil {
		return nil, fmt.Errorf("execution context is incomplete")
	}
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
	systemPrompt := buildOpenCodeSystemPrompt(execCtx)
	userPrompt := buildOpenCodeUserPrompt(execCtx)
	modelID := a.resolveModelID(execCtx.Agent)
	providerConfig := a.buildProviderConfig(execCtx.Agent)
	configContent, err := buildOpenCodeConfigContent(execCtx, modelID, systemPrompt, providerConfig)
	if err != nil {
		return nil, fmt.Errorf("build opencode config: %w", err)
	}

	writeOpenCodeArtifact(ctx, execCtx, "opencode_config", "json", configContent, false)
	writeOpenCodeArtifact(ctx, execCtx, "opencode_prompt", "markdown", buildOpenCodePromptArtifact(systemPrompt, userPrompt), false)

	args := []string{"run"}
	args = append(args, a.cfg.Args...)
	if agentName := openCodeAgentName(execCtx); agentName != "" {
		args = append(args, "--agent", agentName)
	}
	if modelID != "" {
		args = append(args, "--model", modelID)
	}
	args = append(args, "--format", "json", userPrompt)

	env, err := a.buildEnv(execCtx, configContent)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, a.cfg.CommandPath, args...)
	cmd.Dir = workDir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = openCodeGracefulShutdownDelay

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create opencode stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("create opencode stderr pipe: %w", err)
	}

	repoSkillMask, err := maskOpenCodeRepoSkillRoots(execCtx, workDir)
	if err != nil {
		return nil, err
	}
	if repoSkillMask != nil {
		defer func() {
			_ = repoSkillMask.Restore()
		}()
	}

	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("opencode executable %q was not found on PATH", a.cfg.CommandPath)
		}
		return nil, fmt.Errorf("start opencode run: %w", err)
	}

	collector := newOpenCodeStreamCollector(execCtx)
	streamErrs := make(chan error, 2)
	var streamWG sync.WaitGroup
	streamWG.Add(2)
	go consumeOpenCodeJSONStream(stdoutPipe, ctx, collector, &streamWG, streamErrs)
	go consumeOpenCodeTextStream(stderrPipe, "stderr", ctx, collector, &streamWG, streamErrs)

	flushDone := make(chan struct{})
	go collector.FlushLoop(ctx, flushDone)

	waitErr := cmd.Wait()
	close(flushDone)
	streamWG.Wait()
	close(streamErrs)

	collector.FlushPending(ctx, true)
	stdoutText, stderrText := collector.Outputs()
	writeOpenCodeArtifact(ctx, execCtx, "opencode_stdout", "text", stdoutText, false)
	writeOpenCodeArtifact(ctx, execCtx, "opencode_stderr", "text", stderrText, false)

	for streamErr := range streamErrs {
		if streamErr == nil {
			continue
		}
		if waitErr == nil {
			waitErr = streamErr
		}
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		message := strings.TrimSpace(firstNonEmpty(stderrText, waitErr.Error()))
		return nil, fmt.Errorf("opencode run failed: %s", message)
	}
	if eventErr := collector.EventError(); eventErr != "" {
		return nil, fmt.Errorf("opencode reported an error: %s", eventErr)
	}

	responseText := sanitizeOpenCodeOutput(firstNonEmpty(collector.ResponseText(), stdoutText, stderrText))
	if responseText == "" {
		return nil, fmt.Errorf("opencode returned no response (stdout=%q stderr=%q)", truncateOpenCodeString(stdoutText, 500), truncateOpenCodeString(stderrText, 500))
	}
	writeOpenCodeArtifact(ctx, execCtx, "agent_summary", "markdown", responseText, false)
	awaitingInput, waitForApproval, err := processOpenCodeRuntimeHandoff(ctx, execCtx, responseText)
	if err != nil {
		return nil, err
	}
	var repoPersistence map[string]any
	if !awaitingInput && !waitForApproval {
		repoPersistence, err = persistOpenCodeRepositoryChanges(ctx, execCtx, workDir)
		if err != nil {
			return nil, err
		}
	}
	return &Result{
		AssistantMessage: responseText,
		OutputSummary:    collector.OutputSummary(repoPersistence),
		AwaitingInput:    awaitingInput,
		WaitForApproval:  waitForApproval,
	}, nil
}

func (a *OpenCodeAdapter) buildEnv(execCtx *ExecutionContext, configContent string) ([]string, error) {
	env := append(os.Environ(), a.cfg.Env...)
	provider := openCodeProvider(execCtx.Agent)
	switch provider {
	case "", "anthropic":
		env = appendIfMissingEnv(env, "ANTHROPIC_API_KEY", a.cfg.AnthropicAPIKey)
		if strings.TrimSpace(a.cfg.AnthropicBaseURL) != "" {
			env = appendIfMissingEnv(env, "ANTHROPIC_BASE_URL", a.cfg.AnthropicBaseURL)
		}
	case "openai":
		env = appendIfMissingEnv(env, "OPENAI_API_KEY", a.cfg.OpenAIAPIKey)
		env = appendIfMissingEnv(env, "OPENAI_BASE_URL", a.cfg.OpenAIBaseURL)
	case "openrouter":
		env = appendIfMissingEnv(env, "OPENROUTER_API_KEY", a.cfg.OpenRouterAPIKey)
		if strings.TrimSpace(a.cfg.OpenRouterBaseURL) != "" {
			env = appendIfMissingEnv(env, "OPENROUTER_BASE_URL", a.cfg.OpenRouterBaseURL)
		}
	}
	env = upsertEnv(env, "NO_COLOR", "1")
	env = upsertEnv(env, "OPENCODE_CONFIG_CONTENT", configContent)
	if execCtx != nil && execCtx.Run != nil && strings.TrimSpace(execCtx.Run.ID) != "" {
		root := strings.TrimSpace(a.cfg.RuntimeRoot)
		if root == "" {
			root = filepath.Join(os.TempDir(), "agent-runtime-opencode")
		}
		runRoot := filepath.Join(root, sanitizeCodexPathComponent(execCtx.Run.AppID), sanitizeCodexPathComponent(execCtx.Run.ID))
		homeDir := filepath.Join(runRoot, "home")
		if err := os.MkdirAll(homeDir, 0o755); err != nil {
			return nil, fmt.Errorf("create opencode runtime home: %w", err)
		}
		env = upsertEnv(env, "HOME", homeDir)
		env = upsertEnv(env, "XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
		env = upsertEnv(env, "XDG_DATA_HOME", filepath.Join(homeDir, ".local", "share"))
		env = upsertEnv(env, "XDG_CACHE_HOME", filepath.Join(homeDir, ".cache"))
		env = upsertEnv(env, "OPENCODE_HOME", filepath.Join(homeDir, ".opencode"))
	}
	return env, nil
}

func (a *OpenCodeAdapter) resolveModelID(agent *agentcore.Agent) string {
	provider := openCodeProvider(agent)
	modelName := openCodeConfiguredModelName(provider, agentModelName(agent))
	if modelName == "" {
		modelName = defaultOpenCodeModelForProvider(provider)
	}
	if modelName == "" {
		return ""
	}
	return provider + "/" + modelName
}

func (a *OpenCodeAdapter) buildProviderConfig(agent *agentcore.Agent) map[string]any {
	provider := openCodeProvider(agent)
	modelName := openCodeConfiguredModelName(provider, agentModelName(agent))
	options := map[string]any{}
	switch provider {
	case "anthropic":
		if strings.TrimSpace(a.cfg.AnthropicBaseURL) != "" {
			options["baseURL"] = strings.TrimSpace(a.cfg.AnthropicBaseURL)
		}
	case "openrouter":
		if strings.TrimSpace(a.cfg.OpenRouterBaseURL) != "" {
			options["baseURL"] = strings.TrimSpace(a.cfg.OpenRouterBaseURL)
		}
	}
	entry := map[string]any{}
	if len(options) > 0 {
		entry["options"] = options
	}
	if modelName != "" {
		entry["models"] = map[string]any{modelName: map[string]any{}}
	}
	if len(entry) == 0 {
		return nil
	}
	return map[string]any{provider: entry}
}

func maskOpenCodeRepoSkillRoots(execCtx *ExecutionContext, workDir string) (*runtimeworkspace.RepoSkillMask, error) {
	runID := ""
	if execCtx != nil && execCtx.Run != nil {
		runID = execCtx.Run.ID
	}
	return runtimeworkspace.MaskRepoSkillRoots(workDir, runID)
}

type openCodeParsedJSONEvent struct {
	EventType    string
	PartType     string
	DisplayLine  string
	ResponseText string
	TokensUsed   int
	Usage        openCodeUsage
	EventError   string
	Properties   map[string]any
	Part         map[string]any
	DeltaField   string
	Delta        string
}

type openCodeUsage struct {
	InputTokens           int `json:"input_tokens,omitempty"`
	CachedInputTokens     int `json:"cached_input_tokens,omitempty"`
	OutputTokens          int `json:"output_tokens,omitempty"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens,omitempty"`
	TotalTokens           int `json:"total_tokens,omitempty"`
}

type openCodeToolSummary struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	Status     string `json:"status,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

type openCodeLiveTool struct {
	ID              string
	PartID          string
	Name            string
	ParentMessageID string
	Input           string
	ArgsText        string
	StartedAt       time.Time
	Started         bool
	Finished        bool
	ResultMessageID string
}

type openCodeStreamCollector struct {
	execCtx *ExecutionContext
	mu      sync.Mutex

	stdoutFull  strings.Builder
	stderrFull  strings.Builder
	stdoutChunk strings.Builder
	stderrChunk strings.Builder
	response    strings.Builder
	eventError  string
	tokensUsed  int
	usage       openCodeUsage

	assistantMessageID string
	assistantStarted   bool
	assistantCompleted bool
	reasoningMessageID string
	reasoningStarted   bool
	reasoningCompleted bool
	reasoningText      strings.Builder
	assistantPartText  map[string]string
	reasoningPartText  map[string]string
	toolPartToCallID   map[string]string
	liveTools          map[string]*openCodeLiveTool
	toolSummaries      []openCodeToolSummary
	legacyToolUses     int
	currentActivityID  string
}

func newOpenCodeStreamCollector(execCtx *ExecutionContext) *openCodeStreamCollector {
	return &openCodeStreamCollector{
		execCtx:           execCtx,
		assistantPartText: map[string]string{},
		reasoningPartText: map[string]string{},
		toolPartToCallID:  map[string]string{},
		liveTools:         map[string]*openCodeLiveTool{},
	}
}

func (c *openCodeStreamCollector) runID() string {
	if c == nil || c.execCtx == nil || c.execCtx.Run == nil {
		return ""
	}
	return strings.TrimSpace(c.execCtx.Run.ID)
}

func consumeOpenCodeTextStream(reader io.Reader, stream string, ctx context.Context, collector *openCodeStreamCollector, wg *sync.WaitGroup, errCh chan<- error) {
	defer wg.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), openCodeScannerBufferSize)
	for scanner.Scan() {
		collector.AppendLine(ctx, stream, stripOpenCodeANSI(scanner.Text()))
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
		errCh <- fmt.Errorf("read %s: %w", stream, err)
		return
	}
	errCh <- nil
}

func consumeOpenCodeJSONStream(reader io.Reader, ctx context.Context, collector *openCodeStreamCollector, wg *sync.WaitGroup, errCh chan<- error) {
	defer wg.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), openCodeScannerBufferSize)
	for scanner.Scan() {
		line := strings.TrimSpace(stripOpenCodeANSI(scanner.Text()))
		if line == "" {
			continue
		}
		parsed, err := parseOpenCodeJSONEvent(line)
		if err != nil {
			collector.AppendLine(ctx, "stdout", line)
			continue
		}
		if strings.TrimSpace(parsed.DisplayLine) != "" {
			collector.AppendLine(ctx, "stdout", parsed.DisplayLine)
		}
		if parsed.TokensUsed > 0 {
			collector.AddTokens(parsed.TokensUsed)
		}
		if parsed.Usage.InputTokens > 0 || parsed.Usage.OutputTokens > 0 || parsed.Usage.TotalTokens > 0 {
			collector.SetUsage(parsed.Usage)
		}
		if parsed.EventError != "" {
			collector.SetEventError(parsed.EventError)
		}
		collector.ApplyParsedJSONEvent(ctx, parsed)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
		errCh <- fmt.Errorf("read stdout: %w", err)
		return
	}
	collector.FinalizeExecutionEvents(ctx)
	errCh <- nil
}

func parseOpenCodeJSONEvent(line string) (openCodeParsedJSONEvent, error) {
	var event map[string]any
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return openCodeParsedJSONEvent{}, fmt.Errorf("unmarshal event line: %w", err)
	}
	eventType := lookupString(event, "type")
	properties, _ := event["properties"].(map[string]any)
	part := openCodeEventPart(event, properties)
	parsed := openCodeParsedJSONEvent{
		EventType:  eventType,
		PartType:   lookupString(part, "type"),
		Properties: properties,
		Part:       part,
	}
	switch eventType {
	case "text":
		parsed.ResponseText = lookupString(part, "text")
		parsed.DisplayLine = parsed.ResponseText
	case "tool_use":
		parsed.DisplayLine = renderOpenCodeToolUse(part)
	case "step_start":
		parsed.DisplayLine = renderOpenCodeStepStart(part)
	case "step_finish":
		parsed.TokensUsed = openCodeTokensFromPart(part)
		parsed.Usage = openCodeUsageFromPart(part)
		parsed.DisplayLine = renderOpenCodeStepFinish(part, parsed.TokensUsed)
	case "error":
		parsed.EventError = firstNonEmpty(lookupString(event, "error"), lookupString(event, "message"), lookupString(part, "error"), lookupString(part, "message"), renderOpenCodeRawPart(part))
		parsed.EventError = strings.TrimSpace(parsed.EventError)
		if parsed.EventError != "" {
			parsed.DisplayLine = "Error: " + parsed.EventError
		}
	case "message.part.updated":
		switch parsed.PartType {
		case "text":
			parsed.ResponseText = lookupString(part, "text")
		case "tool":
			parsed.DisplayLine = renderOpenCodeToolPart(part)
		case "step-start":
			parsed.DisplayLine = renderOpenCodeStepStart(part)
		case "step-finish":
			parsed.TokensUsed = openCodeTokensFromPart(part)
			parsed.Usage = openCodeUsageFromPart(part)
			parsed.DisplayLine = renderOpenCodeStepFinish(part, parsed.TokensUsed)
		}
	case "message.part.delta":
		parsed.DeltaField = lookupString(properties, "field")
		parsed.Delta = lookupString(properties, "delta")
		if strings.HasSuffix(parsed.DeltaField, "text") {
			parsed.ResponseText = parsed.Delta
		}
	default:
		parsed.DisplayLine = renderOpenCodeGenericEvent(eventType, part)
	}
	return parsed, nil
}

func (c *openCodeStreamCollector) AppendLine(ctx context.Context, stream, line string) {
	c.mu.Lock()
	full, chunk := c.buffersFor(stream)
	full.WriteString(line)
	full.WriteByte('\n')
	chunk.WriteString(line)
	chunk.WriteByte('\n')
	shouldFlush := chunk.Len() >= openCodeChunkFlushBytes
	c.mu.Unlock()
	if shouldFlush {
		c.FlushStream(ctx, stream, true)
	}
}

func (c *openCodeStreamCollector) FlushLoop(ctx context.Context, done <-chan struct{}) {
	ticker := time.NewTicker(openCodeChunkFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.FlushPending(ctx, true)
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *openCodeStreamCollector) FlushPending(ctx context.Context, notify bool) {
	c.FlushStream(ctx, "stdout", notify)
	c.FlushStream(ctx, "stderr", notify)
}

func (c *openCodeStreamCollector) FlushStream(ctx context.Context, stream string, notify bool) {
	c.mu.Lock()
	_, chunk := c.buffersFor(stream)
	content := chunk.String()
	chunk.Reset()
	c.mu.Unlock()
	if content == "" || c.execCtx == nil {
		return
	}
	writeOpenCodeArtifact(ctx, c.execCtx, "opencode_"+stream+"_chunk", "text", content, notify)
}

func (c *openCodeStreamCollector) Outputs() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdoutFull.String(), c.stderrFull.String()
}

func (c *openCodeStreamCollector) ResponseText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.response.String()
}

func (c *openCodeStreamCollector) EventError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.eventError
}

func (c *openCodeStreamCollector) AddTokens(tokens int) {
	if tokens <= 0 {
		return
	}
	c.mu.Lock()
	c.tokensUsed += tokens
	c.usage.TotalTokens = c.tokensUsed
	c.mu.Unlock()
}

func (c *openCodeStreamCollector) SetUsage(usage openCodeUsage) {
	c.mu.Lock()
	c.usage = usage
	if c.usage.TotalTokens == 0 {
		c.usage.TotalTokens = usage.InputTokens + usage.CachedInputTokens + usage.OutputTokens + usage.ReasoningOutputTokens
	}
	c.mu.Unlock()
}

func (c *openCodeStreamCollector) SetEventError(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	c.mu.Lock()
	if c.eventError == "" {
		c.eventError = message
	}
	c.mu.Unlock()
}

func (c *openCodeStreamCollector) ApplyParsedJSONEvent(ctx context.Context, parsed openCodeParsedJSONEvent) {
	switch parsed.EventType {
	case "text":
		messageID := c.ensureAssistantMessageID(firstNonEmpty(lookupString(parsed.Part, "messageID"), lookupString(parsed.Part, "id")))
		partID := firstNonEmpty(lookupString(parsed.Part, "id"), messageID)
		c.applyAssistantTextSnapshot(ctx, messageID, partID, parsed.ResponseText)
	case "message.part.updated", "message.part.delta":
		if parsed.EventType == "message.part.updated" {
			c.applyUpdatedPart(ctx, parsed)
		} else {
			c.applyPartDelta(ctx, parsed)
		}
	case "tool_use":
		c.applyLegacyToolUse(ctx, parsed.Part)
	case "step_start":
		c.emitActivitySnapshot(ctx, parsed.Part, parsed.DisplayLine)
	case "step_finish":
		c.emitActivityDelta(ctx, parsed.Part, parsed.DisplayLine)
		c.completeReasoningStream(ctx)
		c.completeAssistantStream(ctx)
	}
}

func (c *openCodeStreamCollector) FinalizeExecutionEvents(ctx context.Context) {
	for _, tool := range c.liveTools {
		if tool != nil && !tool.Finished {
			c.finishToolCall(ctx, tool, "", "", time.Since(tool.StartedAt).Milliseconds())
		}
	}
	c.completeReasoningStream(ctx)
	if strings.TrimSpace(c.assistantMessageID) != "" && !c.assistantCompleted {
		c.completeAssistantStream(ctx)
	}
}

func (c *openCodeStreamCollector) OutputSummary(repoPersistence map[string]any) json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	body := map[string]any{
		"runtime_kind":            agentcore.RuntimeOpenCode,
		"total_tokens":            c.usage.TotalTokens,
		"input_tokens":            c.usage.InputTokens,
		"cached_input_tokens":     c.usage.CachedInputTokens,
		"output_tokens":           c.usage.OutputTokens,
		"reasoning_output_tokens": c.usage.ReasoningOutputTokens,
		"tool_summaries":          c.toolSummaries,
	}
	if len(repoPersistence) > 0 {
		body["repository"] = repoPersistence
	}
	summary, _ := json.Marshal(body)
	return summary
}

func (c *openCodeStreamCollector) buffersFor(stream string) (*strings.Builder, *strings.Builder) {
	if stream == "stderr" {
		return &c.stderrFull, &c.stderrChunk
	}
	return &c.stdoutFull, &c.stdoutChunk
}

func (c *openCodeStreamCollector) applyUpdatedPart(ctx context.Context, parsed openCodeParsedJSONEvent) {
	switch parsed.PartType {
	case "text":
		messageID := c.ensureAssistantMessageID(lookupString(parsed.Part, "messageID"))
		partID := firstNonEmpty(lookupString(parsed.Part, "id"), messageID)
		c.applyAssistantTextSnapshot(ctx, messageID, partID, parsed.ResponseText)
		if openCodePartHasEnded(parsed.Part) {
			c.completeAssistantStream(ctx)
		}
	case "reasoning":
		messageID := c.ensureReasoningMessageID(firstNonEmpty(lookupString(parsed.Part, "id"), lookupString(parsed.Part, "messageID")))
		partID := firstNonEmpty(lookupString(parsed.Part, "id"), messageID)
		c.applyReasoningSnapshot(ctx, messageID, partID, lookupString(parsed.Part, "text"))
		if openCodePartHasEnded(parsed.Part) {
			c.completeReasoningStream(ctx)
		}
	case "tool":
		c.applyToolPart(ctx, parsed.Part)
	case "step-start":
		c.emitActivitySnapshot(ctx, parsed.Part, parsed.DisplayLine)
	case "step-finish":
		c.emitActivityDelta(ctx, parsed.Part, parsed.DisplayLine)
		c.completeReasoningStream(ctx)
		c.completeAssistantStream(ctx)
	}
}

func (c *openCodeStreamCollector) applyPartDelta(ctx context.Context, parsed openCodeParsedJSONEvent) {
	if parsed.Delta == "" {
		return
	}
	messageID := firstNonEmpty(lookupString(parsed.Properties, "messageID"), c.assistantMessageID)
	partID := lookupString(parsed.Properties, "partID")
	switch {
	case strings.HasSuffix(parsed.DeltaField, "text"):
		if _, ok := c.reasoningPartText[partID]; ok {
			reasoningID := c.ensureReasoningMessageID(firstNonEmpty(c.reasoningMessageID, partID, messageID))
			c.applyReasoningDelta(ctx, reasoningID, partID, parsed.Delta)
			return
		}
		assistantID := c.ensureAssistantMessageID(messageID)
		c.applyAssistantDelta(ctx, assistantID, partID, parsed.Delta)
	case strings.Contains(parsed.DeltaField, "state.input"), strings.Contains(parsed.DeltaField, "state.raw"), strings.HasSuffix(parsed.DeltaField, "input"):
		c.applyToolArgsDelta(ctx, messageID, partID, parsed.Delta)
	}
}

func (c *openCodeStreamCollector) ensureAssistantMessageID(preferred string) string {
	preferred = strings.TrimSpace(preferred)
	if preferred != "" && preferred != c.assistantMessageID {
		if c.assistantStarted && !c.assistantCompleted {
			c.completeAssistantStream(context.Background())
		}
		c.assistantMessageID = preferred
		c.assistantCompleted = false
	}
	if strings.TrimSpace(c.assistantMessageID) == "" {
		c.assistantMessageID = uuid.NewString()
	}
	return c.assistantMessageID
}

func (c *openCodeStreamCollector) ensureReasoningMessageID(preferred string) string {
	preferred = strings.TrimSpace(preferred)
	if preferred != "" && preferred != c.reasoningMessageID {
		if c.reasoningStarted && !c.reasoningCompleted {
			c.completeReasoningStream(context.Background())
		}
		c.reasoningMessageID = preferred
		c.reasoningCompleted = false
	}
	if strings.TrimSpace(c.reasoningMessageID) == "" {
		c.reasoningMessageID = uuid.NewString()
	}
	return c.reasoningMessageID
}

func (c *openCodeStreamCollector) applyAssistantTextSnapshot(ctx context.Context, messageID, partID, text string) {
	if partID == "" {
		partID = messageID
	}
	previous := c.assistantPartText[partID]
	c.assistantPartText[partID] = text
	c.applyAssistantDelta(ctx, messageID, partID, openCodeDeltaFromSnapshot(previous, text))
}

func (c *openCodeStreamCollector) applyAssistantDelta(ctx context.Context, messageID, partID, delta string) {
	if delta == "" {
		return
	}
	messageID = c.ensureAssistantMessageID(messageID)
	c.mu.Lock()
	if !c.assistantStarted {
		c.assistantStarted = true
		c.emitLocked(ctx, "assistant_message_started", map[string]any{"message_id": messageID})
	}
	c.response.WriteString(delta)
	c.mu.Unlock()
	c.emit(ctx, "assistant_message_delta", map[string]any{
		"message_id": messageID,
		"text":       delta,
		"content":    delta,
	})
}

func (c *openCodeStreamCollector) completeAssistantStream(ctx context.Context) {
	c.mu.Lock()
	if !c.assistantStarted || c.assistantCompleted {
		c.mu.Unlock()
		return
	}
	c.assistantCompleted = true
	messageID := c.assistantMessageID
	text := strings.TrimSpace(c.response.String())
	c.mu.Unlock()
	c.emit(ctx, "assistant_message_completed", map[string]any{
		"message_id": messageID,
		"text":       text,
		"content":    text,
	})
}

func (c *openCodeStreamCollector) applyReasoningSnapshot(ctx context.Context, messageID, partID, text string) {
	if partID == "" {
		partID = messageID
	}
	previous := c.reasoningPartText[partID]
	c.reasoningPartText[partID] = text
	c.applyReasoningDelta(ctx, messageID, partID, openCodeDeltaFromSnapshot(previous, text))
}

func (c *openCodeStreamCollector) applyReasoningDelta(ctx context.Context, messageID, partID, delta string) {
	if delta == "" {
		return
	}
	messageID = c.ensureReasoningMessageID(messageID)
	if !c.reasoningStarted {
		c.reasoningStarted = true
		c.emit(ctx, "reasoning_message_started", map[string]any{"message_id": messageID})
	}
	c.reasoningText.WriteString(delta)
	c.emit(ctx, "reasoning_message_delta", map[string]any{
		"message_id": messageID,
		"text":       delta,
		"content":    delta,
	})
	if partID != "" {
		c.reasoningPartText[partID] = c.reasoningPartText[partID]
	}
}

func (c *openCodeStreamCollector) completeReasoningStream(ctx context.Context) {
	if strings.TrimSpace(c.reasoningMessageID) == "" || c.reasoningCompleted {
		return
	}
	c.reasoningCompleted = true
	content := c.reasoningText.String()
	c.emit(ctx, "reasoning_message_completed", map[string]any{
		"message_id": c.reasoningMessageID,
		"text":       content,
		"content":    content,
	})
}

func (c *openCodeStreamCollector) applyLegacyToolUse(ctx context.Context, part map[string]any) {
	toolID := firstNonEmpty(lookupString(part, "callID"), lookupString(part, "id"))
	if toolID == "" {
		c.legacyToolUses++
		toolID = fmt.Sprintf("%s:legacy-tool:%d", shortRunIDForOpenCode(c.runID()), c.legacyToolUses)
	}
	parentMessageID := c.ensureAssistantMessageID(lookupString(part, "messageID"))
	toolName := openCodeToolName(part, nil)
	if toolName == "" {
		toolName = "tool"
	}
	toolInput := openCodeToolInputForState(part, nil)
	argsText := firstNonEmpty(openCodeNormalizedToolInput(toolInput), renderOpenCodeToolUse(part))
	tool := &openCodeLiveTool{
		ID:              toolID,
		PartID:          lookupString(part, "id"),
		Name:            toolName,
		ParentMessageID: parentMessageID,
		Input:           openCodeNormalizedToolInput(toolInput),
		ArgsText:        argsText,
		StartedAt:       time.Now(),
	}
	c.startToolCall(ctx, tool)
	c.finishToolCall(ctx, tool, "", "", 0)
}

func (c *openCodeStreamCollector) applyToolPart(ctx context.Context, part map[string]any) {
	state, _ := part["state"].(map[string]any)
	toolID := firstNonEmpty(lookupString(part, "callID"), lookupString(part, "id"), uuid.NewString())
	partID := lookupString(part, "id")
	if partID == "" {
		partID = toolID
	}
	parentMessageID := c.ensureAssistantMessageID(lookupString(part, "messageID"))
	status := strings.TrimSpace(lookupString(state, "status"))
	tool := c.liveTools[toolID]
	if tool == nil {
		tool = &openCodeLiveTool{
			ID:              toolID,
			PartID:          partID,
			StartedAt:       time.Now(),
			ResultMessageID: uuid.NewString(),
		}
		c.liveTools[toolID] = tool
	}
	tool.PartID = partID
	tool.ParentMessageID = parentMessageID
	tool.Name = firstNonEmpty(openCodeToolName(part, state), tool.Name, "tool")
	tool.Input = openCodeNormalizedToolInput(openCodeToolInputForState(part, state))
	tool.ArgsText = firstNonEmpty(tool.Input, openCodeToolArgsText(part, state), tool.ArgsText)
	c.toolPartToCallID[partID] = toolID
	if !tool.Started {
		c.startToolCall(ctx, tool)
	}
	switch status {
	case "pending", "running":
		if tool.ArgsText != "" {
			c.emitToolArgsDelta(ctx, tool, tool.ArgsText, true)
		}
	case "completed":
		if tool.ArgsText != "" {
			c.emitToolArgsDelta(ctx, tool, tool.ArgsText, true)
		}
		c.finishToolCall(ctx, tool, lookupString(state, "output"), "", openCodeToolDurationMs(state))
	case "error", "failed":
		if tool.ArgsText != "" {
			c.emitToolArgsDelta(ctx, tool, tool.ArgsText, true)
		}
		errText := firstNonEmpty(lookupString(state, "error"), lookupString(state, "output"), lookupString(part, "error"))
		c.finishToolCall(ctx, tool, errText, errText, openCodeToolDurationMs(state))
	}
}

func (c *openCodeStreamCollector) applyToolArgsDelta(ctx context.Context, messageID, partID, delta string) {
	if delta == "" {
		return
	}
	toolID := firstNonEmpty(c.toolPartToCallID[partID], partID)
	if toolID == "" {
		return
	}
	tool := c.liveTools[toolID]
	if tool == nil {
		tool = &openCodeLiveTool{
			ID:              toolID,
			PartID:          partID,
			Name:            "tool",
			ParentMessageID: c.ensureAssistantMessageID(messageID),
			StartedAt:       time.Now(),
			ResultMessageID: uuid.NewString(),
		}
		c.liveTools[toolID] = tool
	}
	if !tool.Started {
		c.startToolCall(ctx, tool)
	}
	c.emitToolArgsDelta(ctx, tool, delta, false)
}

func (c *openCodeStreamCollector) startToolCall(ctx context.Context, tool *openCodeLiveTool) {
	if tool == nil || tool.Started {
		return
	}
	tool.Started = true
	if strings.TrimSpace(tool.ResultMessageID) == "" {
		tool.ResultMessageID = uuid.NewString()
	}
	if existing := c.liveTools[tool.ID]; existing == nil {
		c.liveTools[tool.ID] = tool
	}
	c.emit(ctx, "tool_call_started", map[string]any{
		"tool_call_id":      tool.ID,
		"tool_name":         tool.Name,
		"tool_input":        tool.Input,
		"parent_message_id": tool.ParentMessageID,
		"args_text":         tool.ArgsText,
	})
}

func (c *openCodeStreamCollector) emitToolArgsDelta(ctx context.Context, tool *openCodeLiveTool, value string, replace bool) {
	if tool == nil || strings.TrimSpace(value) == "" {
		return
	}
	delta := value
	if replace {
		if tool.ArgsText == value {
			return
		}
		delta = openCodeDeltaFromSnapshot(tool.ArgsText, value)
		tool.ArgsText = value
	} else {
		tool.ArgsText += value
	}
	if delta == "" {
		return
	}
	c.emit(ctx, "tool_call_args_delta", map[string]any{
		"tool_call_id":      tool.ID,
		"tool_name":         tool.Name,
		"parent_message_id": tool.ParentMessageID,
		"args_delta":        delta,
		"args_text":         tool.ArgsText,
	})
}

func (c *openCodeStreamCollector) finishToolCall(ctx context.Context, tool *openCodeLiveTool, output string, errorText string, durationMs int64) {
	if tool == nil || tool.Finished {
		return
	}
	tool.Finished = true
	output = strings.TrimSpace(output)
	errorText = strings.TrimSpace(errorText)
	if durationMs == 0 && !tool.StartedAt.IsZero() {
		durationMs = time.Since(tool.StartedAt).Milliseconds()
	}
	if output != "" || errorText != "" {
		c.emit(ctx, "tool_call_result", map[string]any{
			"tool_call_id":      tool.ID,
			"tool_name":         tool.Name,
			"parent_message_id": tool.ParentMessageID,
			"result_message_id": tool.ResultMessageID,
			"content":           firstNonEmpty(output, errorText),
			"output_summary":    firstNonEmpty(output, errorText),
			"error":             errorText,
		})
	}
	c.emit(ctx, "tool_call_finished", map[string]any{
		"tool_call_id":      tool.ID,
		"tool_name":         tool.Name,
		"parent_message_id": tool.ParentMessageID,
		"result_message_id": tool.ResultMessageID,
		"output_summary":    firstNonEmpty(output, errorText),
		"content":           firstNonEmpty(output, errorText),
		"duration_ms":       durationMs,
		"error":             errorText,
	})
	delete(c.liveTools, tool.ID)
	c.toolSummaries = append(c.toolSummaries, openCodeToolSummary{
		ID:         tool.ID,
		Name:       tool.Name,
		Status:     "completed",
		Summary:    firstNonEmpty(output, errorText),
		Error:      errorText,
		DurationMs: durationMs,
	})
	if tool.PartID != "" {
		delete(c.toolPartToCallID, tool.PartID)
	}
	c.recordToolCall(ctx, tool, output, errorText)
}

func (c *openCodeStreamCollector) emitActivitySnapshot(ctx context.Context, part map[string]any, content string) {
	activityID := firstNonEmpty(lookupString(part, "stepID"), lookupString(part, "id"))
	if activityID == "" {
		activityID = uuid.NewString()
	}
	c.currentActivityID = activityID
	c.emit(ctx, "activity_snapshot", map[string]any{
		"activity_id":   activityID,
		"activity_type": "step",
		"content":       content,
		"text":          content,
	})
}

func (c *openCodeStreamCollector) emitActivityDelta(ctx context.Context, part map[string]any, content string) {
	activityID := firstNonEmpty(lookupString(part, "stepID"), lookupString(part, "id"), c.currentActivityID)
	if activityID == "" {
		activityID = uuid.NewString()
	}
	c.currentActivityID = activityID
	c.emit(ctx, "activity_delta", map[string]any{
		"activity_id":   activityID,
		"activity_type": "step",
		"content":       content,
		"text":          content,
	})
}

func (c *openCodeStreamCollector) emit(ctx context.Context, eventType string, data map[string]any) {
	c.mu.Lock()
	c.emitLocked(ctx, eventType, data)
	c.mu.Unlock()
}

func (c *openCodeStreamCollector) emitLocked(ctx context.Context, eventType string, data map[string]any) {
	if c == nil || c.execCtx == nil || c.execCtx.EventSink == nil || c.execCtx.Run == nil {
		return
	}
	c.execCtx.EventSink.Emit(ctx, Event{
		AppID: c.execCtx.Run.AppID,
		RunID: c.execCtx.Run.ID,
		Type:  eventType,
		Data:  data,
	})
}

func (c *openCodeStreamCollector) recordToolCall(ctx context.Context, tool *openCodeLiveTool, output string, errorText string) {
	if c == nil || c.execCtx == nil || c.execCtx.Store == nil || c.execCtx.Run == nil || tool == nil {
		return
	}
	inputBody := map[string]any{
		"runtime_kind":      agentcore.RuntimeOpenCode,
		"tool_name":         tool.Name,
		"opencode_tool_id":  tool.ID,
		"opencode_tool_raw": tool.Input,
	}
	if json.Valid([]byte(tool.Input)) {
		inputBody["arguments"] = json.RawMessage(tool.Input)
	}
	input, _ := json.Marshal(inputBody)
	body, _ := json.Marshal(map[string]any{
		"runtime_kind": agentcore.RuntimeOpenCode,
		"summary":      output,
		"error":        errorText,
	})
	_ = c.execCtx.Store.AppendToolCall(ctx, &agentcore.ToolCall{
		AppID:    c.execCtx.Run.AppID,
		RunID:    c.execCtx.Run.ID,
		ToolName: tool.Name,
		Input:    input,
		Output:   body,
		Error:    strings.TrimSpace(errorText),
		Mutating: openCodeToolMutating(tool.Name),
	})
}

func writeOpenCodeArtifact(ctx context.Context, execCtx *ExecutionContext, artifactType string, format string, content string, notify bool) {
	if execCtx == nil || execCtx.ArtifactWriter == nil || content == "" {
		return
	}
	meta, _ := json.Marshal(map[string]any{
		"notify":       notify,
		"runtime_kind": agentcore.RuntimeOpenCode,
	})
	_ = execCtx.ArtifactWriter.WriteArtifact(ctx, agentcore.AgentRunArtifact{
		ArtifactType:  strings.TrimSpace(artifactType),
		Format:        strings.TrimSpace(format),
		StorageMode:   "inline",
		InlineContent: content,
		Metadata:      meta,
	})
}

func buildOpenCodeSystemPrompt(execCtx *ExecutionContext) string {
	parts := []string{}
	if execCtx != nil && execCtx.Agent != nil {
		parts = append(parts, strings.TrimSpace(execCtx.Agent.SystemPrompt))
	}
	if execCtx != nil && execCtx.TargetContext != nil && strings.TrimSpace(execCtx.TargetContext.Summary) != "" {
		parts = append(parts, "Target context:\n"+strings.TrimSpace(execCtx.TargetContext.Summary))
	}
	if execCtx != nil && strings.TrimSpace(execCtx.SkillInstructions) != "" {
		parts = append(parts, "Active skills:\n"+strings.TrimSpace(execCtx.SkillInstructions))
	}
	if runtimeInstructions := buildOpenCodeRuntimeInstructions(execCtx); runtimeInstructions != "" {
		parts = append(parts, "OpenCode Runtime Instructions:\n"+runtimeInstructions)
	}
	return strings.TrimSpace(joinNonEmpty("\n\n", parts...))
}

func buildOpenCodeRuntimeInstructions(execCtx *ExecutionContext) string {
	parts := []string{
		"You are running inside the OpenCode CLI runtime.",
		"Use the local shell and file-editing capabilities available in this workspace directly.",
		"Do not push the branch or open a pull request from this runtime. If you complete implementation, finish with a local git commit only; the platform will handle remote delivery.",
	}
	if execCtx != nil && strings.TrimSpace(execCtx.Run.InvocationMode) == agentcore.InvocationInteractive {
		parts = append(parts,
			"This is an interactive run. Continue from the latest human reply instead of restarting from scratch.",
			"Make repository changes when they materially advance the task, but they are not required on every turn.",
		)
	} else {
		parts = append(parts,
			"This is an autonomous run. Make durable progress on the assigned task before stopping.",
			"Inspect the relevant repository context before making changes, and validate any code changes you do make with practical checks when possible.",
		)
	}
	if execCtx != nil {
		if label := strings.TrimSpace(reviewCheckpointBlockLabel(execCtx)); label != "" {
			parts = append(parts,
				"When a review checkpoint is required, end with a fenced JSON block labelled `"+label+"`.",
				"The JSON payload should include `intent:\"review_checkpoint\"`, `summary`, and any structured findings or checkpoint data requested by the active skill.",
			)
		}
		if requestUserInputUsesRuntimeBridge(execCtx) {
			parts = append(parts,
				"When you need human input, end with a fenced JSON block labelled `helpin-input` containing `intent:\"request_user_input\"`, `title`, `summary`, and optional `options`.",
			)
		}
		if strings.TrimSpace(execCtx.StagedSkillRoot) != "" {
			parts = append(parts, "Runtime skills are staged at:\n"+strings.TrimSpace(execCtx.StagedSkillRoot))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func buildOpenCodeUserPrompt(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.Run == nil {
		return ""
	}
	parts := []string{strings.TrimSpace(execCtx.Run.Input.Instructions)}
	if parts[0] == "" && execCtx.TargetContext != nil {
		parts[0] = strings.TrimSpace(execCtx.TargetContext.Summary)
	}
	if execCtx.Run.Target.Type == "repository" && execCtx.Run.InvocationMode != agentcore.InvocationInteractive {
		parts = append(parts, "This is an implementation run, not an analysis-only pass. Make code changes when the task requires them, run relevant validation when practical, and finish with a concise summary of the concrete files changed.")
	}
	return strings.TrimSpace(joinNonEmpty("\n\n", parts...))
}

func buildOpenCodeConfigContent(execCtx *ExecutionContext, modelID, systemPrompt string, providerConfig map[string]any) (string, error) {
	agentName := openCodeAgentName(execCtx)
	agentConfig := map[string]any{
		"description": openCodeAgentDescription(execCtx),
		"mode":        "primary",
		"prompt":      systemPrompt,
	}
	if modelID != "" {
		agentConfig["model"] = modelID
	}
	if permissions := buildOpenCodePermissions(execCtx); len(permissions) > 0 {
		agentConfig["permission"] = permissions
	}
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"agent": map[string]any{
			agentName: agentConfig,
		},
	}
	if modelID != "" {
		config["model"] = modelID
		config["small_model"] = modelID
	}
	if len(providerConfig) > 0 {
		config["provider"] = providerConfig
	}
	if execCtx != nil && strings.TrimSpace(execCtx.StagedSkillRoot) != "" {
		config["skills"] = map[string]any{"paths": []string{strings.TrimSpace(execCtx.StagedSkillRoot)}}
	}
	payload, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func buildOpenCodePromptArtifact(systemPrompt, userPrompt string) string {
	sections := []string{}
	if strings.TrimSpace(systemPrompt) != "" {
		sections = append(sections, "Developer prompt:\n"+strings.TrimSpace(systemPrompt))
	}
	if strings.TrimSpace(userPrompt) != "" {
		sections = append(sections, "User prompt:\n"+strings.TrimSpace(userPrompt))
	}
	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func buildOpenCodePermissions(execCtx *ExecutionContext) map[string]any {
	if execCtx == nil {
		return nil
	}
	permissions := map[string]any{}
	if openCodeMayEdit(execCtx) {
		permissions["edit"] = "allow"
	} else {
		permissions["edit"] = "deny"
	}
	if openCodeMayRunShell(execCtx) {
		permissions["bash"] = map[string]string{
			"*":           "allow",
			"git push":    "deny",
			"git push *":  "deny",
			"gh pr *":     "deny",
			"hub pull *":  "deny",
			"hub pr *":    "deny",
			"open *":      "deny",
			"xdg-open *":  "deny",
			"osascript *": "deny",
		}
	} else {
		permissions["bash"] = "deny"
	}
	return permissions
}

func openCodeMayEdit(execCtx *ExecutionContext) bool {
	if execCtx == nil {
		return false
	}
	if execCtx.WorkspaceLease != nil && strings.TrimSpace(execCtx.WorkspaceLease.RootPath) != "" {
		return true
	}
	if execCtx.Run != nil && strings.TrimSpace(execCtx.Run.Target.Type) == "repository" {
		return true
	}
	for name := range execCtx.AllowedTools {
		switch name {
		case "write_file", "edit_file", "apply_patch", "commit_and_push":
			return true
		}
	}
	return false
}

func openCodeMayRunShell(execCtx *ExecutionContext) bool {
	if execCtx == nil {
		return false
	}
	if execCtx.WorkspaceLease != nil && strings.TrimSpace(execCtx.WorkspaceLease.RootPath) != "" {
		return true
	}
	for name := range execCtx.AllowedTools {
		switch name {
		case "run_command", "shell", "git", "read_file", "write_file", "edit_file", "apply_patch":
			return true
		}
	}
	return false
}

func openCodeAgentName(execCtx *ExecutionContext) string {
	if execCtx != nil && execCtx.Run != nil {
		switch strings.TrimSpace(execCtx.Run.Target.Type) {
		case "repository":
			if openCodeMayEdit(execCtx) {
				return "agent-runtime-engineer"
			}
			return "agent-runtime-reviewer"
		case "support_conversation":
			return "agent-runtime-support"
		}
	}
	return "agent-runtime"
}

func openCodeAgentDescription(execCtx *ExecutionContext) string {
	if execCtx != nil && execCtx.Run != nil {
		switch strings.TrimSpace(execCtx.Run.Target.Type) {
		case "repository":
			if openCodeMayEdit(execCtx) {
				return "Agent Runtime build agent for repository implementation runs."
			}
			return "Agent Runtime review agent for repository validation and quality checks."
		case "support_conversation":
			return "Agent Runtime support agent for structured support triage."
		}
	}
	return "Agent Runtime OpenCode agent."
}

func openCodeProvider(agent *agentcore.Agent) string {
	provider := "anthropic"
	if agent != nil && strings.TrimSpace(agent.Provider) != "" {
		provider = strings.TrimSpace(agent.Provider)
	}
	switch provider {
	case "", "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	case "openrouter", "openrouter_responses":
		return "openrouter"
	default:
		return provider
	}
}

func agentModelName(agent *agentcore.Agent) string {
	if agent == nil {
		return ""
	}
	return strings.TrimSpace(agent.Model)
}

func defaultOpenCodeModelForProvider(provider string) string {
	switch openCodeProvider(&agentcore.Agent{Provider: provider}) {
	case "openai":
		return "gpt-5-mini"
	case "openrouter":
		return "openai/gpt-5-mini"
	default:
		return "claude-sonnet-4-6"
	}
}

func openCodeConfiguredModelName(provider, modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return ""
	}
	provider = openCodeProvider(&agentcore.Agent{Provider: provider})
	prefix := provider + "/"
	if strings.HasPrefix(modelName, prefix) {
		return strings.TrimPrefix(modelName, prefix)
	}
	return modelName
}

func reviewCheckpointBlockLabel(execCtx *ExecutionContext) string {
	if execCtx == nil {
		return ""
	}
	for _, contract := range execCtx.SkillPolicy.InteractionContracts {
		if strings.TrimSpace(contract.Kind) != "review_checkpoint" {
			continue
		}
		if transport, ok := contract.Transports[agentcore.RuntimeOpenCode]; ok && strings.TrimSpace(transport.BlockLabel) != "" {
			return strings.TrimSpace(transport.BlockLabel)
		}
	}
	return ""
}

func requestUserInputUsesRuntimeBridge(execCtx *ExecutionContext) bool {
	if execCtx == nil {
		return false
	}
	for _, contract := range execCtx.SkillPolicy.InteractionContracts {
		if strings.TrimSpace(contract.Kind) != "request_user_input" {
			continue
		}
		transport, ok := contract.Transports[agentcore.RuntimeOpenCode]
		return ok && strings.TrimSpace(transport.Type) == "runtime_bridge"
	}
	return true
}

func processOpenCodeRuntimeHandoff(ctx context.Context, execCtx *ExecutionContext, responseText string) (bool, bool, error) {
	block, ok := latestOpenCodeHandoffBlock(responseText)
	if !ok {
		return false, false, nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(block), &payload); err != nil {
		return false, false, fmt.Errorf("parse opencode runtime handoff: %w", err)
	}
	intent := strings.TrimSpace(lookupString(payload, "intent"))
	if intent == "" {
		intent = strings.TrimSpace(lookupString(payload, "kind"))
	}
	title := strings.TrimSpace(lookupString(payload, "title"))
	summary := strings.TrimSpace(firstNonEmpty(lookupString(payload, "summary"), lookupString(payload, "question"), lookupString(payload, "message")))
	raw := json.RawMessage(block)
	switch intent {
	case "request_user_input":
		interactionID := uuid.NewString()
		if err := nativePersistInteraction(ctx, execCtx, agentcore.AgentRunInteraction{
			ID:              interactionID,
			InteractionKind: "request_user_input",
			Status:          "pending",
			Title:           title,
			Summary:         summary,
			RequestPayload: nativeInteractionRequestPayload("opencode_request_user_input_v1", map[string]any{
				"title":   title,
				"summary": summary,
				"payload": json.RawMessage(raw),
			}, raw),
		}); err != nil {
			return false, false, err
		}
		nativeWriteInteractionArtifact(ctx, execCtx, "human_input_request", interactionID, raw)
		return true, false, nil
	case "review_checkpoint", "approval_request", "request_approval":
		kind := "approval_request"
		schema := "opencode_approval_request_v1"
		if intent == "review_checkpoint" {
			kind = "review_checkpoint"
			schema = "opencode_review_checkpoint_v1"
		}
		interactionID := uuid.NewString()
		if err := nativePersistInteraction(ctx, execCtx, agentcore.AgentRunInteraction{
			ID:              interactionID,
			InteractionKind: kind,
			Status:          "pending",
			Title:           title,
			Summary:         summary,
			RequestPayload: nativeInteractionRequestPayload(schema, map[string]any{
				"title":   title,
				"summary": summary,
				"payload": json.RawMessage(raw),
			}, raw),
		}); err != nil {
			return false, false, err
		}
		nativeWriteInteractionArtifact(ctx, execCtx, "human_approval_request", interactionID, raw)
		return false, true, nil
	default:
		return false, false, nil
	}
}

func latestOpenCodeHandoffBlock(responseText string) (string, bool) {
	blocks := fencedCodeBlocks(responseText)
	for idx := len(blocks) - 1; idx >= 0; idx-- {
		block := strings.TrimSpace(blocks[idx])
		if block == "" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(block), &payload) != nil {
			continue
		}
		intent := strings.TrimSpace(firstNonEmpty(lookupString(payload, "intent"), lookupString(payload, "kind")))
		switch intent {
		case "request_user_input", "review_checkpoint", "approval_request", "request_approval":
			return block, true
		}
	}
	return "", false
}

func fencedCodeBlocks(text string) []string {
	lines := strings.Split(text, "\n")
	blocks := []string{}
	var current []string
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inBlock {
				blocks = append(blocks, strings.Join(current, "\n"))
				current = nil
				inBlock = false
				continue
			}
			inBlock = true
			current = nil
			continue
		}
		if inBlock {
			current = append(current, line)
		}
	}
	return blocks
}

func persistOpenCodeRepositoryChanges(ctx context.Context, execCtx *ExecutionContext, workDir string) (map[string]any, error) {
	if !shouldPersistOpenCodeRepository(execCtx, workDir) {
		return nil, nil
	}
	changed, err := openCodeRunProducedRepoChanges(ctx, execCtx, workDir)
	if err != nil {
		return nil, err
	}
	if !changed && openCodeRequiresRepositoryChanges(execCtx) {
		changed, err = waitForOpenCodeRepoChanges(ctx, execCtx, workDir, openCodeRepoChangeWaitTimeout, openCodeRepoChangePollEvery)
		if err != nil {
			return nil, err
		}
	}
	if !changed {
		statusSummary, diffStatSummary := captureOpenCodeNoChangeDiagnostics(ctx, execCtx, workDir)
		if openCodeRequiresRepositoryChanges(execCtx) {
			return nil, fmt.Errorf(
				"opencode completed without modifying the repository within %s; git_status=%s; git_diff=%s",
				openCodeRepoChangeWaitTimeout,
				truncateOpenCodeSingleLine(statusSummary, 160),
				truncateOpenCodeSingleLine(diffStatSummary, 160),
			)
		}
		return nil, nil
	}
	if out, err := runOpenCodeGit(ctx, workDir, "add", "-A"); err != nil {
		return nil, fmt.Errorf("stage repository changes: %s", strings.TrimSpace(firstNonEmpty(out, err.Error())))
	}
	diff, err := runOpenCodeGit(ctx, workDir, "diff", "--cached")
	if err != nil {
		return nil, fmt.Errorf("inspect staged diff: %s", strings.TrimSpace(firstNonEmpty(diff, err.Error())))
	}
	filesOutput, err := runOpenCodeGit(ctx, workDir, "diff", "--cached", "--name-only")
	if err != nil {
		return nil, fmt.Errorf("inspect staged files: %s", strings.TrimSpace(firstNonEmpty(filesOutput, err.Error())))
	}
	changedFiles := strings.Fields(filesOutput)
	if strings.TrimSpace(diff) == "" || len(changedFiles) == 0 {
		committedChange, err := detectOpenCodeCommittedChange(ctx, execCtx, workDir)
		if err != nil {
			return nil, err
		}
		if committedChange != nil {
			return persistOpenCodeExistingCommit(ctx, execCtx, committedChange), nil
		}
		if openCodeRequiresRepositoryChanges(execCtx) {
			return nil, fmt.Errorf("opencode completed without producing a staged repository diff")
		}
		return nil, nil
	}
	writeOpenCodeArtifact(ctx, execCtx, "diff", "patch", NormalizeCodexUnifiedDiff(workDir, diff), false)
	writeOpenCodeArtifact(ctx, execCtx, "file_bundle", "json", openCodeJSONString(changedFiles), false)
	branch, err := resolveOpenCodeWorkingBranch(ctx, execCtx, workDir)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"changed":       true,
		"branch":        branch,
		"changed_files": changedFiles,
	}
	if openCodeWorkspaceFinalizerOwnsCommit(execCtx) {
		result["finalize_policy"] = "workspace_local_commit"
		writeOpenCodeArtifact(ctx, execCtx, "git_persistence_result", "json", openCodeJSONString(result), false)
		return result, nil
	}
	commitMessage := openCodeCommitMessage(execCtx)
	if out, err := runOpenCodeGit(ctx, workDir, "commit", "-m", commitMessage); err != nil {
		return nil, fmt.Errorf("commit repository changes: %s", strings.TrimSpace(firstNonEmpty(out, err.Error())))
	}
	shaOutput, err := runOpenCodeGit(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve commit SHA: %s", strings.TrimSpace(firstNonEmpty(shaOutput, err.Error())))
	}
	result["commit_sha"] = strings.TrimSpace(shaOutput)
	result["commit_message"] = commitMessage
	writeOpenCodeArtifact(ctx, execCtx, "git_persistence_result", "json", openCodeJSONString(result), false)
	return result, nil
}

type openCodeCommittedChange struct {
	Diff          string
	ChangedFiles  []string
	CommitSHA     string
	CommitMessage string
	Branch        string
}

func shouldPersistOpenCodeRepository(execCtx *ExecutionContext, workDir string) bool {
	if execCtx == nil || execCtx.Run == nil || execCtx.WorkspaceLease == nil {
		return false
	}
	if strings.TrimSpace(execCtx.Run.Target.Type) != "repository" {
		return false
	}
	if strings.TrimSpace(workDir) == "" || strings.TrimSpace(workDir) == "." {
		return false
	}
	if inside, err := openCodeGitInsideWorkTree(execCtx.Context, workDir); err != nil || !inside {
		return false
	}
	return true
}

func openCodeRunProducedRepoChanges(ctx context.Context, execCtx *ExecutionContext, workDir string) (bool, error) {
	statusOutput, err := runOpenCodeGit(ctx, workDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("inspect repository status: %s", strings.TrimSpace(firstNonEmpty(statusOutput, err.Error())))
	}
	if strings.TrimSpace(statusOutput) != "" {
		return true, nil
	}
	_, filesOutput, err := openCodeRepoDiffAgainstBase(ctx, execCtx, workDir)
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(filesOutput) != "", nil
}

func waitForOpenCodeRepoChanges(ctx context.Context, execCtx *ExecutionContext, workDir string, timeout, interval time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = openCodeRepoChangeWaitTimeout
	}
	if interval <= 0 {
		interval = openCodeRepoChangePollEvery
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastErr error
	for {
		changed, err := openCodeRunProducedRepoChanges(checkCtx, execCtx, workDir)
		if err == nil {
			lastErr = nil
			if changed {
				return true, nil
			}
		} else {
			lastErr = err
		}
		if checkCtx.Err() != nil {
			break
		}
		select {
		case <-checkCtx.Done():
		case <-ticker.C:
		}
	}
	if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		return false, nil
	}
	if lastErr != nil {
		return false, lastErr
	}
	return false, checkCtx.Err()
}

func captureOpenCodeNoChangeDiagnostics(ctx context.Context, execCtx *ExecutionContext, workDir string) (string, string) {
	statusSummary := ""
	if status, err := runOpenCodeGit(ctx, workDir, "status", "--short", "--branch"); err == nil && strings.TrimSpace(status) != "" {
		statusSummary = strings.TrimSpace(status)
		writeOpenCodeArtifact(ctx, execCtx, "git_status", "text", status, false)
	} else if err == nil {
		statusSummary = "clean working tree"
	}
	diffStatSummary := ""
	if diffStat, err := runOpenCodeGit(ctx, workDir, "diff", "--stat"); err == nil && strings.TrimSpace(diffStat) != "" {
		diffStatSummary = strings.TrimSpace(diffStat)
		writeOpenCodeArtifact(ctx, execCtx, "git_diff_stat", "text", diffStat, false)
	} else if err == nil {
		diffStatSummary = "no diff"
	}
	return statusSummary, diffStatSummary
}

func detectOpenCodeCommittedChange(ctx context.Context, execCtx *ExecutionContext, workDir string) (*openCodeCommittedChange, error) {
	statusOutput, err := runOpenCodeGit(ctx, workDir, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("inspect repository status: %s", strings.TrimSpace(firstNonEmpty(statusOutput, err.Error())))
	}
	if strings.TrimSpace(statusOutput) != "" {
		return nil, nil
	}
	diffOutput, filesOutput, err := openCodeRepoDiffAgainstBase(ctx, execCtx, workDir)
	if err != nil {
		return nil, err
	}
	changedFiles := strings.Fields(filesOutput)
	if strings.TrimSpace(diffOutput) == "" || len(changedFiles) == 0 {
		return nil, nil
	}
	shaOutput, err := runOpenCodeGit(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve existing commit SHA: %s", strings.TrimSpace(firstNonEmpty(shaOutput, err.Error())))
	}
	messageOutput, err := runOpenCodeGit(ctx, workDir, "log", "-1", "--pretty=%s")
	if err != nil {
		return nil, fmt.Errorf("resolve existing commit message: %s", strings.TrimSpace(firstNonEmpty(messageOutput, err.Error())))
	}
	branch, _ := resolveOpenCodeWorkingBranch(ctx, execCtx, workDir)
	return &openCodeCommittedChange{
		Diff:          diffOutput,
		ChangedFiles:  changedFiles,
		CommitSHA:     strings.TrimSpace(shaOutput),
		CommitMessage: strings.TrimSpace(messageOutput),
		Branch:        branch,
	}, nil
}

func persistOpenCodeExistingCommit(ctx context.Context, execCtx *ExecutionContext, change *openCodeCommittedChange) map[string]any {
	if change == nil {
		return nil
	}
	writeOpenCodeArtifact(ctx, execCtx, "diff", "patch", NormalizeCodexUnifiedDiff(workDirFromContext(execCtx), change.Diff), false)
	writeOpenCodeArtifact(ctx, execCtx, "file_bundle", "json", openCodeJSONString(change.ChangedFiles), false)
	result := map[string]any{
		"changed":        true,
		"branch":         change.Branch,
		"commit_sha":     change.CommitSHA,
		"commit_message": change.CommitMessage,
		"changed_files":  change.ChangedFiles,
		"precommitted":   true,
	}
	writeOpenCodeArtifact(ctx, execCtx, "git_persistence_result", "json", openCodeJSONString(result), false)
	return result
}

func openCodeRepoDiffAgainstBase(ctx context.Context, execCtx *ExecutionContext, workDir string) (diffOutput string, filesOutput string, err error) {
	var lastErr error
	for _, ref := range openCodeRepoComparisonRefs(execCtx) {
		files, diff, ok, diffErr := openCodeRepoDiffForRef(ctx, workDir, ref)
		if diffErr != nil {
			lastErr = diffErr
			continue
		}
		if ok {
			return diff, files, nil
		}
	}
	if lastErr != nil {
		return "", "", lastErr
	}
	baseBranch := firstNonEmpty(openCodeLeaseString(execCtx, "base_branch"), "main")
	return "", "", fmt.Errorf("inspect repository diff: unable to compare HEAD against %q", baseBranch)
}

func openCodeRepoComparisonRefs(execCtx *ExecutionContext) []string {
	baseBranch := firstNonEmpty(openCodeLeaseString(execCtx, "base_branch"), "main")
	workBranch := openCodeLeaseString(execCtx, "work_branch", "working_branch")
	refs := []string{}
	if workBranch != "" {
		refs = append(refs, "origin/"+workBranch)
	}
	refs = append(refs, "origin/"+baseBranch, baseBranch)
	return refs
}

func openCodeRepoDiffForRef(ctx context.Context, workDir string, ref string) (filesOutput string, diffOutput string, ok bool, err error) {
	filesOutput, err = runOpenCodeGit(ctx, workDir, "diff", "--name-only", ref+"...HEAD")
	if err != nil {
		return "", "", false, nil
	}
	ok = true
	if strings.TrimSpace(filesOutput) == "" {
		return "", "", true, nil
	}
	diffOutput, err = runOpenCodeGit(ctx, workDir, "diff", ref+"...HEAD")
	if err != nil {
		return "", "", true, fmt.Errorf("inspect committed diff: %s", strings.TrimSpace(firstNonEmpty(diffOutput, err.Error())))
	}
	return filesOutput, diffOutput, true, nil
}

func openCodeGitInsideWorkTree(ctx context.Context, workDir string) (bool, error) {
	out, err := runOpenCodeGit(ctx, workDir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

func runOpenCodeGit(ctx context.Context, workDir string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = strings.TrimSpace(workDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), err
	}
	return string(output), nil
}

func resolveOpenCodeWorkingBranch(ctx context.Context, execCtx *ExecutionContext, workDir string) (string, error) {
	if branch := openCodeLeaseString(execCtx, "work_branch", "working_branch"); branch != "" {
		return branch, nil
	}
	branchOutput, err := runOpenCodeGit(ctx, workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve working branch: %s", strings.TrimSpace(firstNonEmpty(branchOutput, err.Error())))
	}
	branch := strings.TrimSpace(branchOutput)
	if branch == "" {
		return "", fmt.Errorf("resolve working branch: git returned an empty branch name")
	}
	return branch, nil
}

func openCodeRequiresRepositoryChanges(execCtx *ExecutionContext) bool {
	if execCtx == nil || execCtx.Run == nil {
		return false
	}
	if strings.TrimSpace(execCtx.Run.InvocationMode) == agentcore.InvocationInteractive {
		return false
	}
	if openCodePresetKey(execCtx) == "review_agent" {
		return false
	}
	return true
}

func openCodePresetKey(execCtx *ExecutionContext) string {
	if execCtx == nil {
		return ""
	}
	if execCtx.Agent != nil && len(execCtx.Agent.ExecutionConfig) > 0 {
		var cfg map[string]any
		if json.Unmarshal(execCtx.Agent.ExecutionConfig, &cfg) == nil {
			if value := firstMapStringAny(cfg, "preset_key", "preset"); value != "" {
				return value
			}
		}
	}
	if execCtx.Run != nil {
		if value := firstMapStringAny(execCtx.Run.Input.Metadata, "preset_key", "preset"); value != "" {
			return value
		}
		if value := firstMapStringAny(execCtx.Run.Input.Trigger, "preset_key", "preset"); value != "" {
			return value
		}
	}
	for _, ref := range execCtx.SkillRefs {
		if strings.TrimSpace(ref.Key) == "review_agent" {
			return "review_agent"
		}
	}
	return ""
}

func openCodeWorkspaceFinalizerOwnsCommit(execCtx *ExecutionContext) bool {
	if execCtx == nil || execCtx.WorkspaceLease == nil {
		return false
	}
	if strings.TrimSpace(execCtx.WorkspaceLease.Provider) != "repository" {
		return false
	}
	spec := runtimeworkspace.RepositorySpecFromLease(*execCtx.WorkspaceLease)
	return spec != nil && strings.TrimSpace(spec.FinalizePolicy) == runtimeworkspace.RepositoryFinalizeLocalCommit
}

func openCodeCommitMessage(execCtx *ExecutionContext) string {
	for _, value := range []string{
		openCodeRunMetadataString(execCtx, "commit_message"),
		openCodeLeaseString(execCtx, "commit_message"),
	} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if execCtx != nil && execCtx.Run != nil && execCtx.Run.Target.ID != "" {
		return "agent-runtime: update " + execCtx.Run.Target.ID
	}
	return "agent-runtime: apply opencode changes"
}

func openCodeRunMetadataString(execCtx *ExecutionContext, key string) string {
	if execCtx == nil || execCtx.Run == nil {
		return ""
	}
	return firstMapStringAny(execCtx.Run.Input.Metadata, key)
}

func openCodeLeaseString(execCtx *ExecutionContext, keys ...string) string {
	if execCtx == nil || execCtx.WorkspaceLease == nil {
		return ""
	}
	return firstMapStringAny(execCtx.WorkspaceLease.Metadata, keys...)
}

func firstMapStringAny(values map[string]interface{}, keys ...string) string {
	if values == nil {
		return ""
	}
	for _, key := range keys {
		raw, ok := values[key]
		if !ok || raw == nil {
			continue
		}
		switch typed := raw.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		default:
			text := strings.TrimSpace(fmt.Sprint(typed))
			if text != "" {
				return text
			}
		}
	}
	return ""
}

func workDirFromContext(execCtx *ExecutionContext) string {
	if execCtx == nil {
		return ""
	}
	return firstNonEmpty(workspaceRoot(execCtx), "")
}

func openCodeJSONString(value any) string {
	payload, _ := json.Marshal(value)
	return string(payload)
}

func truncateOpenCodeSingleLine(value string, limit int) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	return truncateOpenCodeString(value, limit)
}

func openCodeEventPart(event map[string]any, properties map[string]any) map[string]any {
	if part, ok := event["part"].(map[string]any); ok {
		return part
	}
	if part, ok := properties["part"].(map[string]any); ok {
		return part
	}
	if properties != nil {
		return properties
	}
	return map[string]any{}
}

func openCodeUsageFromPart(part map[string]any) openCodeUsage {
	tokens, _ := part["tokens"].(map[string]any)
	var usage openCodeUsage
	usage.InputTokens = lookupInt(tokens, "input")
	usage.OutputTokens = lookupInt(tokens, "output")
	usage.ReasoningOutputTokens = lookupInt(tokens, "reasoning")
	if cache, ok := tokens["cache"].(map[string]any); ok {
		usage.CachedInputTokens = lookupInt(cache, "read") + lookupInt(cache, "write")
	}
	usage.TotalTokens = usage.InputTokens + usage.CachedInputTokens + usage.OutputTokens + usage.ReasoningOutputTokens
	return usage
}

func openCodeTokensFromPart(part map[string]any) int {
	return openCodeUsageFromPart(part).TotalTokens
}

func renderOpenCodeStepStart(part map[string]any) string {
	title := firstNonEmpty(lookupString(part, "title"), lookupString(part, "name"), lookupString(part, "tool"))
	if title == "" {
		return "Step started"
	}
	return "Step started: " + title
}

func renderOpenCodeToolUse(part map[string]any) string {
	title := firstNonEmpty(lookupString(part, "title"), lookupString(part, "name"), lookupString(part, "tool"))
	if title == "" {
		if metadata, ok := part["metadata"].(map[string]any); ok {
			title = firstNonEmpty(lookupString(metadata, "command"), lookupString(metadata, "description"), lookupString(metadata, "path"))
		}
	}
	if title == "" {
		title = renderOpenCodeRawPart(part)
	}
	if title == "" {
		return "Tool used"
	}
	return "Tool: " + title
}

func renderOpenCodeToolPart(part map[string]any) string {
	state, _ := part["state"].(map[string]any)
	if state == nil {
		return renderOpenCodeToolUse(part)
	}
	name := openCodeToolName(part, state)
	if name == "" {
		name = renderOpenCodeToolUse(part)
	}
	status := firstNonEmpty(lookupString(state, "status"), "running")
	switch status {
	case "completed":
		return "Tool completed: " + name
	case "error", "failed":
		return "Tool failed: " + name
	default:
		return "Tool: " + name
	}
}

func renderOpenCodeStepFinish(part map[string]any, tokensUsed int) string {
	details := []string{}
	if stopReason := firstNonEmpty(lookupString(part, "stopReason"), lookupString(part, "stop_reason")); stopReason != "" {
		details = append(details, "stop="+stopReason)
	}
	if tokensUsed > 0 {
		details = append(details, fmt.Sprintf("tokens=%d", tokensUsed))
	}
	if len(details) == 0 {
		return "Step finished"
	}
	return "Step finished (" + strings.Join(details, ", ") + ")"
}

func renderOpenCodeGenericEvent(eventType string, part map[string]any) string {
	label := strings.TrimSpace(eventType)
	if label == "" {
		label = "event"
	}
	raw := renderOpenCodeRawPart(part)
	if raw == "" {
		return "OpenCode " + label
	}
	return "OpenCode " + label + ": " + raw
}

func renderOpenCodeRawPart(part map[string]any) string {
	if len(part) == 0 {
		return ""
	}
	payload, err := json.Marshal(part)
	if err != nil {
		return ""
	}
	return truncateOpenCodeString(string(payload), 1000)
}

func openCodeDeltaFromSnapshot(previous, current string) string {
	if current == "" || current == previous {
		return ""
	}
	if previous == "" {
		return current
	}
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	return current
}

func openCodePartHasEnded(part map[string]any) bool {
	timeMap, _ := part["time"].(map[string]any)
	if len(timeMap) == 0 {
		return false
	}
	_, ok := timeMap["end"]
	return ok && timeMap["end"] != nil
}

func openCodeToolDurationMs(state map[string]any) int64 {
	timeMap, _ := state["time"].(map[string]any)
	if len(timeMap) == 0 {
		return 0
	}
	start := lookupInt64(timeMap, "start")
	end := lookupInt64(timeMap, "end")
	if start <= 0 || end <= 0 || end < start {
		return 0
	}
	return end - start
}

func openCodeToolName(part map[string]any, state map[string]any) string {
	metadata, _ := part["metadata"].(map[string]any)
	stateMetadata, _ := state["metadata"].(map[string]any)
	return firstNonEmpty(
		lookupString(part, "tool"),
		lookupString(part, "name"),
		lookupString(state, "title"),
		lookupString(part, "title"),
		lookupString(stateMetadata, "command"),
		lookupString(metadata, "command"),
		lookupString(stateMetadata, "description"),
		lookupString(metadata, "description"),
	)
}

func openCodeToolArgsText(part map[string]any, state map[string]any) string {
	return firstNonEmpty(
		openCodeNormalizedToolInput(openCodeToolInputForState(part, state)),
		lookupString(state, "title"),
		lookupString(part, "title"),
	)
}

func openCodeToolInputForState(part map[string]any, state map[string]any) string {
	if raw := firstNonEmpty(
		lookupString(state, "raw"),
		lookupJSONString(state, "input"),
		lookupJSONString(part, "input"),
	); raw != "" {
		return raw
	}
	stateMetadata, _ := state["metadata"].(map[string]any)
	metadata, _ := part["metadata"].(map[string]any)
	return firstNonEmpty(
		lookupString(stateMetadata, "command"),
		lookupString(metadata, "command"),
		lookupString(stateMetadata, "path"),
		lookupString(metadata, "path"),
		lookupString(part, "title"),
	)
}

func openCodeNormalizedToolInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(string(normalizeNativeToolInput(json.RawMessage(raw))))
}

func openCodeToolMutating(toolName string) bool {
	name := strings.ToLower(strings.TrimSpace(toolName))
	return strings.Contains(name, "edit") || strings.Contains(name, "write") || strings.Contains(name, "patch") || strings.Contains(name, "bash") || strings.Contains(name, "shell")
}

func lookupString(values map[string]any, key string) string {
	if len(values) == 0 {
		return ""
	}
	raw, ok := values[key]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return strings.TrimSpace(fmt.Sprint(raw))
	}
}

func lookupInt(values map[string]any, key string) int {
	if len(values) == 0 {
		return 0
	}
	raw, ok := values[key]
	if !ok || raw == nil {
		return 0
	}
	switch typed := raw.(type) {
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case int32:
		return int(typed)
	case json.Number:
		value, err := typed.Int64()
		if err == nil {
			return int(value)
		}
	case string:
		var value int
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &value); err == nil {
			return value
		}
	}
	return 0
}

func lookupInt64(values map[string]any, key string) int64 {
	if len(values) == 0 {
		return 0
	}
	raw, ok := values[key]
	if !ok || raw == nil {
		return 0
	}
	switch typed := raw.(type) {
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	case int32:
		return int64(typed)
	case json.Number:
		value, err := typed.Int64()
		if err == nil {
			return value
		}
	case string:
		var value int64
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &value); err == nil {
			return value
		}
	}
	return 0
}

func lookupJSONString(values map[string]any, key string) string {
	if len(values) == 0 {
		return ""
	}
	raw, ok := values[key]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return typed
	default:
		payload, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(payload)
	}
}

func stripOpenCodeANSI(value string) string {
	return ansiPattern.ReplaceAllString(value, "")
}

func sanitizeOpenCodeOutput(value string) string {
	return strings.TrimSpace(stripOpenCodeANSI(value))
}

func appendIfMissingEnv(env []string, key, value string) []string {
	if strings.TrimSpace(value) == "" {
		return env
	}
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return env
		}
	}
	return append(env, prefix+value)
}

func joinNonEmpty(sep string, values ...string) string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return strings.Join(out, sep)
}

func truncateOpenCodeString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func shortRunIDForOpenCode(runID string) string {
	runID = strings.TrimSpace(runID)
	if len(runID) <= 8 {
		return runID
	}
	return runID[:8]
}

func decodeOpenCodeConfig(content string) map[string]any {
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader([]byte(content)))
	decoder.UseNumber()
	_ = decoder.Decode(&body)
	return body
}
