package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func secscanStubLookPathFound(t *testing.T) {
	t.Helper()
	original := secscanLookPath
	secscanLookPath = func(program string) (string, error) { return "/usr/bin/" + program, nil }
	t.Cleanup(func() { secscanLookPath = original })
}

func secscanStubLookPathMissing(t *testing.T) {
	t.Helper()
	original := secscanLookPath
	secscanLookPath = func(program string) (string, error) { return "", fmt.Errorf("%s not found", program) }
	t.Cleanup(func() { secscanLookPath = original })
}

func secscanStubCommandRunner(t *testing.T, stdout, stderr string, err error) *[]string {
	t.Helper()
	original := secscanCommandRunner
	var calls []string
	secscanCommandRunner = func(_ context.Context, root, program string, args, _ []string) (string, string, error) {
		calls = append(calls, program+" "+strings.Join(args, " "))
		return stdout, stderr, err
	}
	t.Cleanup(func() { secscanCommandRunner = original })
	return &calls
}

func TestScanToolsRegisteredAsNonMutating(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"scan_semgrep", "scan_trivy", "scan_gitleaks"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s to be registered", name)
		}
		if def.Mutating {
			t.Fatalf("expected %s to be non-mutating", name)
		}
		if def.Category != "Security" {
			t.Fatalf("expected %s category Security, got %q", name, def.Category)
		}
	}
}

func TestScanToolsRequireWorkspaceLease(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"scan_semgrep", "scan_trivy", "scan_gitleaks"} {
		_, err := registry.Execute(context.Background(), CallContext{}, name, json.RawMessage(`{}`))
		if err == nil || !strings.Contains(err.Error(), "workspace lease") {
			t.Fatalf("%s: expected workspace lease error, got %v", name, err)
		}
	}
}

