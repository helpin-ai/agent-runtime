//go:build unix

package filelock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Open refuses a symlink at the final path. Its parent must be host-owned.
func Open(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = fmt.Errorf("lock path must be a regular file: %s", path)
		}
		return nil, err
	}
	return file, nil
}

func TryLock(file *os.File, exclusive bool) error {
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}

func Unlock(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
