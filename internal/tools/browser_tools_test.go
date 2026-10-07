package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type fakeBrowserRunner struct {
	mu         sync.Mutex
	calls      [][]string
	envs       [][]string
	pageURLs   map[string]string
	connectErr error
}

type fakeBrowserRecordingConverter struct {
	mu      sync.Mutex
	inputs  []string
	outputs []string
	payload []byte
	err     error
}

func (c *fakeBrowserRecordingConverter) Convert(_ context.Context, inputPath, outputPath string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs = append(c.inputs, inputPath)
	c.outputs = append(c.outputs, outputPath)
	if c.err != nil {
		return 0, c.err
	}
	payload := c.payload
	if len(payload) == 0 {
		payload = []byte("\x00\x00\x00\x18ftypisomlocal-fixture")
	}
	if err := os.WriteFile(outputPath, payload, 0600); err != nil {
		return 0, err
	}
	return int64(len(payload)), nil
}

type fakeKernelProvider struct {
	mu         sync.Mutex
	events     []string
	creates    []kernelBrowserCreateRequest
	createErr  error
	replayData []byte
	startedAt  time.Time
}

func (p *fakeKernelProvider) CreateBrowser(_ context.Context, request kernelBrowserCreateRequest) (*kernelBrowserSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates = append(p.creates, request)
	p.events = append(p.events, "create:"+request.Name)
	if p.createErr != nil {
		return nil, p.createErr
	}
	return &kernelBrowserSession{SessionID: "kernel-" + request.Name, CDPWSURL: "wss://kernel.test/cdp?token=secret"}, nil
}

func (p *fakeKernelProvider) DeleteBrowser(_ context.Context, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "delete:"+sessionID)
	return nil
}

func (p *fakeKernelProvider) StartReplay(_ context.Context, sessionID string, request kernelReplayStartRequest) (*kernelReplayStart, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, fmt.Sprintf("start:%s:%d:%t", sessionID, request.MaxDurationSeconds, request.RecordAudio))
	startedAt := p.startedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return &kernelReplayStart{ReplayID: "replay-1", Started: startedAt}, nil
}

func (p *fakeKernelProvider) StopReplay(_ context.Context, sessionID, replayID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "stop:"+sessionID+":"+replayID)
	return nil
}

func (p *fakeKernelProvider) DownloadReplay(_ context.Context, sessionID, replayID string) (io.ReadCloser, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "download:"+sessionID+":"+replayID)
	payload := p.replayData
	if len(payload) == 0 {
		payload = []byte("\x00\x00\x00\x18ftypisomfixture")
	}
	return io.NopCloser(bytes.NewReader(payload)), int64(len(payload)), nil
}

func (p *fakeKernelProvider) eventLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events)
}

func (p *fakeKernelProvider) createLog() []kernelBrowserCreateRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.creates)
}

func (r *fakeBrowserRunner) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
	r.envs = append(r.envs, slices.Clone(env))
	if r.pageURLs == nil {
		r.pageURLs = map[string]string{}
	}
	sessionName := envValue(env, "AGENT_BROWSER_SESSION")
	for i, arg := range args {
		switch arg {
		case "connect":
			if r.connectErr != nil {
				return nil, r.connectErr
			}
		case "open":
			if i+1 < len(args) {
				r.pageURLs[sessionName] = args[i+1]
				return []byte(`{"success":true,"data":{"url":"` + args[i+1] + `","title":"Fixture page"}}`), nil
			}
		case "get":
			if i+1 < len(args) && args[i+1] == "url" {
				return []byte(`{"success":true,"data":{"url":"` + r.pageURLs[sessionName] + `"}}`), nil
			}
			if i+1 < len(args) && args[i+1] == "title" {
				return []byte(`{"success":true,"data":{"title":"Fixture page"}}`), nil
			}
			if i+2 < len(args) && args[i+1] == "text" {
				if args[i+2] == "#long" {
					return []byte(`{"success":true,"data":{"text":"` + strings.Repeat("abcdefghij", 120) + `"}}`), nil
				}
				if args[i+2] == "#missing" {
					return []byte(`{"success":false,"data":null,"error":"Element not found."}`), nil
				}
				return []byte(`{"_boundary":{"nonce":"n1","origin":"` + r.pageURLs[sessionName] + `"},"success":true,"data":{"text":"  Mount Fuji  \n\n\n\nTallest mountain in Japan.\nÅre is in Sweden.  "}}`), nil
			}
		case "screenshot":
			if err := os.WriteFile(args[len(args)-1], []byte("\x89PNG\r\n\x1a\nfixture"), 0600); err != nil {
				return nil, err
			}
		case "record":
			if i+2 < len(args) && args[i+1] == "start" {
				if err := os.WriteFile(args[i+2], []byte("local-webm-fixture"), 0600); err != nil {
					return nil, err
				}
			}
		case "close":
			delete(r.pageURLs, sessionName)
		case "state":
			if i+2 < len(args) && args[i+1] == "save" {
				if err := os.WriteFile(args[i+2], []byte(`{"cookies":[],"origins":[]}`), 0600); err != nil {
					return nil, err
				}
			}
		}
	}
	return []byte(`{"success":true,"data":{"snapshot":"button Submit [ref=e1]"}}`), nil
}

