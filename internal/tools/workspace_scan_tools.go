package tools

// Security scanner tools ported from the Helpin worker (tools_security.go).
// They wrap the semgrep, trivy, and gitleaks CLIs, run them against the
// workspace lease root, and return normalized, filtered, paginated findings.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
)

const (
	toolNameScanSemgrep  = "scan_semgrep"
	toolNameScanTrivy    = "scan_trivy"
	toolNameScanGitleaks = "scan_gitleaks"

	secscanCommandTimeout     = 10 * time.Minute
	secscanBoundedMaxRunes    = 30_000
	secscanDefaultSeedCache   = "/app/.cache"
	secscanDefaultSemgrepDir  = "/app/security-rules/semgrep"
	secscanSemgrepRulesEnvVar = "AGENT_RUNTIME_SEMGREP_RULES_DIR"
	secscanSeedCacheEnvVar    = "AGENT_RUNTIME_SECURITY_SEED_CACHE_DIR"
)

// RegisterWorkspaceScanTools registers the security scanner tools that run
// CLI scanners against the current run's workspace lease root.
func RegisterWorkspaceScanTools(r *Registry) {
	if r == nil {
		return
	}
	pack := newSecscanToolPack()
	for _, item := range []struct {
		def     Definition
		handler Handler
	}{
		{secscanToolDefinition(toolNameScanSemgrep, "Run Semgrep against the checked-out repository and return normalized SAST findings. Scanner output is parsed and compacted server-side."), pack.scanSemgrep},
		{secscanToolDefinition(toolNameScanTrivy, "Run Trivy filesystem scanning against the checked-out repository and return normalized dependency, misconfiguration, and secret findings."), pack.scanTrivy},
		{secscanToolDefinition(toolNameScanGitleaks, "Run Gitleaks against the checked-out repository and return normalized secret findings with raw secret values redacted."), pack.scanGitleaks},
	} {
		r.Register(item.def, item.handler)
	}
}

func secscanToolDefinition(name, description string) Definition {
	return Definition{
		Name:        name,
		Description: description,
		Category:    "Security",
		InputSchema: secscanToolSchema(name),
		Mutating:    false,
	}
}

func secscanToolSchema(scanner string) map[string]interface{} {
	props := map[string]interface{}{
		"scan_paths": map[string]interface{}{
			"type":        "array",
			"description": "Repository-relative paths to scan. Defaults to the whole repository.",
			"items":       map[string]interface{}{"type": "string"},
		},
		"exclude_paths": map[string]interface{}{
			"type":        "array",
			"description": "Repository-relative path prefixes to exclude from returned findings.",
			"items":       map[string]interface{}{"type": "string"},
		},
		"severity_threshold": map[string]interface{}{
			"type":        "string",
			"description": "Minimum normalized severity to return.",
			"enum":        []string{"critical", "high", "medium", "low", "info"},
		},
		"max_findings": map[string]interface{}{
			"type":        "integer",
			"description": "Legacy maximum findings to return after filtering. Prefer page/page_size.",
		},
		"page": map[string]interface{}{
			"type":        "integer",
			"description": "1-based result page after filtering and sorting. Defaults to 1.",
		},
		"page_size": map[string]interface{}{
			"type":        "integer",
			"description": "Maximum findings to return on this page. Defaults to 100, max 200.",
		},
		"summary_only": map[string]interface{}{
			"type":        "boolean",
			"description": "When true, return counts and groups without full findings.",
		},
		"detail_level": map[string]interface{}{
			"type":        "string",
			"description": "Finding detail shape. Use index for compact triage rows, full for verbose scanner details.",
			"enum":        []string{"full", "index"},
		},
		"category": map[string]interface{}{
			"type":        "string",
			"description": "Optional normalized category filter.",
			"enum":        []string{"dependency", "sast", "secret", "misconfig"},
		},
		"rule_ids": map[string]interface{}{
			"type":        "array",
			"description": "Optional scanner rule IDs to include.",
			"items":       map[string]interface{}{"type": "string"},
		},
		"package_names": map[string]interface{}{
			"type":        "array",
			"description": "Optional dependency package names to include.",
			"items":       map[string]interface{}{"type": "string"},
		},
		"vulnerability_ids": map[string]interface{}{
			"type":        "array",
			"description": "Optional CVE/GHSA/OSV vulnerability IDs to include.",
			"items":       map[string]interface{}{"type": "string"},
		},
		"paths": map[string]interface{}{
			"type":        "array",
			"description": "Optional exact or prefix repository paths to include after scanning.",
			"items":       map[string]interface{}{"type": "string"},
		},
		"include_low_info": map[string]interface{}{
			"type":        "boolean",
			"description": "Whether low and informational findings may be returned.",
		},
	}
	switch scanner {
	case toolNameScanSemgrep:
		props["config"] = map[string]interface{}{"type": "string", "description": "Semgrep config path or registry config. Defaults to the bundled rules directory."}
		props["fallback_to_auto_config"] = map[string]interface{}{"type": "boolean", "description": "Fallback to Semgrep auto config when bundled rules are unavailable. Defaults to true."}
	case toolNameScanTrivy:
		props["scanners"] = map[string]interface{}{
			"type":        "array",
			"description": "Trivy scanner kinds to run.",
			"items":       map[string]interface{}{"type": "string", "enum": []string{"vuln", "misconfig", "secret"}},
		}
	case toolNameScanGitleaks:
		props["scan_git_history"] = map[string]interface{}{"type": "boolean", "description": "Run a full git-history scan. Defaults to false."}
	}
	return map[string]interface{}{
		"type":                 "object",
		"properties":           props,
		"required":             []string{},
		"additionalProperties": false,
	}
}

type secscanRequest struct {
	ScanPaths            []string `json:"scan_paths"`
	ExcludePaths         []string `json:"exclude_paths"`
	SeverityThreshold    string   `json:"severity_threshold"`
	MaxFindings          int      `json:"max_findings"`
	Page                 int      `json:"page"`
	PageSize             int      `json:"page_size"`
	SummaryOnly          bool     `json:"summary_only"`
	Category             string   `json:"category"`
	RuleIDs              []string `json:"rule_ids"`
	PackageNames         []string `json:"package_names"`
	VulnerabilityIDs     []string `json:"vulnerability_ids"`
	Paths                []string `json:"paths"`
	IncludeLowInfo       bool     `json:"include_low_info"`
	Config               string   `json:"config"`
	FallbackToAutoConfig bool     `json:"fallback_to_auto_config"`
	Scanners             []string `json:"scanners"`
	ScanGitHistory       bool     `json:"scan_git_history"`
	DetailLevel          string   `json:"detail_level"`

	legacyMaxFindings bool
}

