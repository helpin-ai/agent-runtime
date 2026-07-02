package workspace

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const (
	defaultGitUserName  = "Agent Runtime"
	defaultGitUserEmail = "agent-runtime@example.invalid"
)

type RepositoryProvider struct {
	RootDir      string
	SpecProvider RepositorySpecProvider
}

func (p RepositoryProvider) PrepareWorkspace(ctx context.Context, req PrepareRequest) (*agentcore.WorkspaceLease, error) {
	if p.SpecProvider == nil {
		return nil, fmt.Errorf("repository spec provider is required")
	}
	spec, err := p.SpecProvider.ResolveRepositoryWorkspace(ctx, req)
	if err != nil {
		return nil, err
	}
	NormalizeRepositorySpec(spec)
	if spec == nil || spec.CloneURL == "" {
		return nil, fmt.Errorf("repository workspace spec requires clone_url")
	}
	root := strings.TrimSpace(p.RootDir)
	if root == "" {
		root = filepath.Join(os.TempDir(), "agent-runtime-workspaces")
	}
	runRoot := filepath.Join(root, sanitizePathComponent(req.AppID), sanitizePathComponent(req.RunID))
	repoDir := filepath.Join(runRoot, "repo")
	if info, err := os.Stat(repoDir); err == nil && info.IsDir() {
		if err := configureGitIdentity(ctx, repoDir, spec.CommitIdentity); err != nil {
			return nil, err
		}
		return repositoryLease(req, spec, repoDir), nil
	}
	_ = os.RemoveAll(runRoot)
	if err := os.MkdirAll(runRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create repository workspace root: %w", err)
	}
	if err := cloneRepository(ctx, spec, repoDir); err != nil {
		_ = os.RemoveAll(runRoot)
		return nil, err
	}
	if err := checkoutRepositoryBranch(ctx, repoDir, spec); err != nil {
		_ = os.RemoveAll(runRoot)
		return nil, err
	}
	if err := configureGitIdentity(ctx, repoDir, spec.CommitIdentity); err != nil {
		_ = os.RemoveAll(runRoot)
		return nil, err
	}
	return repositoryLease(req, spec, repoDir), nil
}

func (p RepositoryProvider) FinalizeWorkspace(ctx context.Context, req FinalizeRequest) (*FinalizeResult, error) {
	spec := req.Repository
	if spec == nil {
		spec = RepositorySpecFromLease(req.Lease)
	}
	NormalizeRepositorySpec(spec)
	if spec == nil || strings.TrimSpace(req.Lease.RootPath) == "" {
		return &FinalizeResult{}, nil
	}
	if p.SpecProvider != nil && repositoryAuthRedacted(spec) && (spec.FinalizePolicy == RepositoryFinalizePushBranch || spec.FinalizePolicy == RepositoryFinalizeOpenPR) {
		fresh, err := p.SpecProvider.ResolveRepositoryWorkspace(ctx, PrepareRequest{
			AppID:         req.AppID,
			RunID:         req.RunID,
			AgentID:       req.AgentID,
			RuntimeKind:   req.RuntimeKind,
			Target:        req.Target,
			WorkspaceMode: ModeRepository,
		})
		if err != nil {
			return nil, err
		}
		NormalizeRepositorySpec(fresh)
		if fresh != nil {
			spec = fresh
		}
	}
	switch spec.FinalizePolicy {
	case "", RepositoryFinalizeNone:
		return &FinalizeResult{}, nil
	case RepositoryFinalizeLocalCommit:
		summary, err := commitRepositoryChanges(ctx, req.Lease.RootPath, spec)
		if err != nil {
			return nil, err
		}
		return &FinalizeResult{OutputSummary: summary}, nil
	case RepositoryFinalizePushBranch:
		summary, err := commitAndPushRepositoryChanges(ctx, req.Lease.RootPath, spec)
		if err != nil {
			return nil, err
		}
		return &FinalizeResult{OutputSummary: summary}, nil
	case RepositoryFinalizeOpenPR:
		return nil, fmt.Errorf("repository finalize policy %q is host-owned; use %q and open the pull request from the host finalizer", spec.FinalizePolicy, RepositoryFinalizePushBranch)
	default:
		return nil, fmt.Errorf("repository finalize policy %q is not implemented", spec.FinalizePolicy)
	}
}

