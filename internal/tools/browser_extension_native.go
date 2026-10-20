package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const BrowserPolicyMetadataKey = "runtime_browser_policy"

// BrowserPolicy comes from trusted agent configuration, never tool input.
type BrowserPolicy struct {
	Backend string `json:"backend,omitempty"`
	Profile string `json:"profile,omitempty"`
}

func BrowserPolicyFromConfig(raw json.RawMessage) (BrowserPolicy, error) {
	var config struct {
		Browser BrowserPolicy `json:"browser"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &config); err != nil {
			return BrowserPolicy{}, fmt.Errorf("invalid browser execution configuration: %w", err)
		}
	}
	p := config.Browser
	if !slices.Contains([]string{"", "standard", "extension", "cloud"}, p.Backend) {
		return p, errors.New("unsupported browser backend")
	}
	if p.Profile != "" && (p.Profile == "." || p.Profile == ".." || !filepath.IsLocal(p.Profile) || strings.ContainsAny(p.Profile, "/\\")) {
		return p, errors.New("browser profile must be a single local directory name")
	}
	return p, nil
}

func (r *Registry) browserManager(appID string) *BrowserManager {
	if r == nil || r.state == nil {
		return nil
	}
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	manager := r.state.browsers[appID]
	if manager == nil {
		manager = r.state.browsers[""]
	}
	return manager
}

// FreezeBrowserPolicy replaces caller metadata with policy resolved by runtime.
func (r *Registry) FreezeBrowserPolicy(agent *agentcore.Agent, metadata map[string]interface{}) error {
	delete(metadata, BrowserPolicyMetadataKey)
	policy, err := BrowserPolicyFromConfig(agent.ExecutionConfig)
	if err != nil {
		return err
	}
	manager := r.browserManager(agent.AppID)
	if manager == nil {
		return nil
	}
	if policy.Backend == "" && manager.cfg.HostURL == "" {
		policy.Backend = "standard"
		if manager.cfg.Kernel != nil {
			policy.Backend = "cloud"
		}
	}
	metadata[BrowserPolicyMetadataKey] = policy
	return nil
}

// A paused run keeps its page state, but cannot reserve the profile from a new run.
func (r *Registry) ReleasePausedBrowser(ctx context.Context, appID, runID string) error {
	manager := r.browserManager(appID)
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	session := manager.sessions[appID+"/"+runID]
	manager.mu.Unlock()
	if session == nil || session.policy.Backend != "extension" {
		return nil
	}
	_, err := manager.nativeExtensionCommand(ctx, session, []string{"close"})
	return err
}

type BrowserStatus struct {
	Backend string `json:"backend"`
	Active  bool   `json:"active"`
	Running bool   `json:"running"`
	Held    bool   `json:"held"`
	RunID   string `json:"run_id,omitempty"`
}

func (r *Registry) BrowserControl(ctx context.Context, agent *agentcore.Agent, action string) (BrowserStatus, error) {
	manager := r.browserManager(agent.AppID)
	if manager == nil {
		return BrowserStatus{}, errors.New("browser tools are disabled for this app")
	}
	policy, err := BrowserPolicyFromConfig(agent.ExecutionConfig)
	if err != nil {
		return BrowserStatus{}, err
	}
	if policy.Backend != "extension" {
		return BrowserStatus{}, errors.New("browser controls require Extension mode")
	}
	if !slices.Contains(agent.AllowedTools, "browser_open") {
		return BrowserStatus{}, errors.New("browser access is disabled for this agent")
	}
	ext, err := manager.nativeExtension(agent.AppID, agent.ID, policy.Profile)
	if err != nil {
		return BrowserStatus{}, err
	}
	if action == "status" {
		return ext.status(), nil
	}
	ext.gate.Lock()
	defer ext.gate.Unlock()
	switch action {
	case "open", "takeover":
		if err := manager.launchNativeExtension(ext); err != nil {
			return BrowserStatus{}, err
		}
		if _, err := ext.execute(ctx, "control", []string{"get", "url"}); err != nil {
			return BrowserStatus{}, err
		}
		if err := ext.setHeld(true); err != nil {
			return BrowserStatus{}, err
		}
	case "handback":
		if err := ext.setHeld(false); err != nil {
			return BrowserStatus{}, err
		}
		ext.mu.Lock()
		if ext.runID == "" {
			manager.scheduleExtensionIdleLocked(ext)
		}
		ext.mu.Unlock()
	case "close":
		status := ext.status()
		if status.RunID != "" && !status.Held {
			return BrowserStatus{}, errors.New("take over or stop the current run before closing its browser")
		}
		if err := manager.closeNativeExtension(ctx, ext); err != nil {
			return BrowserStatus{}, err
		}
	default:
		return BrowserStatus{}, errors.New("unsupported browser control")
	}
	return ext.status(), nil
}

func (e *extensionBrowser) status() BrowserStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	active := false
	running := false
	if e.process != nil {
		select {
		case <-e.exited:
		default:
			running = true
			active = time.Since(e.seen) < 30*time.Second
		}
	}
	return BrowserStatus{Backend: "extension", Active: active, Running: running, Held: e.held, RunID: e.runID}
}

// Write the takeover state before releasing the gate, including across restart.
func (e *extensionBrowser) setHeld(held bool) error {
	raw, _ := json.Marshal(map[string]bool{"held": held})
	path := filepath.Join(e.dir, "held.json")
	if err := os.WriteFile(path+".tmp", raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.held = held
	e.epoch = rand.Text()
	return nil
}

func (m *BrowserManager) nativeExtension(appID, agentID, profileKey string) (*extensionBrowser, error) {
	if appID == "" || agentID == "" {
		return nil, errors.New("browser requires app and agent identity")
	}
	if m.cfg.AppID != "" && m.cfg.AppID != appID {
		return nil, errors.New("browser app scope mismatch")
	}
	if profileKey == "" {
		profileKey = shortBrowserHash(agentID)
	}
	if _, err := BrowserPolicyFromConfig(json.RawMessage(fmt.Sprintf(`{"browser":{"profile":%q}}`, profileKey))); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return nil, errors.New("browser manager is shutting down")
	}
	key := appID + "/" + profileKey
	if e := m.extensions[key]; e != nil {
		if e.agentID != agentID {
			return nil, errors.New("browser profile belongs to another agent")
		}
		return e, nil
	}
	if m.bridge == nil {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /extension/browser", m.extensionBridge)
		mux.HandleFunc("POST /extension/browser", m.extensionBridge)
		m.bridge = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		m.bridgeURL = "http://" + listener.Addr().String() + "/extension/browser"
		go func() { _ = m.bridge.Serve(listener) }()
	}
	root := m.cfg.ProfileRoot
	if root == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(base, "agent-runtime", "browser-profiles")
	}
	profile := filepath.Join(root, shortBrowserHash(appID), profileKey)
	if m.cfg.ProfileAppID == appID {
		profile = filepath.Join(root, profileKey)
	}
	if info, err := os.Lstat(profile); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("browser profile may not be a symbolic link")
	}
	if _, err := os.Lstat(filepath.Join(profile, "SingletonLock")); err == nil {
		return nil, errors.New("browser profile is busy; close its other browser window before opening Extension")
	}
	dir := filepath.Join(root, ".runtime-extensions", shortBrowserHash(key))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	ownerPath := filepath.Join(dir, "owner.json")
	var owner struct {
		AppID   string `json:"app_id"`
		AgentID string `json:"agent_id"`
	}
	if raw, err := os.ReadFile(ownerPath); err == nil {
		if json.Unmarshal(raw, &owner) != nil || owner.AppID != appID || owner.AgentID != agentID {
			return nil, errors.New("browser profile belongs to another agent")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	owner.AppID, owner.AgentID = appID, agentID
	raw, _ := json.Marshal(owner)
	if err := os.WriteFile(ownerPath, raw, 0600); err != nil {
		return nil, err
	}
	e := &extensionBrowser{token: rand.Text(), epoch: rand.Text(), queue: make(chan *extensionCommand), pending: map[string]*extensionCommand{}, connected: make(chan struct{}), profile: profile, dir: dir, agentID: agentID}
	if raw, err := os.ReadFile(filepath.Join(dir, "held.json")); err == nil {
		var state struct {
			Held bool `json:"held"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, fmt.Errorf("invalid browser takeover state: %w", err)
		}
		e.held = state.Held
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "page.json")); err == nil {
		var page struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		if validExtensionPage(page.URL) {
			e.page = page.URL
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	entries, err := fs.ReadDir(browserExtensionFiles, "browser_extension")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		raw, err := browserExtensionFiles.ReadFile("browser_extension/" + entry.Name())
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), raw, 0600); err != nil {
			return nil, err
		}
	}
	config, _ := json.Marshal(map[string]string{"url": m.bridgeURL, "token": e.token, "page": e.page})
	if err := os.WriteFile(filepath.Join(dir, "bridge.json"), config, 0600); err != nil {
		return nil, err
	}
	m.extensions[key] = e
	return e, nil
}