func browserTestCallContext(runID string) CallContext {
	run := &agentcore.AgentRun{ID: runID, AppID: "helpin"}
	return CallContext{AppID: run.AppID, RunID: run.ID, Run: run}
}

func browserTestCallContextForApp(appID, runID string) CallContext {
	run := &agentcore.AgentRun{ID: runID, AppID: appID}
	return CallContext{AppID: appID, RunID: run.ID, Run: run}
}

func TestBrowserActIsARoutineMutation(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, KernelAPIKey: "key", Kernel: &fakeKernelProvider{}, AllowedDomains: []string{"*"}, Runner: &fakeBrowserRunner{},
	})
	def, ok := registry.Definition("browser_act")
	if !ok {
		t.Fatal("browser_act is not registered")
	}
	if !def.Mutating {
		t.Fatal("browser_act must remain classified as mutating")
	}
	if got := def.EffectiveRiskLevel(); got != RiskLevelRoutine {
		t.Fatalf("browser_act risk level = %q, want %q", got, RiskLevelRoutine)
	}
}

func TestBrowserConfigDefaultsKernelToHeadless(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_BROWSER_ENABLED", "true")
	t.Setenv("KERNEL_API_KEY", "key")
	t.Setenv("KERNEL_HEADLESS", "")
	if cfg := BrowserToolsConfigFromEnv(); !cfg.KernelHeadless {
		t.Fatal("Kernel browsers must default to headless")
	}
	t.Setenv("KERNEL_HEADLESS", "false")
	if cfg := BrowserToolsConfigFromEnv(); cfg.KernelHeadless {
		t.Fatal("KERNEL_HEADLESS=false must remain an explicit headful override")
	}
}

func TestBrowserConfigUsesLocalChromiumWithoutKernelKey(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_BROWSER_ENABLED", "true")
	t.Setenv("KERNEL_API_KEY", "")
	t.Setenv("AGENT_RUNTIME_BROWSER_CHROMIUM_EXECUTABLE", "/usr/bin/chromium")
	t.Setenv("AGENT_RUNTIME_BROWSER_CHROMIUM_ARGS", "--no-sandbox,--disable-dev-shm-usage")

	cfg := BrowserToolsConfigFromEnv()
	if !cfg.Enabled || cfg.Kernel != nil {
		t.Fatalf("browser config = %#v, want enabled local Chromium without Kernel", cfg)
	}
	if cfg.ChromiumExecutable != "/usr/bin/chromium" {
		t.Fatalf("Chromium executable = %q", cfg.ChromiumExecutable)
	}
	if cfg.ChromiumArgs != "--no-sandbox,--disable-dev-shm-usage" {
		t.Fatalf("Chromium args = %q", cfg.ChromiumArgs)
	}
}

func TestBrowserOpenUsesLocalChromiumWithoutKernel(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", AllowedDomains: []string{"example.com"},
		ChromiumExecutable: "/usr/bin/chromium",
		ChromiumArgs:       "--no-sandbox,--disable-dev-shm-usage",
		Runner:             runner,
	})

	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-local"), "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("calls=%d, want viewport, open, snapshot, URL, and title without a Kernel connect", len(runner.calls))
	}
	for _, call := range runner.calls {
		if slices.Contains(call, "connect") {
			t.Fatalf("local Chromium unexpectedly connected to Kernel: %#v", call)
		}
	}
	if got := envValue(runner.envs[0], "AGENT_BROWSER_EXECUTABLE_PATH"); got != "/usr/bin/chromium" {
		t.Fatalf("Chromium executable env = %q", got)
	}
	for _, call := range runner.calls {
		if !slices.Contains(call, "--args") || !slices.Contains(call, "--no-sandbox,--disable-dev-shm-usage") {
			t.Fatalf("local Chromium args were not forwarded: %#v", call)
		}
	}
}

func TestBrowserOpenDoesNotApplyLocalChromiumArgsToKernelConnection(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, KernelAPIKey: "key", Kernel: &fakeKernelProvider{},
		ChromiumArgs: "--no-sandbox", AllowedDomains: []string{"example.com"}, Runner: runner,
	})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-kernel"), "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	for _, call := range runner.calls {
		if slices.Contains(call, "--args") || slices.Contains(call, "--no-sandbox") {
			t.Fatalf("local Chromium args leaked into Kernel command: %#v", call)
		}
	}
}

