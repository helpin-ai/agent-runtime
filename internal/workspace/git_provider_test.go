package workspace

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestRepositoryProviderPreparesLocalGitWorkspace(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:   "git",
			CloneURL:   remote,
			BaseBranch: "main",
			WorkBranch: "agent/run-1",
			CommitIdentity: &GitIdentity{
				Name:  "Agent",
				Email: "agent@example.com",
			},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-1",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if lease == nil || lease.RootPath == "" {
		t.Fatalf("expected lease root path, got %#v", lease)
	}
	if _, err := os.Stat(filepath.Join(lease.RootPath, "README.md")); err != nil {
		t.Fatalf("expected cloned README: %v", err)
	}
	branch := runGitOutput(t, lease.RootPath, "branch", "--show-current")
	if branch != "agent/run-1" {
		t.Fatalf("expected work branch, got %q", branch)
	}
	if spec := RepositorySpecFromLease(*lease); spec == nil || spec.CloneURL != remote {
		t.Fatalf("expected redacted repository spec in lease, got %#v", lease.Metadata)
	}
}

func TestApplyRepositoryAccessPolicyDisablesFinalizationForReadOnlyWorkspace(t *testing.T) {
	spec := &RepositoryWorkspaceSpec{FinalizePolicy: RepositoryFinalizePushBranch}
	applyRepositoryAccessPolicy(spec, json.RawMessage(`{"workspace":{"access":"read_only"}}`))
	if spec.FinalizePolicy != RepositoryFinalizeNone {
		t.Fatalf("read-only finalize policy = %q, want %q", spec.FinalizePolicy, RepositoryFinalizeNone)
	}

	spec.FinalizePolicy = RepositoryFinalizePushBranch
	applyRepositoryAccessPolicy(spec, json.RawMessage(`{"workspace":{"access":"read_write"}}`))
	if spec.FinalizePolicy != RepositoryFinalizePushBranch {
		t.Fatalf("read-write finalize policy = %q, want push policy preserved", spec.FinalizePolicy)
	}
}

func TestGitHubAuthenticationIsProcessScopedAndNonInteractive(t *testing.T) {
	auth := &RepositoryAuth{
		Type:  " GitHub ",
		Token: "installation-token",
		Env: map[string]string{
			"GIT_TERMINAL_PROMPT": "1",
			"GCM_INTERACTIVE":     "Always",
		},
	}
	NormalizeRepositorySpec(&RepositoryWorkspaceSpec{Auth: auth})

	wantHeader := "http.extraheader=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:installation-token"))
	args := append(gitAuthArgs(auth), "clone", "https://github.com/acme/private.git", "/tmp/repo")
	if len(args) < 4 || args[0] != "-c" || args[1] != wantHeader || args[2] != "clone" {
		t.Fatalf("git clone args = %#v, want process-scoped auth before clone", args)
	}

	env := gitEnv(auth)
	if !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("git environment does not disable terminal prompts: %v", env)
	}
	if !slices.Contains(env, "GCM_INTERACTIVE=Never") {
		t.Fatalf("git environment does not disable credential-manager prompts: %v", env)
	}
	if slices.Contains(env, "GIT_TERMINAL_PROMPT=1") || slices.Contains(env, "GCM_INTERACTIVE=Always") {
		t.Fatalf("host auth environment re-enabled interactive authentication: %v", env)
	}
}

func TestCloneRepositorySendsGitHubInstallationAuthentication(t *testing.T) {
	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:installation-token"))
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		http.Error(w, "authentication deliberately rejected by test server", http.StatusUnauthorized)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := cloneRepository(ctx, &RepositoryWorkspaceSpec{
		CloneURL: server.URL + "/acme/private.git",
		Auth:     &RepositoryAuth{Type: "github", Token: "installation-token"},
	}, filepath.Join(t.TempDir(), "repo"))
	if err == nil {
		t.Fatal("cloneRepository unexpectedly succeeded against rejecting test server")
	}

	select {
	case got := <-received:
		if got != wantHeader {
			t.Fatalf("Authorization header = %q, want %q", got, wantHeader)
		}
	default:
		t.Fatal("git clone did not reach test server")
	}
}

