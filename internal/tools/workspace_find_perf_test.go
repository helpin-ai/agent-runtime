package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestFindSymbolPerformanceOnRealRepository measures the shipped tool against a
// real checkout, which is the only meaningful check of the design: the whole
// point of ripgrep narrowing is that it beats a whole-repo parse (measured at
// ~15s and ~1GB of heap) by orders of magnitude.
//
// Set SYMBOL_PERF_ROOT to a repository path to run it.
func TestFindSymbolPerformanceOnRealRepository(t *testing.T) {
	root := os.Getenv("SYMBOL_PERF_ROOT")
	if root == "" {
		t.Skip("set SYMBOL_PERF_ROOT to a repository path")
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("root unavailable: %v", err)
	}
	requireRipgrep(t)

	names := strings.Split(os.Getenv("SYMBOL_PERF_NAMES"), ",")
	registry, callCtx := workspaceToolTestRegistry(t)
	callCtx.Run.WorkspaceLease.RootPath = root

	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		input, err := json.Marshal(map[string]string{"name": name})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		start := time.Now()
		raw, err := registry.Execute(context.Background(), callCtx, "find_symbol", input)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("find_symbol(%s): %v", name, err)
		}
		out := workspaceToolString(t, raw)
		summary := strings.SplitN(out, "\n", 2)[0]
		t.Logf("%-14s %-9v %s", name, elapsed.Round(time.Millisecond), summary)

		if elapsed > 3*time.Second {
			t.Errorf("find_symbol(%s) took %v; narrowing is not doing its job", name, elapsed)
		}
	}
}

func TestFindCallersPerformanceOnRealRepository(t *testing.T) {
	root := os.Getenv("SYMBOL_PERF_ROOT")
	if root == "" {
		t.Skip("set SYMBOL_PERF_ROOT to a repository path")
	}
	requireRipgrep(t)

	registry, callCtx := workspaceToolTestRegistry(t)
	callCtx.Run.WorkspaceLease.RootPath = root

	for _, name := range strings.Split(os.Getenv("SYMBOL_PERF_NAMES"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		input, err := json.Marshal(map[string]string{"symbol": name})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		start := time.Now()
		raw, err := registry.Execute(context.Background(), callCtx, "find_callers", input)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("find_callers(%s): %v", name, err)
		}
		out := workspaceToolString(t, raw)
		t.Logf("%-14s %-9v %s", name, elapsed.Round(time.Millisecond), strings.SplitN(out, "\n", 2)[0])
		if elapsed > 5*time.Second {
			t.Errorf("find_callers(%s) took %v", name, elapsed)
		}
	}
}
