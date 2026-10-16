package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/sandbox"
	"github.com/helpin-ai/agent-runtime/sandbox/wsl"
	"golang.org/x/sys/unix"
)

func probe() (json.RawMessage, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("WSL sandbox must use an unprivileged user")
	}
	abi, err := sandbox.ABI()
	if err != nil || abi < 3 {
		return nil, fmt.Errorf("WSL requires Landlock ABI >= 3: abi=%d err=%v", abi, err)
	}
	config, err := os.ReadFile("/etc/wsl.conf")
	if err != nil || !wsl.InteropDisabled(config) {
		return nil, errors.New("WSL interop must be disabled")
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil || !strings.Contains(strings.ToLower(string(kernel)), "microsoft") {
		return nil, errors.New("sandbox probe requires WSL2")
	}
	var request wsl.ProbeRequest
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, wsl.MaxMessageBytes))
	if err := decoder.Decode(&request); err != nil {
		return nil, errors.New("invalid sandbox probe request")
	}
	lifetime, endLifetime, err := lifetimeContext()
	if err != nil {
		return nil, err
	}
	defer endLifetime()
	if !filepath.IsAbs(request.Root) || filepath.Dir(filepath.Clean(request.Root)) == filepath.Clean(request.Root) {
		return nil, errors.New("invalid sandbox probe root")
	}
	if _, err := os.ReadFile(request.Outside); err != nil {
		return nil, fmt.Errorf("outside probe fixture unavailable: %w", err)
	}
	baseline := filepath.Join(request.Root, ".unconfined-write")
	if err := os.WriteFile(baseline, []byte("probe"), 0600); err != nil {
		return nil, fmt.Errorf("unprivileged workspace write unavailable (uid %d): %w", os.Geteuid(), err)
	}
	if err := os.Remove(baseline); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(lifetime, 15*time.Second)
	_, interopErr := exec.CommandContext(ctx, request.WindowsExecutable, "/c", "echo", "REX_INTEROP_PROBE").CombinedOutput()
	expired := ctx.Err()
	cancel()
	if interopErr == nil || expired != nil {
		return nil, errors.New("Windows interop denial could not be established")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	confined := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(lifetime, 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, append([]string{"landlock-exec", "--root", request.Root, "--cwd", request.Root, "--"}, args...)...)
		command.Dir = request.Root
		command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + request.Root}
		return command.CombinedOutput()
	}
	if out, err := confined("sh", "-c", "echo probe > inside && cat inside"); err != nil || strings.TrimSpace(string(out)) != "probe" {
		return nil, fmt.Errorf("workspace probe failed: %v %s", err, out)
	}
	link := filepath.Join(request.Root, "escape")
	if err := os.Symlink(request.Outside, link); err != nil {
		return nil, err
	}
	for _, path := range []string{request.Outside, link} {
		if _, err := confined("cat", path); err == nil {
			return nil, errors.New("sandbox allowed an outside read")
		}
	}
	if _, err := confined("sh", "-c", `echo denied > "$1"`, "sh", request.Outside+".write"); err == nil {
		return nil, errors.New("sandbox allowed an outside write")
	}
	if _, err := os.Stat(request.Outside + ".write"); !os.IsNotExist(err) {
		return nil, errors.New("sandbox changed an outside file")
	}
	if _, err := confined(request.WindowsExecutable, "/c", "echo", "REX_INTEROP_PROBE"); err == nil {
		return nil, errors.New("sandbox allowed Windows execution")
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs(request.Root, &filesystem); err != nil {
		return nil, err
	}
	return json.Marshal(wsl.ProbeResult{Kernel: strings.TrimSpace(string(kernel)), ABI: abi, Filesystem: int64(filesystem.Type)})
}
