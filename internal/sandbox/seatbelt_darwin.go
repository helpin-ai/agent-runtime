//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// SeatbeltAvailable verifies that sandbox-exec exists and can apply this
// package's profile on the running macOS version.
func SeatbeltAvailable() error {
	args, err := SeatbeltArgs(SeatbeltPaths{Root: "/tmp"}, "/usr/bin/true", nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, SeatbeltExecutable, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("seatbelt is unavailable: %w: %s", err, output)
	}
	return nil
}
