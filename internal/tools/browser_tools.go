package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	defaultBrowserCommandTimeout   = 45 * time.Second
	defaultBrowserSessionTimeout   = 300
	defaultBrowserSettleWaitMS     = 1500
	defaultBrowserMaxOutput        = 8000
	defaultBrowserViewportWidth    = 1440
	defaultBrowserViewportHeight   = 900
	defaultBrowserReplayFramerate  = 15
	defaultBrowserReplayMaxSeconds = 180
	maxBrowserReplaySeconds        = 600
	browserReplayCleanupGraceSecs  = 60
	maxBrowserScreenshotBytes      = 10 * 1024 * 1024
	maxBrowserRecordingBytes       = 100 * 1024 * 1024
)

type BrowserToolsConfig struct {
	Enabled               bool
	AppID                 string
	Binary                string
	ChromiumExecutable    string
	KernelAPIKey          string
	AllowedDomains        []string
	CommandTimeout        time.Duration
	SessionTimeoutSeconds int
	MaxOutputChars        int
	ArtifactUploadURL     string
	ArtifactUploadToken   string
	HTTPClient            *http.Client
	Runner                BrowserCommandRunner
	Kernel                kernelBrowserProvider
	KernelHeadless        bool
	KernelStealth         bool
	ReplayFramerate       int
	FFmpegBinary          string
	RecordingTrimTimeout  time.Duration
	RecordingTrimThreads  int
	RecordingTrimPrePad   time.Duration
	RecordingTrimPostPad  time.Duration
	RecordingTrimLimiter  chan struct{}
	RecordingTrimmer      browserRecordingTrimmer
	RecordingConverter    browserRecordingConverter
	DisableRecordingTrim  bool
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
	appID           string
	runID           string
	sessionName     string
	navigated       bool
	currentURL      string
	title           string
	viewportSet     bool
	kernelSessionID string
	kernelHeadless  bool
	backend         browserBackend
	connected       bool
	recording       *browserRecording
	mu              sync.Mutex
	idleTimer       *time.Timer
}

type browserBackend string

const (
	browserBackendChromium browserBackend = "chromium"
	browserBackendKernel   browserBackend = "kernel"
)

