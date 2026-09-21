package workspace

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const EphemeralRootEnv = "AGENT_RUNTIME_EPHEMERAL_ROOT"

// ConfinementRoot widens a repository checkout to the run directory shared by
// all of that run's repositories. Other workspace layouts remain unchanged.
func ConfinementRoot(root string) string {
	root = filepath.Clean(root)
	fingerprintDir := filepath.Dir(root)
	repositoriesDir := filepath.Dir(fingerprintDir)
	if filepath.Base(root) == "repo" && filepath.Base(repositoriesDir) == "repositories" && filepath.Dir(repositoriesDir) != repositoriesDir {
		return filepath.Dir(repositoriesDir)
	}
	return root
}

// ToolStateRoot returns the run-local directory used for regenerable
// toolchain state. Without an operator-configured ephemeral root it preserves
// the legacy workspace-local layout.
func ToolStateRoot(workspaceRoot string) (path string, ephemeral bool, err error) {
	base, enabled, err := ephemeralBase()
	if err != nil {
		return "", false, err
	}
	if !enabled {
		return filepath.Join(workspaceRoot, ".agent-runtime"), false, nil
	}
	digest := sha256.Sum256([]byte(ConfinementRoot(workspaceRoot)))
	path = filepath.Join(base, fmt.Sprintf("run-%x", digest))
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", false, fmt.Errorf("prepare ephemeral tool state: %w", err)
	}
	return path, true, nil
}

// CleanupToolState removes only the deterministic state directory for one
// run. Workspace-local state remains owned by the workspace provider.
func CleanupToolState(workspaceRoot string) error {
	path, ephemeral, err := ToolStateRoot(workspaceRoot)
	if err != nil || !ephemeral {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove ephemeral tool state: %w", err)
	}
	return nil
}

// ResetEphemeralRoot removes stale run directories after an execution worker
// restarts. It deliberately leaves unrelated entries untouched so a bad
// operator path cannot turn startup into a broad recursive deletion.
func ResetEphemeralRoot() error {
	base, enabled, err := ephemeralBase()
	if err != nil || !enabled {
		return err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return fmt.Errorf("read ephemeral tool root: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(base, entry.Name())); err != nil {
			return fmt.Errorf("clear stale ephemeral tool state %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func ephemeralBase() (string, bool, error) {
	raw := strings.TrimSpace(os.Getenv(EphemeralRootEnv))
	if raw == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(raw) {
		return "", false, fmt.Errorf("%s must be an absolute path", EphemeralRootEnv)
	}
	base := filepath.Clean(raw)
	if base == string(filepath.Separator) {
		return "", false, fmt.Errorf("%s must not be the filesystem root", EphemeralRootEnv)
	}
	if info, err := os.Lstat(base); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", false, fmt.Errorf("%s must name a real directory", EphemeralRootEnv)
		}
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("inspect ephemeral tool root: %w", err)
	} else if err := os.MkdirAll(base, 0o700); err != nil {
		return "", false, fmt.Errorf("create ephemeral tool root: %w", err)
	}
	return base, true, nil
}