func extensionBrowserArgs(profile, page, dir string) []string {
	return []string{"--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check", "--password-store=basic", "--use-mock-keychain", "--load-extension=" + dir, "--disable-extensions-except=" + dir, "--new-window", page}
}

// BundledExtensionBrowser finds agent-browser's installed Chrome for Testing.
func BundledExtensionBrowser() string {
	home, _ := os.UserHomeDir()
	patterns := []string{
		filepath.Join(home, ".agent-browser", "browsers", "chrome-*", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
		filepath.Join(home, ".agent-browser", "browsers", "chrome-*", "chrome"),
		filepath.Join(home, ".agent-browser", "browsers", "chrome-*", "chrome-linux64", "chrome"),
		filepath.Join(home, ".agent-browser", "browsers", "chrome-*", "chrome-win64", "chrome.exe"),
		filepath.Join(home, ".agent-browser", "browsers", "chrome-*", "chrome-win32", "chrome.exe"),
	}
	var matches []string
	for _, pattern := range patterns {
		found, _ := filepath.Glob(pattern)
		matches = append(matches, found...)
	}
	slices.SortFunc(matches, compareExtensionBrowserVersions)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

func compareExtensionBrowserVersions(a, b string) int {
	version := func(path string) []string {
		for _, part := range strings.Split(path, string(filepath.Separator)) {
			if value, ok := strings.CutPrefix(part, "chrome-"); ok {
				return strings.Split(value, ".")
			}
		}
		return nil
	}
	left, right := version(a), version(b)
	for i := 0; i < len(left) || i < len(right); i++ {
		x, y := 0, 0
		if i < len(left) {
			x, _ = strconv.Atoi(left[i])
		}
		if i < len(right) {
			y, _ = strconv.Atoi(right[i])
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return strings.Compare(a, b)
}

func (m *BrowserManager) launchNativeExtension(e *extensionBrowser) error {
	m.mu.Lock()
	closing := m.closing
	m.mu.Unlock()
	if closing {
		return errors.New("browser manager is shutting down")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.idleTimer != nil {
		e.idleTimer.Stop()
	}
	if e.process != nil {
		select {
		case <-e.exited:
			e.process = nil
		default:
			return nil
		}
	}
	executable := m.cfg.ExtensionExecutable
	if executable == "" {
		executable = firstNonEmptyString(BundledExtensionBrowser(), m.cfg.ChromiumExecutable)
	}
	if executable == "" {
		return errors.New("configure AGENT_RUNTIME_BROWSER_EXTENSION_EXECUTABLE or install Chrome for Testing with agent-browser install")
	}
	if err := os.MkdirAll(e.profile, 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(e.profile, "SingletonLock")); err == nil {
		return errors.New("browser profile is busy; close its other browser window before opening Extension")
	}
	config, _ := json.Marshal(map[string]string{"url": m.bridgeURL, "token": e.token, "page": e.page})
	if err := os.WriteFile(filepath.Join(e.dir, "bridge.json"), config, 0600); err != nil {
		return err
	}
	command := exec.Command(executable, extensionBrowserArgs(e.profile, "about:blank", e.dir)...)
	e.connected, e.seen = make(chan struct{}), time.Time{}

	if err := command.Start(); err != nil {
		return err
	}
	e.process, e.exited = command, make(chan struct{})
	exited := e.exited
	go func() { _ = command.Wait(); close(exited) }()
	return nil
}

func (m *BrowserManager) nativeExtensionCommand(ctx context.Context, session *browserRunSession, command []string) (json.RawMessage, error) {
	e, err := m.nativeExtension(session.appID, session.agentID, session.policy.Profile)
	if err != nil {
		return nil, err
	}
	e.gate.Lock()
	defer e.gate.Unlock()
	e.mu.Lock()
	held, owner := e.held, e.runID
	e.mu.Unlock()
	if len(command) == 0 {
		return nil, errors.New("missing extension command")
	}
	if command[0] == "close" {
		e.mu.Lock()
		if e.runID == session.runID {
			e.runID = ""
			e.epoch = rand.Text()
			m.scheduleExtensionIdleLocked(e)
		}
		e.mu.Unlock()
		return json.RawMessage(`{"success":true,"data":{}}`), nil
	}
	if held {
		return nil, errors.New("the user holds this browser; wait until they hand it back")
	}
	if owner != "" && owner != session.runID {
		return nil, errors.New("browser profile belongs to a different run")
	}
	if !slices.Contains([]string{"open", "snapshot", "get", "click", "fill", "type", "select", "check", "uncheck", "scroll", "wait", "screenshot", "press", "hover"}, command[0]) {
		return nil, errors.New("this operation is unavailable in Extension mode; ask the user to take over")
	}
	if err := m.launchNativeExtension(e); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.runID = session.runID
	e.mu.Unlock()
	if err := e.waitConnected(ctx); err != nil {
		return nil, err
	}
	return e.execute(ctx, session.sessionName, command)
}

func (m *BrowserManager) scheduleExtensionIdleLocked(e *extensionBrowser) {
	if e.idleTimer != nil {
		e.idleTimer.Stop()
	}
	e.idleTimer = time.AfterFunc(time.Duration(m.cfg.SessionTimeoutSeconds)*time.Second, func() {
		e.gate.Lock()
		defer e.gate.Unlock()
		e.mu.Lock()
		idle := e.runID == "" && !e.held
		e.mu.Unlock()
		if idle {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_ = m.closeNativeExtension(ctx, e)
		}
	})
}

func (m *BrowserManager) closeNativeExtension(ctx context.Context, e *extensionBrowser) error {
	e.mu.Lock()
	if e.idleTimer != nil {
		e.idleTimer.Stop()
	}
	process, exited, seen := e.process, e.exited, e.seen
	e.mu.Unlock()
	if process == nil {
		return nil
	}
	select {
	case <-exited:
		return nil
	default:
	}
	var saveErr error
	if time.Since(seen) < 30*time.Second {
		closeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if raw, err := e.execute(closeCtx, "", []string{"get", "url"}); err == nil {
			var response struct {
				Success bool `json:"success"`
				Data    struct {
					URL string `json:"url"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &response) == nil && response.Success && validExtensionPage(response.Data.URL) {
				e.mu.Lock()
				e.page = response.Data.URL
				e.mu.Unlock()
				page, _ := json.Marshal(map[string]string{"url": response.Data.URL})
				if err := os.WriteFile(filepath.Join(e.dir, "page.json"), page, 0600); err != nil {
					saveErr = err
				}
			}
		}

		_, _ = e.execute(closeCtx, "", []string{"close"})
		cancel()
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		_ = process.Process.Signal(os.Interrupt)
	case <-ctx.Done():
		_ = process.Process.Signal(os.Interrupt)
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		_ = process.Process.Kill()
		<-exited
	}
	e.mu.Lock()
	e.process = nil
	e.runID = ""
	e.epoch = rand.Text()
	e.mu.Unlock()
	return saveErr
}

func (r *Registry) CloseBrowsers(ctx context.Context) error {
	if r == nil || r.state == nil {
		return nil
	}
	r.state.mu.RLock()
	managers := make([]*BrowserManager, 0, len(r.state.browsers))
	for _, manager := range r.state.browsers {
		managers = append(managers, manager)
	}
	r.state.mu.RUnlock()
	var errs []error
	for _, manager := range managers {
		manager.mu.Lock()
		manager.closing = true
		extensions := make([]*extensionBrowser, 0, len(manager.extensions))
		for _, ext := range manager.extensions {
			extensions = append(extensions, ext)
		}
		bridge := manager.bridge
		manager.mu.Unlock()
		for _, ext := range extensions {
			ext.gate.Lock()
			err := manager.closeNativeExtension(ctx, ext)
			ext.gate.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
		}
		if bridge != nil {
			_ = bridge.Close()
		}
	}
	return errors.Join(errs...)
}

func validExtensionPage(raw string) bool {
	page, err := url.Parse(raw)
	return err == nil && (page.Scheme == "http" || page.Scheme == "https") && page.Hostname() != ""
}
