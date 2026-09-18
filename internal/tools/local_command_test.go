package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
)

func TestLocalCommandEnvironmentAndStreaming(t *testing.T) {
	registry, call := workspaceToolTestRegistry(t)
	t.Setenv("CLI_PROJECT_SETTING", "project-value")
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	var output bytes.Buffer
	ctx := WithLocalCommandOptions(context.Background(), LocalCommandOptions{Env: append(procenv.Command(), "CLI_PROJECT_SETTING=project-value"), Output: &output})
	result, e := registry.Execute(ctx, call, "run_command", json.RawMessage(`{"program":"python3","args":["-c","import os; print(os.getenv('CLI_PROJECT_SETTING')); print(os.getenv('OPENAI_API_KEY', 'absent'))"]}`))
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(output.String(), "project-value") || !strings.Contains(output.String(), "absent") || strings.Contains(string(result), "must-not-inherit") {
		t.Fatalf("incorrect environment/stream: %s", result)
	}
	result, e = registry.Execute(context.Background(), call, "run_command", json.RawMessage(`{"program":"python3","args":["-c","import os; print(os.getenv('CLI_PROJECT_SETTING', 'absent'))"]}`))
	if e != nil || strings.Contains(string(result), "project-value") {
		t.Fatalf("local environment leaked into default execution: %s %v", result, e)
	}
}
func TestLocalCommandCancellation(t *testing.T) {
	registry, call := workspaceToolTestRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, e := registry.Execute(ctx, call, "run_command", json.RawMessage(`{"program":"python3","args":["-c","import subprocess,time; subprocess.Popen(['python3','-c','import time; time.sleep(60)']); time.sleep(60)"]}`))
	if e == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("command tree did not cancel promptly: %v", e)
	}
}

func TestGitFiltersWorkerSecretsAndPreservesTrustedLocalEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "worker-private")
	args := []string{"-c", "alias.checkenv=!printf %s \"$DATABASE_URL\"", "checkenv"}
	output, err := runWorkspaceGitOnce(context.Background(), t.TempDir(), args...)
	if err != nil || output != "" {
		t.Fatalf("worker environment was exposed: err=%v", err)
	}
	ctx := WithLocalCommandOptions(context.Background(), LocalCommandOptions{Env: []string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=local-project"}})
	output, err = runWorkspaceGitOnce(ctx, t.TempDir(), args...)
	if err != nil || output != "local-project" {
		t.Fatal("trusted CLI environment was lost", err)
	}
}