// SecurityScanSummary counts findings per normalized severity.
type SecurityScanSummary struct {
	Total    int `json:"total"`
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Info     int `json:"info"`
}

// SecurityScanFinding is a normalized finding shared across scanners.
type SecurityScanFinding struct {
	Scanner          string                 `json:"scanner"`
	Category         string                 `json:"category"`
	Severity         string                 `json:"severity"`
	RuleID           string                 `json:"rule_id"`
	Title            string                 `json:"title"`
	Message          string                 `json:"message"`
	Path             string                 `json:"path"`
	StartLine        *int                   `json:"start_line,omitempty"`
	EndLine          *int                   `json:"end_line,omitempty"`
	PackageName      *string                `json:"package_name,omitempty"`
	InstalledVersion *string                `json:"installed_version,omitempty"`
	FixedVersion     *string                `json:"fixed_version,omitempty"`
	VulnerabilityID  *string                `json:"vulnerability_id,omitempty"`
	CWEIDs           []string               `json:"cwe_ids,omitempty"`
	CVSSScore        *float64               `json:"cvss_score,omitempty"`
	References       []string               `json:"references,omitempty"`
	Fingerprint      string                 `json:"fingerprint"`
	Raw              map[string]interface{} `json:"raw,omitempty"`
}

// SecurityScannerResult is the JSON tool result envelope.
type SecurityScannerResult struct {
	Scanner                 string                 `json:"scanner"`
	Summary                 SecurityScanSummary    `json:"summary"`
	SummaryBeforePagination SecurityScanSummary    `json:"summary_before_pagination"`
	TotalFindings           int                    `json:"total_findings"`
	ReturnedFindings        int                    `json:"returned_findings"`
	Page                    int                    `json:"page"`
	PageSize                int                    `json:"page_size"`
	HasMore                 bool                   `json:"has_more"`
	ScanID                  string                 `json:"scan_id,omitempty"`
	CacheHit                bool                   `json:"cache_hit"`
	SummaryOnly             bool                   `json:"summary_only,omitempty"`
	DetailLevel             string                 `json:"detail_level,omitempty"`
	Bounded                 bool                   `json:"bounded,omitempty"`
	Groups                  []SecurityFindingGroup `json:"groups,omitempty"`
	Findings                []SecurityScanFinding  `json:"findings"`
	Warnings                []string               `json:"warnings,omitempty"`
}

// SecurityFindingGroup aggregates filtered findings along one dimension.
type SecurityFindingGroup struct {
	Kind            string              `json:"kind"`
	Key             string              `json:"key"`
	Category        string              `json:"category,omitempty"`
	Severity        string              `json:"severity,omitempty"`
	RuleID          string              `json:"rule_id,omitempty"`
	PackageName     string              `json:"package_name,omitempty"`
	VulnerabilityID string              `json:"vulnerability_id,omitempty"`
	Path            string              `json:"path,omitempty"`
	Count           int                 `json:"count"`
	Summary         SecurityScanSummary `json:"summary"`
}

type secscanCacheEntry struct {
	ScanID    string
	Scanner   string
	Findings  []SecurityScanFinding
	Warnings  []string
	CreatedAt time.Time
}

type secscanToolPack struct {
	mu     sync.Mutex
	caches map[string]map[string]secscanCacheEntry
}

func newSecscanToolPack() *secscanToolPack {
	return &secscanToolPack{caches: map[string]map[string]secscanCacheEntry{}}
}

// secscanCommandRunner is swappable in tests.
var secscanCommandRunner = runSecscanCommand

// secscanLookPath is swappable in tests to simulate missing binaries.
var secscanLookPath = exec.LookPath

func (p *secscanToolPack) scanSemgrep(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	root, err := requireWorkspaceRoot(callCtx, toolNameScanSemgrep)
	if err != nil {
		return nil, err
	}
	req, err := parseSecscanRequest(input)
	if err != nil {
		return nil, err
	}
	if unavailable := secscanBinaryUnavailableResult("semgrep"); unavailable != nil {
		return unavailable, nil
	}
	if req.Config == "" {
		req.Config = secscanSemgrepRulesDir()
	}
	if !secscanJSONFieldPresent(input, "fallback_to_auto_config") {
		req.FallbackToAutoConfig = true
	}
	env, warnings := secscanEnv()
	if req.Config != "auto" && !secscanSemgrepConfigAvailable(req.Config) {
		warnings = append(warnings, fmt.Sprintf("semgrep config %q is unavailable or empty; using --config auto", req.Config))
		req.Config = "auto"
	}
	cacheKey, scanID := secscanCacheKey(root, "semgrep", req)
	if entry, ok := p.getCacheEntry(callCtx, cacheKey); ok {
		return marshalSecscanCachedResult("semgrep", entry.ScanID, true, entry.Findings, req, append(warnings, entry.Warnings...))
	}
	args := []string{"scan", "--config", req.Config, "--json"}
	for _, exclude := range req.ExcludePaths {
		args = append(args, "--exclude", exclude)
	}
	args = append(args, req.ScanPaths...)

	stdout, stderr, commandErr := secscanCommandRunner(ctx, root, "semgrep", args, env)
	warnings = append(warnings, secscanCommandWarnings("semgrep", stderr, commandErr)...)
	if commandErr != nil && req.FallbackToAutoConfig && req.Config != "auto" {
		fallbackArgs := []string{"scan", "--config", "auto", "--json"}
		for _, exclude := range req.ExcludePaths {
			fallbackArgs = append(fallbackArgs, "--exclude", exclude)
		}
		fallbackArgs = append(fallbackArgs, req.ScanPaths...)
		stdout, stderr, commandErr = secscanCommandRunner(ctx, root, "semgrep", fallbackArgs, env)
		warnings = append(warnings, "semgrep bundled config failed; retried with --config auto")
		warnings = append(warnings, secscanCommandWarnings("semgrep fallback", stderr, commandErr)...)
	}
	if commandErr != nil && strings.TrimSpace(stdout) == "" {
		warnings = append(warnings, "semgrep produced no JSON output; returning zero findings")
		return marshalSecscanResult("semgrep", nil, req, warnings)
	}
	findings, parseWarnings, err := parseSemgrepFindings([]byte(stdout))
	if err != nil {
		if commandErr != nil {
			warnings = append(warnings, secscanParseFailureWarnings("semgrep", stdout, err)...)
			return marshalSecscanResult("semgrep", nil, req, warnings)
		}
		return nil, err
	}
	warnings = append(warnings, parseWarnings...)
	p.putCacheEntry(callCtx, cacheKey, secscanCacheEntry{
		ScanID:    scanID,
		Scanner:   "semgrep",
		Findings:  findings,
		Warnings:  append([]string(nil), warnings...),
		CreatedAt: time.Now().UTC(),
	})
	return marshalSecscanCachedResult("semgrep", scanID, false, findings, req, warnings)
}

