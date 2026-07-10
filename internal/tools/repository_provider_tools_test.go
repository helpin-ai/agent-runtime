package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func clearRepoProviderTokenEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{repoProviderTokenEnvVar, "GITHUB_TOKEN", "GH_TOKEN", repoProviderAPIBaseEnvVar} {
		t.Setenv(key, "")
	}
}

func repoProviderTestCallContext(t *testing.T, root string, metadata map[string]interface{}) CallContext {
	t.Helper()
	run := &agentcore.AgentRun{
		ID:    "run-1",
		AppID: "app-a",
		WorkspaceLease: &agentcore.WorkspaceLease{
			ID:       "lease-1",
			RootPath: root,
			Metadata: metadata,
		},
	}
	return CallContext{AppID: run.AppID, RunID: run.ID, Run: run}
}

func initRepoProviderTestGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, output)
		}
	}
	runGit("init", "-b", "main")
	if err := writeRepoProviderTestFile(root, "app.go", "package main\n"); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "initial")
	runGit("checkout", "-b", "feature")
	if err := writeRepoProviderTestFile(root, "app.go", "package main\n\nfunc Added() {}\n"); err != nil {
		t.Fatalf("update fixture: %v", err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "feature change")
	return root
}

func writeRepoProviderTestFile(root, name, content string) error {
	return os.WriteFile(filepath.Join(root, name), []byte(content), 0o644)
}

func TestRepositoryProviderToolsRegisteredAsNonMutating(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"get_pull_request_diff", "get_check_run_logs"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s to be registered", name)
		}
		if def.Mutating {
			t.Fatalf("expected %s to be non-mutating", name)
		}
		if def.Category != "Git" {
			t.Fatalf("expected %s category Git, got %q", name, def.Category)
		}
	}
}

