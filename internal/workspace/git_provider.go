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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/procenv"
)

const (
	defaultGitUserName  = "Agent Runtime"
	defaultGitUserEmail = "agent-runtime@example.invalid"

	branchSyncNotApplicable     = "not_applicable"
	branchSyncSameBranch        = "same_branch"
	branchSyncUpToDate          = "up_to_date"
	branchSyncMerged            = "merged"
	branchSyncConflicted        = "conflicted"
	branchSyncRecreatedFromBase = "recreated_from_base"
	branchSyncUnrelatedHistory  = "unrelated_history"
)

type RepositoryProvider struct {
	RootDir      string
	SpecProvider RepositorySpecProvider
}

type branchSyncState struct {
	Status        string
	BaseBranch    string
	WorkBranch    string
	ConflictFiles []string
	BackupBranch  string
}

func (p RepositoryProvider) PrepareWorkspace(ctx context.Context, req PrepareRequest) (*agentcore.WorkspaceLease, error) {
	spec, err := p.resolveSpec(ctx, req)
	if err != nil {
		return nil, err
	}
	applyRepositoryAccessPolicy(spec, req.ExecutionConfig)
	root := strings.TrimSpace(p.RootDir)
	if root == "" {
		root = filepath.Join(os.TempDir(), "agent-runtime-workspaces")
	}
	runRoot := filepath.Join(SessionRoot(ctx, root), sanitizePathComponent(req.AppID), sanitizePathComponent(req.RunID))
	// A run may attach more than one repository dynamically. Keep each checkout
	// under a repository-specific directory; using a single runRoot/repo path
	// lets the second checkout replace the first while both leases continue to
	// claim different repository identities.
	repositoryRoot := filepath.Join(runRoot, "repositories", repositoryFingerprint(spec))
	repoDir := filepath.Join(repositoryRoot, "repo")
	if info, err := os.Stat(repoDir); err == nil && info.IsDir() {
		if ok, _ := repositoryCheckoutMatchesSpec(ctx, repoDir, spec); !ok {
			_ = os.RemoveAll(repositoryRoot)
		} else {
			// Identity must be configured before the sync: base/work branch
			// syncs create merge commits, which fail without user.name/email.
			if err := configureGitIdentity(ctx, repoDir, spec.CommitIdentity); err != nil {
				return nil, err
			}
			if err := excludeRuntimeArtifacts(ctx, repoDir); err != nil {
				return nil, err
			}
			syncState, err := syncRepositoryBaseIntoWorkBranch(ctx, repoDir, spec, req.RuntimeKind)
			if err != nil {
				return nil, err
			}
			return SessionLease(ctx, repositoryLease(req, spec, repoDir, syncState)), nil
		}
	}
	_ = os.RemoveAll(repositoryRoot)
	if err := os.MkdirAll(repositoryRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create repository workspace root: %w", err)
	}
	if err := cloneRepository(ctx, spec, repoDir); err != nil {
		_ = os.RemoveAll(repositoryRoot)
		return nil, err
	}
	if err := checkoutRepositoryBranch(ctx, repoDir, spec); err != nil {
		_ = os.RemoveAll(repositoryRoot)
		return nil, err
	}
	if err := configureGitIdentity(ctx, repoDir, spec.CommitIdentity); err != nil {
		_ = os.RemoveAll(repositoryRoot)
		return nil, err
	}
	if err := excludeRuntimeArtifacts(ctx, repoDir); err != nil {
		_ = os.RemoveAll(repositoryRoot)
		return nil, err
	}
	syncState, err := syncRepositoryBaseIntoWorkBranch(ctx, repoDir, spec, req.RuntimeKind)
	if err != nil {
		_ = os.RemoveAll(repositoryRoot)
		return nil, err
	}
	return SessionLease(ctx, repositoryLease(req, spec, repoDir, syncState)), nil
}

