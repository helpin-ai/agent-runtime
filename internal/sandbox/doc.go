// Package sandbox confines agent-selected commands with Landlock. The worker
// re-executes itself with a landlock-exec subcommand, which calls Restrict and
// then execs the real program, because Go cannot run code between fork and
// exec.
package sandbox