func TestRepositoryProviderPushBranchFinalize(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/run-1",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{
				Name:  "Agent",
				Email: "agent@example.com",
			},
			Metadata: map[string]interface{}{"commit_message": "agent changes"},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-1",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lease.RootPath, "feature.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}

	result, err := provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:       "app-a",
		RunID:       "run-1",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:       *lease,
		Outcome:     agentcore.RunStatusCompleted,
	})
	if err != nil {
		t.Fatalf("finalize workspace: %v", err)
	}
	var summary struct {
		Repository struct {
			Changed bool   `json:"changed"`
			Pushed  bool   `json:"pushed"`
			Branch  string `json:"branch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(result.OutputSummary, &summary); err != nil {
		t.Fatalf("decode output summary: %v", err)
	}
	if !summary.Repository.Changed || !summary.Repository.Pushed || summary.Repository.Branch != "agent/run-1" {
		t.Fatalf("unexpected finalize summary: %s", string(result.OutputSummary))
	}
	refs := runGitOutput(t, tmp, "ls-remote", "--heads", remote, "agent/run-1")
	if !strings.Contains(refs, "refs/heads/agent/run-1") {
		t.Fatalf("expected pushed work branch, got %q", refs)
	}
}

func TestRepositoryProviderFinalizeRefreshesOnlyAuth(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")

	specProvider := &sequenceRepositorySpecProvider{specs: []*RepositoryWorkspaceSpec{
		{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/original",
			FinalizePolicy: RepositoryFinalizePushBranch,
			Auth:           &RepositoryAuth{Type: "bearer", Token: "prepare-token"},
			CommitIdentity: &GitIdentity{
				Name:  "Agent",
				Email: "agent@example.com",
			},
			Metadata: map[string]interface{}{"commit_message": "agent changes"},
		},
		{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/renamed",
			FinalizePolicy: RepositoryFinalizePushBranch,
			Auth:           &RepositoryAuth{Type: "bearer", Token: "finalize-token"},
			CommitIdentity: &GitIdentity{
				Name:  "Renamed",
				Email: "renamed@example.com",
			},
			Metadata: map[string]interface{}{"commit_message": "renamed changes"},
		},
	}}
	provider := RepositoryProvider{
		RootDir:      tmp,
		SpecProvider: specProvider,
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-branch-stability",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "task", ID: "task-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lease.RootPath, "feature.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}

	result, err := provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:       "app-a",
		RunID:       "run-branch-stability",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "task", ID: "task-1"},
		Lease:       *lease,
		Outcome:     agentcore.RunStatusCompleted,
	})
	if err != nil {
		t.Fatalf("finalize workspace: %v", err)
	}
	if specProvider.calls != 2 {
		t.Fatalf("expected prepare plus finalize spec resolution, got %d calls", specProvider.calls)
	}
	var summary struct {
		Repository struct {
			Changed bool   `json:"changed"`
			Pushed  bool   `json:"pushed"`
			Branch  string `json:"branch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(result.OutputSummary, &summary); err != nil {
		t.Fatalf("decode output summary: %v", err)
	}
	if !summary.Repository.Changed || !summary.Repository.Pushed || summary.Repository.Branch != "agent/original" {
		t.Fatalf("unexpected finalize summary: %s", string(result.OutputSummary))
	}
	refs := runGitOutput(t, tmp, "ls-remote", "--heads", remote)
	if !strings.Contains(refs, "refs/heads/agent/original") {
		t.Fatalf("expected pushed original work branch, got %q", refs)
	}
	if strings.Contains(refs, "refs/heads/agent/renamed") {
		t.Fatalf("did not expect renamed branch to be pushed, got %q", refs)
	}
}