type browserRecording struct {
	backend            browserBackend
	replayID           string
	fileName           string
	localDir           string
	localRawPath       string
	startedAt          time.Time
	maxDurationSeconds int
	recordAudio        bool
	stopped            bool
	timelineStartedAt  time.Time
	stoppedAt          time.Time
	actionWindows      []browserRecordingWindow
	stopTimer          *time.Timer
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
	if !enabled {
		return BrowserToolsConfig{}
	}
	timeoutSeconds := boundedEnvInt("AGENT_RUNTIME_BROWSER_SESSION_TIMEOUT_SECONDS", defaultBrowserSessionTimeout, 60, 900)
	maxOutput := boundedEnvInt("AGENT_RUNTIME_BROWSER_MAX_OUTPUT_CHARS", defaultBrowserMaxOutput, 1000, 20000)
	commandTimeout := time.Duration(boundedEnvInt("AGENT_RUNTIME_BROWSER_COMMAND_TIMEOUT_SECONDS", int(defaultBrowserCommandTimeout/time.Second), 5, 120)) * time.Second
	baseURL := firstNonEmptyString(os.Getenv("KERNEL_BASE_URL"), os.Getenv("KERNEL_ENDPOINT"))
	cfg := BrowserToolsConfig{
		Enabled:               true,
		Binary:                firstNonEmptyString(os.Getenv("AGENT_BROWSER_BINARY"), "agent-browser"),
		ChromiumExecutable:    strings.TrimSpace(os.Getenv("AGENT_RUNTIME_BROWSER_CHROMIUM_EXECUTABLE")),
		KernelAPIKey:          apiKey,
		CommandTimeout:        commandTimeout,
		SessionTimeoutSeconds: timeoutSeconds,
		MaxOutputChars:        maxOutput,
		KernelHeadless:        envBoolDefault("KERNEL_HEADLESS", true),
		KernelStealth:         envBoolDefault("KERNEL_STEALTH", true),
		ReplayFramerate:       boundedEnvInt("AGENT_RUNTIME_BROWSER_REPLAY_FRAMERATE", defaultBrowserReplayFramerate, 1, 20),
		FFmpegBinary:          firstNonEmptyString(os.Getenv("AGENT_RUNTIME_BROWSER_FFMPEG_BINARY"), "ffmpeg"),
		RecordingTrimTimeout:  time.Duration(boundedEnvInt("AGENT_RUNTIME_BROWSER_RECORDING_TRIM_TIMEOUT_SECONDS", int(defaultBrowserRecordingTrimTimeout/time.Second), 10, 300)) * time.Second,
		RecordingTrimThreads:  boundedEnvInt("AGENT_RUNTIME_BROWSER_RECORDING_TRIM_THREADS", defaultBrowserRecordingTrimThreads, 1, 4),
		RecordingTrimPrePad:   time.Duration(boundedEnvInt("AGENT_RUNTIME_BROWSER_RECORDING_TRIM_PRE_PADDING_MS", int(defaultBrowserRecordingTrimPrePadding/time.Millisecond), 0, 5000)) * time.Millisecond,
		RecordingTrimPostPad:  time.Duration(boundedEnvInt("AGENT_RUNTIME_BROWSER_RECORDING_TRIM_POST_PADDING_MS", int(defaultBrowserRecordingTrimPostPadding/time.Millisecond), 0, 5000)) * time.Millisecond,
		RecordingTrimLimiter:  globalBrowserRecordingTrimLimiter(),
		DisableRecordingTrim:  !envBoolDefault("AGENT_RUNTIME_BROWSER_RECORDING_SMART_TRIM_ENABLED", true),
	}
	if apiKey != "" {
		cfg.Kernel = newSDKKernelBrowserProvider(apiKey, baseURL)
	}
	return cfg
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
		RiskLevel:   RiskLevelRoutine,
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
		r.Register(Definition{
			Name:        "browser_record",
			Description: "Start or stop a bounded recording of the current private browser session. Stopping removes idle agent-reasoning gaps when browser actions were captured, then persists one MP4 as a durable private host-app artifact without returning video bytes or provider URLs to the model.",
			Category:    "Browser",
			Mutating:    true,
			RiskLevel:   RiskLevelRoutine,
			InputSchema: browserRecordSchema(),
		}, manager.record)
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
	if cfg.ReplayFramerate <= 0 {
		cfg.ReplayFramerate = defaultBrowserReplayFramerate
	}
	if cfg.RecordingTrimTimeout <= 0 {
		cfg.RecordingTrimTimeout = defaultBrowserRecordingTrimTimeout
	}
	if cfg.RecordingTrimThreads <= 0 {
		cfg.RecordingTrimThreads = defaultBrowserRecordingTrimThreads
	}
	if cfg.RecordingTrimPrePad <= 0 {
		cfg.RecordingTrimPrePad = defaultBrowserRecordingTrimPrePadding
	}
	if cfg.RecordingTrimPostPad <= 0 {
		cfg.RecordingTrimPostPad = defaultBrowserRecordingTrimPostPadding
	}
	if cfg.RecordingTrimLimiter == nil {
		cfg.RecordingTrimLimiter = make(chan struct{}, 1)
	}
	if cfg.RecordingTrimmer == nil && !cfg.DisableRecordingTrim {
		cfg.RecordingTrimmer = &ffmpegBrowserRecordingTrimmer{
			binary: firstNonEmptyString(cfg.FFmpegBinary, "ffmpeg"), timeout: cfg.RecordingTrimTimeout,
			threads: cfg.RecordingTrimThreads, limiter: cfg.RecordingTrimLimiter,
		}
	}
	if cfg.RecordingConverter == nil {
		cfg.RecordingConverter = &ffmpegBrowserRecordingConverter{
			binary: firstNonEmptyString(cfg.FFmpegBinary, "ffmpeg"), timeout: cfg.RecordingTrimTimeout,
			threads: cfg.RecordingTrimThreads, limiter: cfg.RecordingTrimLimiter,
		}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
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

func browserRecordSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"action":               map[string]any{"type": "string", "enum": []string{"start", "stop"}, "description": "Start or stop the current run-scoped browser recording."},
			"name":                 map[string]any{"type": "string", "description": "Short descriptive recording name. Used only with action=start and defaults to browser-recording."},
			"max_duration_seconds": map[string]any{"type": "integer", "minimum": 10, "maximum": maxBrowserReplaySeconds, "description": "Maximum recording duration. Used only with action=start and defaults to 180 seconds."},
			"record_audio":         map[string]any{"type": "boolean", "description": "Include browser audio. Used only with action=start and defaults to false."},
		},
		"required": []string{"action"},
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
	if err := m.ensureConnectedLocked(ctx, session, m.cfg.KernelHeadless, session.sessionName); err != nil {
		return nil, err
	}
	recording, windowStart := beginBrowserRecordingWindow(session)
	defer finishBrowserRecordingWindow(recording, windowStart)
	if !session.viewportSet {
		if _, err := m.run(ctx, session, nil, "set", "viewport", strconv.Itoa(defaultBrowserViewportWidth), strconv.Itoa(defaultBrowserViewportHeight)); err != nil {
			return nil, err
		}
		session.viewportSet = true
	}
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
	recording, windowStart := beginBrowserRecordingWindow(session)
	defer finishBrowserRecordingWindow(recording, windowStart)
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
	dir, err := os.MkdirTemp("", "agent-runtime-browser-screenshot-"+session.sessionName+"-")
	if err != nil {
		return nil, fmt.Errorf("prepare screenshot directory: %w", err)
	}
	defer os.RemoveAll(dir)
	filename := sanitizeBrowserAssetName(params.Name, "browser-screenshot") + "." + format
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
	asset, err := m.uploadAsset(ctx, session, browserArtifactUpload{
		Path: path, FileName: filename, ArtifactType: "browser_screenshot",
		ContentType: browserContentType(format), Size: info.Size(), MaxBytes: maxBrowserScreenshotBytes,
		Metadata: map[string]any{"annotated": params.Annotate, "full_page": params.FullPage},
	})
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

func (m *BrowserManager) record(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Action             string `json:"action"`
		Name               string `json:"name"`
		MaxDurationSeconds int    `json:"max_duration_seconds"`
		RecordAudio        *bool  `json:"record_audio"`
	}
	if err := decodeStrictBrowserInput(input, &params); err != nil {
		return nil, err
	}
	params.Action = strings.ToLower(strings.TrimSpace(params.Action))
	params.Name = strings.TrimSpace(params.Name)
	if params.Action != "start" && params.Action != "stop" {
		return nil, fmt.Errorf("action must be start or stop")
	}
	if strings.TrimSpace(m.cfg.ArtifactUploadURL) == "" {
		return nil, fmt.Errorf("browser recording storage is not configured")
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
	if session.backend == browserBackendKernel && strings.TrimSpace(session.kernelSessionID) == "" {
		return nil, fmt.Errorf("Kernel browser session is not connected; call browser_open before using this tool")
	}
	if params.Action == "start" {
		return m.startRecordingLocked(ctx, session, params.Name, params.MaxDurationSeconds, params.RecordAudio)
	}
	if params.Name != "" || params.MaxDurationSeconds != 0 || params.RecordAudio != nil {
		return nil, fmt.Errorf("name, max_duration_seconds, and record_audio are only valid with action start")
	}
	return m.finalizeRecordingLocked(ctx, session)
}

func (m *BrowserManager) startRecordingLocked(ctx context.Context, session *browserRunSession, name string, maxDurationSeconds int, recordAudio *bool) (json.RawMessage, error) {
	if session.recording != nil {
		return nil, fmt.Errorf("browser recording is already active; call browser_record with action stop")
	}
	if maxDurationSeconds == 0 {
		maxDurationSeconds = defaultBrowserReplayMaxSeconds
	}
	if maxDurationSeconds < 10 || maxDurationSeconds > maxBrowserReplaySeconds {
		return nil, fmt.Errorf("max_duration_seconds must be between 10 and 600")
	}
	audio := recordAudio != nil && *recordAudio
	if err := m.refreshPageStateLocked(ctx, session); err != nil {
		return nil, err
	}
	fileName := sanitizeBrowserAssetName(name, "browser-recording") + ".mp4"
	var recording *browserRecording
	if session.backend == browserBackendKernel {
		if session.kernelHeadless {
			if err := m.upgradeToHeadfulLocked(ctx, session); err != nil {
				return nil, err
			}
		}
		if err := m.refreshPageStateLocked(ctx, session); err != nil {
			return nil, err
		}
		replay, err := m.cfg.Kernel.StartReplay(ctx, session.kernelSessionID, kernelReplayStartRequest{
			Framerate: m.cfg.ReplayFramerate, MaxDurationSeconds: maxDurationSeconds, RecordAudio: audio,
		})
		if err != nil {
			return nil, err
		}
		startedAt := replay.Started
		if startedAt.IsZero() {
			startedAt = time.Now().UTC()
		}
		recording = &browserRecording{
			backend: browserBackendKernel, replayID: replay.ReplayID, fileName: fileName,
			startedAt: startedAt.UTC(), timelineStartedAt: startedAt.UTC(), maxDurationSeconds: maxDurationSeconds, recordAudio: audio,
		}
	} else {
		if audio {
			return nil, fmt.Errorf("local Chromium recording does not support audio; set record_audio to false")
		}
		dir, err := os.MkdirTemp("", "agent-runtime-browser-recording-"+session.sessionName+"-")
		if err != nil {
			return nil, fmt.Errorf("prepare local browser recording directory: %w", err)
		}
		rawPath := filepath.Join(dir, "raw-"+strings.TrimSuffix(fileName, ".mp4")+".webm")
		if _, err := m.run(ctx, session, nil, "record", "start", rawPath); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("start local Chromium recording: %w", err)
		}
		startedAt := time.Now().UTC()
		recording = &browserRecording{
			backend: browserBackendChromium, fileName: fileName, localDir: dir, localRawPath: rawPath,
			startedAt: startedAt, timelineStartedAt: startedAt, maxDurationSeconds: maxDurationSeconds,
		}
		recording.stopTimer = time.AfterFunc(time.Duration(maxDurationSeconds)*time.Second, func() {
			session.mu.Lock()
			defer session.mu.Unlock()
			if session.recording != recording || recording.stopped {
				return
			}
			stopCtx, cancel := context.WithTimeout(context.Background(), m.cfg.CommandTimeout)
			defer cancel()
			if err := m.stopLocalRecordingLocked(stopCtx, session, recording); err != nil {
				slog.WarnContext(stopCtx, "stop local Chromium recording at maximum duration",
					"app_id", session.appID, "run_id", session.runID, "error", err)
			}
		})
	}
	session.recording = recording
	// Keep the run session alive through the recording's maximum-duration stop and a
	// short processing grace period even when the normal idle timeout is lower.
	m.resetIdleTimerAfter(session, time.Duration(max(m.cfg.SessionTimeoutSeconds, maxDurationSeconds+browserReplayCleanupGraceSecs))*time.Second)
	return json.Marshal(map[string]any{
		"status": "recording", "url": session.currentURL, "title": session.title,
		"file_name": session.recording.fileName, "started_at": session.recording.startedAt,
		"max_duration_seconds": maxDurationSeconds, "record_audio": audio,
	})
}

