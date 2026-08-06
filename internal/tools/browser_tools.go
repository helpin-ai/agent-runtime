package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
)

const (
	defaultBrowserCommandTimeout = 45 * time.Second
	defaultBrowserSessionTimeout = 300
	defaultBrowserSettleWaitMS   = 1500
	defaultBrowserMaxOutput      = 8000
	maxBrowserScreenshotBytes    = 10 * 1024 * 1024
)

type BrowserToolsConfig struct {
	Enabled               bool
	AppID                 string
	Binary                string
	KernelAPIKey          string
	AllowedDomains        []string
	CommandTimeout        time.Duration
	SessionTimeoutSeconds int
	MaxOutputChars        int
	ArtifactUploadURL     string
	ArtifactUploadToken   string
	HTTPClient            *http.Client
	Runner                BrowserCommandRunner
}

type BrowserCommandRunner interface {
	Run(ctx context.Context, env []string, args ...string) ([]byte, error)
}

type execBrowserCommandRunner struct {
	binary string
}

func (r execBrowserCommandRunner) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("agent-browser: %s", message)
	}
	return output, nil
}

type browserRunSession struct {
	appID       string
	runID       string
	sessionName string
	navigated   bool
	currentURL  string
	title       string
	mu          sync.Mutex
	idleTimer   *time.Timer
}

type BrowserManager struct {
	cfg      BrowserToolsConfig
	mu       sync.Mutex
	sessions map[string]*browserRunSession
}