func (p *secscanToolPack) scanTrivy(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	root, err := requireWorkspaceRoot(callCtx, toolNameScanTrivy)
	if err != nil {
		return nil, err
	}
	req, err := parseSecscanRequest(input)
	if err != nil {
		return nil, err
	}
	if len(req.Scanners) == 0 {
		req.Scanners = []string{"vuln", "misconfig", "secret"}
	}
	for _, scanner := range req.Scanners {
		switch scanner {
		case "vuln", "misconfig", "secret":
		default:
			return nil, fmt.Errorf("unsupported trivy scanner %q", scanner)
		}
	}
	if unavailable := secscanBinaryUnavailableResult("trivy"); unavailable != nil {
		return unavailable, nil
	}
	cacheKey, scanID := secscanCacheKey(root, "trivy", req)
	if entry, ok := p.getCacheEntry(callCtx, cacheKey); ok {
		return marshalSecscanCachedResult("trivy", entry.ScanID, true, entry.Findings, req, entry.Warnings)
	}
	args := []string{
		"fs",
		"--cache-dir", filepath.Join(secscanRuntimeCacheDir(), "trivy"),
		"--format", "json",
		"--skip-version-check",
		"--scanners", strings.Join(req.Scanners, ","),
		"--severity", "LOW,MEDIUM,HIGH,CRITICAL",
	}
	warnings := prepareSecscanTrivyCache()
	env, envWarnings := secscanEnv()
	warnings = append(warnings, envWarnings...)
	args = append(args, req.ScanPaths...)
	stdout, stderr, commandErr := secscanCommandRunner(ctx, root, "trivy", args, env)
	warnings = append(warnings, secscanCommandWarnings("trivy", stderr, commandErr)...)
	if commandErr != nil && strings.TrimSpace(stdout) == "" {
		warnings = append(warnings, "trivy produced no JSON output; returning zero findings")
		return marshalSecscanResult("trivy", nil, req, warnings)
	}
	findings, parseWarnings, err := parseTrivyFindings([]byte(stdout))
	if err != nil {
		if commandErr != nil {
			warnings = append(warnings, secscanParseFailureWarnings("trivy", stdout, err)...)
			return marshalSecscanResult("trivy", nil, req, warnings)
		}
		return nil, err
	}
	warnings = append(warnings, parseWarnings...)
	p.putCacheEntry(callCtx, cacheKey, secscanCacheEntry{
		ScanID:    scanID,
		Scanner:   "trivy",
		Findings:  findings,
		Warnings:  append([]string(nil), warnings...),
		CreatedAt: time.Now().UTC(),
	})
	return marshalSecscanCachedResult("trivy", scanID, false, findings, req, warnings)
}

func (p *secscanToolPack) scanGitleaks(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	root, err := requireWorkspaceRoot(callCtx, toolNameScanGitleaks)
	if err != nil {
		return nil, err
	}
	req, err := parseSecscanRequest(input)
	if err != nil {
		return nil, err
	}
	if !secscanJSONFieldPresent(input, "severity_threshold") {
		req.SeverityThreshold = "high"
	}
	if unavailable := secscanBinaryUnavailableResult("gitleaks"); unavailable != nil {
		return unavailable, nil
	}
	cacheKey, scanID := secscanCacheKey(root, "gitleaks", req)
	if entry, ok := p.getCacheEntry(callCtx, cacheKey); ok {
		return marshalSecscanCachedResult("gitleaks", entry.ScanID, true, entry.Findings, req, entry.Warnings)
	}
	args := gitleaksCommandArgs(req)
	env, warnings := secscanEnv()
	stdout, stderr, commandErr := secscanCommandRunner(ctx, root, "gitleaks", args, env)
	warnings = append(warnings, secscanCommandWarnings("gitleaks", stderr, commandErr)...)
	if !req.ScanGitHistory {
		warnings = append(warnings, "gitleaks ran against the working tree only; git history was not scanned")
	}
	if commandErr != nil && strings.TrimSpace(stdout) == "" {
		warnings = append(warnings, "gitleaks produced no JSON output; returning zero findings")
		return marshalSecscanResult("gitleaks", nil, req, warnings)
	}
	findings, parseWarnings, err := parseGitleaksFindings([]byte(stdout))
	if err != nil {
		if commandErr != nil {
			warnings = append(warnings, secscanParseFailureWarnings("gitleaks", stdout, err)...)
			return marshalSecscanResult("gitleaks", nil, req, warnings)
		}
		return nil, err
	}
	warnings = append(warnings, parseWarnings...)
	p.putCacheEntry(callCtx, cacheKey, secscanCacheEntry{
		ScanID:    scanID,
		Scanner:   "gitleaks",
		Findings:  findings,
		Warnings:  append([]string(nil), warnings...),
		CreatedAt: time.Now().UTC(),
	})
	return marshalSecscanCachedResult("gitleaks", scanID, false, findings, req, warnings)
}

func gitleaksCommandArgs(req secscanRequest) []string {
	args := []string{"detect", "--source", ".", "--report-format", "json", "--report-path", "-", "--redact", "--exit-code", "0"}
	if !req.ScanGitHistory {
		args = append(args, "--no-git")
	}
	return args
}

// secscanBinaryUnavailableResult returns a graceful tool-result payload when
// the scanner binary is not installed, so agents get a clear message instead
// of an opaque execution error.
func secscanBinaryUnavailableResult(program string) json.RawMessage {
	if _, err := secscanLookPath(program); err == nil {
		return nil
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"scanner":  program,
		"status":   "unavailable",
		"error":    fmt.Sprintf("%s binary is not installed in this runtime; install it or use another scanner", program),
		"findings": []SecurityScanFinding{},
	})
	return payload
}

func (p *secscanToolPack) cacheScope(callCtx CallContext) string {
	key := callCtx.AppID + "/" + callCtx.RunID
	if key == "/" && callCtx.Run != nil {
		key = callCtx.Run.AppID + "/" + callCtx.Run.ID
	}
	return key
}