func TestScanToolsDegradeGracefullyWhenBinaryMissing(t *testing.T) {
	secscanStubLookPathMissing(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	for _, tc := range []struct {
		tool    string
		scanner string
	}{
		{tool: "scan_semgrep", scanner: "semgrep"},
		{tool: "scan_trivy", scanner: "trivy"},
		{tool: "scan_gitleaks", scanner: "gitleaks"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			output, err := registry.Execute(context.Background(), callCtx, tc.tool, json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("expected graceful tool result, got error: %v", err)
			}
			var result struct {
				Scanner string `json:"scanner"`
				Status  string `json:"status"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatalf("decode output: %v; raw=%s", err, string(output))
			}
			if result.Scanner != tc.scanner || result.Status != "unavailable" {
				t.Fatalf("unexpected result %+v", result)
			}
			if !strings.Contains(result.Error, "not installed") {
				t.Fatalf("expected clear missing-binary message, got %q", result.Error)
			}
		})
	}
}

func TestParseSecscanRequestValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		wantErr string
		check   func(t *testing.T, req secscanRequest)
	}{
		{
			name:  "defaults",
			input: `{}`,
			check: func(t *testing.T, req secscanRequest) {
				if req.SeverityThreshold != "medium" || req.Page != 1 || req.PageSize != 100 || req.DetailLevel != "full" {
					t.Fatalf("unexpected defaults %+v", req)
				}
				if len(req.ScanPaths) != 1 || req.ScanPaths[0] != "." {
					t.Fatalf("expected default scan path, got %v", req.ScanPaths)
				}
			},
		},
		{
			name:    "absolute scan path rejected",
			input:   `{"scan_paths":["/etc"]}`,
			wantErr: "scan_paths[0]",
		},
		{
			name:    "traversal rejected",
			input:   `{"scan_paths":["../other"]}`,
			wantErr: "must stay within the repository",
		},
		{
			name:    "exclude entire repository rejected",
			input:   `{"exclude_paths":["."]}`,
			wantErr: "cannot exclude the entire repository",
		},
		{
			name:    "invalid category",
			input:   `{"category":"bogus"}`,
			wantErr: "category must be one of",
		},
		{
			name:    "invalid detail level",
			input:   `{"detail_level":"verbose"}`,
			wantErr: "detail_level must be full or index",
		},
		{
			name:  "legacy max findings sets page size",
			input: `{"max_findings":50}`,
			check: func(t *testing.T, req secscanRequest) {
				if !req.legacyMaxFindings || req.PageSize != 50 {
					t.Fatalf("expected legacy max_findings handling, got %+v", req)
				}
			},
		},
		{
			name:  "page size capped",
			input: `{"page_size":500}`,
			check: func(t *testing.T, req secscanRequest) {
				if req.PageSize != 200 {
					t.Fatalf("expected page size cap 200, got %d", req.PageSize)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parseSecscanRequest(json.RawMessage(tc.input))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, req)
			}
		})
	}
}

func TestScanSemgrepParsesAndFiltersFindings(t *testing.T) {
	secscanStubLookPathFound(t)
	semgrepJSON := `{
		"results": [
			{"check_id":"go.lang.security.audit.sqli","path":"main.go","start":{"line":10},"end":{"line":12},"extra":{"message":"possible sql injection","severity":"ERROR","metadata":{"cwe":["CWE-89"]}}},
			{"check_id":"go.lang.style.naming","path":"util.go","start":{"line":3},"end":{"line":3},"extra":{"message":"style issue","severity":"INFO","metadata":{}}}
		],
		"errors": []
	}`
	calls := secscanStubCommandRunner(t, semgrepJSON, "", nil)
	registry, callCtx := workspaceToolTestRegistry(t)

	output, err := registry.Execute(context.Background(), callCtx, "scan_semgrep", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("scan_semgrep returned error: %v", err)
	}
	var result SecurityScannerResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v; raw=%s", err, string(output))
	}
	if result.Scanner != "semgrep" {
		t.Fatalf("unexpected scanner %q", result.Scanner)
	}
	// INFO finding filtered by default threshold (medium, no low/info).
	if result.TotalFindings != 1 || len(result.Findings) != 1 {
		t.Fatalf("expected 1 filtered finding, got total=%d returned=%d", result.TotalFindings, len(result.Findings))
	}
	finding := result.Findings[0]
	if finding.RuleID != "go.lang.security.audit.sqli" || finding.Severity != "high" || finding.Category != "sast" {
		t.Fatalf("unexpected finding %+v", finding)
	}
	if result.Summary.High != 1 || result.Summary.Total != 1 {
		t.Fatalf("unexpected summary %+v", result.Summary)
	}
	if len(*calls) == 0 || !strings.HasPrefix((*calls)[0], "semgrep scan --config") {
		t.Fatalf("unexpected command invocations %v", *calls)
	}
}

func TestScanSemgrepCachesFindingsPerRun(t *testing.T) {
	secscanStubLookPathFound(t)
	semgrepJSON := `{"results":[{"check_id":"rule.one","path":"a.go","start":{"line":1},"end":{"line":1},"extra":{"message":"m","severity":"ERROR","metadata":{}}}],"errors":[]}`
	calls := secscanStubCommandRunner(t, semgrepJSON, "", nil)
	registry, callCtx := workspaceToolTestRegistry(t)

	if _, err := registry.Execute(context.Background(), callCtx, "scan_semgrep", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("first scan failed: %v", err)
	}
	output, err := registry.Execute(context.Background(), callCtx, "scan_semgrep", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("second scan failed: %v", err)
	}
	var result SecurityScannerResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if !result.CacheHit {
		t.Fatalf("expected cache hit on second identical scan")
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one scanner invocation, got %d", len(*calls))
	}
}

func TestScanTrivyParsesVulnerabilitiesMisconfigsAndSecrets(t *testing.T) {
	secscanStubLookPathFound(t)
	trivyJSON := `{
		"Results": [
			{
				"Target": "go.mod",
				"Class": "lang-pkgs",
				"Type": "gomod",
				"Vulnerabilities": [
					{"VulnerabilityID":"CVE-2024-0001","PkgName":"example.com/pkg","InstalledVersion":"1.0.0","FixedVersion":"1.0.1","Severity":"CRITICAL","Title":"bad vuln","Description":"desc","PrimaryURL":"https://example.com/cve","CVSS":{"nvd":{"V3Score":9.8}}}
				]
			},
			{
				"Target": "Dockerfile",
				"Class": "config",
				"Type": "dockerfile",
				"Misconfigurations": [
					{"ID":"DS002","AVDID":"AVD-DS-0002","Title":"root user","Description":"runs as root","Message":"specify USER","Severity":"HIGH","CauseMetadata":{"StartLine":1,"EndLine":2}}
				]
			},
			{
				"Target": "config.env",
				"Class": "secret",
				"Secrets": [
					{"RuleID":"aws-access-key-id","Category":"AWS","Severity":"CRITICAL","Title":"AWS Access Key","StartLine":4,"EndLine":4}
				]
			}
		]
	}`
	secscanStubCommandRunner(t, trivyJSON, "", nil)
	registry, callCtx := workspaceToolTestRegistry(t)

	output, err := registry.Execute(context.Background(), callCtx, "scan_trivy", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("scan_trivy returned error: %v", err)
	}
	var result SecurityScannerResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.TotalFindings != 3 {
		t.Fatalf("expected 3 findings, got %d (%s)", result.TotalFindings, string(output))
	}
	categories := map[string]bool{}
	for _, finding := range result.Findings {
		categories[finding.Category] = true
	}
	for _, want := range []string{"dependency", "misconfig", "secret"} {
		if !categories[want] {
			t.Fatalf("missing category %s in findings: %v", want, categories)
		}
	}
}

func TestScanTrivyRejectsUnsupportedScannerKind(t *testing.T) {
	secscanStubLookPathFound(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	_, err := registry.Execute(context.Background(), callCtx, "scan_trivy", json.RawMessage(`{"scanners":["license"]}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported trivy scanner") {
		t.Fatalf("expected unsupported scanner error, got %v", err)
	}
}

func TestScanGitleaksParsesFindingsAndDefaultsHighThreshold(t *testing.T) {
	secscanStubLookPathFound(t)
	gitleaksJSON := `[
		{"RuleID":"generic-api-key","Description":"Generic API Key","File":"cfg/settings.go","StartLine":8,"EndLine":8,"StartColumn":5,"EndColumn":40,"Entropy":4.2,"Tags":["key"]}
	]`
	calls := secscanStubCommandRunner(t, gitleaksJSON, "", nil)
	registry, callCtx := workspaceToolTestRegistry(t)

	output, err := registry.Execute(context.Background(), callCtx, "scan_gitleaks", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("scan_gitleaks returned error: %v", err)
	}
	var result SecurityScannerResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.TotalFindings != 1 || result.Findings[0].Severity != "high" || result.Findings[0].Category != "secret" {
		t.Fatalf("unexpected result %s", string(output))
	}
	if result.Findings[0].RuleID != "generic-api-key" {
		t.Fatalf("unexpected rule id %q", result.Findings[0].RuleID)
	}
	foundWorkingTreeWarning := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "working tree only") {
			foundWorkingTreeWarning = true
		}
	}
	if !foundWorkingTreeWarning {
		t.Fatalf("expected working-tree warning, got %v", result.Warnings)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0], "--no-git") {
		t.Fatalf("expected --no-git by default, got %v", *calls)
	}
}

