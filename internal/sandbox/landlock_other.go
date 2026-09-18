//go:build !linux

package sandbox

import "errors"

// ABI reports 0: Landlock is Linux only.
func ABI() (int, error) { return 0, nil }

// Restrict always fails off Linux; execution workers are Linux only.
func Restrict(string) error { return errors.New("landlock is only available on linux") }

// RestrictWithReadExec always fails off Linux; execution workers are Linux only.
func RestrictWithReadExec(string, []string) error {
	return errors.New("landlock is only available on linux")
}
