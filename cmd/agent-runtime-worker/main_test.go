package main

import (
	"testing"
	"time"
)

func TestWorkerStopTimeout(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_WORKER_STOP_TIMEOUT", "45s")
	if got := workerStopTimeout(); got != 45*time.Second {
		t.Fatalf("unexpected duration timeout: %s", got)
	}

	t.Setenv("AGENT_RUNTIME_WORKER_STOP_TIMEOUT", "90")
	if got := workerStopTimeout(); got != 90*time.Second {
		t.Fatalf("unexpected seconds timeout: %s", got)
	}

	t.Setenv("AGENT_RUNTIME_WORKER_STOP_TIMEOUT", "")
	if got := workerStopTimeout(); got != 2*time.Minute {
		t.Fatalf("unexpected default timeout: %s", got)
	}
}