func (m *BrowserManager) finalizeRecordingLocked(ctx context.Context, session *browserRunSession) (json.RawMessage, error) {
	recording := session.recording
	if recording == nil {
		return nil, fmt.Errorf("browser recording is not active; call browser_record with action start")
	}
	if recording.stopTimer != nil {
		recording.stopTimer.Stop()
	}
	if !recording.stopped {
		if recording.backend == browserBackendKernel {
			if err := m.cfg.Kernel.StopReplay(ctx, session.kernelSessionID, recording.replayID); err != nil {
				return nil, err
			}
			recording.stopped = true
			recording.stoppedAt = time.Now().UTC()
		} else if err := m.stopLocalRecordingLocked(ctx, session, recording); err != nil {
			return nil, err
		}
	}
	if recording.stoppedAt.IsZero() {
		recording.stoppedAt = time.Now().UTC()
	}
	dir := recording.localDir
	rawPath := recording.localRawPath
	var rawSize int64
	if recording.backend == browserBackendKernel {
		var err error
		dir, err = os.MkdirTemp("", "agent-runtime-browser-recording-"+session.sessionName+"-")
		if err != nil {
			return nil, fmt.Errorf("prepare browser recording directory: %w", err)
		}
		defer os.RemoveAll(dir)
		rawPath = filepath.Join(dir, "raw-"+recording.fileName)
		rawSize, err = m.downloadReplay(ctx, session, recording, rawPath)
		if err != nil {
			return nil, err
		}
	} else {
		info, err := os.Stat(rawPath)
		if err != nil {
			return nil, fmt.Errorf("read local Chromium recording: %w", err)
		}
		rawSize = info.Size()
		if rawSize <= 0 || rawSize > maxBrowserRecordingBytes {
			return nil, fmt.Errorf("browser recording must be between 1 byte and 100 MB")
		}
	}
	rawDuration := browserRecordingRawDuration(recording)
	uploadPath := rawPath
	uploadSize := rawSize
	outputDuration := rawDuration
	trimmedIdle := time.Duration(0)
	trimStatus := "disabled"
	trimmed := false
	trimWindowCount := 0
	if m.cfg.RecordingTrimmer != nil {
		trimStatus = "no_actions"
		windows, selectedDuration, windowErr := normalizeBrowserRecordingWindows(recording.actionWindows,
			rawDuration, m.cfg.RecordingTrimPrePad, m.cfg.RecordingTrimPostPad)
		if windowErr != nil && len(recording.actionWindows) > 0 {
			trimStatus = "fallback"
			slog.WarnContext(ctx, "browser recording action windows could not be normalized; uploading original recording",
				"app_id", session.appID, "run_id", session.runID, "error", windowErr)
		} else if windowErr == nil && rawDuration-selectedDuration >= minBrowserRecordingTrimSavings {
			trimWindowCount = len(windows)
			trimmedPath := filepath.Join(dir, recording.fileName)
			result, trimErr := m.cfg.RecordingTrimmer.Trim(ctx, browserRecordingTrimRequest{
				InputPath: rawPath, OutputPath: trimmedPath, RawDuration: rawDuration,
				Windows: recording.actionWindows, IncludeAudio: recording.recordAudio,
				PrePadding: m.cfg.RecordingTrimPrePad, PostPadding: m.cfg.RecordingTrimPostPad,
			})
			if trimErr != nil {
				trimStatus = "fallback"
				slog.WarnContext(ctx, "browser recording smart trim failed; uploading original recording",
					"app_id", session.appID, "run_id", session.runID, "error", trimErr)
			} else if result.OutputDuration <= 0 || result.OutputDuration > rawDuration ||
				result.SizeBytes <= 0 || result.SizeBytes > maxBrowserRecordingBytes {
				trimStatus = "fallback"
				slog.WarnContext(ctx, "browser recording smart trim returned invalid bounds; uploading original recording",
					"app_id", session.appID, "run_id", session.runID)
			} else {
				uploadPath = trimmedPath
				uploadSize = result.SizeBytes
				outputDuration = result.OutputDuration
				trimmedIdle = max(rawDuration-outputDuration, 0)
				trimWindowCount = result.WindowCount
				trimStatus = "trimmed"
				trimmed = true
			}
		} else if windowErr == nil {
			trimStatus = "not_needed"
			trimWindowCount = len(windows)
		}
	}
	if recording.backend == browserBackendChromium && !trimmed {
		convertedPath := filepath.Join(dir, recording.fileName)
		convertedSize, err := m.cfg.RecordingConverter.Convert(ctx, rawPath, convertedPath)
		if err != nil {
			return nil, fmt.Errorf("convert local Chromium recording to MP4: %w", err)
		}
		uploadPath = convertedPath
		uploadSize = convertedSize
	}
	asset, err := m.uploadAsset(ctx, session, browserArtifactUpload{
		Path: uploadPath, FileName: recording.fileName, ArtifactType: "browser_recording",
		ContentType: "video/mp4", Size: uploadSize, MaxBytes: maxBrowserRecordingBytes,
		Metadata: map[string]any{
			"started_at": recording.startedAt, "stopped_at": recording.stoppedAt,
			"elapsed_ms": outputDuration.Milliseconds(), "raw_duration_ms": rawDuration.Milliseconds(),
			"output_duration_ms": outputDuration.Milliseconds(), "trimmed_idle_ms": trimmedIdle.Milliseconds(),
			"smart_trimmed": trimmed, "trim_status": trimStatus, "trim_window_count": trimWindowCount,
			"max_duration_seconds": recording.maxDurationSeconds, "record_audio": recording.recordAudio,
		},
	})
	if err != nil {
		return nil, err
	}
	if recording.localDir != "" {
		_ = os.RemoveAll(recording.localDir)
	}
	session.recording = nil
	return json.Marshal(map[string]any{
		"status": "captured", "url": session.currentURL, "title": session.title,
		"artifact_type": "browser_recording", "artifact_id": asset.ArtifactID,
		"artifact_ref": asset.ArtifactRef, "visibility": asset.Visibility,
		"file_name":    firstNonEmptyString(asset.FileName, recording.fileName),
		"content_type": firstNonEmptyString(asset.ContentType, "video/mp4"),
		"format":       "mp4", "size_bytes": uploadSize,
		"started_at": recording.startedAt, "stopped_at": recording.stoppedAt,
		"elapsed_ms": outputDuration.Milliseconds(), "raw_duration_ms": rawDuration.Milliseconds(),
		"output_duration_ms": outputDuration.Milliseconds(), "trimmed_idle_ms": trimmedIdle.Milliseconds(),
		"smart_trimmed": trimmed, "trim_status": trimStatus, "trim_window_count": trimWindowCount,
		"record_audio": recording.recordAudio,
	})
}

