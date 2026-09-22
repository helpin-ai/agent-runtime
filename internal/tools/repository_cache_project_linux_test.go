//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"github.com/helpin-ai/agent-runtime/internal/sandbox"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

// TestRepositoryCacheProjectStaging is an opt-in full-project benchmark. The
// operator supplies an immutable source fixture, not a live user checkout.
func TestRepositoryCacheProjectStaging(t *testing.T) {
	source := os.Getenv("AGENT_RUNTIME_CACHE_PROJECT_SOURCE")
	base := os.Getenv("AGENT_RUNTIME_CACHE_SMOKE_ROOT")
	if source == "" || base == "" {
		t.Skip("requires disposable project source and shared smoke root")
	}
	phase := os.Getenv("AGENT_RUNTIME_CACHE_SMOKE_PHASE")
	if phase != "seed" && phase != "reuse" {
		t.Fatal("phase must be seed or reuse")
	}
	// Normal agent runs are online. Offline mode is an explicit diagnostic
	// for complete cache availability, not a production rollout requirement.
	offline := os.Getenv("AGENT_RUNTIME_CACHE_PROJECT_OFFLINE") == "1"
	t.Logf("phase=%s offline=%t", phase, offline)
	if abi, err := sandbox.ABI(); err != nil || abi < 2 {
		t.Fatalf("Landlock required: %d %v", abi, err)
	}
	t.Setenv(workspace.RepositoryCacheModeEnv, "repository")
	t.Setenv(workspace.EphemeralRootEnv, filepath.Join(t.TempDir(), "agent-runtime-ephemeral"))
	runID := os.Getenv("AGENT_RUNTIME_CACHE_SMOKE_RUN")
	if runID == "" {
		runID = phase
	}
	root := filepath.Join(base, "app", runID, "repositories", "full-project", "repo")
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("project fixture must be a new run")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	copyCmd := exec.CommandContext(ctx, "cp", "-a", source+"/.", root)
	if out, err := copyCmd.CombinedOutput(); err != nil {
		cancel()
		t.Fatalf("copy source: %v %s", err, out)
	}
	cancel()
	workspace.RegisterRepositoryCache(root, "app", map[string]interface{}{"workspace_id": "full-project-benchmark", "repository_id": "helpin"})
	release, err := workspace.AcquireRepositoryCache(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	binary := buildWorker(t)
	sandboxExecutable = func() (string, error) { return binary, nil }
	SetCommandSandbox(CommandSandboxLandlock)
	t.Cleanup(func() { sandboxExecutable = os.Executable; SetCommandSandbox(CommandSandboxNone) })
	env, err := sandboxCommandEnv(root, procenv.Command())
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, "CI=true", "NODE_OPTIONS=--max-old-space-size=3072", "PNPM_DISABLE_SELF_UPDATE_CHECK=true")
	measure := func(label, dir string, args ...string) {
		t.Helper()
		started := time.Now()
		before := projectCacheCounters()
		stagingBefore := projectStagingCounters()
		cwd := filepath.Join(root, dir)
		program, cmdArgs, err := sandboxCommand(root, cwd, "pnpm", args)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, program, cmdArgs...)
		cmd.Dir = cwd
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = time.Second
		log, err := os.CreateTemp("", "project-cache-"+phase+"-"+label+"-*.log")
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		cmd.Stdout = log
		cmd.Stderr = log
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					t.Logf("%s %s progress %s log=%s", phase, label, time.Since(started).Round(time.Second), log.Name())
				}
			}
		}()
		err = cmd.Run()
		after := projectCacheCounters()
		for key, value := range before {
			after[key] -= value
		}
		result, _ := json.Marshal(map[string]interface{}{"phase": phase, "offline": offline, "label": label, "seconds": time.Since(started).Seconds(), "metrics_delta": after, "staging_before": stagingBefore, "staging_after": projectStagingCounters(), "passed": err == nil, "log": log.Name()})
		t.Log(string(result))
		if err != nil {
			out, _ := os.ReadFile(log.Name())
			if len(out) > 4000 {
				out = out[len(out)-4000:]
			}
			t.Fatalf("%s: %v %s", label, err, out)
		}
	}
	args := []string{"install", "--frozen-lockfile", "--ignore-scripts", "--ignore-pnpmfile", "--reporter=append-only"}
	if offline {
		args = append(args, "--offline")
	}
	measure("install", ".", args...)
	measure("widget", ".", "--filter", "@helpin-ai/widget-core", "build")
	measure("typecheck", "frontend", "exec", "tsc", "-b", "--force")
	measure("tests", "frontend", "exec", "vitest", "run", "src/components/__tests__/HelpinWidgetVisibility.test.tsx", "--minWorkers=1", "--maxWorkers=1")
}

func projectStagingCounters() map[string]float64 {
	result := map[string]float64{}
	body, err := os.ReadFile("/tmp/agent-runtime-workspaces/.stats")
	if err != nil {
		return result
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[0] != "juicefs_staging_blocks" && fields[0] != "juicefs_staging_block_bytes") {
			continue
		}
		if value, err := strconv.ParseFloat(fields[1], 64); err == nil {
			result[fields[0]] = value
		}
	}
	return result
}

func projectCacheCounters() map[string]float64 {
	result := map[string]float64{}
	for _, path := range []string{"/tmp/agent-runtime-workspaces/.stats", "/sys/fs/cgroup/cpu.stat"} {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			if strings.HasPrefix(fields[0], "juicefs_blockcache_") || fields[0] == "juicefs_fuse_ops_durations_histogram_seconds_sum" || fields[0] == "usage_usec" {
				value, _ := strconv.ParseFloat(fields[1], 64)
				result[fields[0]] = value
			}
		}
	}
	return result
}