func TestBrowserOpenFallsBackToLocalChromiumWhenKernelCreditsAreUnavailable(t *testing.T) {
	runner := &fakeBrowserRunner{}
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"fallback-recording","artifact_ref":"helpin://artifacts/fallback-recording","visibility":"private","file_name":"credit-fallback.mp4","content_type":"video/mp4"}`))
	}))
	defer uploader.Close()
	kernelProvider := &fakeKernelProvider{createErr: &kernelOperationError{
		operation: "create Kernel browser", statusCode: http.StatusPaymentRequired, creditUnavailable: true,
	}}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", Kernel: kernelProvider,
		AllowedDomains: []string{"example.com"}, Runner: runner, ArtifactUploadURL: uploader.URL,
		DisableRecordingTrim: true, RecordingConverter: &fakeBrowserRecordingConverter{},
	})

	callCtx := browserTestCallContext("run-fallback")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if len(kernelProvider.createLog()) != 1 {
		t.Fatalf("Kernel create attempts = %d, want 1", len(kernelProvider.createLog()))
	}
	for _, call := range runner.calls {
		if slices.Contains(call, "connect") {
			t.Fatalf("credit fallback unexpectedly connected to Kernel: %#v", call)
		}
	}
	if _, ok := registry.Definition("browser_record"); !ok {
		t.Fatal("credit fallback is missing browser_record")
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"start","name":"credit-fallback","max_duration_seconds":10}`)); err != nil {
		t.Fatalf("start credit-fallback recording: %v", err)
	}
	stopped, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"stop"}`))
	if err != nil || !strings.Contains(string(stopped), "helpin://artifacts/fallback-recording") {
		t.Fatalf("stop credit-fallback recording = %s, %v", stopped, err)
	}
}

func TestBrowserRecordUsesLocalChromiumAndUploadsPrivateMP4(t *testing.T) {
	var uploaded []byte
	var metadata string
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxBrowserRecordingBytes); err != nil {
			t.Fatalf("parse upload: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer file.Close()
		uploaded, _ = io.ReadAll(file)
		metadata = r.FormValue("metadata")
		if header.Filename != "local-flow.mp4" || r.FormValue("artifact_type") != "browser_recording" {
			t.Fatalf("unexpected recording envelope: file=%q type=%q", header.Filename, r.FormValue("artifact_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"local-recording-1","artifact_ref":"helpin://artifacts/local-recording-1","visibility":"private","file_name":"local-flow.mp4","content_type":"video/mp4"}`))
	}))
	defer uploader.Close()

	runner := &fakeBrowserRunner{}
	converter := &fakeBrowserRecordingConverter{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", AllowedDomains: []string{"example.com"}, Runner: runner,
		ArtifactUploadURL: uploader.URL, DisableRecordingTrim: true, RecordingConverter: converter,
	})
	callCtx := browserTestCallContext("run-local-recording")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	started, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"start","name":"Local flow","max_duration_seconds":10}`))
	if err != nil || !strings.Contains(string(started), `"status":"recording"`) {
		t.Fatalf("start local recording = %s, %v", started, err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_act", json.RawMessage(`{"action":"scroll","value":"down","amount":100}`)); err != nil {
		t.Fatalf("browser_act: %v", err)
	}
	stopped, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"stop"}`))
	if err != nil {
		t.Fatalf("stop local recording: %v", err)
	}
	if !strings.HasPrefix(string(uploaded[4:]), "ftyp") || !strings.Contains(string(stopped), "helpin://artifacts/local-recording-1") {
		t.Fatalf("unexpected local recording output=%s uploaded=%q", stopped, uploaded)
	}
	if !strings.Contains(metadata, `"record_audio":false`) || !strings.Contains(metadata, `"content_type":"video/mp4"`) {
		t.Fatalf("unexpected local recording metadata: %s", metadata)
	}
	converter.mu.Lock()
	convertCalls := len(converter.inputs)
	converter.mu.Unlock()
	if convertCalls != 1 {
		t.Fatalf("local recording conversions=%d, want 1", convertCalls)
	}
	var recordStart, recordStop bool
	for _, call := range runner.calls {
		recordStart = recordStart || slices.Contains(call, "start") && slices.Contains(call, "record")
		recordStop = recordStop || slices.Contains(call, "stop") && slices.Contains(call, "record")
	}
	if !recordStart || !recordStop {
		t.Fatalf("local recording calls=%#v", runner.calls)
	}
}

