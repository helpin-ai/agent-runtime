// Package browser provides local browser process integration for desktop hosts.
package browser

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// RunCLI captures one CLI response without waiting for a daemon's inherited pipes.
func RunCLI(ctx context.Context, binary string, env []string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = env
	command.WaitDelay = 2 * time.Second
	output, err := command.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil && command.ProcessState.Success() {
		err = nil
	}
	return output, err
}