func (p RepositoryProvider) ValidateWorkspace(ctx context.Context, req PrepareRequest, lease agentcore.WorkspaceLease) (*agentcore.WorkspaceLease, bool, error) {
	if !LeaseInSession(ctx, &lease) {
		return nil, false, nil
	}
	spec, err := p.resolveSpec(ctx, req)
	if err != nil {
		return nil, false, err
	}
	applyRepositoryAccessPolicy(spec, req.ExecutionConfig)
	repoDir := strings.TrimSpace(lease.RootPath)
	if repoDir == "" {
		return nil, false, nil
	}
	if fingerprint := strings.TrimSpace(stringFromMetadata(lease.Metadata, "repository_fingerprint")); fingerprint != "" && fingerprint != repositoryFingerprint(spec) {
		return nil, false, nil
	}
	ok, err := repositoryCheckoutMatchesSpec(ctx, repoDir, spec)
	if err != nil || !ok {
		return nil, false, err
	}
	if err := configureGitIdentity(ctx, repoDir, spec.CommitIdentity); err != nil {
		return nil, false, err
	}
	syncState := branchSyncState{
		Status:     stringFromMetadata(lease.Metadata, "branch_sync_status"),
		BaseBranch: strings.TrimSpace(spec.BaseBranch),
		WorkBranch: strings.TrimSpace(spec.WorkBranch),
	}
	if syncState.Status == "" {
		syncState.Status = branchSyncNotApplicable
	}
	next := repositoryLease(req, spec, repoDir, syncState)
	next.ID = firstNonEmpty(lease.ID, next.ID)
	next.CleanupPolicy = firstNonEmpty(lease.CleanupPolicy, next.CleanupPolicy)
	if attached, ok := lease.Metadata["repository_workspaces"]; ok {
		next.Metadata["repository_workspaces"] = attached
	}
	return SessionLease(ctx, next), true, nil
}

func applyRepositoryAccessPolicy(spec *RepositoryWorkspaceSpec, executionConfig json.RawMessage) {
	if spec != nil && AccessModeFromConfig(executionConfig) == AccessReadOnly {
		spec.FinalizePolicy = RepositoryFinalizeNone
	}
}

func (p RepositoryProvider) resolveSpec(ctx context.Context, req PrepareRequest) (*RepositoryWorkspaceSpec, error) {
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
	return spec, nil
}

