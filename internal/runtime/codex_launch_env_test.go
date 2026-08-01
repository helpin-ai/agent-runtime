package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sleepShort() { time.Sleep(10 * time.Millisecond) }

// The codex app-server is agent-driven, so its process environment must not
// carry the worker's credentials. This drives a real child process and reads
// back the environment it actually received.
func TestCodexAppServerClientDoesNotInheritRuntimeSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pw@db/agent")
	t.Setenv("AGENT_RUNTIME_SERVICE_TOKEN", "super-secret")
	t.Setenv("AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY", "0123456789abcdef")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	dir := t.TempDir()
	envDump := filepath.Join(dir, "env.txt")
	// Stands in for the codex binary: records its environment, then exits. The
	// client only needs the process to start for cmd.Env to have been applied.
	fakeCodex := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nenv > " + envDump + "\n"
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	client := newCodexAppServerClient(fakeCodex, dir, []string{"CODEX_HOME=" + dir})
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	waitForFile(t, envDump)
	got := readEnvDump(t, envDump)

	for _, leaked := range []string{"DATABASE_URL", "AGENT_RUNTIME_SERVICE_TOKEN", "AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY"} {
		if _, ok := got[leaked]; ok {
			t.Fatalf("%s leaked into the codex process environment", leaked)
		}
	}
	if got["CODEX_HOME"] != dir {
		t.Fatalf("CODEX_HOME = %q, want %q", got["CODEX_HOME"], dir)
	}
	// Codex authenticates to the provider itself, so this one is required.
	if got["OPENAI_API_KEY"] != "sk-test" {
		t.Fatalf("OPENAI_API_KEY = %q, want it passed through", got["OPENAI_API_KEY"])
	}
	if got["PATH"] == "" {
		t.Fatalf("PATH = empty, want passthrough")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for range 200 {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		sleepShort()
	}
	t.Fatalf("timed out waiting for %s", path)
}

func readEnvDump(t *testing.T, path string) map[string]string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	env := make(map[string]string)
	for _, line := range strings.Split(string(content), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			env[key] = value
		}
	}
	return env
}