func (m *BrowserManager) stopLocalRecordingLocked(ctx context.Context, session *browserRunSession, recording *browserRecording) error {
	if recording == nil || recording.stopped {
		return nil
	}
	if _, err := m.run(ctx, session, nil, "record", "stop"); err != nil {
		return fmt.Errorf("stop local Chromium recording: %w", err)
	}
	recording.stopped = true
	recording.stoppedAt = time.Now().UTC()
	return nil
}

func (m *BrowserManager) downloadReplay(ctx context.Context, session *browserRunSession, recording *browserRecording, path string) (int64, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<(attempt-1)) * 250 * time.Millisecond
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(wait):
			}
		}
		body, contentLength, err := m.cfg.Kernel.DownloadReplay(ctx, session.kernelSessionID, recording.replayID)
		if err != nil {
			lastErr = err
			continue
		}
		if contentLength > maxBrowserRecordingBytes {
			body.Close()
			return 0, fmt.Errorf("browser recording exceeds the 100 MB limit")
		}
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if createErr != nil {
			body.Close()
			return 0, fmt.Errorf("prepare browser recording: %w", createErr)
		}
		size, copyErr := io.Copy(file, io.LimitReader(body, maxBrowserRecordingBytes+1))
		closeErr := errors.Join(file.Close(), body.Close())
		if copyErr != nil || closeErr != nil {
			lastErr = errors.Join(copyErr, closeErr)
			continue
		}
		if size <= 0 || size > maxBrowserRecordingBytes {
			return 0, fmt.Errorf("browser recording must be between 1 byte and 100 MB")
		}
		if err := validateMP4File(path); err != nil {
			lastErr = err
			continue
		}
		return size, nil
	}
	return 0, fmt.Errorf("download browser recording after processing: %w", lastErr)
}

