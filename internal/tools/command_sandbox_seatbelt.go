package tools

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/sandbox"
	runtimeworkspace "github.com/helpin-ai/agent-runtime/internal/workspace"
)

// SeatbeltDenyReadEnv lists extra directories (path-list separated) that
// Seatbelt-confined commands may not read, for example a host app's data.
const SeatbeltDenyReadEnv = "AGENT_RUNTIME_SEATBELT_DENY_READ"

var seatbeltExecutable = sandbox.SeatbeltExecutable

// seatbeltCommand rewrites program and args to run under sandbox-exec. It
// grants write access to the same trusted paths the Landlock shim receives:
// the run's confinement root, ephemeral tool state and an authorized shared
// repository cache. Toolchains need no grant because reads stay broad.
func seatbeltCommand(root, program string, args []string, privateCache bool) (string, []string, error) {
	paths := sandbox.SeatbeltPaths{Root: sandboxConfinementRoot(root), DenyRead: seatbeltDenyRead()}
	if state, ephemeral, err := runtimeworkspace.ToolStateRoot(root); err != nil {
		return "", nil, err
	} else if ephemeral {
		paths.ReadWrite = append(paths.ReadWrite, state)
	}
	if path := runtimeworkspace.SharedRepositoryCachePath(root); path != "" && !privateCache {
		paths.ReadWrite = append(paths.ReadWrite, path)
	}
	shimArgs, err := sandbox.SeatbeltArgs(paths, program, args)
	if err != nil {
		return "", nil, err
	}
	return seatbeltExecutable, shimArgs, nil
}

// seatbeltDenyRead returns credential folders in the user's home plus any
// host-configured directories.
func seatbeltDenyRead() []string {
	var paths []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, name := range []string{".ssh", ".aws", ".gnupg", ".config/gh", ".netrc", ".docker", ".kube", "Library/Keychains", "Library/Cookies"} {
			paths = append(paths, filepath.Join(home, name))
		}
	}
	for _, path := range filepath.SplitList(os.Getenv(SeatbeltDenyReadEnv)) {
		if path = strings.TrimSpace(path); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}
