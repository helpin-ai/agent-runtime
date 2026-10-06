// Package sandbox confines agent-selected commands with Landlock. The worker
// re-executes itself with a landlock-exec subcommand, which calls Restrict and
// then execs the real program, because Go cannot run code between fork and
// exec.
//
// On macOS, Seatbelt (sandbox-exec) provides the equivalent confinement for
// the opt-in "seatbelt" execution isolation mode; see seatbelt.go.
package sandbox