func validateMP4File(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil {
		return fmt.Errorf("browser recording is not a complete MP4: %w", err)
	}
	if string(header[4:8]) != "ftyp" {
		return fmt.Errorf("browser recording is not a valid MP4")
	}
	return nil
}

func beginBrowserRecordingWindow(session *browserRunSession) (*browserRecording, time.Duration) {
	if session == nil || session.recording == nil || session.recording.stopped {
		return nil, 0
	}
	recording := session.recording
	start := time.Since(recording.timelineStartedAt)
	if start < 0 {
		start = 0
	}
	return recording, start
}

func finishBrowserRecordingWindow(recording *browserRecording, start time.Duration) {
	if recording == nil || recording.stopped {
		return
	}
	end := time.Since(recording.timelineStartedAt)
	if end <= start {
		end = start + time.Millisecond
	}
	recording.actionWindows = append(recording.actionWindows, browserRecordingWindow{Start: start, End: end})
}

func browserRecordingRawDuration(recording *browserRecording) time.Duration {
	if recording == nil {
		return time.Millisecond
	}
	duration := recording.stoppedAt.Sub(recording.timelineStartedAt)
	maximum := time.Duration(recording.maxDurationSeconds) * time.Second
	if maximum > 0 && duration > maximum {
		duration = maximum
	}
	if duration <= 0 {
		return time.Millisecond
	}
	return duration
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
	args := []string{"--session", session.sessionName, "--json", "--content-boundaries", "--max-output", strconv.Itoa(m.cfg.MaxOutputChars)}
	args = append(args, extraGlobal...)
	args = append(args, command...)
	return m.cfg.Runner.Run(commandCtx, m.environment(session), args...)
}