func TestRepositoryProviderPushBranchFinalizePushesCleanAheadCommits(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/run-clean-ahead",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{
				Name:  "Agent",
				Email: "agent@example.com",
			},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-clean-ahead",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lease.RootPath, "committed.txt"), []byte("already committed\n"), 0o644); err != nil {
		t.Fatalf("write committed file: %v", err)
	}
	runGit(t, lease.RootPath, "add", "committed.txt")
	runGit(t, lease.RootPath, "commit", "-m", "agent committed change")
	status := runGitOutput(t, lease.RootPath, "status", "--porcelain")
	if status != "" {
		t.Fatalf("expected clean working tree, got %q", status)
	}

	result, err := provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:       "app-a",
		RunID:       "run-clean-ahead",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:       *lease,
		Outcome:     agentcore.RunStatusCompleted,
	})
	if err != nil {
		t.Fatalf("finalize workspace: %v", err)
	}
	var summary struct {
		Repository struct {
			Changed bool   `json:"changed"`
			Pushed  bool   `json:"pushed"`
			Branch  string `json:"branch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(result.OutputSummary, &summary); err != nil {
		t.Fatalf("decode output summary: %v", err)
	}
	if !summary.Repository.Changed || !summary.Repository.Pushed || summary.Repository.Branch != "agent/run-clean-ahead" {
		t.Fatalf("unexpected finalize summary for clean ahead commit: %s", string(result.OutputSummary))
	}
	refs := runGitOutput(t, tmp, "ls-remote", "--heads", remote, "agent/run-clean-ahead")
	if !strings.Contains(refs, "refs/heads/agent/run-clean-ahead") {
		t.Fatalf("expected pushed work branch, got %q", refs)
	}
}

func TestRepositoryProviderPushBranchFinalizeMergesRemoteWorkBranchBeforePush(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, seed, "checkout", "-B", "agent/non-fast-forward", "main")
	runGit(t, seed, "push", "-u", "origin", "agent/non-fast-forward")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/non-fast-forward",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{Name: "Agent", Email: "agent@example.com"},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-non-fast-forward",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}

	other := filepath.Join(tmp, "other")
	runGit(t, tmp, "clone", "--branch", "agent/non-fast-forward", remote, other)
	runGit(t, other, "config", "user.name", "Other")
	runGit(t, other, "config", "user.email", "other@example.com")
	if err := os.WriteFile(filepath.Join(other, "remote.txt"), []byte("remote branch update\n"), 0o644); err != nil {
		t.Fatalf("write remote file: %v", err)
	}
	runGit(t, other, "add", "remote.txt")
	runGit(t, other, "commit", "-m", "remote work branch update")
	runGit(t, other, "push", "origin", "agent/non-fast-forward")

	if err := os.WriteFile(filepath.Join(lease.RootPath, "local.txt"), []byte("local agent update\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	result, err := provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:       "app-a",
		RunID:       "run-non-fast-forward",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:       *lease,
		Outcome:     agentcore.RunStatusCompleted,
	})
	if err != nil {
		t.Fatalf("finalize workspace should merge remote branch before push: %v", err)
	}
	var summary struct {
		Repository struct {
			Changed bool   `json:"changed"`
			Pushed  bool   `json:"pushed"`
			Branch  string `json:"branch"`
			Commit  string `json:"commit"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(result.OutputSummary, &summary); err != nil {
		t.Fatalf("decode output summary: %v", err)
	}
	if !summary.Repository.Changed || !summary.Repository.Pushed || summary.Repository.Branch != "agent/non-fast-forward" || summary.Repository.Commit == "" {
		t.Fatalf("unexpected finalize summary: %s", string(result.OutputSummary))
	}
	verify := filepath.Join(tmp, "verify-non-fast-forward")
	runGit(t, tmp, "clone", "--branch", "agent/non-fast-forward", remote, verify)
	if _, err := os.Stat(filepath.Join(verify, "local.txt")); err != nil {
		t.Fatalf("expected local file pushed after merge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verify, "remote.txt")); err != nil {
		t.Fatalf("expected remote work branch file preserved after merge: %v", err)
	}
}

func TestRepositoryProviderPushBranchFinalizeRetriesNonFastForwardPush(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, seed, "checkout", "-B", "agent/racy-push", "main")
	runGit(t, seed, "push", "-u", "origin", "agent/racy-push")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/racy-push",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{Name: "Agent", Email: "agent@example.com"},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-racy-push",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}

	marker := filepath.Join(tmp, "pre-push-ran")
	racer := filepath.Join(tmp, "racer")
	hook := "#!/bin/sh\n" +
		"set -eu\n" +
		"if [ -f " + shellQuote(marker) + " ]; then exit 0; fi\n" +
		"touch " + shellQuote(marker) + "\n" +
		"git clone " + shellQuote(remote) + " " + shellQuote(racer) + "\n" +
		"cd " + shellQuote(racer) + "\n" +
		"git checkout agent/racy-push\n" +
		"git config user.name Racer\n" +
		"git config user.email racer@example.com\n" +
		"printf 'remote race update\\n' > race.txt\n" +
		"git add race.txt\n" +
		"git commit -m 'remote race update'\n" +
		"git push origin agent/racy-push\n"
	hookPath := filepath.Join(lease.RootPath, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		t.Fatalf("write pre-push hook: %v", err)
	}

	if err := os.WriteFile(filepath.Join(lease.RootPath, "local.txt"), []byte("local agent update\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	result, err := provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:       "app-a",
		RunID:       "run-racy-push",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:       *lease,
		Outcome:     agentcore.RunStatusCompleted,
	})
	if err != nil {
		t.Fatalf("finalize workspace should merge and retry a racy non-fast-forward push: %v", err)
	}
	var summary struct {
		Repository struct {
			Pushed bool   `json:"pushed"`
			Branch string `json:"branch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(result.OutputSummary, &summary); err != nil {
		t.Fatalf("decode output summary: %v", err)
	}
	if !summary.Repository.Pushed || summary.Repository.Branch != "agent/racy-push" {
		t.Fatalf("unexpected finalize summary: %s", string(result.OutputSummary))
	}
	verify := filepath.Join(tmp, "verify-racy-push")
	runGit(t, tmp, "clone", "--branch", "agent/racy-push", remote, verify)
	if _, err := os.Stat(filepath.Join(verify, "local.txt")); err != nil {
		t.Fatalf("expected local file pushed after retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verify, "race.txt")); err != nil {
		t.Fatalf("expected racy remote file preserved after retry merge: %v", err)
	}
}

func TestIsNonFastForwardPushErrorRecognizesConcurrentRefUpdate(t *testing.T) {
	err := errors.New("remote: error: cannot lock ref 'refs/heads/agent/racy-push': is at f695179 but expected ee449d5\nremote rejected: incorrect old value provided")
	if !isNonFastForwardPushError(err) {
		t.Fatal("concurrent remote ref update should be treated as a retryable non-fast-forward push")
	}
}

func TestRepositoryProviderStartsFromExistingRemoteWorkBranch(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, seed, "checkout", "-B", "agent/existing-run", "main")
	if err := os.WriteFile(filepath.Join(seed, "existing.txt"), []byte("existing branch work\n"), 0o644); err != nil {
		t.Fatalf("write existing branch file: %v", err)
	}
	runGit(t, seed, "add", "existing.txt")
	runGit(t, seed, "commit", "-m", "existing branch work")
	runGit(t, seed, "push", "-u", "origin", "agent/existing-run")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/existing-run",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{
				Name:  "Agent",
				Email: "agent@example.com",
			},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-existing-branch",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lease.RootPath, "existing.txt")); err != nil {
		t.Fatalf("expected existing remote work branch contents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lease.RootPath, "new.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	result, err := provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:       "app-a",
		RunID:       "run-existing-branch",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:       *lease,
		Outcome:     agentcore.RunStatusCompleted,
	})
	if err != nil {
		t.Fatalf("finalize workspace: %v", err)
	}
	var summary struct {
		Repository struct {
			Changed bool   `json:"changed"`
			Pushed  bool   `json:"pushed"`
			Branch  string `json:"branch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(result.OutputSummary, &summary); err != nil {
		t.Fatalf("decode output summary: %v", err)
	}
	if !summary.Repository.Changed || !summary.Repository.Pushed || summary.Repository.Branch != "agent/existing-run" {
		t.Fatalf("unexpected finalize summary: %s", string(result.OutputSummary))
	}
	verify := filepath.Join(tmp, "verify")
	runGit(t, tmp, "clone", "--branch", "agent/existing-run", remote, verify)
	if _, err := os.Stat(filepath.Join(verify, "existing.txt")); err != nil {
		t.Fatalf("expected existing file to remain on remote branch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verify, "new.txt")); err != nil {
		t.Fatalf("expected new file to be pushed to remote branch: %v", err)
	}
}

func TestRepositoryProviderSyncsBaseIntoExistingWorkBranch(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, seed, "checkout", "-B", "agent/sync", "main")
	if err := os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	runGit(t, seed, "add", "feature.txt")
	runGit(t, seed, "commit", "-m", "feature")
	runGit(t, seed, "push", "-u", "origin", "agent/sync")
	runGit(t, seed, "checkout", "main")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base file: %v", err)
	}
	runGit(t, seed, "add", "base.txt")
	runGit(t, seed, "commit", "-m", "base change")
	runGit(t, seed, "push", "origin", "main")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/sync",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{Name: "Agent", Email: "agent@example.com"},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-sync",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if got := lease.Metadata["branch_sync_status"]; got != branchSyncMerged {
		t.Fatalf("branch sync status = %#v, want %q", got, branchSyncMerged)
	}
	if _, err := os.Stat(filepath.Join(lease.RootPath, "base.txt")); err != nil {
		t.Fatalf("expected base change merged into workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lease.RootPath, "feature.txt")); err != nil {
		t.Fatalf("expected existing feature branch content preserved: %v", err)
	}
	if _, err := provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:       "app-a",
		RunID:       "run-sync",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:       *lease,
		Outcome:     agentcore.RunStatusCompleted,
	}); err != nil {
		t.Fatalf("finalize workspace: %v", err)
	}
	verify := filepath.Join(tmp, "verify-sync")
	runGit(t, tmp, "clone", "--branch", "agent/sync", remote, verify)
	if _, err := os.Stat(filepath.Join(verify, "base.txt")); err != nil {
		t.Fatalf("expected merged base file pushed to remote branch: %v", err)
	}
}