func (p RepositoryProvider) FinalizeWorkspace(ctx context.Context, req FinalizeRequest) (*FinalizeResult, error) {
	// Pauses preserve the checkout for review/resume; failures and cancellation
	// must not publish partial work. Only successful completion permits delivery.
	if req.Outcome != agentcore.RunStatusCompleted {
		return &FinalizeResult{}, nil
	}
	spec := req.Repository
	if spec == nil {
		spec = RepositorySpecFromLease(req.Lease)
	}
	NormalizeRepositorySpec(spec)
	if spec == nil || strings.TrimSpace(req.Lease.RootPath) == "" {
		return &FinalizeResult{}, nil
	}
	if spec.Metadata["delivery_mode"] == "preview" || req.Lease.Metadata["delivery_mode"] == "preview" {
		return &FinalizeResult{OutputSummary: json.RawMessage(`{"repository":{"delivery_mode":"preview","pushed":false}}`)}, nil
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
			mergeRepositoryAuth(spec, fresh)
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

func (p RepositoryProvider) CleanupWorkspace(ctx context.Context, req CleanupRequest) error {
	if strings.TrimSpace(req.AppID) == "" || strings.TrimSpace(req.RunID) == "" {
		return nil
	}
	root := strings.TrimSpace(p.RootDir)
	if root == "" {
		root = filepath.Join(os.TempDir(), "agent-runtime-workspaces")
	}
	// Cleanup is run-scoped so it removes the primary checkout and every
	// dynamically attached repository together.
	runRoot := filepath.Join(SessionRoot(ctx, root), sanitizePathComponent(req.AppID), sanitizePathComponent(req.RunID))
	return os.RemoveAll(runRoot)
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
		spec.Auth.Type = strings.ToLower(strings.TrimSpace(spec.Auth.Type))
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

// UpdateRepositoryLeaseBranch records a successful create_branch operation in
// every field used to validate the retained checkout on a later activity.
func UpdateRepositoryLeaseBranch(lease *agentcore.WorkspaceLease, branch string) error {
	if lease == nil {
		return fmt.Errorf("repository workspace lease is required")
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("repository branch is required")
	}
	if lease.Metadata == nil {
		lease.Metadata = map[string]interface{}{}
	}
	spec := RepositorySpecFromLease(*lease)
	if spec == nil {
		return fmt.Errorf("repository workspace spec is unavailable")
	}
	spec.WorkBranch = branch
	delete(spec.Metadata, "detached_head")
	lease.Metadata["work_branch"] = branch
	lease.Metadata["branch_sync_work_branch"] = branch
	delete(lease.Metadata, "detached_head")
	lease.Metadata["repository_spec"] = redactedRepositorySpec(spec)
	lease.Metadata["repository_fingerprint"] = repositoryFingerprint(spec)
	return nil
}

// UpdateRepositoryLeaseDetachedHead records an intentional detached checkout
// without moving HEAD or reconstructing the prior branch on the next activity.
func UpdateRepositoryLeaseDetachedHead(lease *agentcore.WorkspaceLease, commit string) error {
	if lease == nil {
		return fmt.Errorf("repository workspace lease is required")
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("detached repository commit is required")
	}
	if lease.Metadata == nil {
		lease.Metadata = map[string]interface{}{}
	}
	spec := RepositorySpecFromLease(*lease)
	if spec == nil {
		return fmt.Errorf("repository workspace spec is unavailable")
	}
	spec.WorkBranch = ""
	if spec.Metadata == nil {
		spec.Metadata = map[string]interface{}{}
	}
	spec.Metadata["detached_head"] = commit
	delete(lease.Metadata, "work_branch")
	delete(lease.Metadata, "branch_sync_work_branch")
	lease.Metadata["detached_head"] = commit
	lease.Metadata["repository_spec"] = redactedRepositorySpec(spec)
	lease.Metadata["repository_fingerprint"] = repositoryFingerprint(spec)
	return nil
}

func repositoryLease(req PrepareRequest, spec *RepositoryWorkspaceSpec, repoDir string, syncState branchSyncState) *agentcore.WorkspaceLease {
	RegisterRepositoryCache(repoDir, req.AppID, spec.Metadata)
	metadata := map[string]interface{}{}
	for key, value := range spec.Metadata {
		metadata[key] = value
	}
	metadata["repository_spec"] = redactedRepositorySpec(spec)
	metadata["repository_fingerprint"] = repositoryFingerprint(spec)
	metadata["clone_url"] = spec.CloneURL
	if spec.BaseBranch != "" {
		metadata["base_branch"] = spec.BaseBranch
	}
	if spec.WorkBranch != "" {
		metadata["work_branch"] = spec.WorkBranch
	}
	applyBranchSyncMetadata(metadata, syncState)
	cleanupPolicy := CleanupOnTerminal
	if spec.Metadata["delivery_mode"] == "preview" {
		cleanupPolicy = CleanupManual
	}
	return &agentcore.WorkspaceLease{
		ID:            stableLeaseID(req.AppID, req.RunID, spec.CloneURL),
		Provider:      "repository",
		RootPath:      repoDir,
		CleanupPolicy: cleanupPolicy,
		Metadata:      metadata,
	}
}

func applyBranchSyncMetadata(metadata map[string]interface{}, state branchSyncState) {
	if metadata == nil {
		return
	}
	status := strings.TrimSpace(state.Status)
	if status == "" {
		status = branchSyncNotApplicable
	}
	metadata["branch_sync_status"] = status
	if state.BaseBranch != "" {
		metadata["branch_sync_base_branch"] = state.BaseBranch
	}
	if state.WorkBranch != "" {
		metadata["branch_sync_work_branch"] = state.WorkBranch
	}
	if len(state.ConflictFiles) > 0 {
		metadata["branch_sync_conflict_files"] = append([]string(nil), state.ConflictFiles...)
	}
	if strings.TrimSpace(state.BackupBranch) != "" {
		metadata["branch_sync_backup_branch"] = strings.TrimSpace(state.BackupBranch)
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

func mergeRepositoryAuth(spec, fresh *RepositoryWorkspaceSpec) {
	if spec == nil || fresh == nil {
		return
	}
	spec.Auth = fresh.Auth
}

// runtimeArtifactsDir is created inside the checkout for staged skills. It
// must never reach a commit or a pushed branch, so it is excluded through the
// repository's private exclude file rather than a tracked .gitignore.
const runtimeArtifactsDir = ".agent-runtime"

func excludeRuntimeArtifacts(ctx context.Context, repoDir string) error {
	output, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	excludePath := strings.TrimSpace(string(output))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(repoDir, excludePath)
	}
	pattern := "/" + runtimeArtifactsDir + "/"
	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read git exclude file: %w", err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("create git exclude directory: %w", err)
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += pattern + "\n"
	if err := os.WriteFile(excludePath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write git exclude file: %w", err)
	}
	return nil
}

func cloneRepository(ctx context.Context, spec *RepositoryWorkspaceSpec, repoDir string) error {
	cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// Keep authentication process-scoped. `git clone -c ...` stores the value
	// in the new repository before fetching and requires a successful-clone
	// cleanup to remove it; a global `git -c ... clone` option applies to this
	// invocation only and cannot leave the credential behind on disk.
	args := append(gitAuthArgs(spec.Auth), "clone")
	if spec.BaseBranch != "" {
		args = append(args, "--branch", spec.BaseBranch)
	}
	args = append(args, spec.CloneURL, repoDir)
	cmd := exec.CommandContext(cloneCtx, "git", args...)
	cmd.Env = gitEnv(spec.Auth)
	if output, err := cmd.CombinedOutput(); err != nil {
		return commandError("git clone", err, output)
	}
	return nil
}

func checkoutRepositoryBranch(ctx context.Context, repoDir string, spec *RepositoryWorkspaceSpec) error {
	branch := strings.TrimSpace(spec.WorkBranch)
	if branch == "" {
		return nil
	}
	startPoint := ""
	if remoteBranchExists(ctx, repoDir, branch) {
		startPoint = "origin/" + branch
	} else if strings.TrimSpace(spec.BaseBranch) != "" {
		startPoint = "origin/" + strings.TrimSpace(spec.BaseBranch)
	}
	checkoutCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	args := []string{"checkout", "-B", branch}
	if startPoint != "" {
		args = append(args, startPoint)
	}
	cmd := exec.CommandContext(checkoutCtx, "git", args...)
	cmd.Dir = repoDir
	cmd.Env = gitEnv(spec.Auth)
	if output, err := cmd.CombinedOutput(); err != nil {
		return commandError("git checkout", err, output)
	}
	return nil
}

func remoteBranchExists(ctx context.Context, repoDir, branch string) bool {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false
	}
	ref := "refs/remotes/origin/" + branch
	if _, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--verify", "--quiet", ref); err == nil {
		return true
	}
	if _, err := gitOutput(ctx, repoDir, nil, "fetch", "origin", branch+":refs/remotes/origin/"+branch); err != nil {
		return false
	}
	_, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func repositoryCheckoutMatchesSpec(ctx context.Context, repoDir string, spec *RepositoryWorkspaceSpec) (bool, error) {
	if spec == nil || strings.TrimSpace(repoDir) == "" {
		return false, nil
	}
	if _, err := os.Stat(repoDir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if _, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--is-inside-work-tree"); err != nil {
		return false, nil
	}
	origin, err := gitOutput(ctx, repoDir, nil, "remote", "get-url", "origin")
	if err != nil {
		return false, nil
	}
	if normalizeRepositoryURL(string(origin)) != normalizeRepositoryURL(spec.CloneURL) {
		return false, nil
	}
	if branch := strings.TrimSpace(spec.WorkBranch); branch != "" {
		current, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil || strings.TrimSpace(string(current)) != branch {
			return false, nil
		}
	}
	return true, nil
}

func syncRepositoryBaseIntoWorkBranch(ctx context.Context, repoDir string, spec *RepositoryWorkspaceSpec, runtimeKind string) (branchSyncState, error) {
	state := branchSyncState{
		Status:     branchSyncNotApplicable,
		BaseBranch: strings.TrimSpace(spec.BaseBranch),
		WorkBranch: strings.TrimSpace(spec.WorkBranch),
	}
	if state.WorkBranch != "" {
		// Catch the local work branch up with its remote counterpart first:
		// reused workspaces (and runs racing another push to the same branch)
		// can otherwise sit behind origin and only discover it at push time,
		// when the run is already over and nobody can resolve a conflict.
		// Conflicts surfaced here go through the same handoff the base merge
		// uses, so the agent resolves them during the run.
		if err := syncRemoteWorkBranchIntoLocal(ctx, repoDir, spec, runtimeKind, &state); err != nil {
			return state, err
		}
		if state.Status == branchSyncConflicted {
			return state, nil
		}
	}
	if state.BaseBranch == "" || state.WorkBranch == "" {
		return state, nil
	}
	if state.BaseBranch == state.WorkBranch {
		state.Status = branchSyncSameBranch
		return state, nil
	}
	baseRef, err := fetchRemoteTrackingBranch(ctx, repoDir, spec.Auth, state.BaseBranch)
	if err != nil {
		return state, fmt.Errorf("fetch base branch for sync: %w", err)
	}
	needsMerge, err := workingBranchNeedsBaseSync(ctx, repoDir, spec.Auth, baseRef, state.BaseBranch, state.WorkBranch)
	if err != nil {
		if isUnrelatedHistoryError(err) {
			if err := recoverUnrelatedWorkingBranch(ctx, repoDir, spec, &state, baseRef); err != nil {
				state.Status = branchSyncUnrelatedHistory
				return state, err
			}
			return state, nil
		}
		return state, fmt.Errorf("check base sync status: %w", err)
	}
	if !needsMerge {
		state.Status = branchSyncUpToDate
		return state, nil
	}
	_, mergeErr := gitOutput(ctx, repoDir, spec.Auth, "merge", "--no-ff", "--no-edit", baseRef)
	if mergeErr == nil {
		state.Status = branchSyncMerged
		return state, nil
	}
	conflictFiles, conflictErr := gitMergeConflictFiles(ctx, repoDir)
	if conflictErr == nil && len(conflictFiles) > 0 {
		state.Status = branchSyncConflicted
		state.ConflictFiles = conflictFiles
		if runtimeSupportsMergeConflictHandoff(runtimeKind) {
			return state, nil
		}
		_, _ = gitOutput(ctx, repoDir, spec.Auth, "merge", "--abort")
		return state, fmt.Errorf("base branch sync produced merge conflicts that runtime %q cannot resolve: %s", strings.TrimSpace(runtimeKind), strings.Join(conflictFiles, ", "))
	}
	_, _ = gitOutput(ctx, repoDir, spec.Auth, "merge", "--abort")
	return state, fmt.Errorf("sync base branch into working branch: %w", mergeErr)
}

// syncRemoteWorkBranchIntoLocal merges origin/<work-branch> into the local
// work branch when the local tip is behind it. Best-effort on inspection
// failures (missing remote ref, shallow/unrelated history): those cases fall
// through to the base sync and the push-time merge retry.
func syncRemoteWorkBranchIntoLocal(ctx context.Context, repoDir string, spec *RepositoryWorkspaceSpec, runtimeKind string, state *branchSyncState) error {
	workRef, err := fetchRemoteTrackingBranch(ctx, repoDir, spec.Auth, state.WorkBranch)
	if err != nil {
		if isMissingRemoteRefError(err) {
			return nil
		}
		return fmt.Errorf("fetch remote work branch for sync: %w", err)
	}
	output, err := gitOutput(ctx, repoDir, spec.Auth, "rev-list", "--count", "HEAD.."+workRef)
	if err != nil {
		return nil
	}
	behind, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || behind == 0 {
		return nil
	}
	_, mergeErr := gitOutput(ctx, repoDir, spec.Auth, "merge", "--no-edit", workRef)
	if mergeErr == nil {
		return nil
	}
	conflictFiles, conflictErr := gitMergeConflictFiles(ctx, repoDir)
	if conflictErr == nil && len(conflictFiles) > 0 {
		state.Status = branchSyncConflicted
		state.ConflictFiles = conflictFiles
		if runtimeSupportsMergeConflictHandoff(runtimeKind) {
			return nil
		}
		_, _ = gitOutput(ctx, repoDir, spec.Auth, "merge", "--abort")
		return fmt.Errorf("remote work branch sync produced merge conflicts that runtime %q cannot resolve: %s", strings.TrimSpace(runtimeKind), strings.Join(conflictFiles, ", "))
	}
	_, _ = gitOutput(ctx, repoDir, spec.Auth, "merge", "--abort")
	return fmt.Errorf("sync remote work branch into local working branch: %w", mergeErr)
}

func runtimeSupportsMergeConflictHandoff(runtimeKind string) bool {
	switch strings.TrimSpace(runtimeKind) {
	case agentcore.RuntimeNativeSDK:
		return true
	default:
		return false
	}
}

func fetchRemoteTrackingBranch(ctx context.Context, repoDir string, auth *RepositoryAuth, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", fmt.Errorf("branch is required")
	}
	ref := "refs/remotes/origin/" + branch
	refspec := fmt.Sprintf("refs/heads/%s:%s", branch, ref)
	if _, err := gitOutput(ctx, repoDir, auth, "fetch", "origin", refspec); err != nil {
		return "", err
	}
	return "origin/" + branch, nil
}

func workingBranchNeedsBaseSync(ctx context.Context, repoDir string, auth *RepositoryAuth, baseRefName, baseBranch, workingBranch string) (bool, error) {
	mergeBase, err := gitOutput(ctx, repoDir, auth, "merge-base", "HEAD", strings.TrimSpace(baseRefName))
	if err != nil || strings.TrimSpace(string(mergeBase)) == "" {
		if retried, retryErr := retryMergeBaseAfterFetchingHistory(ctx, repoDir, auth, baseRefName, baseBranch, workingBranch); retryErr == nil && strings.TrimSpace(retried) != "" {
			mergeBase = []byte(retried)
			err = nil
		}
	}
	if err != nil || strings.TrimSpace(string(mergeBase)) == "" {
		return false, fmt.Errorf("unrelated history")
	}
	output, err := gitOutput(ctx, repoDir, auth, "rev-list", "--count", "HEAD.."+strings.TrimSpace(baseRefName))
	if err != nil {
		return false, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return false, fmt.Errorf("parse rev-list count: %w", err)
	}
	return count > 0, nil
}

func retryMergeBaseAfterFetchingHistory(ctx context.Context, repoDir string, auth *RepositoryAuth, baseRefName, baseBranch, workingBranch string) (string, error) {
	shallow, err := gitOutput(ctx, repoDir, auth, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(shallow)) != "true" {
		return "", fmt.Errorf("repository is not shallow")
	}
	refspecs := []string{}
	if branch := strings.TrimSpace(baseBranch); branch != "" {
		refspecs = append(refspecs, fmt.Sprintf("refs/heads/%s:refs/remotes/origin/%s", branch, branch))
	}
	if branch := strings.TrimSpace(workingBranch); branch != "" && branch != strings.TrimSpace(baseBranch) {
		refspecs = append(refspecs, fmt.Sprintf("refs/heads/%s:refs/remotes/origin/%s", branch, branch))
	}
	args := append([]string{"fetch", "--update-shallow", "--unshallow", "origin"}, refspecs...)
	if _, err := gitOutput(ctx, repoDir, auth, args...); err != nil {
		return "", err
	}
	out, err := gitOutput(ctx, repoDir, auth, "merge-base", "HEAD", strings.TrimSpace(baseRefName))
	if err != nil {
		return strings.TrimSpace(string(out)), err
	}
	return strings.TrimSpace(string(out)), nil
}

func recoverUnrelatedWorkingBranch(ctx context.Context, repoDir string, spec *RepositoryWorkspaceSpec, state *branchSyncState, baseRefName string) error {
	if spec == nil || state == nil {
		return fmt.Errorf("sync base branch into working branch: repository spec is required")
	}
	if metadataHasActivePR(spec.Metadata) {
		state.Status = branchSyncUnrelatedHistory
		return fmt.Errorf("sync base branch into working branch: working branch %q does not share history with base branch %q and has an active pull request", state.WorkBranch, state.BaseBranch)
	}
	head, err := gitOutput(ctx, repoDir, spec.Auth, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("sync base branch into working branch: resolve unrelated branch head: %w", err)
	}
	headSHA := strings.TrimSpace(string(head))
	backupBranch := buildUnrelatedHistoryBackupBranch(headSHA)
	if _, err := gitOutput(ctx, repoDir, spec.Auth, "branch", "-f", backupBranch, "HEAD"); err != nil {
		return fmt.Errorf("sync base branch into working branch: create backup branch: %w", err)
	}
	if _, err := gitOutput(ctx, repoDir, spec.Auth, "push", "-u", "origin", backupBranch); err != nil {
		return fmt.Errorf("sync base branch into working branch: push backup branch: %w", err)
	}
	if _, err := gitOutput(ctx, repoDir, spec.Auth, "checkout", "-B", state.WorkBranch, baseRefName); err != nil {
		return fmt.Errorf("sync base branch into working branch: recreate working branch from base: %w", err)
	}
	leaseRef := fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", state.WorkBranch, headSHA)
	if _, err := gitOutput(ctx, repoDir, spec.Auth, "push", leaseRef, "-u", "origin", state.WorkBranch); err != nil {
		return fmt.Errorf("sync base branch into working branch: reset remote working branch from base: %w", err)
	}
	state.Status = branchSyncRecreatedFromBase
	state.BackupBranch = backupBranch
	return nil
}

func gitMergeConflictFiles(ctx context.Context, repoDir string) ([]string, error) {
	output, err := gitOutput(ctx, repoDir, nil, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(strings.TrimSpace(string(output)))
	if len(fields) == 0 {
		return nil, nil
	}
	return fields, nil
}

func isUnrelatedHistoryError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unrelated")
}

func buildUnrelatedHistoryBackupBranch(headSHA string) string {
	shortSHA := strings.TrimSpace(headSHA)
	if len(shortSHA) > 12 {
		shortSHA = shortSHA[:12]
	}
	if shortSHA == "" {
		shortSHA = "unknown"
	}
	return fmt.Sprintf("agent-runtime-backup/unrelated-history/%s-%s", time.Now().UTC().Format("20060102150405"), shortSHA)
}

func metadataHasActivePR(metadata map[string]interface{}) bool {
	for _, key := range []string{"active_pr_number", "final_pr_number"} {
		value := metadata[key]
		switch typed := value.(type) {
		case int:
			if typed > 0 {
				return true
			}
		case int64:
			if typed > 0 {
				return true
			}
		case float64:
			if typed > 0 {
				return true
			}
		case string:
			if strings.TrimSpace(typed) != "" && strings.TrimSpace(typed) != "0" {
				return true
			}
		}
	}
	return false
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
		cmd.Env = procenv.Command()
		if output, err := cmd.CombinedOutput(); err != nil {
			return commandError("git config "+pair[0], err, output)
		}
	}
	return nil
}

func commitRepositoryChanges(ctx context.Context, repoDir string, spec *RepositoryWorkspaceSpec) (json.RawMessage, error) {
	if err := validateRepositoryNoUnresolvedConflicts(ctx, repoDir); err != nil {
		return nil, err
	}
	statusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	statusCmd.Dir = repoDir
	statusCmd.Env = procenv.Command()
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
	addCmd.Env = procenv.Command()
	if output, err := addCmd.CombinedOutput(); err != nil {
		return nil, commandError("git add", err, output)
	}
	if err := validateRepositoryStagedChanges(ctx, repoDir); err != nil {
		return nil, err
	}
	message := "Agent Runtime changes"
	if value := strings.TrimSpace(stringFromMetadata(spec.Metadata, "commit_message")); value != "" {
		message = value
	}
	commitCmd := exec.CommandContext(ctx, "git", "commit", "-m", message)
	commitCmd.Dir = repoDir
	commitCmd.Env = procenv.Command()
	if output, err := commitCmd.CombinedOutput(); err != nil {
		return nil, commandError("git commit", err, output)
	}
	revCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	revCmd.Dir = repoDir
	revCmd.Env = procenv.Command()
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
	shouldPush := changed || aheadCount > 0
	if !shouldPush {
		return summary, nil
	}
	if err := pushRepositoryBranchSafely(ctx, repoDir, spec.Auth, branch); err != nil {
		return nil, err
	}
	rev, err := gitOutput(ctx, repoDir, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	repo["branch"] = branch
	repo["pushed"] = true
	repo["commit"] = strings.TrimSpace(string(rev))
	repo["ahead_count"] = aheadCount
	repo["upstream_exists"] = upstreamExists
	if aheadCount > 0 {
		repo["changed"] = true
	}
	body["repository"] = repo
	return json.Marshal(body)
}

func validateRepositoryNoUnresolvedConflicts(ctx context.Context, repoDir string) error {
	conflicts, err := gitMergeConflictFiles(ctx, repoDir)
	if err != nil {
		return fmt.Errorf("verify merge resolution: %w", err)
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("cannot commit repository changes while merge conflicts remain unresolved: %s", strings.Join(conflicts, ", "))
	}
	return nil
}

func validateRepositoryStagedChanges(ctx context.Context, repoDir string) error {
	output, err := gitOutput(ctx, repoDir, nil, "diff", "--cached", "--check")
	if err != nil {
		text := strings.TrimSpace(string(output))
		normalized := strings.ToLower(text)
		if strings.Contains(normalized, "leftover conflict marker") || strings.Contains(normalized, "conflict marker") {
			return fmt.Errorf("cannot commit repository changes while merge conflict markers remain in staged files: %s", text)
		}
		return fmt.Errorf("verify staged changes: %w", err)
	}
	return nil
}

func pushRepositoryBranchSafely(ctx context.Context, repoDir string, auth *RepositoryAuth, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" || branch == "HEAD" {
		return fmt.Errorf("repository push requires a named work branch")
	}
	upstreamExists, err := fetchRemoteWorkBranchForPush(ctx, repoDir, auth, branch)
	if err != nil {
		return err
	}
	if upstreamExists {
		if err := mergeRemoteWorkBranchBeforePush(ctx, repoDir, auth, branch); err != nil {
			return err
		}
	}
	if _, err := gitOutput(ctx, repoDir, auth, "push", "-u", "origin", branch); err != nil {
		if !isNonFastForwardPushError(err) {
			return err
		}
		if err := mergeRemoteWorkBranchBeforePush(ctx, repoDir, auth, branch); err != nil {
			return err
		}
		if _, retryErr := gitOutput(ctx, repoDir, auth, "push", "-u", "origin", branch); retryErr != nil {
			return fmt.Errorf("git push: push still rejected after fetching and merging remote work branch %q: %w", branch, retryErr)
		}
	}
	return nil
}

func fetchRemoteWorkBranchForPush(ctx context.Context, repoDir string, auth *RepositoryAuth, branch string) (bool, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" || branch == "HEAD" {
		return false, fmt.Errorf("git push: branch name is required")
	}
	ref := "refs/remotes/origin/" + branch
	refspec := fmt.Sprintf("refs/heads/%s:%s", branch, ref)
	if _, err := gitOutput(ctx, repoDir, auth, "fetch", "origin", refspec); err != nil {
		if isMissingRemoteRefError(err) {
			return false, nil
		}
		return false, fmt.Errorf("git push: fetch remote work branch before push: %w", err)
	}
	return true, nil
}