func (m *BrowserManager) environment(session *browserRunSession) []string {
	overrides := []string{
		"AGENT_BROWSER_SESSION=" + session.sessionName,
	}
	if executable := strings.TrimSpace(m.cfg.ChromiumExecutable); executable != "" {
		overrides = append(overrides, "AGENT_BROWSER_EXECUTABLE_PATH="+executable)
	}
	if allowedDomains := agentBrowserAllowedDomains(m.cfg.AllowedDomains); allowedDomains != "" {
		overrides = append(overrides, "AGENT_BROWSER_ALLOWED_DOMAINS="+allowedDomains)
	}
	return procenv.Sanitized(overrides...)
}

func (m *BrowserManager) ensureConnectedLocked(ctx context.Context, session *browserRunSession, headless bool, browserName string) error {
	if session.connected {
		return nil
	}
	if m.cfg.Kernel == nil {
		session.backend = browserBackendChromium
		session.connected = true
		return nil
	}
	browser, err := m.cfg.Kernel.CreateBrowser(ctx, kernelBrowserCreateRequest{
		Name: browserName, Headless: headless, Stealth: m.cfg.KernelStealth,
		TimeoutSeconds: max(m.cfg.SessionTimeoutSeconds, maxBrowserReplaySeconds+browserReplayCleanupGraceSecs),
		ViewportWidth:  defaultBrowserViewportWidth, ViewportHeight: defaultBrowserViewportHeight,
	})
	if err != nil {
		if kernelCreditUnavailable(err) {
			slog.WarnContext(ctx, "Kernel browser credits are unavailable; using local Chromium",
				"app_id", session.appID, "run_id", session.runID)
			session.backend = browserBackendChromium
			session.connected = true
			return nil
		}
		return err
	}
	session.kernelSessionID = browser.SessionID
	if _, err := m.run(ctx, session, nil, "connect", browser.CDPWSURL); err != nil {
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		deleteErr := m.cfg.Kernel.DeleteBrowser(deleteCtx, browser.SessionID)
		session.kernelSessionID = ""
		return errors.Join(safeKernelOperationError("connect agent-browser to Kernel session", err), deleteErr)
	}
	session.connected = true
	session.backend = browserBackendKernel
	session.kernelHeadless = headless
	return nil
}