func TestBrowserOpenDoesNotHideUnrelatedKernelFailure(t *testing.T) {
	runner := &fakeBrowserRunner{}
	wantErr := &kernelOperationError{operation: "create Kernel browser", statusCode: http.StatusUnauthorized}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", Kernel: &fakeKernelProvider{createErr: wantErr},
		AllowedDomains: []string{"example.com"}, Runner: runner,
	})

	_, err := registry.Execute(context.Background(), browserTestCallContext("run-error"), "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`))
	if !errors.Is(err, wantErr) {
		t.Fatalf("browser_open error = %v, want unrelated Kernel error", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unrelated Kernel failure launched Chromium: %#v", runner.calls)
	}
}

func TestBrowserOpenUsesEphemeralRunSessionAndBoundedSnapshot(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://must-not-leak")
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "kernel-secret",
		Kernel: &fakeKernelProvider{}, AllowedDomains: []string{"stage.example.com"}, Runner: runner,
	})
	out, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://stage.example.com/app"}`))
	if err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if !strings.Contains(string(out), "snapshot") || !strings.Contains(string(out), `"url":"https://stage.example.com/app"`) || !strings.Contains(string(out), `"title":"Fixture page"`) {
		t.Fatalf("unexpected output: %s", out)
	}
	if len(runner.calls) != 7 {
		t.Fatalf("calls=%d, want connect, viewport, open, settle wait, snapshot, URL, and title", len(runner.calls))
	}
	if got := runner.calls[1][len(runner.calls[1])-4:]; !slices.Equal(got, []string{"set", "viewport", "1440", "900"}) {
		t.Fatalf("default browser viewport call = %#v", got)
	}
	if got := runner.calls[3][len(runner.calls[3])-2:]; !slices.Equal(got, []string{"wait", "1500"}) {
		t.Fatalf("default browser settle call = %#v", got)
	}
	joinedEnv := strings.Join(runner.envs[0], "\n")
	if !strings.Contains(joinedEnv, "AGENT_BROWSER_SESSION=ar-") {
		t.Fatalf("missing agent-browser run env: %s", joinedEnv)
	}
	if strings.Contains(joinedEnv, "KERNEL_API_KEY=") || strings.Contains(joinedEnv, "kernel-secret") {
		t.Fatalf("Kernel API key leaked to browser subprocess: %s", joinedEnv)
	}
	if !strings.Contains(joinedEnv, "AGENT_BROWSER_ALLOWED_DOMAINS=stage.example.com") {
		t.Fatalf("explicit app domain policy was not forwarded: %s", joinedEnv)
	}
	if strings.Contains(joinedEnv, "KERNEL_PROFILE_NAME=") {
		t.Fatalf("persistent Kernel profile unexpectedly configured: %s", joinedEnv)
	}
	if strings.Contains(joinedEnv, "must-not-leak") {
		t.Fatalf("runtime secret leaked to browser subprocess: %s", joinedEnv)
	}
	if err := registry.CloseRun(context.Background(), "helpin", "run-1"); err != nil {
		t.Fatalf("close run: %v", err)
	}
	if got := runner.calls[len(runner.calls)-1][len(runner.calls[len(runner.calls)-1])-1]; got != "close" {
		t.Fatalf("last command=%q, want close", got)
	}
}

func TestBrowserOpenRejectsUnknownAndDisallowedDomain(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, KernelAPIKey: "key", AllowedDomains: []string{"example.com"}, Runner: &fakeBrowserRunner{}})
	callCtx := browserTestCallContext("run-1")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","extra":true}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected strict decode error, got %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://evil.example.net"}`)); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected domain error, got %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":10001}`)); err == nil || !strings.Contains(err.Error(), "wait_ms") {
		t.Fatalf("expected wait_ms validation error, got %v", err)
	}
}

func TestBrowserOpenRedactsKernelCDPCredentialsFromConnectionErrors(t *testing.T) {
	runner := &fakeBrowserRunner{connectErr: fmt.Errorf("dial wss://kernel.test/cdp?token=secret failed")}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, KernelAPIKey: "key", Kernel: &fakeKernelProvider{},
		AllowedDomains: []string{"example.com"}, Runner: runner,
	})
	_, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://example.com"}`))
	if err == nil || !strings.Contains(err.Error(), "connect agent-browser to Kernel session failed") {
		t.Fatalf("expected safe connection error, got %v", err)
	}
	if strings.Contains(err.Error(), "token=") || strings.Contains(err.Error(), "kernel.test") {
		t.Fatalf("Kernel CDP credentials leaked through error: %v", err)
	}
}

func TestBrowserOpenSetsDefaultViewportOncePerSession(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key",
		Kernel: &fakeKernelProvider{}, AllowedDomains: []string{"example.com"}, Runner: runner,
	})
	callCtx := browserTestCallContext("run-1")
	for _, target := range []string{"https://example.com/one", "https://example.com/two"} {
		input := json.RawMessage(`{"url":"` + target + `","wait_ms":0}`)
		if _, err := registry.Execute(context.Background(), callCtx, "browser_open", input); err != nil {
			t.Fatalf("browser_open %s: %v", target, err)
		}
	}
	viewportCalls := 0
	for _, call := range runner.calls {
		if len(call) >= 4 && slices.Equal(call[len(call)-4:], []string{"set", "viewport", "1440", "900"}) {
			viewportCalls++
		}
	}
	if viewportCalls != 1 {
		t.Fatalf("default viewport calls=%d, want 1 per session", viewportCalls)
	}
}

func TestBrowserOpenAllowsAllDomainsWhenAppPolicyUsesWildcard(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, AppID: "helpin", KernelAPIKey: "key", Kernel: &fakeKernelProvider{}, AllowedDomains: []string{"*"}, Runner: runner})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://arbitrary.example.net/login"}`)); err != nil {
		t.Fatalf("wildcard browser policy rejected URL: %v", err)
	}
	if allowedDomains := envValue(runner.envs[0], "AGENT_BROWSER_ALLOWED_DOMAINS"); allowedDomains != "" {
		t.Fatalf("wildcard must omit agent-browser's literal allowlist, got %q", allowedDomains)
	}
}