func (p *secscanToolPack) getCacheEntry(callCtx CallContext, key string) (secscanCacheEntry, bool) {
	if p == nil || strings.TrimSpace(key) == "" {
		return secscanCacheEntry{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	scope := p.caches[p.cacheScope(callCtx)]
	if scope == nil {
		return secscanCacheEntry{}, false
	}
	entry, ok := scope[key]
	return entry, ok
}

func (p *secscanToolPack) putCacheEntry(callCtx CallContext, key string, entry secscanCacheEntry) {
	if p == nil || strings.TrimSpace(key) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	scopeKey := p.cacheScope(callCtx)
	if p.caches[scopeKey] == nil {
		p.caches[scopeKey] = map[string]secscanCacheEntry{}
	}
	p.caches[scopeKey][key] = entry
}

func secscanCacheKey(root, scanner string, req secscanRequest) (string, string) {
	cacheInput := map[string]interface{}{
		"scanner":                 strings.TrimSpace(scanner),
		"work_dir":                strings.TrimSpace(root),
		"scan_paths":              secscanSortedStrings(req.ScanPaths),
		"exclude_paths":           secscanSortedStrings(req.ExcludePaths),
		"config":                  strings.TrimSpace(req.Config),
		"fallback_to_auto_config": req.FallbackToAutoConfig,
		"scanners":                secscanSortedStrings(req.Scanners),
		"scan_git_history":        req.ScanGitHistory,
	}
	payload, err := json.Marshal(cacheInput)
	if err != nil {
		return "", ""
	}
	sum := sha256.Sum256(payload)
	key := hex.EncodeToString(sum[:])
	return key, key[:16]
}

func secscanSortedStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	sort.Strings(out)
	return out
}

func parseSecscanRequest(input json.RawMessage) (secscanRequest, error) {
	var req secscanRequest
	if len(strings.TrimSpace(string(input))) > 0 {
		if err := json.Unmarshal(input, &req); err != nil {
			return req, fmt.Errorf("parse input: %w", err)
		}
	}
	if len(req.ScanPaths) == 0 {
		req.ScanPaths = []string{"."}
	}
	for idx, path := range req.ScanPaths {
		clean, err := secscanCleanRelativePath(path)
		if err != nil {
			return req, fmt.Errorf("scan_paths[%d]: %w", idx, err)
		}
		req.ScanPaths[idx] = clean
	}
	for idx, path := range req.ExcludePaths {
		clean, err := secscanCleanRelativePath(path)
		if err != nil {
			return req, fmt.Errorf("exclude_paths[%d]: %w", idx, err)
		}
		if clean == "." {
			return req, fmt.Errorf("exclude_paths[%d] cannot exclude the entire repository", idx)
		}
		req.ExcludePaths[idx] = clean
	}
	req.SeverityThreshold = normalizeSecscanSeverity(req.SeverityThreshold)
	if req.SeverityThreshold == "" {
		req.SeverityThreshold = "medium"
	}
	rawCategory := req.Category
	req.Category = normalizeSecscanCategory(req.Category)
	if req.Category == "" && strings.TrimSpace(rawCategory) != "" {
		return req, fmt.Errorf("category must be one of dependency, sast, secret, or misconfig")
	}
	for idx, path := range req.Paths {
		clean, err := secscanCleanRelativePath(path)
		if err != nil {
			return req, fmt.Errorf("paths[%d]: %w", idx, err)
		}
		req.Paths[idx] = clean
	}
	req.RuleIDs = secscanNormalizeFilters(req.RuleIDs)
	req.PackageNames = secscanNormalizeFilters(req.PackageNames)
	req.VulnerabilityIDs = secscanNormalizeFilters(req.VulnerabilityIDs)
	req.DetailLevel = strings.ToLower(strings.TrimSpace(req.DetailLevel))
	if req.DetailLevel == "" {
		req.DetailLevel = "full"
	}
	if req.DetailLevel != "full" && req.DetailLevel != "index" {
		return req, fmt.Errorf("detail_level must be full or index")
	}
	if req.Page <= 0 {
		req.Page = 1
	}
	if secscanJSONFieldPresent(input, "max_findings") && !secscanJSONFieldPresent(input, "page_size") && !secscanJSONFieldPresent(input, "page") {
		req.legacyMaxFindings = true
		if req.MaxFindings <= 0 {
			req.MaxFindings = 200
		}
		if req.MaxFindings > 1000 {
			req.MaxFindings = 1000
		}
		req.PageSize = req.MaxFindings
	} else {
		if req.PageSize <= 0 {
			req.PageSize = 100
		}
		if req.PageSize > 200 {
			req.PageSize = 200
		}
	}
	return req, nil
}

func secscanJSONFieldPresent(input json.RawMessage, field string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input, &raw); err != nil {
		return false
	}
	_, ok := raw[field]
	return ok
}

func runSecscanCommand(ctx context.Context, root, program string, args, env []string) (string, string, error) {
	if strings.TrimSpace(root) == "" {
		return "", "", fmt.Errorf("scanner requires a checked-out repository workspace")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cmdCtx, cancel := context.WithTimeout(ctx, secscanCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, program, args...)
	cmd.Dir = root
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = mergeSecscanEnv(procenv.Sanitized(), env)
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func secscanRuntimeCacheDir() string {
	return filepath.Join(os.TempDir(), "agent-runtime-security-cache")
}

func secscanSemgrepRulesDir() string {
	if dir := strings.TrimSpace(os.Getenv(secscanSemgrepRulesEnvVar)); dir != "" {
		return dir
	}
	return secscanDefaultSemgrepDir
}

func secscanSeedCacheDir() string {
	if dir := strings.TrimSpace(os.Getenv(secscanSeedCacheEnvVar)); dir != "" {
		return dir
	}
	return secscanDefaultSeedCache
}

func secscanEnv() ([]string, []string) {
	var warnings []string
	cacheDir := secscanRuntimeCacheDir()
	semgrepCache := filepath.Join(cacheDir, "semgrep")
	if err := os.MkdirAll(semgrepCache, 0o755); err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to create semgrep runtime cache: %v", err))
	}
	homeDir := filepath.Join(cacheDir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to create scanner home directory: %v", err))
	}
	env := []string{
		"HOME=" + homeDir,
		"XDG_CACHE_HOME=" + cacheDir,
		"SEMGREP_SETTINGS_FILE=" + filepath.Join(semgrepCache, "settings.yml"),
		"TRIVY_CACHE_DIR=" + filepath.Join(cacheDir, "trivy"),
	}
	return env, warnings
}

func mergeSecscanEnv(base, overrides []string) []string {
	merged := append([]string(nil), base...)
	for _, entry := range overrides {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		merged = secscanUpsertEnv(merged, key, value)
	}
	return merged
}

func secscanUpsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for idx, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[idx] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func prepareSecscanTrivyCache() []string {
	var warnings []string
	runtimeDir := filepath.Join(secscanRuntimeCacheDir(), "trivy")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return []string{fmt.Sprintf("failed to create trivy runtime cache: %v", err)}
	}
	if secscanDirectoryHasEntries(runtimeDir) {
		return warnings
	}
	seedDir := filepath.Join(secscanSeedCacheDir(), "trivy")
	if !secscanDirectoryHasEntries(seedDir) {
		return warnings
	}
	if err := secscanCopyDirectoryContents(seedDir, runtimeDir); err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to seed trivy runtime cache from image cache: %v", err))
	}
	return warnings
}

