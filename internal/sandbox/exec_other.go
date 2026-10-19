//go:build !linux

package sandbox

// RunExec cannot apply Linux confinement on another operating system.
func RunExec(args []string) int { return 126 }
