package tools

// GitHub pull-request / check-run tools ported from the Helpin worker
// (tools_github.go).
//
// Auth design: the workspace lease only stores a REDACTED repository spec
// (token/password/extra-header are stripped before the spec is put on lease
// metadata — see internal/workspace/git_provider.go redactedRepositorySpec),
// and the clone's http.extraheader git config is unset after clone. So the
// unredacted RepositoryAuth token is NOT reachable from tool CallContext at
// call time. These tools therefore:
//   1. resolve provider/owner/repo from the lease's repository_spec metadata
//      (clone_url) or explicit params, and
//   2. use a token exposed via the execution environment
//      (AGENT_RUNTIME_GITHUB_TOKEN, GITHUB_TOKEN, or GH_TOKEN).
//
// When no token is configured, get_pull_request_diff degrades to a local
// `git diff <base>...HEAD` against the workspace clone (the primary use is
// reviewing the current branch's changes), and get_check_run_logs returns a
// clear tool-result explaining the missing credential — check-run logs only
// exist in the GitHub API, so there is no local fallback.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	toolNameGetPullRequestDiff = "get_pull_request_diff"
	toolNameGetCheckRunLogs    = "get_check_run_logs"

	repoProviderTokenEnvVar   = "AGENT_RUNTIME_GITHUB_TOKEN"
	repoProviderAPIBaseEnvVar = "AGENT_RUNTIME_GITHUB_API_BASE"

	repoProviderRequestTimeout = 30 * time.Second
	repoProviderMaxDiffBytes   = 60_000
)

// repoProviderHTTPClient is swappable in tests.
var repoProviderHTTPClient = &http.Client{Timeout: repoProviderRequestTimeout}

// RegisterRepositoryProviderTools registers read-only GitHub PR/check tools.
func RegisterRepositoryProviderTools(r *Registry) {
	if r == nil {
		return
	}
	r.Register(Definition{
		Name:        toolNameGetPullRequestDiff,
		Description: "Load the changed files and patches for a GitHub pull request. Defaults owner/repo from the current repository workspace when omitted. Falls back to a local git diff of the checked-out branch when no GitHub token is configured or pull_number is omitted.",
		Category:    "Git",
		Mutating:    false,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]interface{}{
					"type":        "string",
					"description": "Repository owner. Defaults from the current run repository when omitted.",
				},
				"repo": map[string]interface{}{
					"type":        "string",
					"description": "Repository name. Defaults from the current run repository when omitted.",
				},
				"pull_number": map[string]interface{}{
					"type":        "integer",
					"description": "Pull request number. Omit to diff the checked-out branch against its base branch locally.",
				},
				"base": map[string]interface{}{
					"type":        "string",
					"description": "Base branch for the local diff fallback. Defaults to the workspace base branch.",
				},
			},
			"additionalProperties": false,
		},
	}, toolGetPullRequestDiff)
	r.Register(Definition{
		Name:        toolNameGetCheckRunLogs,
		Description: "Load a GitHub check run's conclusion, output text, and annotations for diagnosing failed checks. Requires a GitHub token in the runtime environment.",
		Category:    "Git",
		Mutating:    false,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"owner": map[string]interface{}{
					"type":        "string",
					"description": "Repository owner. Defaults from the current run repository when omitted.",
				},
				"repo": map[string]interface{}{
					"type":        "string",
					"description": "Repository name. Defaults from the current run repository when omitted.",
				},
				"check_run_id": map[string]interface{}{
					"type":        "integer",
					"description": "GitHub check run ID.",
				},
			},
			"required":             []string{"check_run_id"},
			"additionalProperties": false,
		},
	}, toolGetCheckRunLogs)
}