func secscanDirectoryHasEntries(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

func secscanCopyDirectoryContents(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func secscanCommandWarnings(label, stderr string, err error) []string {
	var warnings []string
	if trimmed := strings.TrimSpace(stderr); trimmed != "" {
		warnings = append(warnings, fmt.Sprintf("%s stderr: %s", label, secscanTruncate(trimmed, 1000)))
	}
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("%s exited with %v; parser will use JSON output if available", label, err))
	}
	return warnings
}

func secscanParseFailureWarnings(label, stdout string, err error) []string {
	var warnings []string
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("%s JSON output could not be parsed: %v", label, err))
	}
	if strings.TrimSpace(stdout) != "" {
		warnings = append(warnings, fmt.Sprintf("%s stdout was omitted because scanner parse failures may include sensitive findings", label))
	}
	return warnings
}

func secscanSemgrepConfigAvailable(config string) bool {
	config = strings.TrimSpace(config)
	if config == "" || config == "auto" || strings.Contains(config, "://") {
		return true
	}
	if !filepath.IsAbs(config) && !strings.HasPrefix(config, ".") {
		return true
	}
	info, err := os.Stat(config)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return true
	}
	return secscanDirectoryHasEntries(config)
}

func marshalSecscanResult(scanner string, findings []SecurityScanFinding, req secscanRequest, warnings []string) (json.RawMessage, error) {
	return marshalSecscanCachedResult(scanner, "", false, findings, req, warnings)
}

func marshalSecscanCachedResult(scanner, scanID string, cacheHit bool, findings []SecurityScanFinding, req secscanRequest, warnings []string) (json.RawMessage, error) {
	filtered, filterWarnings := filterSecscanFindings(findings, req)
	warnings = append(warnings, filterWarnings...)
	pageFindings, page, pageSize, hasMore := paginateSecscanFindings(filtered, req)
	returnedFindings := pageFindings
	if req.SummaryOnly {
		returnedFindings = []SecurityScanFinding{}
	} else if req.DetailLevel == "index" {
		returnedFindings = compactSecscanFindings(returnedFindings)
	}
	summary := summarizeSecscanFindings(filtered)
	result := SecurityScannerResult{
		Scanner:                 scanner,
		Summary:                 summary,
		SummaryBeforePagination: summary,
		TotalFindings:           len(filtered),
		ReturnedFindings:        len(returnedFindings),
		Page:                    page,
		PageSize:                pageSize,
		HasMore:                 hasMore,
		ScanID:                  scanID,
		CacheHit:                cacheHit,
		SummaryOnly:             req.SummaryOnly,
		DetailLevel:             req.DetailLevel,
		Findings:                returnedFindings,
		Warnings:                secscanDedupeStrings(warnings),
	}
	if req.SummaryOnly || req.DetailLevel == "full" {
		result.Groups = summarizeSecscanFindingGroups(filtered)
	}
	if req.DetailLevel == "index" && !req.SummaryOnly {
		return marshalBoundedSecscanIndexResult(result)
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal scanner result: %w", err)
	}
	return out, nil
}

func compactSecscanFindings(findings []SecurityScanFinding) []SecurityScanFinding {
	out := make([]SecurityScanFinding, 0, len(findings))
	for _, finding := range findings {
		finding.Title = secscanTruncate(strings.TrimSpace(finding.Title), 237)
		finding.Message = ""
		finding.References = nil
		finding.CWEIDs = nil
		finding.Raw = nil
		out = append(out, finding)
	}
	return out
}

func marshalBoundedSecscanIndexResult(result SecurityScannerResult) (json.RawMessage, error) {
	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal scanner result: %w", err)
	}
	if len([]rune(string(out))) <= secscanBoundedMaxRunes {
		return out, nil
	}

	originalFindings := result.Findings
	result.Bounded = true
	result.HasMore = true
	result.Warnings = secscanDedupeStrings(append(result.Warnings, "response_bounded; narrow by category/package/CVE/rule/path or request next page"))
	low, high, best := 0, len(originalFindings), 0
	for low <= high {
		count := low + (high-low)/2
		result.Findings = originalFindings[:count]
		result.ReturnedFindings = count
		out, err = json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("marshal scanner result: %w", err)
		}
		if len([]rune(string(out))) <= secscanBoundedMaxRunes {
			best = count
			low = count + 1
		} else {
			high = count - 1
		}
	}
	result.Findings = originalFindings[:best]
	result.ReturnedFindings = best
	out, err = json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal scanner result: %w", err)
	}
	return out, nil
}

func filterSecscanFindings(findings []SecurityScanFinding, req secscanRequest) ([]SecurityScanFinding, []string) {
	var warnings []string
	excludes := req.ExcludePaths
	deduped := make([]SecurityScanFinding, 0, len(findings))
	seen := make(map[string]bool, len(findings))
	for _, finding := range findings {
		finding.Severity = normalizeSecscanSeverity(finding.Severity)
		if finding.Severity == "" {
			finding.Severity = "info"
		}
		finding.Category = normalizeSecscanCategory(finding.Category)
		finding.Path = normalizeSecscanOutputPath(finding.Path)
		if !secscanPathIncluded(finding.Path, req.ScanPaths) {
			continue
		}
		if secscanPathExcluded(finding.Path, excludes) {
			continue
		}
		if req.Category != "" && normalizeSecscanCategory(finding.Category) != req.Category {
			continue
		}
		if len(req.Paths) > 0 && !secscanPathIncluded(finding.Path, req.Paths) {
			continue
		}
		if len(req.RuleIDs) > 0 && !secscanStringInFoldedSet(finding.RuleID, req.RuleIDs) {
			continue
		}
		if len(req.PackageNames) > 0 && !secscanStringPtrInFoldedSet(finding.PackageName, req.PackageNames) {
			continue
		}
		if len(req.VulnerabilityIDs) > 0 && !secscanStringPtrInFoldedSet(finding.VulnerabilityID, req.VulnerabilityIDs) {
			continue
		}
		if !req.IncludeLowInfo && (finding.Severity == "low" || finding.Severity == "info") {
			continue
		}
		if secscanSeverityRank(finding.Severity) < secscanSeverityRank(req.SeverityThreshold) {
			continue
		}
		key := strings.Join([]string{finding.Scanner, finding.Category, finding.RuleID, finding.Path, secscanIntPtrString(finding.StartLine), finding.Fingerprint}, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, finding)
	}
	sort.SliceStable(deduped, func(i, j int) bool {
		a, b := deduped[i], deduped[j]
		if secscanSeverityRank(a.Severity) != secscanSeverityRank(b.Severity) {
			return secscanSeverityRank(a.Severity) > secscanSeverityRank(b.Severity)
		}
		for _, cmp := range [][2]string{{a.Scanner, b.Scanner}, {a.Path, b.Path}, {a.RuleID, b.RuleID}, {a.Fingerprint, b.Fingerprint}} {
			if cmp[0] != cmp[1] {
				return cmp[0] < cmp[1]
			}
		}
		return secscanIntPtrValue(a.StartLine) < secscanIntPtrValue(b.StartLine)
	})
	if req.legacyMaxFindings && len(deduped) > req.MaxFindings {
		warnings = append(warnings, fmt.Sprintf("findings truncated from %d to max_findings=%d", len(deduped), req.MaxFindings))
		deduped = deduped[:req.MaxFindings]
	}
	return deduped, warnings
}