func TestScanCommandFailureWithNoOutputReturnsZeroFindings(t *testing.T) {
	secscanStubLookPathFound(t)
	secscanStubCommandRunner(t, "", "boom", fmt.Errorf("exit status 2"))
	registry, callCtx := workspaceToolTestRegistry(t)

	output, err := registry.Execute(context.Background(), callCtx, "scan_gitleaks", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("expected graceful zero-finding result, got %v", err)
	}
	var result SecurityScannerResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.TotalFindings != 0 {
		t.Fatalf("expected zero findings, got %d", result.TotalFindings)
	}
	joined := strings.Join(result.Warnings, "\n")
	if !strings.Contains(joined, "gitleaks produced no JSON output") || !strings.Contains(joined, "gitleaks stderr: boom") {
		t.Fatalf("expected failure warnings, got %v", result.Warnings)
	}
}

func TestFilterSecscanFindingsSeverityPathsAndPagination(t *testing.T) {
	findings := []SecurityScanFinding{
		{Scanner: "semgrep", Category: "sast", Severity: "critical", RuleID: "r1", Path: "a/b.go", Fingerprint: "f1"},
		{Scanner: "semgrep", Category: "sast", Severity: "medium", RuleID: "r2", Path: "a/c.go", Fingerprint: "f2"},
		{Scanner: "semgrep", Category: "sast", Severity: "low", RuleID: "r3", Path: "a/d.go", Fingerprint: "f3"},
		{Scanner: "semgrep", Category: "sast", Severity: "critical", RuleID: "r4", Path: "vendor/x.go", Fingerprint: "f4"},
		// duplicate of f1
		{Scanner: "semgrep", Category: "sast", Severity: "critical", RuleID: "r1", Path: "a/b.go", Fingerprint: "f1"},
	}
	req, err := parseSecscanRequest(json.RawMessage(`{"exclude_paths":["vendor"]}`))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	filtered, warnings := filterSecscanFindings(findings, req)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings %v", warnings)
	}
	// low filtered (below threshold + include_low_info false), vendor excluded, duplicate deduped.
	if len(filtered) != 2 {
		t.Fatalf("expected 2 findings, got %d: %+v", len(filtered), filtered)
	}
	if filtered[0].Severity != "critical" || filtered[1].Severity != "medium" {
		t.Fatalf("expected severity-sorted output, got %+v", filtered)
	}

	page, pageNum, pageSize, hasMore := paginateSecscanFindings(filtered, secscanRequest{Page: 1, PageSize: 1})
	if len(page) != 1 || pageNum != 1 || pageSize != 1 || !hasMore {
		t.Fatalf("unexpected pagination page=%d size=%d hasMore=%v len=%d", pageNum, pageSize, hasMore, len(page))
	}
}

func TestSummaryOnlyOmitsFindingsButKeepsGroups(t *testing.T) {
	secscanStubLookPathFound(t)
	semgrepJSON := `{"results":[{"check_id":"rule.one","path":"a.go","start":{"line":1},"end":{"line":1},"extra":{"message":"m","severity":"ERROR","metadata":{}}}],"errors":[]}`
	secscanStubCommandRunner(t, semgrepJSON, "", nil)
	registry, callCtx := workspaceToolTestRegistry(t)

	output, err := registry.Execute(context.Background(), callCtx, "scan_semgrep", json.RawMessage(`{"summary_only":true}`))
	if err != nil {
		t.Fatalf("scan_semgrep returned error: %v", err)
	}
	var result SecurityScannerResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("expected no findings in summary_only mode, got %d", len(result.Findings))
	}
	if result.TotalFindings != 1 || len(result.Groups) == 0 {
		t.Fatalf("expected counts and groups, got total=%d groups=%d", result.TotalFindings, len(result.Groups))
	}
}
