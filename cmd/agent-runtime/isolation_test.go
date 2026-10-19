package main

import (
	"errors"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestServerExecutionIsolation(t *testing.T) {
	t.Cleanup(func() { tools.SetCommandSandbox(tools.CommandSandboxNone) })
	available := func() error { return nil }
	unavailable := func() error { return errors.New("no seatbelt") }

	for _, value := range []string{"", "none", "landlock", "best_effort", "anything"} {
		tools.SetCommandSandbox(tools.CommandSandboxNone)
		coding, err := serverExecutionIsolation(value, false, available)
		if err != nil || coding || tools.CommandSandboxMode() != tools.CommandSandboxNone {
			t.Fatalf("%q must keep the server unconfined without coding tools: coding=%v err=%v mode=%s", value, coding, err, tools.CommandSandboxMode())
		}
	}
	if _, err := serverExecutionIsolation("seatbelt", false, unavailable); err == nil {
		t.Fatal("seatbelt requested but unavailable must fail startup")
	}
	coding, err := serverExecutionIsolation("seatbelt", false, available)
	if err != nil || !coding || tools.CommandSandboxMode() != tools.CommandSandboxSeatbelt {
		t.Fatalf("seatbelt must confine commands and admit coding: coding=%v err=%v", coding, err)
	}
	coding, err = serverExecutionIsolation("seatbelt", true, available)
	if err != nil || coding {
		t.Fatalf("durable deployments keep coding on execution workers: coding=%v err=%v", coding, err)
	}
}

func TestServerWSLRequiresVerifiedConfiguration(t *testing.T) {
	tools.SetCommandSandbox(tools.CommandSandboxNone)
	t.Cleanup(func() { tools.SetCommandSandbox(tools.CommandSandboxNone) })
	t.Setenv("AGENT_RUNTIME_WSL_DISTRIBUTION", "")
	t.Setenv("AGENT_RUNTIME_WSL_HELPER", "")
	t.Setenv("AGENT_RUNTIME_WSL_PROBE_ROOT", "")
	for _, durable := range []bool{false, true} {
		coding, err := serverExecutionIsolation("wsl", durable, func() error { return nil })
		if err == nil || coding || tools.CommandSandboxMode() != tools.CommandSandboxNone {
			t.Fatalf("unverified WSL enabled commands: durable=%v coding=%v err=%v", durable, coding, err)
		}
	}
}