func summarizeSecscanFindings(findings []SecurityScanFinding) SecurityScanSummary {
	var summary SecurityScanSummary
	for _, finding := range findings {
		summary.Total++
		switch normalizeSecscanSeverity(finding.Severity) {
		case "critical":
			summary.Critical++
		case "high":
			summary.High++
		case "medium":
			summary.Medium++
		case "low":
			summary.Low++
		default:
			summary.Info++
		}
	}
	return summary
}

func paginateSecscanFindings(findings []SecurityScanFinding, req secscanRequest) ([]SecurityScanFinding, int, int, bool) {
	page := req.Page
	if page <= 0 {
		page = 1
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	start := (page - 1) * pageSize
	if start >= len(findings) {
		return []SecurityScanFinding{}, page, pageSize, false
	}
	end := start + pageSize
	if end > len(findings) {
		end = len(findings)
	}
	return findings[start:end], page, pageSize, end < len(findings)
}

func summarizeSecscanFindingGroups(findings []SecurityScanFinding) []SecurityFindingGroup {
	type groupAccumulator struct {
		group    SecurityFindingGroup
		findings []SecurityScanFinding
	}
	groups := make(map[string]*groupAccumulator)
	add := func(kind, key string, finding SecurityScanFinding) {
		if strings.TrimSpace(key) == "" {
			return
		}
		mapKey := kind + "\x00" + key
		acc := groups[mapKey]
		if acc == nil {
			acc = &groupAccumulator{group: SecurityFindingGroup{
				Kind:            kind,
				Key:             key,
				Category:        normalizeSecscanCategory(finding.Category),
				Severity:        finding.Severity,
				RuleID:          finding.RuleID,
				PackageName:     secscanDerefString(finding.PackageName),
				VulnerabilityID: secscanDerefString(finding.VulnerabilityID),
				Path:            finding.Path,
			}}
			groups[mapKey] = acc
		}
		acc.findings = append(acc.findings, finding)
		acc.group.Count++
		if secscanSeverityRank(finding.Severity) > secscanSeverityRank(acc.group.Severity) {
			acc.group.Severity = finding.Severity
		}
	}
	for _, finding := range findings {
		add("severity", finding.Severity, finding)
		add("category", normalizeSecscanCategory(finding.Category), finding)
		add("rule", finding.RuleID, finding)
		if finding.PackageName != nil {
			add("package", *finding.PackageName, finding)
		}
		if finding.VulnerabilityID != nil {
			add("vulnerability", *finding.VulnerabilityID, finding)
		}
		add("path", finding.Path, finding)
	}
	out := make([]SecurityFindingGroup, 0, len(groups))
	for _, acc := range groups {
		acc.group.Summary = summarizeSecscanFindings(acc.findings)
		out = append(out, acc.group)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func secscanCleanRelativePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == "." {
		return ".", nil
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("must be relative to the repository root")
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("must stay within the repository")
	}
	return clean, nil
}

func normalizeSecscanOutputPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "." {
		return path
	}
	return strings.TrimPrefix(path, "./")
}

func secscanPathIncluded(path string, includes []string) bool {
	if len(includes) == 0 {
		return true
	}
	path = normalizeSecscanOutputPath(path)
	for _, include := range includes {
		include = normalizeSecscanOutputPath(include)
		if include == "." || path == include || strings.HasPrefix(path, strings.TrimSuffix(include, "/")+"/") {
			return true
		}
	}
	return false
}

func secscanPathExcluded(path string, excludes []string) bool {
	path = normalizeSecscanOutputPath(path)
	for _, exclude := range excludes {
		exclude = normalizeSecscanOutputPath(exclude)
		if path == exclude || strings.HasPrefix(path, strings.TrimSuffix(exclude, "/")+"/") {
			return true
		}
	}
	return false
}

func normalizeSecscanSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical", "crit":
		return "critical"
	case "high", "error":
		return "high"
	case "medium", "med", "warning", "warn", "moderate":
		return "medium"
	case "low":
		return "low"
	case "info", "informational", "inventory", "experiment", "unknown", "negligible":
		return "info"
	default:
		return ""
	}
}

func normalizeSecscanCategory(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "dependency", "vuln", "vulnerability", "package":
		return "dependency"
	case "sast", "code", "static":
		return "sast"
	case "secret", "secrets":
		return "secret"
	case "misconfig", "misconfiguration", "config", "iac":
		return "misconfig"
	default:
		return ""
	}
}