// A reused workspace whose local work branch fell behind origin (another run
// pushed to the same branch in the meantime) must catch up at prepare time so
// the agent works on the remote tip instead of failing the final push.
func TestRepositoryProviderSyncsRemoteWorkBranchIntoReusedWorkspace(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, seed, "checkout", "-B", "agent/behind", "main")
	runGit(t, seed, "push", "-u", "origin", "agent/behind")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/behind",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{Name: "Agent", Email: "agent@example.com"},
		}},
	}
	request := PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-behind",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	}
	if _, err := provider.PrepareWorkspace(context.Background(), request); err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}

	// Another run pushes to the same work branch while this workspace exists.
	if err := os.WriteFile(filepath.Join(seed, "upstream.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatalf("write upstream file: %v", err)
	}
	runGit(t, seed, "add", "upstream.txt")
	runGit(t, seed, "commit", "-m", "upstream work")
	runGit(t, seed, "push", "origin", "agent/behind")

	lease, err := provider.PrepareWorkspace(context.Background(), request)
	if err != nil {
		t.Fatalf("re-prepare workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lease.RootPath, "upstream.txt")); err != nil {
		t.Fatalf("expected remote work branch commit synced into reused workspace: %v", err)
	}

	if err := os.WriteFile(filepath.Join(lease.RootPath, "local.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	if _, err := provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:       "app-a",
		RunID:       "run-behind",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:       *lease,
		Outcome:     agentcore.RunStatusCompleted,
	}); err != nil {
		t.Fatalf("finalize workspace: %v", err)
	}
	verify := filepath.Join(tmp, "verify-behind")
	runGit(t, tmp, "clone", "--branch", "agent/behind", remote, verify)
	for _, name := range []string{"upstream.txt", "local.txt"} {
		if _, err := os.Stat(filepath.Join(verify, name)); err != nil {
			t.Fatalf("expected %s on remote branch after finalize: %v", name, err)
		}
	}
}

func TestRepositoryProviderExposesMergeConflictsAndBlocksFinalize(t *testing.T) {
	tmp, remote := setupConflictingRepository(t)

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/conflict",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{Name: "Agent", Email: "agent@example.com"},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-conflict",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if got := lease.Metadata["branch_sync_status"]; got != branchSyncConflicted {
		t.Fatalf("branch sync status = %#v, want %q", got, branchSyncConflicted)
	}
	conflicts, _ := lease.Metadata["branch_sync_conflict_files"].([]string)
	if len(conflicts) != 1 || conflicts[0] != "conflict.txt" {
		t.Fatalf("conflict files = %#v, want [conflict.txt]", lease.Metadata["branch_sync_conflict_files"])
	}
	_, err = provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:       "app-a",
		RunID:       "run-conflict",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:       *lease,
		Outcome:     agentcore.RunStatusCompleted,
	})
	if err == nil || !strings.Contains(err.Error(), "merge conflicts remain unresolved") {
		t.Fatalf("expected unresolved conflict finalize error, got %v", err)
	}
}

