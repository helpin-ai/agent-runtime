//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// x/sys exposes the Landlock constants and attribute structs but no wrappers
// for the three syscalls, so they are issued directly.

const readExecute = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR

// abi1Access is every filesystem access flag Landlock ABI 1 understands.
const abi1Access = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK | unix.LANDLOCK_ACCESS_FS_MAKE_SYM

// ABI reports the Landlock ABI version of the running kernel, or 0 when the
// kernel does not support Landlock or it is disabled.
func ABI() (int, error) {
	version, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	switch errno {
	case 0:
		return int(version), nil
	case unix.ENOSYS, unix.EOPNOTSUPP:
		return 0, nil
	}
	return 0, fmt.Errorf("probe landlock abi: %w", errno)
}

// handledAccess is the widest access set the given ABI can restrict.
func handledAccess(abi int) uint64 {
	access := uint64(abi1Access)
	if abi >= 2 {
		access |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		access |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	return access
}

// Restrict confines the calling thread to the system toolchain paths plus full
// access under runRoot. It is irreversible and inherited across exec, so call
// it from the thread that will exec the confined program.
func Restrict(runRoot string) error {
	return RestrictWithReadExec(runRoot, nil)
}

// RestrictWithReadExec confines the calling thread like Restrict and grants
// read/execute access to operator-selected toolchain paths. The caller must
// derive these paths from the worker environment, never agent input.
func RestrictWithReadExec(runRoot string, readExecPaths []string) error {
	abi, err := ABI()
	if err != nil {
		return err
	}
	if abi < 1 {
		return errors.New("landlock is not available on this kernel")
	}
	if info, err := os.Stat(runRoot); err != nil {
		return fmt.Errorf("run root: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("run root %s is not a directory", runRoot)
	}
	handled := handledAccess(abi)
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	// Only the filesystem field is sent so kernels before network and scope
	// support accept the attribute.
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr.Access_fs), 0)
	if errno != 0 {
		return fmt.Errorf("create landlock ruleset: %w", errno)
	}
	ruleset := int(fd)
	defer unix.Close(ruleset)
	rules := []struct {
		path     string
		access   uint64
		required bool
	}{
		{"/usr", readExecute, false}, {"/lib", readExecute, false}, {"/lib64", readExecute, false}, {"/bin", readExecute, false},
		{"/sbin", readExecute, false}, {"/etc", readExecute, false}, {"/opt", readExecute, false}, {"/app", readExecute, false},
		{"/proc", unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR, false},
		{"/dev", unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR, false},
		{"/dev/null", unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE, false},
		{"/dev/zero", unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE, false},
		{"/dev/urandom", unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE, false},
		{runRoot, handled, true},
	}
	for _, path := range readExecPaths {
		if path = filepath.Clean(strings.TrimSpace(path)); path != "." && path != "" {
			rules = append(rules, struct {
				path     string
				access   uint64
				required bool
			}{path, readExecute, true})
		}
	}
	for _, rule := range rules {
		if err := addPathRule(ruleset, rule.path, rule.access&handled); err != nil {
			if !rule.required && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
	}
	// systemd-resolved hosts link /etc/resolv.conf into /run; grant read on
	// that one directory rather than all of /run, which may hold secrets.
	for _, dir := range resolverConfigDirs() {
		if err := addPathRule(ruleset, dir, unix.LANDLOCK_ACCESS_FS_READ_FILE|unix.LANDLOCK_ACCESS_FS_READ_DIR); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("apply landlock ruleset: %w", errno)
	}
	return nil
}

// resolverConfigDirs returns directories outside the static rules that the
// resolver configuration files resolve into, if any.
func resolverConfigDirs() []string {
	var dirs []string
	for _, name := range []string{"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf"} {
		target, err := filepath.EvalSymlinks(name)
		if err != nil {
			continue
		}
		dir := filepath.Dir(target)
		if strings.HasPrefix(dir, "/etc/") || dir == "/etc" {
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

func addPathRule(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("landlock rule %s: %w", path, err)
	}
	defer unix.Close(fd)
	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&attr)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock rule %s: %w", path, errno)
	}
	return nil
}
