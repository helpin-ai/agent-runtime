package workspace

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func validateLocalCacheFilesystem(path string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return fmt.Errorf("stat local repository cache filesystem: %w", err)
	}
	// Reject the shared filesystems used in our deployments, including JuiceFS
	// (FUSE). Operators must still provision a genuinely node-local disk.
	switch uint32(stat.Type) {
	case 0x65735546, 0x6969, 0xff534d42, 0xfe534d42, 0x00c36400, 0x01021997:
		return fmt.Errorf("repository cache requires local storage, not FUSE/NFS/SMB/Ceph/9p")
	}
	return nil
}