func TestRepositoryProviderRejectsMergeConflictsForNativeSDK(t *testing.T) {
	tmp, remote := setupConflictingRepository(t)
	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/conflict",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{Name: "Agent", Email: "agent@example.com"},
		}},
	}
	_, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-conflict-native",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeNativeSDK,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot resolve") {
		t.Fatalf("expected native merge conflict prepare error, got %v", err)
	}
}

func setupConflictingRepository(t *testing.T) (string, string) {
	t.Helper()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "conflict.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "conflict.txt")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, seed, "checkout", "-B", "agent/conflict", "main")
	if err := os.WriteFile(filepath.Join(seed, "conflict.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature conflict file: %v", err)
	}
	runGit(t, seed, "add", "conflict.txt")
	runGit(t, seed, "commit", "-m", "feature")
	runGit(t, seed, "push", "-u", "origin", "agent/conflict")
	runGit(t, seed, "checkout", "main")
	if err := os.WriteFile(filepath.Join(seed, "conflict.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base conflict file: %v", err)
	}
	runGit(t, seed, "add", "conflict.txt")
	runGit(t, seed, "commit", "-m", "base")
	runGit(t, seed, "push", "origin", "main")
	return tmp, remote
}

func TestRepositoryProviderRecoversUnrelatedWorkBranchWithoutActivePR(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "main")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, seed, "checkout", "--orphan", "agent/unrelated")
	runGit(t, seed, "rm", "-rf", ".")
	if err := os.WriteFile(filepath.Join(seed, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
	runGit(t, seed, "add", "other.txt")
	runGit(t, seed, "commit", "-m", "unrelated")
	runGit(t, seed, "push", "-u", "origin", "agent/unrelated")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:       "git",
			CloneURL:       remote,
			BaseBranch:     "main",
			WorkBranch:     "agent/unrelated",
			FinalizePolicy: RepositoryFinalizePushBranch,
			CommitIdentity: &GitIdentity{Name: "Agent", Email: "agent@example.com"},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-unrelated",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if got := lease.Metadata["branch_sync_status"]; got != branchSyncRecreatedFromBase {
		t.Fatalf("branch sync status = %#v, want %q", got, branchSyncRecreatedFromBase)
	}
	backup := strings.TrimSpace(runGitOutput(t, tmp, "ls-remote", "--heads", remote, "agent-runtime-backup/unrelated-history/*"))
	if !strings.Contains(backup, "agent-runtime-backup/unrelated-history/") {
		t.Fatalf("expected backup branch, got %q", backup)
	}
	if _, err := os.Stat(filepath.Join(lease.RootPath, "README.md")); err != nil {
		t.Fatalf("expected recreated branch from base: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lease.RootPath, "other.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected unrelated file removed from recreated branch, err=%v", err)
	}
}

func TestRepositoryProviderRejectsUnrelatedWorkBranchWithActivePR(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "commit", "-m", "main")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, seed, "checkout", "--orphan", "agent/unrelated-pr")
	runGit(t, seed, "rm", "-rf", ".")
	if err := os.WriteFile(filepath.Join(seed, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
	runGit(t, seed, "add", "other.txt")
	runGit(t, seed, "commit", "-m", "unrelated")
	runGit(t, seed, "push", "-u", "origin", "agent/unrelated-pr")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:   "git",
			CloneURL:   remote,
			BaseBranch: "main",
			WorkBranch: "agent/unrelated-pr",
			Metadata:   map[string]interface{}{"active_pr_number": 42},
		}},
	}
	_, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-unrelated-pr",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err == nil || !strings.Contains(err.Error(), "active pull request") {
		t.Fatalf("expected active PR unrelated-history error, got %v", err)
	}
}

