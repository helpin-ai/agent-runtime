package main

import "github.com/helpin-ai/agent-runtime/internal/sandbox"

const landlockExecCommand = "landlock-exec"

func runLandlockExec(args []string) int { return sandbox.RunExec(args) }