func secscanSeverityRank(value string) int {
	switch normalizeSecscanSeverity(value) {
	case "critical":
		return 5
	case "high":
		return 4
	case "medium":
		return 3
	case "low":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

func secscanNormalizeFilters(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func secscanStringInFoldedSet(value string, filters []string) bool {
	value = strings.TrimSpace(value)
	for _, filter := range filters {
		if strings.EqualFold(value, strings.TrimSpace(filter)) {
			return true
		}
	}
	return false
}

func secscanStringPtrInFoldedSet(value *string, filters []string) bool {
	if value == nil {
		return false
	}
	return secscanStringInFoldedSet(*value, filters)
}

func secscanDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func secscanHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func secscanStringPtr(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func secscanIntPtr(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}

func secscanFloatPtr(value float64) *float64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func secscanIntPtrString(value *int) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

func secscanIntPtrValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func secscanCompactMessage(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	return secscanTruncate(value, limit)
}

func secscanTruncate(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func secscanIsURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}

func secscanDedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func secscanCapStrings(values []string, max int) []string {
	values = secscanDedupeStrings(values)
	if max > 0 && len(values) > max {
		return values[:max]
	}
	return values
}

func parseSemgrepFindings(data []byte) ([]SecurityScanFinding, []string, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil, fmt.Errorf("semgrep returned empty JSON output")
	}
	var payload struct {
		Results []struct {
			CheckID string `json:"check_id"`
			Path    string `json:"path"`
			Start   struct {
				Line int `json:"line"`
			} `json:"start"`
			End struct {
				Line int `json:"line"`
			} `json:"end"`
			Extra struct {
				Message     string                 `json:"message"`
				Severity    string                 `json:"severity"`
				Fingerprint string                 `json:"fingerprint"`
				Metadata    map[string]interface{} `json:"metadata"`
			} `json:"extra"`
		} `json:"results"`
		Errors []interface{} `json:"errors"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil, fmt.Errorf("parse semgrep JSON: %w", err)
	}
	var warnings []string
	if len(payload.Errors) > 0 {
		warnings = append(warnings, fmt.Sprintf("semgrep reported %d parser/runtime errors", len(payload.Errors)))
	}
	findings := make([]SecurityScanFinding, 0, len(payload.Results))
	for _, result := range payload.Results {
		refs := secscanMetadataStringSlice(result.Extra.Metadata, "references")
		for _, key := range []string{"source", "shortlink"} {
			if value, ok := result.Extra.Metadata[key].(string); ok && secscanIsURL(value) {
				refs = append(refs, value)
			}
		}
		raw := secscanCompactMetadata(result.Extra.Metadata, []string{"impact", "likelihood", "confidence", "owasp", "technology", "category"})
		severity := normalizeSecscanSeverity(result.Extra.Severity)
		if secscanSeverityRank(severity) < secscanSeverityRank("high") && strings.EqualFold(secscanMetadataString(result.Extra.Metadata, "impact"), "high") && strings.EqualFold(secscanMetadataString(result.Extra.Metadata, "confidence"), "high") {
			severity = "high"
		}
		fingerprint := strings.TrimSpace(result.Extra.Fingerprint)
		if fingerprint == "" {
			fingerprint = secscanHash("semgrep", result.CheckID, result.Path, fmt.Sprintf("%d", result.Start.Line), fmt.Sprintf("%d", result.End.Line), result.Extra.Message)
		}
		findings = append(findings, SecurityScanFinding{
			Scanner:     "semgrep",
			Category:    "sast",
			Severity:    severity,
			RuleID:      result.CheckID,
			Title:       secscanSemgrepTitle(result.CheckID, result.Extra.Metadata),
			Message:     secscanCompactMessage(result.Extra.Message, 500),
			Path:        result.Path,
			StartLine:   secscanIntPtr(result.Start.Line),
			EndLine:     secscanIntPtr(result.End.Line),
			CWEIDs:      secscanMetadataStringSlice(result.Extra.Metadata, "cwe"),
			References:  secscanCapStrings(refs, 10),
			Fingerprint: fingerprint,
			Raw:         raw,
		})
	}
	return findings, warnings, nil
}

func secscanSemgrepTitle(checkID string, metadata map[string]interface{}) string {
	for _, key := range []string{"shortlink", "source"} {
		if value, ok := metadata[key].(string); ok && value != "" && !secscanIsURL(value) {
			return value
		}
	}
	parts := strings.Split(checkID, ".")
	if len(parts) > 0 && parts[len(parts)-1] != "" {
		return strings.ReplaceAll(parts[len(parts)-1], "-", " ")
	}
	return checkID
}

func parseTrivyFindings(data []byte) ([]SecurityScanFinding, []string, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil, fmt.Errorf("trivy returned empty JSON output")
	}
	var payload struct {
		Results []secscanTrivyResult `json:"Results"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil, fmt.Errorf("parse trivy JSON: %w", err)
	}
	var findings []SecurityScanFinding
	for _, result := range payload.Results {
		findings = append(findings, parseSecscanTrivyVulnerabilities(result)...)
		findings = append(findings, parseSecscanTrivyMisconfigurations(result)...)
		findings = append(findings, parseSecscanTrivySecrets(result)...)
	}
	return findings, nil, nil
}

type secscanTrivyResult struct {
	Target            string                      `json:"Target"`
	Class             string                      `json:"Class"`
	Type              string                      `json:"Type"`
	Vulnerabilities   []secscanTrivyVulnerability `json:"Vulnerabilities"`
	Misconfigurations []secscanTrivyMisconfig     `json:"Misconfigurations"`
	Secrets           []secscanTrivySecret        `json:"Secrets"`
}

type secscanTrivyVulnerability struct {
	VulnerabilityID  string                            `json:"VulnerabilityID"`
	PkgName          string                            `json:"PkgName"`
	InstalledVersion string                            `json:"InstalledVersion"`
	FixedVersion     string                            `json:"FixedVersion"`
	Severity         string                            `json:"Severity"`
	Title            string                            `json:"Title"`
	Description      string                            `json:"Description"`
	PrimaryURL       string                            `json:"PrimaryURL"`
	References       []string                          `json:"References"`
	CVSS             map[string]map[string]interface{} `json:"CVSS"`
	CWEIDs           []string                          `json:"CweIDs"`
	PkgPath          string                            `json:"PkgPath"`
}

type secscanTrivyMisconfig struct {
	ID            string   `json:"ID"`
	AVDID         string   `json:"AVDID"`
	Type          string   `json:"Type"`
	Title         string   `json:"Title"`
	Description   string   `json:"Description"`
	Message       string   `json:"Message"`
	Resolution    string   `json:"Resolution"`
	Severity      string   `json:"Severity"`
	PrimaryURL    string   `json:"PrimaryURL"`
	References    []string `json:"References"`
	CauseMetadata struct {
		StartLine int `json:"StartLine"`
		EndLine   int `json:"EndLine"`
	} `json:"CauseMetadata"`
}

type secscanTrivySecret struct {
	RuleID    string `json:"RuleID"`
	Category  string `json:"Category"`
	Severity  string `json:"Severity"`
	Title     string `json:"Title"`
	StartLine int    `json:"StartLine"`
	EndLine   int    `json:"EndLine"`
}

func parseSecscanTrivyVulnerabilities(result secscanTrivyResult) []SecurityScanFinding {
	findings := make([]SecurityScanFinding, 0, len(result.Vulnerabilities))
	for _, vuln := range result.Vulnerabilities {
		path := vuln.PkgPath
		if path == "" {
			path = result.Target
		}
		title := strings.TrimSpace(vuln.Title)
		if title == "" {
			title = vuln.VulnerabilityID
		}
		refs := append([]string{}, vuln.References...)
		if secscanIsURL(vuln.PrimaryURL) {
			refs = append([]string{vuln.PrimaryURL}, refs...)
		}
		findings = append(findings, SecurityScanFinding{
			Scanner:          "trivy",
			Category:         "dependency",
			Severity:         normalizeSecscanSeverity(vuln.Severity),
			RuleID:           vuln.VulnerabilityID,
			Title:            title,
			Message:          secscanCompactMessage(vuln.Description, 500),
			Path:             path,
			PackageName:      secscanStringPtr(vuln.PkgName),
			InstalledVersion: secscanStringPtr(vuln.InstalledVersion),
			FixedVersion:     secscanStringPtr(vuln.FixedVersion),
			VulnerabilityID:  secscanStringPtr(vuln.VulnerabilityID),
			CWEIDs:           vuln.CWEIDs,
			CVSSScore:        secscanFloatPtr(secscanMaxTrivyCVSS(vuln.CVSS)),
			References:       secscanCapStrings(refs, 10),
			Fingerprint:      secscanHash("trivy", "vuln", result.Target, vuln.PkgName, vuln.InstalledVersion, vuln.VulnerabilityID),
			Raw: map[string]interface{}{
				"target":          result.Target,
				"dependency_file": result.Target,
				"ecosystem":       result.Type,
			},
		})
	}
	return findings
}

func parseSecscanTrivyMisconfigurations(result secscanTrivyResult) []SecurityScanFinding {
	findings := make([]SecurityScanFinding, 0, len(result.Misconfigurations))
	for _, misconfig := range result.Misconfigurations {
		ruleID := misconfig.AVDID
		if ruleID == "" {
			ruleID = misconfig.ID
		}
		message := misconfig.Message
		if message == "" {
			message = misconfig.Description
		}
		refs := append([]string{}, misconfig.References...)
		if secscanIsURL(misconfig.PrimaryURL) {
			refs = append([]string{misconfig.PrimaryURL}, refs...)
		}
		raw := map[string]interface{}{"target": result.Target}
		if strings.TrimSpace(misconfig.Resolution) != "" {
			raw["resolution"] = misconfig.Resolution
		}
		findings = append(findings, SecurityScanFinding{
			Scanner:     "trivy",
			Category:    "misconfig",
			Severity:    normalizeSecscanSeverity(misconfig.Severity),
			RuleID:      ruleID,
			Title:       misconfig.Title,
			Message:     secscanCompactMessage(message, 500),
			Path:        result.Target,
			StartLine:   secscanIntPtr(misconfig.CauseMetadata.StartLine),
			EndLine:     secscanIntPtr(misconfig.CauseMetadata.EndLine),
			References:  secscanCapStrings(refs, 10),
			Fingerprint: secscanHash("trivy", "misconfig", result.Target, ruleID, fmt.Sprintf("%d", misconfig.CauseMetadata.StartLine), message),
			Raw:         raw,
		})
	}
	return findings
}

func parseSecscanTrivySecrets(result secscanTrivyResult) []SecurityScanFinding {
	findings := make([]SecurityScanFinding, 0, len(result.Secrets))
	for _, secret := range result.Secrets {
		severity := normalizeSecscanSeverity(secret.Severity)
		if severity == "" {
			severity = "high"
		}
		title := strings.TrimSpace(secret.Title)
		if title == "" {
			title = secret.RuleID
		}
		raw := map[string]interface{}{"target": result.Target}
		if strings.TrimSpace(secret.Category) != "" {
			raw["category"] = secret.Category
		}
		findings = append(findings, SecurityScanFinding{
			Scanner:     "trivy",
			Category:    "secret",
			Severity:    severity,
			RuleID:      secret.RuleID,
			Title:       title,
			Message:     fmt.Sprintf("Secret detected by Trivy rule %s. Raw secret value redacted.", secret.RuleID),
			Path:        result.Target,
			StartLine:   secscanIntPtr(secret.StartLine),
			EndLine:     secscanIntPtr(secret.EndLine),
			Fingerprint: secscanHash("trivy", "secret", result.Target, secret.RuleID, fmt.Sprintf("%d", secret.StartLine)),
			Raw:         raw,
		})
	}
	return findings
}

func secscanMaxTrivyCVSS(values map[string]map[string]interface{}) float64 {
	var max float64
	for _, vendor := range values {
		for _, key := range []string{"V3Score", "V2Score"} {
			switch value := vendor[key].(type) {
			case float64:
				if value > max {
					max = value
				}
			case int:
				if float64(value) > max {
					max = float64(value)
				}
			}
		}
	}
	return max
}

func parseGitleaksFindings(data []byte) ([]SecurityScanFinding, []string, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil, fmt.Errorf("gitleaks returned empty JSON output")
	}
	var payload []struct {
		RuleID      string   `json:"RuleID"`
		Description string   `json:"Description"`
		File        string   `json:"File"`
		StartLine   int      `json:"StartLine"`
		EndLine     int      `json:"EndLine"`
		StartColumn int      `json:"StartColumn"`
		EndColumn   int      `json:"EndColumn"`
		Entropy     float64  `json:"Entropy"`
		Tags        []string `json:"Tags"`
		Fingerprint string   `json:"Fingerprint"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil, fmt.Errorf("parse gitleaks JSON: %w", err)
	}
	findings := make([]SecurityScanFinding, 0, len(payload))
	for _, result := range payload {
		fingerprint := strings.TrimSpace(result.Fingerprint)
		if fingerprint == "" {
			fingerprint = secscanHash("gitleaks", result.RuleID, result.File, fmt.Sprintf("%d", result.StartLine), fmt.Sprintf("%d", result.StartColumn), fmt.Sprintf("%d", result.EndColumn))
		}
		raw := map[string]interface{}{}
		if result.Entropy > 0 {
			raw["entropy"] = result.Entropy
		}
		if len(result.Tags) > 0 {
			raw["tags"] = result.Tags
		}
		findings = append(findings, SecurityScanFinding{
			Scanner:     "gitleaks",
			Category:    "secret",
			Severity:    "high",
			RuleID:      result.RuleID,
			Title:       result.Description,
			Message:     fmt.Sprintf("Potential secret detected by Gitleaks rule %s. Raw secret value redacted.", result.RuleID),
			Path:        result.File,
			StartLine:   secscanIntPtr(result.StartLine),
			EndLine:     secscanIntPtr(result.EndLine),
			Fingerprint: fingerprint,
			Raw:         raw,
		})
	}
	return findings, nil, nil
}

func secscanMetadataString(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return value
}

func secscanMetadataStringSlice(metadata map[string]interface{}, key string) []string {
	if metadata == nil {
		return nil
	}
	value, ok := metadata[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return typed
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if str, ok := item.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	case string:
		if typed != "" {
			return []string{typed}
		}
	}
	return nil
}

func secscanCompactMetadata(metadata map[string]interface{}, keys []string) map[string]interface{} {
	if metadata == nil {
		return nil
	}
	out := make(map[string]interface{})
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
