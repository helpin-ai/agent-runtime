package filelock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Open opens the reparse point itself so a link cannot redirect the lock.
// The parent directory must be host-owned, as on Unix.
func Open(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(handle, &info)
	if err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		windows.CloseHandle(handle)
		if err == nil {
			err = fmt.Errorf("lock path must be a real file: %s", path)
		}
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func TryLock(file *os.File, exclusive bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	// Windows locks are mandatory: keep the locked byte beyond diagnostic text.
	err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &windows.Overlapped{Offset: 1 << 30})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}

func Unlock(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{Offset: 1 << 30})
}
