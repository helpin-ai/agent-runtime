//go:build !linux

package workspace

// Non-Linux local development relies on operator placement. Execution workers
// run Linux, where known network/shared filesystems are rejected.
func validateLocalCacheFilesystem(string) error { return nil }