func TestBrowserSessionsAreIsolatedPerRun(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, KernelAPIKey: "key", Kernel: &fakeKernelProvider{}, AllowedDomains: []string{"example.com"}, Runner: runner})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://example.com/one","wait_ms":0}`)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-2"), "browser_open", json.RawMessage(`{"url":"https://example.com/two","wait_ms":0}`)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	sessionOne := envValue(runner.envs[0], "AGENT_BROWSER_SESSION")
	sessionTwo := envValue(runner.envs[6], "AGENT_BROWSER_SESSION")
	if sessionOne == "" || sessionTwo == "" || sessionOne == sessionTwo {
		t.Fatalf("run sessions must be non-empty and isolated: run-1=%q run-2=%q", sessionOne, sessionTwo)
	}
}

func TestBrowserSessionClosesAfterIdleTimeout(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"example.com"},
		Kernel: &fakeKernelProvider{}, SessionTimeoutSeconds: 1, CommandTimeout: time.Second, Runner: runner,
	})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("open: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		closed := len(runner.calls) > 1 && runner.calls[len(runner.calls)-1][len(runner.calls[len(runner.calls)-1])-1] == "close"
		runner.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("browser session was not closed after idle timeout")
}

func TestBrowserRecordUsesKernelReplayAndUploadsPrivateMP4(t *testing.T) {
	var uploaded []byte
	var metadata string
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxBrowserRecordingBytes); err != nil {
			t.Fatalf("parse upload: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer file.Close()
		uploaded, _ = io.ReadAll(file)
		metadata = r.FormValue("metadata")
		if header.Filename != "login-flow.mp4" || r.FormValue("artifact_type") != "browser_recording" {
			t.Fatalf("unexpected recording envelope: file=%q type=%q", header.Filename, r.FormValue("artifact_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"recording-1","artifact_ref":"helpin://artifacts/recording-1","visibility":"private","file_name":"login-flow.mp4","content_type":"video/mp4","size_bytes":23}`))
	}))
	defer uploader.Close()

	kernelProvider := &fakeKernelProvider{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", Kernel: kernelProvider,
		AllowedDomains: []string{"example.com"}, Runner: &fakeBrowserRunner{}, ArtifactUploadURL: uploader.URL,
		KernelHeadless: true,
	})
	callCtx := browserTestCallContext("run-1")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com/login","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	started, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"start","name":"Login flow","max_duration_seconds":90,"record_audio":true}`))
	if err != nil {
		t.Fatalf("start recording: %v", err)
	}
	if !strings.Contains(string(started), `"status":"recording"`) || strings.Contains(string(started), "replay-1") || strings.Contains(string(started), "kernel-") || strings.Contains(string(started), "token=") {
		t.Fatalf("unexpected start output: %s", started)
	}
	stopped, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"stop"}`))
	if err != nil {
		t.Fatalf("stop recording: %v", err)
	}
	if !strings.HasPrefix(string(uploaded[4:]), "ftyp") || !strings.Contains(string(stopped), "helpin://artifacts/recording-1") || strings.Contains(string(stopped), "replay-1") || strings.Contains(string(stopped), "kernel-") {
		t.Fatalf("unexpected captured recording output=%s uploaded=%q", stopped, uploaded)
	}
	if !strings.Contains(metadata, `"max_duration_seconds":90`) || !strings.Contains(metadata, `"record_audio":true`) || !strings.Contains(metadata, `"content_type":"video/mp4"`) || strings.Contains(metadata, "replay-1") || strings.Contains(metadata, "kernel-") {
		t.Fatalf("unexpected recording metadata: %s", metadata)
	}
	wantEvents := []string{
		"create:ar-" + shortBrowserHash("helpin/run-1"),
		"delete:kernel-ar-" + shortBrowserHash("helpin/run-1"),
		"create:ar-" + shortBrowserHash("helpin/run-1") + "-recording",
		"start:kernel-ar-" + shortBrowserHash("helpin/run-1") + "-recording:90:true",
		"stop:kernel-ar-" + shortBrowserHash("helpin/run-1") + "-recording:replay-1",
		"download:kernel-ar-" + shortBrowserHash("helpin/run-1") + "-recording:replay-1",
	}
	if got := kernelProvider.eventLog(); !slices.Equal(got, wantEvents) {
		t.Fatalf("Kernel replay events=%#v, want %#v", got, wantEvents)
	}
	creates := kernelProvider.createLog()
	if len(creates) != 2 || !creates[0].Headless || creates[1].Headless {
		t.Fatalf("recording must promote headless to headful exactly once: %#v", creates)
	}
}