func mergeRemoteWorkBranchBeforePush(ctx context.Context, repoDir string, auth *RepositoryAuth, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" || branch == "HEAD" {
		return fmt.Errorf("git push: branch name is required")
	}
	ref := "refs/remotes/origin/" + branch
	refspec := fmt.Sprintf("refs/heads/%s:%s", branch, ref)
	if _, err := gitOutput(ctx, repoDir, auth, "fetch", "origin", refspec); err != nil {
		return fmt.Errorf("git push: fetch remote work branch before push: %w", err)
	}
	if _, err := gitOutput(ctx, repoDir, auth, "merge", "--no-ff", "--no-edit", "origin/"+branch); err == nil {
		return nil
	}
	conflictFiles, conflictErr := gitMergeConflictFiles(ctx, repoDir)
	_, _ = gitOutput(ctx, repoDir, auth, "merge", "--abort")
	if conflictErr == nil && len(conflictFiles) > 0 {
		return fmt.Errorf("git push: remote work branch %q has changes that conflict with local changes: %s", branch, strings.Join(conflictFiles, ", "))
	}
	return fmt.Errorf("git push: merge remote work branch %q before push", branch)
}

func isMissingRemoteRefError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "couldn't find remote ref") ||
		strings.Contains(text, "could not find remote ref") ||
		strings.Contains(text, "couldn't find remote branch") ||
		strings.Contains(text, "could not find remote branch")
}