// upgradeToHeadfulLocked replaces a run's inexpensive headless browser only
// when native Kernel replay is explicitly requested. Cookies and web storage
// are transferred through a private temporary state file, then the current URL
// is reopened. In-memory page state and unsaved form values cannot survive the
// browser replacement.
func (m *BrowserManager) upgradeToHeadfulLocked(ctx context.Context, session *browserRunSession) error {
	if session != nil && session.backend != browserBackendKernel {
		return fmt.Errorf("browser recording requires Kernel; this run is using local Chromium")
	}
	if session == nil || !session.kernelHeadless {
		return nil
	}
	if !session.connected || strings.TrimSpace(session.kernelSessionID) == "" {
		return fmt.Errorf("Kernel browser session is not connected; call browser_open before using this tool")
	}
	dir, err := os.MkdirTemp("", "agent-runtime-browser-state-"+session.sessionName+"-")
	if err != nil {
		return fmt.Errorf("prepare headful browser upgrade: %w", err)
	}
	defer os.RemoveAll(dir)
	statePath := filepath.Join(dir, "state.json")
	if _, err := m.run(ctx, session, nil, "state", "save", statePath); err != nil {
		return fmt.Errorf("prepare headful browser upgrade: save browser state: %w", err)
	}
	currentURL := session.currentURL
	if _, err := m.run(ctx, session, nil, "close"); err != nil {
		return fmt.Errorf("prepare headful browser upgrade: close headless browser connection: %w", err)
	}
	oldSessionID := session.kernelSessionID
	session.connected = false
	session.kernelSessionID = ""
	session.viewportSet = false
	session.navigated = false
	if err := m.cfg.Kernel.DeleteBrowser(ctx, oldSessionID); err != nil {
		return fmt.Errorf("prepare headful browser upgrade: delete headless Kernel browser: %w", err)
	}
	if err := m.ensureConnectedLocked(ctx, session, false, session.sessionName+"-recording"); err != nil {
		return fmt.Errorf("prepare headful browser upgrade: %w", err)
	}
	if _, err := m.run(ctx, session, nil, "set", "viewport", strconv.Itoa(defaultBrowserViewportWidth), strconv.Itoa(defaultBrowserViewportHeight)); err != nil {
		return fmt.Errorf("prepare headful browser upgrade: set viewport: %w", err)
	}
	session.viewportSet = true
	if _, err := m.run(ctx, session, nil, "state", "load", statePath); err != nil {
		return fmt.Errorf("prepare headful browser upgrade: restore browser state: %w", err)
	}
	openOutput, err := m.run(ctx, session, nil, "open", currentURL)
	if err != nil {
		return fmt.Errorf("prepare headful browser upgrade: reopen current page: %w", err)
	}
	session.currentURL = firstNonEmptyString(browserResultString(openOutput, "url"), currentURL)
	session.title = browserResultString(openOutput, "title")
	session.navigated = isNavigatedBrowserURL(session.currentURL)
	return nil
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
	m.resetIdleTimerAfter(session, time.Duration(m.cfg.SessionTimeoutSeconds)*time.Second)
}