func TestBrowserRecordSmartTrimsAgentReasoningGaps(t *testing.T) {
	var uploaded []byte
	var metadata string
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxBrowserRecordingBytes); err != nil {
			t.Fatalf("parse upload: %v", err)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer file.Close()
		uploaded, _ = io.ReadAll(file)
		metadata = r.FormValue("metadata")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"recording-1","artifact_ref":"helpin://artifacts/recording-1","visibility":"private","file_name":"demo.mp4","content_type":"video/mp4"}`))
	}))
	defer uploader.Close()

	trimmer := &fakeBrowserRecordingTrimmer{payload: []byte("\x00\x00\x00\x18ftypisomtrimmed-demo")}
	kernelProvider := &fakeKernelProvider{startedAt: time.Now().UTC().Add(-20 * time.Second)}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", Kernel: kernelProvider,
		AllowedDomains: []string{"example.com"}, Runner: &fakeBrowserRunner{}, ArtifactUploadURL: uploader.URL,
		RecordingTrimmer: trimmer,
	})
	callCtx := browserTestCallContext("run-1")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"start","name":"demo"}`)); err != nil {
		t.Fatalf("start recording: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_act", json.RawMessage(`{"action":"click","ref":"@e1"}`)); err != nil {
		t.Fatalf("browser_act: %v", err)
	}
	stopped, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"stop"}`))
	if err != nil {
		t.Fatalf("stop recording: %v", err)
	}
	if len(trimmer.requests) != 1 || len(trimmer.requests[0].Windows) != 1 {
		t.Fatalf("trim requests = %#v, want one action window", trimmer.requests)
	}
	if !strings.Contains(string(uploaded), "trimmed-demo") {
		t.Fatalf("uploaded recording was not trimmed: %q", uploaded)
	}
	if !strings.Contains(metadata, `"smart_trimmed":true`) || !strings.Contains(metadata, `"trim_status":"trimmed"`) || !strings.Contains(metadata, `"trim_window_count":1`) {
		t.Fatalf("unexpected trim metadata: %s", metadata)
	}
	if !strings.Contains(string(stopped), `"smart_trimmed":true`) || !strings.Contains(string(stopped), `"output_duration_ms":2000`) {
		t.Fatalf("unexpected trim output: %s", stopped)
	}
}

func TestBrowserRecordFallsBackToOriginalWhenSmartTrimFails(t *testing.T) {
	var uploaded []byte
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxBrowserRecordingBytes); err != nil {
			t.Fatalf("parse upload: %v", err)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer file.Close()
		uploaded, _ = io.ReadAll(file)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"recording-1","artifact_ref":"helpin://artifacts/recording-1","visibility":"private","content_type":"video/mp4"}`))
	}))
	defer uploader.Close()

	rawRecording := []byte("\x00\x00\x00\x18ftypisomoriginal-demo")
	trimmer := &fakeBrowserRecordingTrimmer{err: fmt.Errorf("encoder unavailable")}
	kernelProvider := &fakeKernelProvider{startedAt: time.Now().UTC().Add(-20 * time.Second), replayData: rawRecording}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", Kernel: kernelProvider,
		AllowedDomains: []string{"example.com"}, Runner: &fakeBrowserRunner{}, ArtifactUploadURL: uploader.URL,
		RecordingTrimmer: trimmer,
	})
	callCtx := browserTestCallContext("run-1")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"start"}`)); err != nil {
		t.Fatalf("start recording: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_act", json.RawMessage(`{"action":"click","ref":"@e1"}`)); err != nil {
		t.Fatalf("browser_act: %v", err)
	}
	stopped, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"stop"}`))
	if err != nil {
		t.Fatalf("stop recording: %v", err)
	}
	if !slices.Equal(uploaded, rawRecording) {
		t.Fatalf("uploaded recording = %q, want original %q", uploaded, rawRecording)
	}
	if !strings.Contains(string(stopped), `"smart_trimmed":false`) || !strings.Contains(string(stopped), `"trim_status":"fallback"`) {
		t.Fatalf("unexpected fallback output: %s", stopped)
	}
}