func isNonFastForwardPushError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "non-fast-forward") ||
		strings.Contains(text, "fetch first") ||
		strings.Contains(text, "updates were rejected") ||
		strings.Contains(text, "tip of your current branch is behind") ||
		strings.Contains(text, "failed to update ref") ||
		strings.Contains(text, "incorrect old value provided")
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
	count, err := repositoryRevCount(ctx, repoDir, upstream+"..HEAD")
	return count, true, err
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
	return repositoryRevCount(ctx, repoDir, base+"..HEAD")
}

func repositoryRevCount(ctx context.Context, repoDir, revRange string) (int, error) {
	output, err := gitOutput(ctx, repoDir, nil, "rev-list", "--count", revRange)
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, fmt.Errorf("parse repository rev count: %w", err)
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
		return output, commandError("git "+strings.Join(args, " "), err, output)
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
	switch strings.ToLower(strings.TrimSpace(auth.Type)) {
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
	overrides := map[string]string{
		// Repository setup is a host-owned background operation. It must fail
		// with the provider's authentication error instead of opening a terminal
		// credential prompt that no user can answer.
		"GIT_TERMINAL_PROMPT": "0",
		"GCM_INTERACTIVE":     "Never",
	}
	if auth != nil {
		for key, value := range auth.Env {
			if key = strings.TrimSpace(key); key != "" {
				overrides[key] = value
			}
		}
	}
	// Non-interactive behavior is mandatory even if a host auth environment
	// accidentally tries to override it.
	overrides["GIT_TERMINAL_PROMPT"] = "0"
	overrides["GCM_INTERACTIVE"] = "Never"

	// gitEnv serves trusted worker-side git only, never sandboxed commands, so
	// the host's git transport settings (CA bundle, SSH agent, GIT_CONFIG_*)
	// pass through alongside the sanitized base.
	base := procenv.HostGit()
	env := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, overridden := overrides[key]; overridden {
				continue
			}
		}
		env = append(env, entry)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+overrides[key])
	}
	return env
}

func stableLeaseID(parts ...string) string {
	hash := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return "lease_" + hex.EncodeToString(hash[:])[:16]
}

func repositoryFingerprint(spec *RepositoryWorkspaceSpec) string {
	if spec == nil {
		return ""
	}
	parts := []string{
		normalizeRepositoryURL(spec.CloneURL),
		strings.TrimSpace(spec.BaseBranch),
		strings.TrimSpace(spec.WorkBranch),
		stringFromMetadata(spec.Metadata, "repository_id"),
		stringFromMetadata(spec.Metadata, "repo_full_name"),
	}
	hash := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(hash[:])[:16]
}

func normalizeRepositoryURL(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimRight(value, "/")
	if strings.HasSuffix(value, ".git") {
		value = strings.TrimSuffix(value, ".git")
	}
	return value
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