func (p RepositoryProvider) CleanupWorkspace(_ context.Context, req CleanupRequest) error {
	root := filepath.Dir(strings.TrimSpace(req.Lease.RootPath))
	if root == "." || root == "" {
		return nil
	}
	return os.RemoveAll(root)
}

func NormalizeRepositorySpec(spec *RepositoryWorkspaceSpec) {
	if spec == nil {
		return
	}
	spec.Provider = strings.TrimSpace(spec.Provider)
	spec.CloneURL = strings.TrimSpace(spec.CloneURL)
	spec.BaseBranch = strings.TrimSpace(spec.BaseBranch)
	spec.WorkBranch = strings.TrimSpace(spec.WorkBranch)
	spec.FinalizePolicy = strings.TrimSpace(spec.FinalizePolicy)
	if spec.FinalizePolicy == "" {
		spec.FinalizePolicy = RepositoryFinalizeNone
	}
	if spec.Metadata == nil {
		spec.Metadata = map[string]interface{}{}
	}
	if spec.Auth != nil {
		spec.Auth.Type = strings.TrimSpace(spec.Auth.Type)
		spec.Auth.Token = strings.TrimSpace(spec.Auth.Token)
		spec.Auth.Username = strings.TrimSpace(spec.Auth.Username)
		spec.Auth.Password = strings.TrimSpace(spec.Auth.Password)
		spec.Auth.ExtraHeader = strings.TrimSpace(spec.Auth.ExtraHeader)
	}
	if spec.CommitIdentity != nil {
		spec.CommitIdentity.Name = strings.TrimSpace(spec.CommitIdentity.Name)
		spec.CommitIdentity.Email = strings.TrimSpace(spec.CommitIdentity.Email)
	}
}

func RepositorySpecFromLease(lease agentcore.WorkspaceLease) *RepositoryWorkspaceSpec {
	raw, ok := lease.Metadata["repository_spec"]
	if !ok {
		return nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var spec RepositoryWorkspaceSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return nil
	}
	NormalizeRepositorySpec(&spec)
	return &spec
}

func repositoryLease(req PrepareRequest, spec *RepositoryWorkspaceSpec, repoDir string) *agentcore.WorkspaceLease {
	metadata := map[string]interface{}{}
	for key, value := range spec.Metadata {
		metadata[key] = value
	}
	metadata["repository_spec"] = redactedRepositorySpec(spec)
	if spec.BaseBranch != "" {
		metadata["base_branch"] = spec.BaseBranch
	}
	if spec.WorkBranch != "" {
		metadata["work_branch"] = spec.WorkBranch
	}
	return &agentcore.WorkspaceLease{
		ID:            stableLeaseID(req.AppID, req.RunID, spec.CloneURL),
		Provider:      "repository",
		RootPath:      repoDir,
		CleanupPolicy: CleanupOnTerminal,
		Metadata:      metadata,
	}
}

func redactedRepositorySpec(spec *RepositoryWorkspaceSpec) RepositoryWorkspaceSpec {
	if spec == nil {
		return RepositoryWorkspaceSpec{}
	}
	cp := *spec
	if spec.Metadata != nil {
		cp.Metadata = map[string]interface{}{}
		for key, value := range spec.Metadata {
			cp.Metadata[key] = value
		}
	}
	if spec.Auth != nil {
		cp.Auth = &RepositoryAuth{Type: spec.Auth.Type, Username: spec.Auth.Username}
	}
	return cp
}

func repositoryAuthRedacted(spec *RepositoryWorkspaceSpec) bool {
	if spec == nil || spec.Auth == nil {
		return true
	}
	return spec.Auth.Token == "" && spec.Auth.Password == "" && spec.Auth.ExtraHeader == "" && len(spec.Auth.Env) == 0
}