func toolGetPullRequestDiff(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Owner      string `json:"owner"`
		Repo       string `json:"repo"`
		PullNumber int    `json:"pull_number"`
		Base       string `json:"base"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	repoRef := resolveRepoProviderRef(callCtx, params.Owner, params.Repo)
	token := repoProviderToken()

	if params.PullNumber > 0 && token != "" {
		if repoRef.Owner == "" || repoRef.Repo == "" {
			return nil, fmt.Errorf("owner and repo are required; the workspace does not reference a GitHub repository")
		}
		var files []struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
			Changes   int    `json:"changes"`
			Patch     string `json:"patch,omitempty"`
		}
		if err := repoProviderRequest(ctx, repoRef, token, fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", repoRef.Owner, repoRef.Repo, params.PullNumber), &files); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{
			"mode":        "github_api",
			"owner":       repoRef.Owner,
			"repo":        repoRef.Repo,
			"pull_number": params.PullNumber,
			"files":       files,
		})
	}

	// Local fallback: diff the checked-out branch against its base branch.
	result, err := localWorkspaceBranchDiff(ctx, callCtx, params.Base)
	if err != nil {
		return nil, err
	}
	if params.PullNumber > 0 {
		result["pull_number"] = params.PullNumber
		result["note"] = "no GitHub token is configured in the runtime environment (set " + repoProviderTokenEnvVar + " or GITHUB_TOKEN); returned the local branch diff of the workspace clone instead of the pull request file list"
	}
	return json.Marshal(result)
}

func toolGetCheckRunLogs(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Owner      string `json:"owner"`
		Repo       string `json:"repo"`
		CheckRunID int64  `json:"check_run_id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if params.CheckRunID <= 0 {
		return nil, fmt.Errorf("check_run_id is required")
	}
	repoRef := resolveRepoProviderRef(callCtx, params.Owner, params.Repo)
	if repoRef.Owner == "" || repoRef.Repo == "" {
		return nil, fmt.Errorf("owner and repo are required; the workspace does not reference a GitHub repository")
	}
	token := repoProviderToken()
	if token == "" {
		// Graceful tool-result: check-run logs only exist in the GitHub API,
		// so there is no local fallback when credentials are absent.
		return json.Marshal(map[string]any{
			"status": "unavailable",
			"error":  "no GitHub token is configured in the runtime environment; set " + repoProviderTokenEnvVar + " or GITHUB_TOKEN to enable get_check_run_logs",
		})
	}

	var checkRun struct {
		ID          int64      `json:"id"`
		Name        string     `json:"name"`
		HTMLURL     string     `json:"html_url"`
		Status      string     `json:"status"`
		Conclusion  string     `json:"conclusion"`
		StartedAt   *time.Time `json:"started_at"`
		CompletedAt *time.Time `json:"completed_at"`
		Output      struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
			Text    string `json:"text"`
		} `json:"output"`
	}
	if err := repoProviderRequest(ctx, repoRef, token, fmt.Sprintf("/repos/%s/%s/check-runs/%d", repoRef.Owner, repoRef.Repo, params.CheckRunID), &checkRun); err != nil {
		return nil, err
	}
	var annotations []struct {
		Path            string `json:"path"`
		StartLine       int    `json:"start_line"`
		EndLine         int    `json:"end_line"`
		AnnotationLevel string `json:"annotation_level"`
		Message         string `json:"message"`
		Title           string `json:"title"`
	}
	if err := repoProviderRequest(ctx, repoRef, token, fmt.Sprintf("/repos/%s/%s/check-runs/%d/annotations?per_page=50", repoRef.Owner, repoRef.Repo, params.CheckRunID), &annotations); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"owner":       repoRef.Owner,
		"repo":        repoRef.Repo,
		"check_run":   checkRun,
		"annotations": annotations,
	})
}

type repoProviderRef struct {
	Provider string
	Owner    string
	Repo     string
	Host     string
}

// resolveRepoProviderRef fills owner/repo from explicit params first, then
// from the workspace lease's redacted repository spec metadata.
func resolveRepoProviderRef(callCtx CallContext, owner, repo string) repoProviderRef {
	ref := repoProviderRef{Owner: strings.TrimSpace(owner), Repo: strings.TrimSpace(repo)}
	spec := repoProviderSpecFromLease(callCtx)
	if spec != nil {
		ref.Provider = strings.TrimSpace(spec.Provider)
		host, specOwner, specRepo := parseRepoProviderCloneURL(spec.CloneURL)
		ref.Host = host
		if ref.Owner == "" {
			ref.Owner = specOwner
		}
		if ref.Repo == "" {
			ref.Repo = specRepo
		}
		if full := repoProviderMetadataString(spec.Metadata, "repo_full_name"); full != "" {
			parts := strings.SplitN(full, "/", 2)
			if len(parts) == 2 {
				if ref.Owner == "" {
					ref.Owner = strings.TrimSpace(parts[0])
				}
				if ref.Repo == "" {
					ref.Repo = strings.TrimSpace(parts[1])
				}
			}
		}
	}
	return ref
}

type repoProviderLeaseSpec struct {
	Provider   string                 `json:"provider"`
	CloneURL   string                 `json:"clone_url"`
	BaseBranch string                 `json:"base_branch"`
	WorkBranch string                 `json:"work_branch"`
	Metadata   map[string]interface{} `json:"metadata"`
}

func repoProviderSpecFromLease(callCtx CallContext) *repoProviderLeaseSpec {
	if callCtx.Run == nil || callCtx.Run.WorkspaceLease == nil || callCtx.Run.WorkspaceLease.Metadata == nil {
		return nil
	}
	raw, ok := callCtx.Run.WorkspaceLease.Metadata["repository_spec"]
	if !ok {
		return nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var spec repoProviderLeaseSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return nil
	}
	return &spec
}