func TestBrowserRecordIsFinalizedBeforeKernelSessionDeletion(t *testing.T) {
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxBrowserRecordingBytes); err != nil {
			t.Fatalf("parse upload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"recording-1","artifact_ref":"helpin://artifacts/recording-1","visibility":"private","content_type":"video/mp4"}`))
	}))
	defer uploader.Close()

	kernelProvider := &fakeKernelProvider{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", Kernel: kernelProvider,
		AllowedDomains: []string{"example.com"}, Runner: &fakeBrowserRunner{}, ArtifactUploadURL: uploader.URL,
	})
	callCtx := browserTestCallContext("run-1")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"start"}`)); err != nil {
		t.Fatalf("start recording: %v", err)
	}
	if err := registry.CloseRun(context.Background(), "helpin", "run-1"); err != nil {
		t.Fatalf("close run: %v", err)
	}
	events := kernelProvider.eventLog()
	if len(events) != 5 || !strings.HasPrefix(events[2], "stop:") || !strings.HasPrefix(events[3], "download:") || !strings.HasPrefix(events[4], "delete:") {
		t.Fatalf("recording must stop and download before Kernel browser deletion: %#v", events)
	}
}

func TestBrowserScreenshotUploadsWithoutReturningImageBytes(t *testing.T) {
	var uploaded []byte
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxBrowserScreenshotBytes); err != nil {
			t.Fatalf("parse upload: %v", err)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		defer file.Close()
		uploaded, _ = io.ReadAll(file)
		if r.FormValue("app_id") != "helpin" || r.FormValue("run_id") != "run-1" || r.FormValue("artifact_type") != "browser_screenshot" {
			t.Fatalf("unexpected artifact envelope: app=%q run=%q type=%q", r.FormValue("app_id"), r.FormValue("run_id"), r.FormValue("artifact_type"))
		}
		if r.FormValue("workspace_id") != "" {
			t.Fatalf("host-specific workspace_id leaked into generic artifact contract")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"asset-1","artifact_ref":"helpin://artifacts/asset-1","visibility":"private","file_name":"settings-page.png","content_type":"image/png","size_bytes":16}`))
	}))
	defer uploader.Close()
	registry := NewRegistry()
	runner := &fakeBrowserRunner{}
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"example.com"},
		Kernel: &fakeKernelProvider{}, Runner: runner, ArtifactUploadURL: uploader.URL,
	})
	if _, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_open", json.RawMessage(`{"url":"https://example.com/settings","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	out, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_screenshot", json.RawMessage(`{"name":"Settings page","annotate":true,"full_page":true}`))
	if err != nil {
		t.Fatalf("browser_screenshot: %v", err)
	}
	if len(uploaded) == 0 {
		t.Fatal("screenshot was not uploaded")
	}
	if strings.Contains(string(out), "iVBOR") || !strings.Contains(string(out), "helpin://artifacts/asset-1") || strings.Contains(string(out), "object_key") || !strings.Contains(string(out), `"url":"https://example.com/settings"`) || !strings.Contains(string(out), `"full_page":true`) {
		t.Fatalf("unexpected screenshot output: %s", out)
	}
}

func TestBrowserScreenshotRejectsFreshBlankSession(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"*"},
		Runner: runner, ArtifactUploadURL: "https://host.test/artifacts",
	})
	_, err := registry.Execute(context.Background(), browserTestCallContext("run-1"), "browser_screenshot", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "call browser_open") {
		t.Fatalf("expected unopened-page repair error, got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("blank session executed browser commands: %#v", runner.calls)
	}
}

func TestBrowserScreenshotOnlyRegisteredWithAppArtifactProvider(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry.ForApp("helpin"), BrowserToolsConfig{Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"*"}, Runner: &fakeBrowserRunner{}})
	defs := registry.CloneForApp("helpin").Definitions()
	for _, def := range defs {
		if def.Name == "browser_screenshot" {
			t.Fatal("browser_screenshot registered without an app artifact provider")
		}
	}
}

func TestBrowserToolsRejectAnotherAppContext(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry.ForApp("helpin"), BrowserToolsConfig{Enabled: true, AppID: "helpin", KernelAPIKey: "key", AllowedDomains: []string{"*"}, Runner: &fakeBrowserRunner{}})
	run := &agentcore.AgentRun{ID: "run-1", AppID: "usermaven"}
	_, err := registry.CloneForApp("helpin").Execute(context.Background(), CallContext{AppID: "usermaven", RunID: run.ID, Run: run}, "browser_snapshot", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "not configured for app") {
		t.Fatalf("expected cross-app rejection, got %v", err)
	}
}