func cloneRepository(ctx context.Context, spec *RepositoryWorkspaceSpec, repoDir string) error {
	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	args := []string{"clone"}
	args = append(args, gitAuthArgs(spec.Auth)...)
	if spec.BaseBranch != "" {
		args = append(args, "--branch", spec.BaseBranch)
	}
	args = append(args, spec.CloneURL, repoDir)
	cmd := exec.CommandContext(cloneCtx, "git", args...)
	cmd.Env = gitEnv(spec.Auth)
	if output, err := cmd.CombinedOutput(); err != nil {
		return commandError("git clone", err, output)
	}
	unset := exec.CommandContext(cloneCtx, "git", "config", "--unset-all", "http.extraheader")
	unset.Dir = repoDir
	_ = unset.Run()
	return nil
}

func checkoutRepositoryBranch(ctx context.Context, repoDir string, spec *RepositoryWorkspaceSpec) error {
	branch := strings.TrimSpace(spec.WorkBranch)
	if branch == "" {
		return nil
	}
	checkoutCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	args := []string{"checkout", "-B", branch}
	if spec.BaseBranch != "" {
		args = append(args, "origin/"+spec.BaseBranch)
	}
	cmd := exec.CommandContext(checkoutCtx, "git", args...)
	cmd.Dir = repoDir
	cmd.Env = gitEnv(spec.Auth)
	if output, err := cmd.CombinedOutput(); err != nil {
		return commandError("git checkout", err, output)
	}
	return nil
}

func configureGitIdentity(ctx context.Context, repoDir string, identity *GitIdentity) error {
	name := defaultGitUserName
	email := defaultGitUserEmail
	if identity != nil {
		if identity.Name != "" {
			name = identity.Name
		}
		if identity.Email != "" {
			email = identity.Email
		}
	}
	for _, pair := range [][2]string{{"user.name", name}, {"user.email", email}} {
		cmd := exec.CommandContext(ctx, "git", "config", pair[0], pair[1])
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			return commandError("git config "+pair[0], err, output)
		}
	}
	return nil
}

func commitRepositoryChanges(ctx context.Context, repoDir string, spec *RepositoryWorkspaceSpec) (json.RawMessage, error) {
	statusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	statusCmd.Dir = repoDir
	statusOutput, err := statusCmd.Output()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(statusOutput)) == "" {
		return json.Marshal(map[string]interface{}{
			"repository": map[string]interface{}{"changed": false},
		})
	}
	addCmd := exec.CommandContext(ctx, "git", "add", "-A")
	addCmd.Dir = repoDir
	if output, err := addCmd.CombinedOutput(); err != nil {
		return nil, commandError("git add", err, output)
	}
	message := "Agent Runtime changes"
	if value := strings.TrimSpace(stringFromMetadata(spec.Metadata, "commit_message")); value != "" {
		message = value
	}
	commitCmd := exec.CommandContext(ctx, "git", "commit", "-m", message)
	commitCmd.Dir = repoDir
	if output, err := commitCmd.CombinedOutput(); err != nil {
		return nil, commandError("git commit", err, output)
	}
	revCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	revCmd.Dir = repoDir
	rev, err := revCmd.Output()
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]interface{}{
		"repository": map[string]interface{}{
			"changed": true,
			"commit":  strings.TrimSpace(string(rev)),
			"branch":  spec.WorkBranch,
		},
	})
}