func repoProviderMetadataString(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

// parseRepoProviderCloneURL extracts host, owner, and repo name from an
// https or ssh git clone URL.
func parseRepoProviderCloneURL(cloneURL string) (host, owner, repo string) {
	cloneURL = strings.TrimSpace(cloneURL)
	if cloneURL == "" {
		return "", "", ""
	}
	var pathPart string
	if strings.HasPrefix(cloneURL, "git@") {
		// git@github.com:owner/repo.git
		rest := strings.TrimPrefix(cloneURL, "git@")
		hostPart, after, ok := strings.Cut(rest, ":")
		if !ok {
			return "", "", ""
		}
		host = hostPart
		pathPart = after
	} else {
		parsed, err := url.Parse(cloneURL)
		if err != nil || parsed.Host == "" {
			return "", "", ""
		}
		host = parsed.Hostname()
		pathPart = strings.TrimPrefix(parsed.Path, "/")
	}
	pathPart = strings.TrimSuffix(strings.TrimSuffix(pathPart, "/"), ".git")
	segments := strings.Split(pathPart, "/")
	if len(segments) < 2 {
		return host, "", ""
	}
	owner = strings.TrimSpace(segments[len(segments)-2])
	repo = strings.TrimSpace(segments[len(segments)-1])
	return host, owner, repo
}

func repoProviderToken() string {
	for _, key := range []string{repoProviderTokenEnvVar, "GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token
		}
	}
	return ""
}

func repoProviderAPIBase(ref repoProviderRef) string {
	if base := strings.TrimSpace(os.Getenv(repoProviderAPIBaseEnvVar)); base != "" {
		return strings.TrimRight(base, "/")
	}
	host := strings.ToLower(strings.TrimSpace(ref.Host))
	if host != "" && host != "github.com" && host != "www.github.com" {
		// GitHub Enterprise Server layout.
		return "https://" + host + "/api/v3"
	}
	return "https://api.github.com"
}

func repoProviderRequest(ctx context.Context, ref repoProviderRef, token, path string, out any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(ctx, repoProviderRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, repoProviderAPIBase(ref)+path, nil)
	if err != nil {
		return fmt.Errorf("build github request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := repoProviderHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request github: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return fmt.Errorf("read github response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github request failed (%d)", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode github response: %w", err)
	}
	return nil
}

// localWorkspaceBranchDiff produces a diff of the checked-out branch against
// its base branch using the local clone in the workspace lease root.
func localWorkspaceBranchDiff(ctx context.Context, callCtx CallContext, baseOverride string) (map[string]any, error) {
	root, err := requireWorkspaceRoot(callCtx, toolNameGetPullRequestDiff)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(baseOverride)
	if base == "" {
		base = repoProviderBaseBranch(callCtx)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	gitCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	diffRange := "HEAD"
	if base != "" {
		candidates := []string{"origin/" + base + "...HEAD", base + "...HEAD"}
		for _, candidate := range candidates {
			check := exec.CommandContext(gitCtx, "git", "rev-parse", "--verify", strings.SplitN(candidate, "...", 2)[0])
			check.Dir = root
			if check.Run() == nil {
				diffRange = candidate
				break
			}
		}
		if diffRange == "HEAD" {
			base = ""
		}
	}

	var statOut, diffOut []byte
	if diffRange == "HEAD" {
		// No usable base ref: fall back to uncommitted changes.
		statOut, _ = repoProviderGit(gitCtx, root, "diff", "--stat", "HEAD")
		diffOut, err = repoProviderGit(gitCtx, root, "diff", "HEAD")
	} else {
		statOut, _ = repoProviderGit(gitCtx, root, "diff", "--stat", diffRange)
		diffOut, err = repoProviderGit(gitCtx, root, "diff", diffRange)
	}
	if err != nil {
		return nil, fmt.Errorf("git diff failed: %w", err)
	}
	diff := string(diffOut)
	truncated := false
	if len(diff) > repoProviderMaxDiffBytes {
		diff = diff[:repoProviderMaxDiffBytes] + "\n... (diff truncated)"
		truncated = true
	}
	result := map[string]any{
		"mode":      "local_diff",
		"base":      base,
		"range":     diffRange,
		"stat":      strings.TrimSpace(string(statOut)),
		"diff":      diff,
		"truncated": truncated,
	}
	if base == "" {
		result["note"] = "no base branch could be resolved; diff shows uncommitted changes against HEAD"
	}
	return result, nil
}

func repoProviderBaseBranch(callCtx CallContext) string {
	if callCtx.Run == nil || callCtx.Run.WorkspaceLease == nil || callCtx.Run.WorkspaceLease.Metadata == nil {
		return ""
	}
	if base, _ := callCtx.Run.WorkspaceLease.Metadata["base_branch"].(string); strings.TrimSpace(base) != "" {
		return strings.TrimSpace(base)
	}
	if spec := repoProviderSpecFromLease(callCtx); spec != nil {
		return strings.TrimSpace(spec.BaseBranch)
	}
	return ""
}

func repoProviderGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	return cmd.Output()
}