func TestBrowserSessionsAndPoliciesAreNamespacedPerApp(t *testing.T) {
	registry := NewRegistry()
	runnerA := &fakeBrowserRunner{}
	runnerB := &fakeBrowserRunner{}
	RegisterBrowserTools(registry.ForApp("app-a"), BrowserToolsConfig{
		Enabled: true, AppID: "app-a", KernelAPIKey: "key",
		Kernel: &fakeKernelProvider{}, AllowedDomains: []string{"a.example.com"}, Runner: runnerA,
	})
	RegisterBrowserTools(registry.ForApp("app-b"), BrowserToolsConfig{
		Enabled: true, AppID: "app-b", KernelAPIKey: "key",
		Kernel: &fakeKernelProvider{}, AllowedDomains: []string{"b.example.com"}, Runner: runnerB,
	})
	if _, err := registry.CloneForApp("app-a").Execute(context.Background(), browserTestCallContextForApp("app-a", "shared-run"), "browser_open", json.RawMessage(`{"url":"https://a.example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("app-a snapshot: %v", err)
	}
	if _, err := registry.CloneForApp("app-b").Execute(context.Background(), browserTestCallContextForApp("app-b", "shared-run"), "browser_open", json.RawMessage(`{"url":"https://b.example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("app-b snapshot: %v", err)
	}
	sessionA := envValue(runnerA.envs[0], "AGENT_BROWSER_SESSION")
	sessionB := envValue(runnerB.envs[0], "AGENT_BROWSER_SESSION")
	if sessionA == "" || sessionB == "" || sessionA == sessionB {
		t.Fatalf("sessions must be non-empty and app-isolated: app-a=%q app-b=%q", sessionA, sessionB)
	}
	if _, err := registry.CloneForApp("app-a").Execute(context.Background(), browserTestCallContextForApp("app-a", "run-c"), "browser_open", json.RawMessage(`{"url":"https://b.example.com"}`)); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("app-a accepted app-b domain policy: %v", err)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, value := range env {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return ""
}

func TestBrowserReadPagesThroughPageText(t *testing.T) {
	runner := &fakeBrowserRunner{}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, KernelAPIKey: "key", Kernel: &fakeKernelProvider{}, AllowedDomains: []string{"*"}, Runner: runner, MaxOutputChars: 8000})
	callCtx := browserTestCallContext("run-read")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_read", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "browser_open") {
		t.Fatalf("expected an open page to be required, got %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com/fuji"}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}

	out, err := registry.Execute(context.Background(), callCtx, "browser_read", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("browser_read: %v", err)
	}
	var whole struct {
		URL        string          `json:"url"`
		Title      string          `json:"title"`
		Selector   string          `json:"selector"`
		Text       string          `json:"text"`
		Offset     int             `json:"offset"`
		Total      int             `json:"total_chars"`
		NextOffset *int            `json:"next_offset"`
		Boundary   json.RawMessage `json:"_boundary"`
	}
	if err := json.Unmarshal(out, &whole); err != nil {
		t.Fatal(err)
	}
	want := "Mount Fuji\n\nTallest mountain in Japan.\nÅre is in Sweden."
	if whole.Text != want || whole.Total != len([]rune(want)) || whole.NextOffset != nil || whole.Selector != "body" || whole.URL != "https://example.com/fuji" || whole.Title != "Fixture page" || len(whole.Boundary) == 0 {
		t.Fatalf("unexpected read: %s", out)
	}
	read := runner.calls[len(runner.calls)-3]
	if !slices.Equal(read[len(read)-3:], []string{"get", "text", "body"}) || !slices.Contains(read, strconv.Itoa(browserReadSourceLimit)) {
		t.Fatalf("read command = %#v", read)
	}

	// Parts: a small limit, then continue from next_offset.
	out, err = registry.Execute(context.Background(), callCtx, "browser_read", json.RawMessage(`{"selector":"@e3","max_chars":500,"offset":40}`))
	if err != nil {
		t.Fatalf("browser_read part: %v", err)
	}
	var part struct {
		Text       string `json:"text"`
		Offset     int    `json:"offset"`
		NextOffset *int   `json:"next_offset"`
	}
	_ = json.Unmarshal(out, &part)
	if part.Offset != 40 || part.Text != string([]rune(want)[40:]) || part.NextOffset != nil {
		t.Fatalf("unexpected part: %s", out)
	}

	if _, err := registry.Execute(context.Background(), callCtx, "browser_read", json.RawMessage(`{"selector":"#missing"}`)); err == nil || !strings.Contains(err.Error(), "Element not found") {
		t.Fatalf("expected the browser's error, got %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_read", json.RawMessage(`{"offset":-1}`)); err == nil {
		t.Fatal("expected a negative offset to be rejected")
	}
}

func TestBrowserReadNextOffset(t *testing.T) {
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{Enabled: true, KernelAPIKey: "key", Kernel: &fakeKernelProvider{}, AllowedDomains: []string{"*"}, Runner: &fakeBrowserRunner{}, MaxOutputChars: 8000})
	callCtx := browserTestCallContext("run-long")
	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com/long"}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	var parts []string
	offset := 0
	for range 5 {
		out, err := registry.Execute(context.Background(), callCtx, "browser_read", json.RawMessage(`{"selector":"#long","max_chars":500,"offset":`+strconv.Itoa(offset)+`}`))
		if err != nil {
			t.Fatalf("browser_read: %v", err)
		}
		var part struct {
			Text       string `json:"text"`
			Total      int    `json:"total_chars"`
			NextOffset *int   `json:"next_offset"`
		}
		_ = json.Unmarshal(out, &part)
		parts = append(parts, part.Text)
		if part.Total != 1200 {
			t.Fatalf("total = %d", part.Total)
		}
		if part.NextOffset == nil {
			break
		}
		offset = *part.NextOffset
	}
	if len(parts) != 3 || strings.Join(parts, "") != strings.Repeat("abcdefghij", 120) {
		t.Fatalf("parts = %d, joined length %d", len(parts), len(strings.Join(parts, "")))
	}
}

func TestTidyBrowserText(t *testing.T) {
	if got := tidyBrowserText(" a \r\n\r\n\r\n b "); got != "a\n\nb" {
		t.Fatalf("tidy = %q", got)
	}
}