func commitAndPushRepositoryChanges(ctx context.Context, repoDir string, spec *RepositoryWorkspaceSpec) (json.RawMessage, error) {
	summary, err := commitRepositoryChanges(ctx, repoDir, spec)
	if err != nil {
		return nil, err
	}
	var body map[string]interface{}
	_ = json.Unmarshal(summary, &body)
	repo, _ := body["repository"].(map[string]interface{})
	changed, _ := repo["changed"].(bool)
	branch := strings.TrimSpace(spec.WorkBranch)
	if branch == "" {
		branchBytes, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return nil, err
		}
		branch = strings.TrimSpace(string(branchBytes))
	}
	if branch == "" || branch == "HEAD" {
		return nil, fmt.Errorf("repository push requires a named work branch")
	}
	aheadCount, upstreamExists, err := repositoryAheadCount(ctx, repoDir, branch, spec.BaseBranch)
	if err != nil {
		return nil, err
	}
	shouldPush := changed || !upstreamExists || aheadCount > 0
	if !shouldPush {
		return summary, nil
	}
	if _, err := gitOutput(ctx, repoDir, spec.Auth, "push", "-u", "origin", branch); err != nil {
		return nil, err
	}
	repo["branch"] = branch
	repo["pushed"] = true
	repo["ahead_count"] = aheadCount
	repo["upstream_exists"] = upstreamExists
	if aheadCount > 0 {
		repo["changed"] = true
		if _, ok := repo["commit"]; !ok {
			rev, err := gitOutput(ctx, repoDir, nil, "rev-parse", "HEAD")
			if err != nil {
				return nil, err
			}
			repo["commit"] = strings.TrimSpace(string(rev))
		}
	}
	body["repository"] = repo
	return json.Marshal(body)
}

func repositoryAheadCount(ctx context.Context, repoDir, branch, baseBranch string) (int, bool, error) {
	upstream := "origin/" + strings.TrimSpace(branch)
	if strings.TrimSpace(branch) == "" {
		return 0, false, fmt.Errorf("repository ahead count requires a branch")
	}
	if _, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--verify", "--quiet", upstream); err != nil {
		count, err := repositoryAheadCountAgainstBase(ctx, repoDir, baseBranch)
		return count, false, err
	}
	output, err := gitOutput(ctx, repoDir, nil, "rev-list", "--count", upstream+"..HEAD")
	if err != nil {
		return 0, true, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, true, fmt.Errorf("parse repository ahead count: %w", err)
	}
	return count, true, nil
}

func repositoryAheadCountAgainstBase(ctx context.Context, repoDir, baseBranch string) (int, error) {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return 0, nil
	}
	base := "origin/" + baseBranch
	if _, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--verify", "--quiet", base); err != nil {
		return 0, nil
	}
	output, err := gitOutput(ctx, repoDir, nil, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, fmt.Errorf("parse repository base ahead count: %w", err)
	}
	return count, nil
}

func gitOutput(ctx context.Context, repoDir string, auth *RepositoryAuth, args ...string) ([]byte, error) {
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmdArgs := append(gitAuthArgs(auth), args...)
	cmd := exec.CommandContext(gitCtx, "git", cmdArgs...)
	cmd.Dir = repoDir
	cmd.Env = gitEnv(auth)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, commandError("git "+strings.Join(args, " "), err, output)
	}
	return output, nil
}

func gitAuthArgs(auth *RepositoryAuth) []string {
	if auth == nil {
		return nil
	}
	if auth.ExtraHeader != "" {
		return []string{"-c", "http.extraheader=" + auth.ExtraHeader}
	}
	if auth.Token == "" {
		return nil
	}
	switch auth.Type {
	case "github":
		return []string{"-c", "http.extraheader=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+auth.Token))}
	case "gitlab":
		return []string{"-c", "http.extraheader=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+auth.Token))}
	case "bearer":
		return []string{"-c", "http.extraheader=Authorization: Bearer " + auth.Token}
	default:
		if auth.Username != "" || auth.Password != "" {
			return []string{"-c", "http.extraheader=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(auth.Username+":"+firstNonEmpty(auth.Password, auth.Token)))}
		}
		return nil
	}
}

func gitEnv(auth *RepositoryAuth) []string {
	env := os.Environ()
	if auth == nil {
		return env
	}
	for key, value := range auth.Env {
		if strings.TrimSpace(key) != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func stableLeaseID(parts ...string) string {
	hash := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return "lease_" + hex.EncodeToString(hash[:])[:16]
}

func sanitizePathComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "run"
	}
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			out.WriteRune(r)
		case r >= '0' && r <= '9':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
	}
	value = strings.Trim(out.String(), "-")
	if value == "" {
		return "run"
	}
	return value
}

func commandError(label string, err error, output []byte) error {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return fmt.Errorf("%s failed: %w", label, err)
	}
	return fmt.Errorf("%s failed: %w: %s", label, err, text)
}

func stringFromMetadata(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
