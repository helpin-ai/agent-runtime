//go:build !windows

// Package privatefile reserves sensitive files with owner-only access.
package privatefile

import "os"

func Create(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
}
