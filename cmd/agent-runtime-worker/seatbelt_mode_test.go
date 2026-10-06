package main

import (
	"runtime"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestCommandSandboxModeSeatbelt(t *testing.T) {
	mode, err := commandSandboxMode(true, "seatbelt")
	if runtime.GOOS != "darwin" {
		if err == nil {
			t.Fatalf("seatbelt must fail closed off macOS, got %q", mode)
		}
		return
	}
	if err != nil || mode != tools.CommandSandboxSeatbelt {
		t.Fatalf("seatbelt on macOS: %q %v", mode, err)
	}
	if mode, err := commandSandboxMode(false, "seatbelt"); err != nil || mode != tools.CommandSandboxNone {
		t.Fatalf("non-execution workers stay unconfined: %q %v", mode, err)
	}
}
