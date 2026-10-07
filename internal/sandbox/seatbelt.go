package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SeatbeltExecutable is the macOS tool that applies a Seatbelt profile and
// execs the confined program.
const SeatbeltExecutable = "/usr/bin/sandbox-exec"

// SeatbeltPaths are the trusted paths a Seatbelt-confined command may use.
// Every path must come from runtime configuration, never agent input.
type SeatbeltPaths struct {
	// Root receives full access, like the Landlock run root.
	Root string
	// ReadWrite are additional trusted state and cache directories.
	ReadWrite []string
	// DenyRead are directories the command must not read, such as secrets.
	DenyRead []string
}

// seatbeltProfile is fixed text. Paths are supplied as -D parameters and
// referenced with (param ...), so no path can change the profile itself.
// Reads stay broad (toolchains, system libraries), matching the Landlock
// read posture; writes are limited to the run root and trusted paths;
// network access is not restricted, which matches Landlock below ABI 4.
func seatbeltProfile(readWrite, denyRead int) string {
	var builder strings.Builder
	builder.WriteString("(version 1)\n(deny default)\n")
	builder.WriteString("(allow process-exec)\n(allow process-fork)\n(allow signal (target same-sandbox))\n")
	builder.WriteString("(allow sysctl-read)\n(allow mach-lookup)\n(allow ipc-posix-shm-read-data)\n(allow ipc-posix-sem)\n")
	builder.WriteString("(allow file-read*)\n")
	if denyRead > 0 {
		builder.WriteString("(deny file-read*")
		for index := 0; index < denyRead; index++ {
			fmt.Fprintf(&builder, " (subpath (param \"DENY_READ_%d\"))", index)
		}
		builder.WriteString(")\n")
	}
	builder.WriteString("(allow file-write* (subpath (param \"RUN_ROOT\"))")
	for index := 0; index < readWrite; index++ {
		fmt.Fprintf(&builder, " (subpath (param \"READ_WRITE_%d\"))", index)
	}
	builder.WriteString(" (literal \"/dev/null\") (literal \"/dev/tty\") (literal \"/dev/dtracehelper\") (literal \"/dev/zero\"))\n")
	builder.WriteString("(allow file-ioctl (literal \"/dev/tty\") (literal \"/dev/dtracehelper\"))\n")
	builder.WriteString("(allow network*)\n")
	return builder.String()
}

// canonicalPaths resolves symlinks (Seatbelt matches resolved paths: /tmp
// is /private/tmp on macOS), drops empty and duplicate entries and keeps
// only absolute paths. Paths that do not exist are resolved through their
// nearest existing parent so a deny rule still applies if created later.
func canonicalPaths(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		resolved := resolvePath(filepath.Clean(path))
		if resolved == string(filepath.Separator) || seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, resolved)
	}
	return out
}

func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent, base := filepath.Dir(path), filepath.Base(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolvePath(parent), base)
}

// SeatbeltArgs returns sandbox-exec arguments that confine program to the
// given paths. The caller runs SeatbeltExecutable with these arguments.
func SeatbeltArgs(paths SeatbeltPaths, program string, args []string) ([]string, error) {
	roots := canonicalPaths([]string{paths.Root})
	if len(roots) != 1 {
		return nil, fmt.Errorf("seatbelt: an absolute run root is required")
	}
	readWrite := canonicalPaths(paths.ReadWrite)
	denyRead := canonicalPaths(paths.DenyRead)
	command := []string{"-p", seatbeltProfile(len(readWrite), len(denyRead)), "-D", "RUN_ROOT=" + roots[0]}
	for index, path := range readWrite {
		command = append(command, "-D", fmt.Sprintf("READ_WRITE_%d=%s", index, path))
	}
	for index, path := range denyRead {
		command = append(command, "-D", fmt.Sprintf("DENY_READ_%d=%s", index, path))
	}
	command = append(command, "--", program)
	return append(command, args...), nil
}