func TestRepositoryProviderValidateRejectsWrongRepositoryLease(t *testing.T) {
	tmp := t.TempDir()
	remoteA := filepath.Join(tmp, "remote-a.git")
	remoteB := filepath.Join(tmp, "remote-b.git")
	seedA := filepath.Join(tmp, "seed-a")
	seedB := filepath.Join(tmp, "seed-b")
	for _, setup := range []struct {
		remote string
		seed   string
		text   string
	}{
		{remoteA, seedA, "a\n"},
		{remoteB, seedB, "b\n"},
	} {
		runGit(t, tmp, "init", "--bare", setup.remote)
		runGit(t, tmp, "clone", setup.remote, setup.seed)
		runGit(t, setup.seed, "config", "user.name", "Test")
		runGit(t, setup.seed, "config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(setup.seed, "README.md"), []byte(setup.text), 0o644); err != nil {
			t.Fatalf("write seed file: %v", err)
		}
		runGit(t, setup.seed, "add", "README.md")
		runGit(t, setup.seed, "commit", "-m", "initial")
		runGit(t, setup.seed, "branch", "-M", "main")
		runGit(t, setup.seed, "push", "-u", "origin", "main")
	}
	providerA := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:   "git",
			CloneURL:   remoteA,
			BaseBranch: "main",
			WorkBranch: "agent/wrong",
		}},
	}
	lease, err := providerA.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-wrong-lease",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-a"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	providerB := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:   "git",
			CloneURL:   remoteB,
			BaseBranch: "main",
			WorkBranch: "agent/wrong",
		}},
	}
	_, valid, err := providerB.ValidateWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-wrong-lease",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-b"},
	}, *lease)
	if err != nil {
		t.Fatalf("validate workspace: %v", err)
	}
	if valid {
		t.Fatal("expected wrong repository lease to be rejected")
	}
}

