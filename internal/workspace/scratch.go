package workspace

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// ScratchLimitBytes bounds retained analysis data and its private package cache.
const ScratchLimitBytes int64 = 512 << 20

// NewScratch prepares run-local storage on the execution worker's writable volume.
func NewScratch(appID, runID string) (*agentcore.WorkspaceLease, error) {
	base := os.Getenv("AGENT_RUNTIME_WORKSPACE_ROOT")
	if base == "" {
		base = "/tmp/agent-runtime-workspaces"
	}
	id := fmt.Sprintf("analysis-%x", sha256.Sum256([]byte(appID+"\x00"+runID)))
	root := filepath.Join(base, id)
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &agentcore.WorkspaceLease{ID: id, Provider: "analysis", RootPath: root, CleanupPolicy: CleanupOnTerminal, Metadata: map[string]any{"workspace_mode": "analysis", "expires": "run_end", "size_limit_bytes": ScratchLimitBytes}}, nil
}

// CheckScratchSize never follows symlinks. This is a storage guard, not a sandbox.
func CheckScratchSize(root string) error {
	var size int64
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		if size > ScratchLimitBytes {
			return fmt.Errorf("analysis workspace exceeds the 512 MiB size limit; remove unneeded files")
		}
		return nil
	})
}

// CleanupScratch removes only the deterministic scratch path for this app/run.
func CleanupScratch(appID, runID string) error {
	base := os.Getenv("AGENT_RUNTIME_WORKSPACE_ROOT")
	if base == "" {
		base = "/tmp/agent-runtime-workspaces"
	}
	id := fmt.Sprintf("analysis-%x", sha256.Sum256([]byte(appID+"\x00"+runID)))
	return os.RemoveAll(filepath.Join(base, id))
}
