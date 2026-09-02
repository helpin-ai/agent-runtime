package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/tools"
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

func TestWorkerHealthSeparatesLivenessFromProviderReadiness(t *testing.T) {
	registry := tools.NewRegistry()
	registry.SetProviderHealth(tools.ProviderHealth{AppID: "helpin", Provider: "helpin", Ready: false})
	handler := workerHealthHandler(registry)

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d, want %d", health.Code, http.StatusOK)
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status=%d, want %d", ready.Code, http.StatusServiceUnavailable)
	}
}