func TestRepositoryProviderKeepsMultipleRepositoriesForOneRunIndependent(t *testing.T) {
	tmp := t.TempDir()
	type repositoryFixture struct {
		remote string
		seed   string
		text   string
	}
	fixtures := []repositoryFixture{
		{remote: filepath.Join(tmp, "events.git"), seed: filepath.Join(tmp, "events-seed"), text: "rust-pipeline\n"},
		{remote: filepath.Join(tmp, "website.git"), seed: filepath.Join(tmp, "website-seed"), text: "nextjs-website\n"},
	}
	for _, fixture := range fixtures {
		runGit(t, tmp, "init", "--bare", fixture.remote)
		runGit(t, tmp, "clone", fixture.remote, fixture.seed)
		runGit(t, fixture.seed, "config", "user.name", "Test")
		runGit(t, fixture.seed, "config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(fixture.seed, "README.md"), []byte(fixture.text), 0o644); err != nil {
			t.Fatalf("write fixture README: %v", err)
		}
		runGit(t, fixture.seed, "add", "README.md")
		runGit(t, fixture.seed, "commit", "-m", "initial")
		runGit(t, fixture.seed, "branch", "-M", "main")
		runGit(t, fixture.seed, "push", "-u", "origin", "main")
	}

	request := PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-multi-repo",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeNativeSDK,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo"},
	}
	prepare := func(fixture repositoryFixture, repositoryID string) *agentcore.WorkspaceLease {
		t.Helper()
		provider := RepositoryProvider{
			RootDir: tmp,
			SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
				Provider:   "git",
				CloneURL:   fixture.remote,
				BaseBranch: "main",
				Metadata:   map[string]interface{}{"repository_id": repositoryID},
			}},
		}
		lease, err := provider.PrepareWorkspace(context.Background(), request)
		if err != nil {
			t.Fatalf("prepare %s: %v", repositoryID, err)
		}
		return lease
	}

	eventsLease := prepare(fixtures[0], "events")
	websiteLease := prepare(fixtures[1], "website")
	if eventsLease.RootPath == websiteLease.RootPath {
		t.Fatalf("multi-repository leases share root %q", eventsLease.RootPath)
	}
	for _, check := range []struct {
		lease *agentcore.WorkspaceLease
		want  string
	}{
		{eventsLease, fixtures[0].text},
		{websiteLease, fixtures[1].text},
	} {
		body, err := os.ReadFile(filepath.Join(check.lease.RootPath, "README.md"))
		if err != nil {
			t.Fatalf("read checkout README: %v", err)
		}
		if string(body) != check.want {
			t.Fatalf("checkout at %s contains %q, want %q", check.lease.RootPath, body, check.want)
		}
	}

	provider := RepositoryProvider{RootDir: tmp}
	if err := provider.CleanupWorkspace(context.Background(), CleanupRequest{
		AppID: "app-a", RunID: "run-multi-repo", Lease: *eventsLease,
	}); err != nil {
		t.Fatalf("cleanup multi-repository run: %v", err)
	}
	for _, lease := range []*agentcore.WorkspaceLease{eventsLease, websiteLease} {
		if _, err := os.Stat(lease.RootPath); !os.IsNotExist(err) {
			t.Fatalf("checkout root %s remains after cleanup: %v", lease.RootPath, err)
		}
	}
}

type staticRepositorySpecProvider struct {
	spec *RepositoryWorkspaceSpec
}

func (p staticRepositorySpecProvider) ResolveRepositoryWorkspace(context.Context, PrepareRequest) (*RepositoryWorkspaceSpec, error) {
	return p.spec, nil
}

type sequenceRepositorySpecProvider struct {
	specs []*RepositoryWorkspaceSpec
	calls int
}

func (p *sequenceRepositorySpecProvider) ResolveRepositoryWorkspace(context.Context, PrepareRequest) (*RepositoryWorkspaceSpec, error) {
	if len(p.specs) == 0 {
		return nil, nil
	}
	index := p.calls
	if index >= len(p.specs) {
		index = len(p.specs) - 1
	}
	p.calls++
	return p.specs[index], nil
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, string(output))
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return string(bytesTrimSpace(output))
}

func shellQuote(value string) string {
	return strconv.Quote(value)
}

func bytesTrimSpace(value []byte) []byte {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\n' || value[0] == '\t' || value[0] == '\r') {
		value = value[1:]
	}
	for len(value) > 0 {
		last := value[len(value)-1]
		if last != ' ' && last != '\n' && last != '\t' && last != '\r' {
			break
		}
		value = value[:len(value)-1]
	}
	return value
}