func TestParseRepoProviderCloneURL(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cloneURL  string
		wantHost  string
		wantOwner string
		wantRepo  string
	}{
		{name: "https with .git", cloneURL: "https://github.com/helpin-ai/agent-runtime.git", wantHost: "github.com", wantOwner: "helpin-ai", wantRepo: "agent-runtime"},
		{name: "https without .git", cloneURL: "https://github.com/acme/widgets", wantHost: "github.com", wantOwner: "acme", wantRepo: "widgets"},
		{name: "ssh", cloneURL: "git@github.com:acme/widgets.git", wantHost: "github.com", wantOwner: "acme", wantRepo: "widgets"},
		{name: "enterprise host", cloneURL: "https://ghe.example.com/team/tool.git", wantHost: "ghe.example.com", wantOwner: "team", wantRepo: "tool"},
		{name: "empty", cloneURL: "", wantHost: "", wantOwner: "", wantRepo: ""},
		{name: "garbage", cloneURL: "not a url", wantHost: "", wantOwner: "", wantRepo: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, owner, repo := parseRepoProviderCloneURL(tc.cloneURL)
			if host != tc.wantHost || owner != tc.wantOwner || repo != tc.wantRepo {
				t.Fatalf("got host=%q owner=%q repo=%q, want host=%q owner=%q repo=%q", host, owner, repo, tc.wantHost, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

func TestResolveRepoProviderRefFromLeaseSpec(t *testing.T) {
	callCtx := repoProviderTestCallContext(t, t.TempDir(), map[string]interface{}{
		"repository_spec": map[string]interface{}{
			"provider":  "github",
			"clone_url": "https://github.com/helpin-ai/agent-runtime.git",
		},
	})
	ref := resolveRepoProviderRef(callCtx, "", "")
	if ref.Owner != "helpin-ai" || ref.Repo != "agent-runtime" || ref.Provider != "github" {
		t.Fatalf("unexpected ref %+v", ref)
	}
	// Explicit params win.
	ref = resolveRepoProviderRef(callCtx, "other", "repo")
	if ref.Owner != "other" || ref.Repo != "repo" {
		t.Fatalf("expected explicit params to win, got %+v", ref)
	}
}

func TestGetPullRequestDiffLocalFallbackWithoutToken(t *testing.T) {
	clearRepoProviderTokenEnv(t)
	root := initRepoProviderTestGitRepo(t)
	registry := NewRegistry()
	callCtx := repoProviderTestCallContext(t, root, map[string]interface{}{
		"base_branch": "main",
		"repository_spec": map[string]interface{}{
			"provider":    "github",
			"clone_url":   "https://github.com/acme/widgets.git",
			"base_branch": "main",
		},
	})

	output, err := registry.Execute(context.Background(), callCtx, "get_pull_request_diff", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get_pull_request_diff returned error: %v", err)
	}
	var result struct {
		Mode      string `json:"mode"`
		Base      string `json:"base"`
		Range     string `json:"range"`
		Diff      string `json:"diff"`
		Stat      string `json:"stat"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v; raw=%s", err, string(output))
	}
	if result.Mode != "local_diff" || result.Base != "main" {
		t.Fatalf("unexpected result %+v", result)
	}
	if !strings.Contains(result.Diff, "func Added()") {
		t.Fatalf("expected diff to contain feature change, got %q", result.Diff)
	}
	if !strings.Contains(result.Stat, "app.go") {
		t.Fatalf("expected stat to mention app.go, got %q", result.Stat)
	}
}

func TestGetPullRequestDiffWithPullNumberButNoTokenFallsBackWithNote(t *testing.T) {
	clearRepoProviderTokenEnv(t)
	root := initRepoProviderTestGitRepo(t)
	registry := NewRegistry()
	callCtx := repoProviderTestCallContext(t, root, map[string]interface{}{
		"base_branch": "main",
		"repository_spec": map[string]interface{}{
			"provider":  "github",
			"clone_url": "https://github.com/acme/widgets.git",
		},
	})

	output, err := registry.Execute(context.Background(), callCtx, "get_pull_request_diff", json.RawMessage(`{"pull_number":7}`))
	if err != nil {
		t.Fatalf("get_pull_request_diff returned error: %v", err)
	}
	var result struct {
		Mode       string `json:"mode"`
		PullNumber int    `json:"pull_number"`
		Note       string `json:"note"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.Mode != "local_diff" || result.PullNumber != 7 {
		t.Fatalf("unexpected result %+v", result)
	}
	if !strings.Contains(result.Note, "no GitHub token") {
		t.Fatalf("expected token note, got %q", result.Note)
	}
}

func TestGetPullRequestDiffUsesGitHubAPIWithEnvToken(t *testing.T) {
	clearRepoProviderTokenEnv(t)
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"filename":"main.go","status":"modified","additions":3,"deletions":1,"changes":4,"patch":"@@ -1 +1 @@"}]`))
	}))
	defer server.Close()
	t.Setenv(repoProviderTokenEnvVar, "test-token")
	t.Setenv(repoProviderAPIBaseEnvVar, server.URL)

	registry := NewRegistry()
	callCtx := repoProviderTestCallContext(t, t.TempDir(), map[string]interface{}{
		"repository_spec": map[string]interface{}{
			"provider":  "github",
			"clone_url": "https://github.com/acme/widgets.git",
		},
	})
	output, err := registry.Execute(context.Background(), callCtx, "get_pull_request_diff", json.RawMessage(`{"pull_number":42}`))
	if err != nil {
		t.Fatalf("get_pull_request_diff returned error: %v", err)
	}
	var result struct {
		Mode  string `json:"mode"`
		Owner string `json:"owner"`
		Repo  string `json:"repo"`
		Files []struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		} `json:"files"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.Mode != "github_api" || result.Owner != "acme" || result.Repo != "widgets" {
		t.Fatalf("unexpected result %+v", result)
	}
	if len(result.Files) != 1 || result.Files[0].Filename != "main.go" {
		t.Fatalf("unexpected files %+v", result.Files)
	}
	if gotPath != "/repos/acme/widgets/pulls/42/files" {
		t.Fatalf("unexpected request path %q", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("unexpected auth header %q", gotAuth)
	}
}

func TestGetCheckRunLogsWithoutTokenReturnsGracefulResult(t *testing.T) {
	clearRepoProviderTokenEnv(t)
	registry := NewRegistry()
	callCtx := repoProviderTestCallContext(t, t.TempDir(), map[string]interface{}{
		"repository_spec": map[string]interface{}{
			"provider":  "github",
			"clone_url": "https://github.com/acme/widgets.git",
		},
	})
	output, err := registry.Execute(context.Background(), callCtx, "get_check_run_logs", json.RawMessage(`{"check_run_id":1}`))
	if err != nil {
		t.Fatalf("expected graceful tool result, got error: %v", err)
	}
	var result struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.Status != "unavailable" || !strings.Contains(result.Error, "no GitHub token") {
		t.Fatalf("unexpected result %+v", result)
	}
}

func TestGetCheckRunLogsRequiresCheckRunID(t *testing.T) {
	registry := NewRegistry()
	callCtx := repoProviderTestCallContext(t, t.TempDir(), nil)
	_, err := registry.Execute(context.Background(), callCtx, "get_check_run_logs", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "check_run_id is required") {
		t.Fatalf("expected check_run_id error, got %v", err)
	}
}

func TestGetCheckRunLogsFetchesRunAndAnnotations(t *testing.T) {
	clearRepoProviderTokenEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/annotations"):
			_, _ = w.Write([]byte(`[{"path":"main.go","start_line":4,"end_line":4,"annotation_level":"failure","message":"undefined: x","title":"build error"}]`))
		default:
			_, _ = w.Write([]byte(`{"id":99,"name":"ci","html_url":"https://github.com/acme/widgets/runs/99","status":"completed","conclusion":"failure","output":{"title":"Build failed","summary":"1 error","text":"details"}}`))
		}
	}))
	defer server.Close()
	t.Setenv("GITHUB_TOKEN", "gh-token")
	t.Setenv(repoProviderAPIBaseEnvVar, server.URL)

	registry := NewRegistry()
	callCtx := repoProviderTestCallContext(t, t.TempDir(), map[string]interface{}{
		"repository_spec": map[string]interface{}{
			"provider":  "github",
			"clone_url": "https://github.com/acme/widgets.git",
		},
	})
	output, err := registry.Execute(context.Background(), callCtx, "get_check_run_logs", json.RawMessage(`{"check_run_id":99}`))
	if err != nil {
		t.Fatalf("get_check_run_logs returned error: %v", err)
	}
	var result struct {
		Owner    string `json:"owner"`
		Repo     string `json:"repo"`
		CheckRun struct {
			ID         int64  `json:"id"`
			Conclusion string `json:"conclusion"`
			Output     struct {
				Title string `json:"title"`
			} `json:"output"`
		} `json:"check_run"`
		Annotations []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"annotations"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.Owner != "acme" || result.Repo != "widgets" || result.CheckRun.ID != 99 || result.CheckRun.Conclusion != "failure" {
		t.Fatalf("unexpected result %+v", result)
	}
	if len(result.Annotations) != 1 || result.Annotations[0].Message != "undefined: x" {
		t.Fatalf("unexpected annotations %+v", result.Annotations)
	}
}

func TestRepoProviderAPIBaseDerivation(t *testing.T) {
	clearRepoProviderTokenEnv(t)
	for _, tc := range []struct {
		name string
		ref  repoProviderRef
		want string
	}{
		{name: "github.com", ref: repoProviderRef{Host: "github.com"}, want: "https://api.github.com"},
		{name: "empty host", ref: repoProviderRef{}, want: "https://api.github.com"},
		{name: "enterprise", ref: repoProviderRef{Host: "ghe.example.com"}, want: "https://ghe.example.com/api/v3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoProviderAPIBase(tc.ref); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
