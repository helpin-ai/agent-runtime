package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func persistentRepositoryForTest(t *testing.T) string {
	t.Helper()
	t.Setenv(workspace.RepositoryCacheModeEnv, "workspace")
	t.Setenv(workspace.EphemeralRootEnv, filepath.Join(t.TempDir(), "agent-runtime-ephemeral"))
	root := filepath.Join(t.TempDir(), "app", "run", "repositories", "repo-id", "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRepositoryCacheEnvironmentPersistsAllToolchainsButNotHome(t *testing.T) {
	root := persistentRepositoryForTest(t)
	env, err := sandboxCommandEnv(root, []string{"PATH=/usr/bin", "DATABASE_URL=secret"})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	cache, persistent, err := workspace.OpenToolCacheRoot(root)
	if err != nil || !persistent {
		t.Fatalf("cache: %v %v", persistent, err)
	}
	defer cache.Close()
	for _, key := range []string{"npm_config_cache", "npm_config_store_dir", "YARN_CACHE_FOLDER", "PIP_CACHE_DIR", "UV_CACHE_DIR", "POETRY_CACHE_DIR", "GOPATH", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "CARGO_TARGET_DIR"} {
		if !strings.HasPrefix(values[key], cache.Name()+string(filepath.Separator)) {
			t.Fatalf("%s not persistent: %q", key, values[key])
		}
	}
	for _, key := range []string{"HOME", "TMPDIR"} {
		if !strings.HasPrefix(values[key], os.Getenv(workspace.EphemeralRootEnv)+string(filepath.Separator)) {
			t.Fatalf("%s persisted: %q", key, values[key])
		}
	}
	if _, ok := values["DATABASE_URL"]; ok {
		t.Fatal("worker credentials exposed")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(cache.Name(), "escape")); err != nil {
		t.Fatal(err)
	}
	if err := cache.MkdirAll("escape/created", 0o700); err == nil {
		t.Fatal("cache preparation followed escaping symlink")
	}
}

func TestRepositoryPythonEnvironmentSurvivesWorkerReplacement(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	root := persistentRepositoryForTest(t)
	program, args, env, err := pythonCommandEnvironment(context.Background(), root, "python3", []string{"-I", "-c", "import site,pathlib; (pathlib.Path(site.getsitepackages()[0])/'retained_dependency.py').write_text('value=42')"}, procenv.Command())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(program, args...)
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed dependency: %v %s", err, output)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "PIP_NO_CACHE_DIR=") {
			t.Fatal("repository pip caching remains disabled")
		}
	}
	if err := workspace.ResetEphemeralRoot(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workspace.EphemeralRootEnv, filepath.Join(t.TempDir(), "agent-runtime-ephemeral"))
	resumed, args, env, err := pythonCommandEnvironment(context.Background(), root, "python3", []string{"-I", "-c", "import retained_dependency; assert retained_dependency.value == 42; print('retained')"}, procenv.Command())
	if err != nil {
		t.Fatal(err)
	}
	if resumed != program {
		t.Fatal("worker replacement discarded venv")
	}
	cmd = exec.Command(resumed, args...)
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "retained" {
		t.Fatalf("lost dependency: %v %s", err, output)
	}
}

func TestSharedRepositoryEnvironmentKeepsCredentialsAndVenvPrivate(t *testing.T) {
	root := persistentRepositoryForTest(t)
	t.Setenv(workspace.RepositoryCacheModeEnv, "repository")
	workspace.RegisterRepositoryCache(root, "app", map[string]interface{}{"workspace_id": "ws", "repository_id": "repo"})
	env, err := sandboxCommandEnv(root, procenv.Command())
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	shared := workspace.SharedRepositoryCachePath(root)
	for _, key := range []string{"npm_config_store_dir", "npm_config_cache_dir", "PIP_CACHE_DIR", "UV_CACHE_DIR", "GOCACHE", "GOMODCACHE"} {
		if !strings.HasPrefix(values[key], shared+string(filepath.Separator)) {
			t.Fatalf("%s not shared", key)
		}
	}
	for _, key := range []string{"HOME", "TMPDIR", "CARGO_HOME", "CARGO_TARGET_DIR", "POETRY_VIRTUALENVS_PATH"} {
		if strings.HasPrefix(values[key], shared) {
			t.Fatalf("%s shared sensitive/mutable state", key)
		}
	}
	if values["npm_config_package_import_method"] != "copy" || values["UV_LINK_MODE"] != "copy" {
		t.Fatal("unsafe cache import default")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	program, _, env, err := pythonCommandEnvironment(context.Background(), root, "python3", []string{"-c", "pass"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(program, shared) {
		t.Fatal("venv was shared")
	}
	if !strings.Contains(strings.Join(env, "\n"), "PIP_CACHE_DIR="+filepath.Join(shared, "cache", "pip")) {
		t.Fatal("private venv lost shared pip cache")
	}
}
