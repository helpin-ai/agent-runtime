package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/helpin-ai/agent-runtime/internal/sandbox"
)

// landlockExecCommand is the subcommand runCommand re-executes the worker
// with. It is dispatched before flag parsing so worker flags never apply.
const landlockExecCommand = "landlock-exec"

type landlockExecArgs struct {
	root, cwd string
	readExec  stringListFlag
	argv      []string
}

type stringListFlag []string

func (f *stringListFlag) String() string { return fmt.Sprint([]string(*f)) }
func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func parseLandlockExecArgs(args []string) (landlockExecArgs, error) {
	var parsed landlockExecArgs
	set := flag.NewFlagSet(landlockExecCommand, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&parsed.root, "root", "", "run root the program is confined to")
	set.StringVar(&parsed.cwd, "cwd", "", "working directory for the program")
	set.Var(&parsed.readExec, "read-exec", "trusted toolchain path granted read/execute access (repeatable)")
	if err := set.Parse(args); err != nil {
		return parsed, err
	}
	parsed.argv = set.Args()
	switch {
	case parsed.root == "" || parsed.cwd == "":
		return parsed, fmt.Errorf("--root and --cwd are required")
	case len(parsed.argv) == 0:
		return parsed, fmt.Errorf("program is required after --")
	}
	return parsed, nil
}

// runLandlockExec confines this process to the run root and replaces it with
// the requested program. The parent already prepared the environment.
func runLandlockExec(args []string) int {
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "%s: %v\n", landlockExecCommand, err)
		return 126
	}
	parsed, err := parseLandlockExecArgs(args)
	if err != nil {
		return fail(err)
	}
	// Landlock and no_new_privs apply to the calling thread; exec from it.
	runtime.LockOSThread()
	if err := sandbox.RestrictWithReadExec(parsed.root, parsed.readExec); err != nil {
		return fail(err)
	}
	if err := os.Chdir(parsed.cwd); err != nil {
		return fail(err)
	}
	program, err := exec.LookPath(parsed.argv[0])
	if err != nil {
		return fail(err)
	}
	return fail(syscall.Exec(program, parsed.argv, os.Environ()))
}