func (m *BrowserManager) resetIdleTimerAfter(session *browserRunSession, timeout time.Duration) {
	if session == nil || m.cfg.SessionTimeoutSeconds <= 0 {
		return
	}
	if session.idleTimer != nil {
		session.idleTimer.Stop()
	}
	appID, runID := session.appID, session.runID
	session.idleTimer = time.AfterFunc(timeout, func() {
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
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	var errs []error
	if session.recording != nil {
		recording := session.recording
		if _, err := m.finalizeRecordingLocked(closeCtx, session); err != nil {
			errs = append(errs, err)
		}
		if recording.stopTimer != nil {
			recording.stopTimer.Stop()
		}
		if recording.localDir != "" {
			_ = os.RemoveAll(recording.localDir)
		}
	}
	if session.connected {
		if _, err := m.run(closeCtx, session, nil, "close"); err != nil {
			errs = append(errs, err)
		}
	}
	if session.kernelSessionID != "" && m.cfg.Kernel != nil {
		if err := m.cfg.Kernel.DeleteBrowser(closeCtx, session.kernelSessionID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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

type browserArtifactUpload struct {
	File         *os.File
	Path         string
	FileName     string
	ArtifactType string
	ContentType  string
	Size         int64
	MaxBytes     int64
	Metadata     map[string]any
}

func (m *BrowserManager) uploadAsset(ctx context.Context, session *browserRunSession, upload browserArtifactUpload) (*browserAsset, error) {
	file := upload.File
	if file == nil {
		var err error
		file, err = os.Open(upload.Path)
		if err != nil {
			return nil, fmt.Errorf("open browser artifact: %w", err)
		}
		defer file.Close()
	}
	if upload.Size <= 0 || upload.Size > upload.MaxBytes {
		return nil, fmt.Errorf("browser artifact has an invalid size")
	}
	metadata := make(map[string]any, len(upload.Metadata)+3)
	for key, value := range upload.Metadata {
		metadata[key] = value
	}
	metadata["file_name"] = upload.FileName
	metadata["content_type"] = upload.ContentType
	metadata["size_bytes"] = upload.Size
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	reader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	contentType := multipartWriter.FormDataContentType()
	producerDone := make(chan error, 1)
	go func() {
		writeErr := writeBrowserArtifactMultipart(multipartWriter, file, session, upload, encodedMetadata)
		if closeErr := multipartWriter.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = pipeWriter.CloseWithError(writeErr)
		producerDone <- writeErr
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.ArtifactUploadURL, reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-producerDone
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if m.cfg.ArtifactUploadToken != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.ArtifactUploadToken)
	}
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		_ = reader.CloseWithError(err)
	}
	producerErr := <-producerDone
	if err != nil {
		return nil, fmt.Errorf("upload browser artifact: %w", errors.Join(err, producerErr))
	}
	if producerErr != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("upload browser artifact: %w", producerErr)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upload browser artifact: host returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	var asset browserAsset
	if err := json.Unmarshal(payload, &asset); err != nil {
		return nil, fmt.Errorf("decode browser artifact: %w", err)
	}
	if strings.TrimSpace(asset.ArtifactID) == "" || strings.TrimSpace(asset.ArtifactRef) == "" {
		return nil, fmt.Errorf("upload browser artifact: host response is missing artifact_id or artifact_ref")
	}
	if strings.TrimSpace(asset.Visibility) != "private" {
		return nil, fmt.Errorf("upload browser artifact: host response visibility must be private")
	}
	return &asset, nil
}

func writeBrowserArtifactMultipart(writer *multipart.Writer, file *os.File, session *browserRunSession, upload browserArtifactUpload, metadata []byte) error {
	for _, field := range []struct{ key, value string }{
		{key: "app_id", value: session.appID},
		{key: "run_id", value: session.runID},
		{key: "artifact_type", value: upload.ArtifactType},
		{key: "metadata", value: string(metadata)},
	} {
		if err := writer.WriteField(field.key, field.value); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile("file", upload.FileName)
	if err != nil {
		return err
	}
	written, err := io.Copy(part, io.LimitReader(file, upload.MaxBytes+1))
	if err != nil {
		return err
	}
	if written != upload.Size {
		return fmt.Errorf("browser artifact size changed while uploading")
	}
	return nil
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

func sanitizeBrowserAssetName(name, fallback string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fallback
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
		return fallback
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

func envBoolDefault(name string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
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
