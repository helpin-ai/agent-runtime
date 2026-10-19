// Package filelock provides nonblocking, process-owned locks on real files.
package filelock

import "errors"

var ErrBusy = errors.New("file is locked by another owner")
