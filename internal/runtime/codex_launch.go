package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

func ResolveCodexLaunch(commandPath, fallbackWorkDir string) (bin string, prefixArgs []string, workDir string, err error) {
	path := strings.TrimSpace(commandPath)
	if path == "" {
		return "codex", nil, fallbackWorkDir, nil
	}
	if strings.HasSuffix(path, ".js") {
		return "node", []string{path}, fallbackWorkDir, nil
	}
	info, statErr := os.Stat(path)
	if statErr == nil && info.IsDir() {
		if binary := firstExistingCodexRepoBinary(path); binary != "" {
			return binary, nil, fallbackWorkDir, nil
		}
		if launcher := codexRepoNodeLauncher(path); launcher != "" {
			return "node", []string{launcher}, fallbackWorkDir, nil
		}
		if isCodexRepo(path) {
			return "", nil, "", fmt.Errorf(
				"codex path %q does not contain a runnable Codex binary; build Codex first and set CODEX_PATH to the binary, or populate codex-cli/vendor with the packaged native binary",
				path,
			)
		}
	}
	return path, nil, fallbackWorkDir, nil
}

func isCodexRepo(root string) bool {
	for _, candidate := range []string{
		filepath.Join(root, "codex-rs", "Cargo.toml"),
		filepath.Join(root, "codex-cli", "bin", "codex.js"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return true
		}
	}
	return false
}

func firstExistingCodexRepoBinary(root string) string {
	for _, candidate := range codexRepoBinaryCandidates(root) {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if goruntime.GOOS == "windows" || info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

func codexRepoBinaryCandidates(root string) []string {
	name := codexNativeBinaryName()
	return []string{
		filepath.Join(root, "codex-rs", "target", "release", name),
		filepath.Join(root, "codex-rs", "target", "debug", name),
		filepath.Join(root, "target", "release", name),
		filepath.Join(root, "target", "debug", name),
	}
}

func codexRepoNodeLauncher(root string) string {
	launcher := filepath.Join(root, "codex-cli", "bin", "codex.js")
	if _, err := os.Stat(launcher); err != nil {
		return ""
	}
	if _, err := os.Stat(codexRepoVendorBinary(root)); err != nil {
		return ""
	}
	return launcher
}

func codexRepoVendorBinary(root string) string {
	target := codexVendorTargetTriple()
	if target == "" {
		return ""
	}
	return filepath.Join(root, "codex-cli", "vendor", target, "codex", codexNativeBinaryName())
}

func codexNativeBinaryName() string {
	if goruntime.GOOS == "windows" {
		return "codex.exe"
	}
	return "codex"
}

func codexVendorTargetTriple() string {
	switch goruntime.GOOS {
	case "linux", "android":
		switch goruntime.GOARCH {
		case "amd64":
			return "x86_64-unknown-linux-musl"
		case "arm64":
			return "aarch64-unknown-linux-musl"
		}
	case "darwin":
		switch goruntime.GOARCH {
		case "amd64":
			return "x86_64-apple-darwin"
		case "arm64":
			return "aarch64-apple-darwin"
		}
	case "windows":
		switch goruntime.GOARCH {
		case "amd64":
			return "x86_64-pc-windows-msvc"
		case "arm64":
			return "aarch64-pc-windows-msvc"
		}
	}
	return ""
}