type browserAsset struct {
	ArtifactID  string `json:"artifact_id"`
	ArtifactRef string `json:"artifact_ref"`
	Visibility  string `json:"visibility"`
	FileName    string `json:"file_name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
}

// BrowserToolsConfigFromEnv returns shared browser infrastructure settings.
// App policy, domains, and artifact destinations are supplied
// separately through AGENT_RUNTIME_APP_CONFIG.
func BrowserToolsConfigFromEnv() BrowserToolsConfig {
	enabled := envTruthy("AGENT_RUNTIME_BROWSER_ENABLED")
	apiKey := strings.TrimSpace(os.Getenv("KERNEL_API_KEY"))
	if !enabled || apiKey == "" {
		return BrowserToolsConfig{}
	}
	timeoutSeconds := boundedEnvInt("AGENT_RUNTIME_BROWSER_SESSION_TIMEOUT_SECONDS", defaultBrowserSessionTimeout, 60, 900)
	maxOutput := boundedEnvInt("AGENT_RUNTIME_BROWSER_MAX_OUTPUT_CHARS", defaultBrowserMaxOutput, 1000, 20000)
	commandTimeout := time.Duration(boundedEnvInt("AGENT_RUNTIME_BROWSER_COMMAND_TIMEOUT_SECONDS", int(defaultBrowserCommandTimeout/time.Second), 5, 120)) * time.Second
	return BrowserToolsConfig{
		Enabled:               true,
		Binary:                firstNonEmptyString(os.Getenv("AGENT_BROWSER_BINARY"), "agent-browser"),
		KernelAPIKey:          apiKey,
		CommandTimeout:        commandTimeout,
		SessionTimeoutSeconds: timeoutSeconds,
		MaxOutputChars:        maxOutput,
	}
}

func RegisterBrowserTools(r *Registry, cfg BrowserToolsConfig) {
	if r == nil || !cfg.Enabled {
		return
	}
	manager := newBrowserManager(cfg)
	r.RegisterRunCloser(manager)
	r.Register(Definition{
		Name:        "browser_open",
		Description: "Open an allowed web page in the current host-app scope's ephemeral browser session, wait briefly for client rendering, and return the final URL, title, and a compact interactive snapshot. Use fetch_url or crawl_url for public pages that do not require browser interaction.",
		Category:    "Browser",
		Mutating:    false,
		InputSchema: browserOpenSchema(),
	}, manager.open)
	r.Register(Definition{
		Name:        "browser_snapshot",
		Description: "Return the current page URL, title, and a refreshed bounded accessibility snapshot with element references. References are session-scoped and must be refreshed after navigation or a resumed run.",
		Category:    "Browser",
		Mutating:    false,
		InputSchema: browserSnapshotSchema(),
	}, manager.snapshot)
	r.Register(Definition{
		Name:        "browser_act",
		Description: "Perform one bounded browser interaction using an element reference from the latest snapshot, then return the resulting URL, title, and refreshed compact snapshot.",
		Category:    "Browser",
		Mutating:    true,
		RiskLevel:   RiskLevelSensitive,
		InputSchema: browserActSchema(),
	}, manager.act)
	if strings.TrimSpace(cfg.ArtifactUploadURL) != "" {
		r.Register(Definition{
			Name:        "browser_screenshot",
			Description: "Capture the current page as a durable private host-app artifact. Image bytes are uploaded directly and are never returned as text to the model. Set annotate=true for numbered interactive-element labels.",
			Category:    "Browser",
			Mutating:    true,
			RiskLevel:   RiskLevelRoutine,
			InputSchema: browserScreenshotSchema(),
		}, manager.screenshot)
	}
}

func newBrowserManager(cfg BrowserToolsConfig) *BrowserManager {
	if strings.TrimSpace(cfg.Binary) == "" {
		cfg.Binary = "agent-browser"
	}
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = defaultBrowserCommandTimeout
	}
	if cfg.SessionTimeoutSeconds <= 0 {
		cfg.SessionTimeoutSeconds = defaultBrowserSessionTimeout
	}
	if cfg.MaxOutputChars <= 0 {
		cfg.MaxOutputChars = defaultBrowserMaxOutput
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 45 * time.Second}
	}
	if cfg.Runner == nil {
		cfg.Runner = execBrowserCommandRunner{binary: cfg.Binary}
	}
	return &BrowserManager{cfg: cfg, sessions: map[string]*browserRunSession{}}
}

func browserOpenSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"url":     map[string]any{"type": "string", "minLength": 1, "description": "HTTP or HTTPS URL allowed by the current app's browser policy."},
			"depth":   map[string]any{"type": "integer", "minimum": 1, "maximum": 8, "description": "Snapshot depth. Defaults to 5."},
			"wait_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000, "description": "Additional client-rendering settle time after the load event. Defaults to 1500 milliseconds."},
		},
		"required": []string{"url"},
	}
}

func browserSnapshotSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"selector": map[string]any{"type": "string", "description": "Optional CSS selector used only to scope snapshot output."},
			"depth":    map[string]any{"type": "integer", "minimum": 1, "maximum": 8, "description": "Snapshot depth. Defaults to 5."},
		},
		"required": []string{},
	}
}

func browserActSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"click", "fill", "type", "press", "select", "check", "uncheck", "hover", "scroll", "wait"}, "description": "One browser action to execute."},
			"ref":    map[string]any{"type": "string", "description": "Element reference such as @e4. Required for element actions."},
			"value":  map[string]any{"type": "string", "description": "Text, key, select option, scroll direction, or wait target required by the selected action."},
			"amount": map[string]any{"type": "integer", "minimum": 1, "maximum": 5000, "description": "Optional scroll amount in pixels or wait duration in milliseconds."},
			"depth":  map[string]any{"type": "integer", "minimum": 1, "maximum": 8, "description": "Returned snapshot depth. Defaults to 5."},
		},
		"required": []string{"action"},
	}
}

func browserScreenshotSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"name":      map[string]any{"type": "string", "description": "Short descriptive asset name. Defaults to browser-screenshot."},
			"selector":  map[string]any{"type": "string", "description": "Optional element reference or CSS selector to capture."},
			"full_page": map[string]any{"type": "boolean", "description": "Capture the full page instead of the viewport. Defaults to false."},
			"annotate":  map[string]any{"type": "boolean", "description": "Add numbered labels to interactive elements. Defaults to false."},
			"format":    map[string]any{"type": "string", "enum": []string{"png", "jpeg"}, "description": "Image format. Defaults to png."},
		},
		"required": []string{},
	}
}

func (m *BrowserManager) open(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		URL    string `json:"url"`
		Depth  int    `json:"depth"`
		WaitMS *int   `json:"wait_ms"`
	}
	if err := decodeStrictBrowserInput(input, &params); err != nil {
		return nil, err
	}
	params.URL = strings.TrimSpace(params.URL)
	if params.URL == "" {
		return nil, fmt.Errorf("url is required")
	}
	if err := m.validateURL(params.URL); err != nil {
		return nil, err
	}
	waitMS, err := browserSettleWaitMS(params.WaitMS)
	if err != nil {
		return nil, err
	}
	session, err := m.session(callCtx)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	openOutput, err := m.run(ctx, session, nil, "open", params.URL)
	if err != nil {
		return nil, err
	}
	session.currentURL = firstNonEmptyString(browserResultString(openOutput, "url"), params.URL)
	session.title = browserResultString(openOutput, "title")
	session.navigated = isNavigatedBrowserURL(session.currentURL)
	if waitMS > 0 {
		if _, err := m.run(ctx, session, nil, "wait", strconv.Itoa(waitMS)); err != nil {
			return nil, err
		}
	}
	return m.snapshotLocked(ctx, session, "", boundedDepth(params.Depth))
}

func (m *BrowserManager) snapshot(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Selector string `json:"selector"`
		Depth    int    `json:"depth"`
	}
	if err := decodeStrictBrowserInput(input, &params); err != nil {
		return nil, err
	}
	session, err := m.session(callCtx)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := requireNavigatedBrowserSession(session); err != nil {
		return nil, err
	}
	return m.snapshotLocked(ctx, session, strings.TrimSpace(params.Selector), boundedDepth(params.Depth))
}

func (m *BrowserManager) act(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Action string `json:"action"`
		Ref    string `json:"ref"`
		Value  string `json:"value"`
		Amount int    `json:"amount"`
		Depth  int    `json:"depth"`
	}
	if err := decodeStrictBrowserInput(input, &params); err != nil {
		return nil, err
	}
	params.Action = strings.ToLower(strings.TrimSpace(params.Action))
	params.Ref = strings.TrimSpace(params.Ref)
	params.Value = strings.TrimSpace(params.Value)
	args, err := browserActionArgs(params.Action, params.Ref, params.Value, params.Amount)
	if err != nil {
		return nil, err
	}
	session, err := m.session(callCtx)
	if err != nil {
		return nil, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := requireNavigatedBrowserSession(session); err != nil {
		return nil, err
	}
	if _, err := m.run(ctx, session, nil, args...); err != nil {
		return nil, err
	}
	return m.snapshotLocked(ctx, session, "", boundedDepth(params.Depth))
}

func (m *BrowserManager) screenshot(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name     string `json:"name"`
		Selector string `json:"selector"`
		FullPage bool   `json:"full_page"`
		Annotate bool   `json:"annotate"`
		Format   string `json:"format"`
	}
	if err := decodeStrictBrowserInput(input, &params); err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.cfg.ArtifactUploadURL) == "" {
		return nil, fmt.Errorf("browser screenshot storage is not configured")
	}
	format := strings.ToLower(strings.TrimSpace(params.Format))
	if format == "" {
		format = "png"
	}
	if format != "png" && format != "jpeg" {
		return nil, fmt.Errorf("format must be png or jpeg")
	}
	session, err := m.session(callCtx)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(os.TempDir(), "agent-runtime-browser", session.sessionName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("prepare screenshot directory: %w", err)
	}
	defer os.RemoveAll(dir)
	filename := sanitizeBrowserAssetName(params.Name) + "." + format
	path := filepath.Join(dir, filename)
	args := []string{"screenshot"}
	if selector := strings.TrimSpace(params.Selector); selector != "" {
		args = append(args, selector)
	}
	if params.FullPage {
		args = append(args, "--full")
	}
	args = append(args, path)
	extraGlobal := []string{"--screenshot-format", format}
	if params.Annotate {
		extraGlobal = append(extraGlobal, "--annotate")
	}
	session.mu.Lock()
	if err := requireNavigatedBrowserSession(session); err != nil {
		session.mu.Unlock()
		return nil, err
	}
	if err := m.refreshPageStateLocked(ctx, session); err != nil {
		session.mu.Unlock()
		return nil, err
	}
	_, runErr := m.run(ctx, session, extraGlobal, args...)
	session.mu.Unlock()
	if runErr != nil {
		return nil, runErr
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read browser screenshot: %w", err)
	}
	if info.Size() <= 0 || info.Size() > maxBrowserScreenshotBytes {
		return nil, fmt.Errorf("browser screenshot exceeds the 10 MB limit")
	}
	asset, err := m.uploadAsset(ctx, session, path, filename, format, params.Annotate, params.FullPage, info.Size())
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"status":       "captured",
		"url":          session.currentURL,
		"title":        session.title,
		"artifact_id":  asset.ArtifactID,
		"artifact_ref": asset.ArtifactRef,
		"visibility":   asset.Visibility,
		"file_name":    firstNonEmptyString(asset.FileName, filename),
		"content_type": firstNonEmptyString(asset.ContentType, browserContentType(format)),
		"format":       format,
		"size_bytes":   info.Size(),
		"annotated":    params.Annotate,
		"full_page":    params.FullPage,
	})
}

func (m *BrowserManager) snapshotLocked(ctx context.Context, session *browserRunSession, selector string, depth int) (json.RawMessage, error) {
	if err := requireNavigatedBrowserSession(session); err != nil {
		return nil, err
	}
	args := []string{"snapshot", "-i", "-c", "-d", strconv.Itoa(depth)}
	if selector != "" {
		args = append(args, "-s", selector)
	}
	output, err := m.run(ctx, session, nil, args...)
	if err != nil {
		return nil, err
	}
	if err := m.refreshPageStateLocked(ctx, session); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"url":      session.currentURL,
		"title":    session.title,
		"snapshot": compactBrowserOutput(output, m.cfg.MaxOutputChars),
	})
}

func (m *BrowserManager) refreshPageStateLocked(ctx context.Context, session *browserRunSession) error {
	urlOutput, err := m.run(ctx, session, nil, "get", "url")
	if err != nil {
		return err
	}
	currentURL := strings.TrimSpace(browserResultString(urlOutput, "url"))
	if !isNavigatedBrowserURL(currentURL) {
		session.navigated = false
		session.currentURL = currentURL
		return fmt.Errorf("browser session has no open page; call browser_open before using this tool")
	}
	session.currentURL = currentURL
	session.navigated = true
	if titleOutput, titleErr := m.run(ctx, session, nil, "get", "title"); titleErr == nil {
		session.title = strings.TrimSpace(browserResultString(titleOutput, "title"))
	}
	return nil
}

func requireNavigatedBrowserSession(session *browserRunSession) error {
	if session == nil || !session.navigated {
		return fmt.Errorf("browser session has no open page; call browser_open before using this tool")
	}
	return nil
}

func isNavigatedBrowserURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && strings.TrimSpace(parsed.Hostname()) != ""
}

func browserSettleWaitMS(value *int) (int, error) {
	if value == nil {
		return defaultBrowserSettleWaitMS, nil
	}
	if *value < 0 || *value > 10000 {
		return 0, fmt.Errorf("wait_ms must be between 0 and 10000")
	}
	return *value, nil
}

func browserResultString(output []byte, key string) string {
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(output, &payload) != nil || payload.Data == nil {
		return ""
	}
	value, _ := payload.Data[key].(string)
	return strings.TrimSpace(value)
}

func (m *BrowserManager) run(ctx context.Context, session *browserRunSession, extraGlobal []string, command ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, m.cfg.CommandTimeout)
	defer cancel()
	args := []string{"--session", session.sessionName, "--json", "--provider", "kernel", "--content-boundaries", "--max-output", strconv.Itoa(m.cfg.MaxOutputChars)}
	args = append(args, extraGlobal...)
	args = append(args, command...)
	return m.cfg.Runner.Run(commandCtx, m.environment(session), args...)
}

func (m *BrowserManager) environment(session *browserRunSession) []string {
	overrides := []string{
		"KERNEL_API_KEY=" + m.cfg.KernelAPIKey,
		"KERNEL_TIMEOUT_SECONDS=" + strconv.Itoa(m.cfg.SessionTimeoutSeconds),
		"AGENT_BROWSER_PROVIDER=kernel",
		"AGENT_BROWSER_SESSION=" + session.sessionName,
	}
	if allowedDomains := agentBrowserAllowedDomains(m.cfg.AllowedDomains); allowedDomains != "" {
		overrides = append(overrides, "AGENT_BROWSER_ALLOWED_DOMAINS="+allowedDomains)
	}
	return procenv.Sanitized(overrides...)
}

// agent-browser treats an absent allowlist as unrestricted and does not
// recognize a bare "*" wildcard. Agent Runtime does recognize "*", so translate
// that app policy by omitting AGENT_BROWSER_ALLOWED_DOMAINS entirely.
func agentBrowserAllowedDomains(domains []string) string {
	normalized := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.TrimSpace(domain)
		if domain == "*" {
			return ""
		}
		if domain != "" {
			normalized = append(normalized, domain)
		}
	}
	return strings.Join(normalized, ",")
}

func (m *BrowserManager) session(callCtx CallContext) (*browserRunSession, error) {
	appID := firstNonEmptyString(callCtx.AppID, func() string {
		if callCtx.Run != nil {
			return callCtx.Run.AppID
		}
		return ""
	}())
	runID := firstNonEmptyString(callCtx.RunID, func() string {
		if callCtx.Run != nil {
			return callCtx.Run.ID
		}
		return ""
	}())
	if appID == "" || runID == "" || callCtx.Run == nil {
		return nil, fmt.Errorf("browser tools require an active agent run")
	}
	if configuredAppID := strings.TrimSpace(m.cfg.AppID); configuredAppID != "" && configuredAppID != appID {
		return nil, fmt.Errorf("browser tools are not configured for app %q", appID)
	}
	key := appID + "/" + runID
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.sessions[key]; existing != nil {
		m.resetIdleTimer(existing)
		return existing, nil
	}
	session := &browserRunSession{
		appID: appID, runID: runID, sessionName: "ar-" + shortBrowserHash(key),
	}
	m.sessions[key] = session
	m.resetIdleTimer(session)
	return session, nil
}

func (m *BrowserManager) resetIdleTimer(session *browserRunSession) {
	if session == nil || m.cfg.SessionTimeoutSeconds <= 0 {
		return
	}
	if session.idleTimer != nil {
		session.idleTimer.Stop()
	}
	appID, runID := session.appID, session.runID
	session.idleTimer = time.AfterFunc(time.Duration(m.cfg.SessionTimeoutSeconds)*time.Second, func() {
		_ = m.CloseRun(context.Background(), appID, runID)
	})
}

func (m *BrowserManager) CloseRun(ctx context.Context, appID, runID string) error {
	key := strings.TrimSpace(appID) + "/" + strings.TrimSpace(runID)
	m.mu.Lock()
	session := m.sessions[key]
	if session != nil {
		delete(m.sessions, key)
		if session.idleTimer != nil {
			session.idleTimer.Stop()
		}
	}
	m.mu.Unlock()
	if session == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	_, err := m.run(closeCtx, session, nil, "close")
	return err
}

func (m *BrowserManager) validateURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || strings.TrimSpace(parsed.Hostname()) == "" {
		return fmt.Errorf("url must be a valid http or https URL")
	}
	if len(m.cfg.AllowedDomains) == 0 {
		return fmt.Errorf("browser allowed domains are not configured")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	for _, allowed := range m.cfg.AllowedDomains {
		allowed = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(allowed, ".")))
		if allowed == "*" || allowed == host || (strings.HasPrefix(allowed, "*.") && strings.HasSuffix(host, strings.TrimPrefix(allowed, "*"))) {
			return nil
		}
	}
	return fmt.Errorf("url domain %q is not allowed for browser access", host)
}

func (m *BrowserManager) uploadAsset(ctx context.Context, session *browserRunSession, path, filename, format string, annotated, fullPage bool, size int64) (*browserAsset, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open browser screenshot: %w", err)
	}
	defer file.Close()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadata, err := json.Marshal(map[string]any{
		"file_name": filename, "content_type": browserContentType(format),
		"size_bytes": size, "annotated": annotated, "full_page": fullPage,
	})
	if err != nil {
		return nil, err
	}
	for key, value := range map[string]string{"app_id": session.appID, "run_id": session.runID, "artifact_type": "browser_screenshot", "metadata": string(metadata)} {
		if err := writer.WriteField(key, value); err != nil {
			return nil, err
		}
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, io.LimitReader(file, maxBrowserScreenshotBytes+1)); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.ArtifactUploadURL, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if m.cfg.ArtifactUploadToken != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.ArtifactUploadToken)
	}
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload browser screenshot: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upload browser screenshot: host returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	var asset browserAsset
	if err := json.Unmarshal(payload, &asset); err != nil {
		return nil, fmt.Errorf("decode browser screenshot asset: %w", err)
	}
	if strings.TrimSpace(asset.ArtifactID) == "" || strings.TrimSpace(asset.ArtifactRef) == "" {
		return nil, fmt.Errorf("upload browser screenshot: host response is missing artifact_id or artifact_ref")
	}
	if strings.TrimSpace(asset.Visibility) != "private" {
		return nil, fmt.Errorf("upload browser screenshot: host response visibility must be private")
	}
	_ = format
	return &asset, nil
}

func browserContentType(format string) string {
	if format == "jpeg" {
		return "image/jpeg"
	}
	return "image/png"
}

func browserActionArgs(action, ref, value string, amount int) ([]string, error) {
	requireRef := func() error {
		if ref == "" {
			return fmt.Errorf("ref is required for action %s", action)
		}
		if !strings.HasPrefix(ref, "@e") {
			return fmt.Errorf("ref must be an element reference such as @e4")
		}
		return nil
	}
	switch action {
	case "click", "check", "uncheck", "hover":
		if err := requireRef(); err != nil {
			return nil, err
		}
		return []string{action, ref}, nil
	case "fill", "type", "select":
		if err := requireRef(); err != nil {
			return nil, err
		}
		if value == "" {
			return nil, fmt.Errorf("value is required for action %s", action)
		}
		return []string{action, ref, value}, nil
	case "press":
		if value == "" {
			return nil, fmt.Errorf("value is required for action press")
		}
		return []string{"press", value}, nil
	case "scroll":
		if value != "up" && value != "down" && value != "left" && value != "right" {
			return nil, fmt.Errorf("value must be up, down, left, or right for action scroll")
		}
		if amount <= 0 {
			amount = 600
		}
		return []string{"scroll", value, strconv.Itoa(amount)}, nil
	case "wait":
		if amount > 0 {
			return []string{"wait", strconv.Itoa(amount)}, nil
		}
		if value == "" {
			return nil, fmt.Errorf("value or amount is required for action wait")
		}
		return []string{"wait", value}, nil
	default:
		return nil, fmt.Errorf("action must be click, fill, type, press, select, check, uncheck, hover, scroll, or wait")
	}
}

func decodeStrictBrowserInput(input json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("parse input: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("parse input: expected one JSON object")
	}
	return nil
}

func compactBrowserOutput(output []byte, maxChars int) json.RawMessage {
	text := strings.TrimSpace(string(output))
	if maxChars > 0 && len(text) > maxChars {
		text = text[:maxChars] + "\n[truncated]"
	}
	var payload any
	if json.Unmarshal([]byte(text), &payload) == nil {
		compacted, _ := json.Marshal(payload)
		return compacted
	}
	encoded, _ := json.Marshal(map[string]any{"snapshot": text})
	return encoded
}

func boundedDepth(depth int) int {
	if depth <= 0 {
		return 5
	}
	if depth > 8 {
		return 8
	}
	return depth
}

func sanitizeBrowserAssetName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "browser-screenshot"
	}
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	sanitized := strings.Trim(strings.TrimSpace(b.String()), "-")
	if sanitized == "" {
		return "browser-screenshot"
	}
	return sanitized
}

func shortBrowserHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:20]
}
func splitBrowserCSV(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	out := make([]string, 0, len(fields))
	for _, value := range fields {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
func envTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
func boundedEnvInt(name string, fallback, min, max int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
