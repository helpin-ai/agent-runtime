package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/durable"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	tclient "go.temporal.io/sdk/client"
	tworker "go.temporal.io/sdk/worker"
)

func TestConfiguredQueuesConstructTemporalWorkers(t *testing.T) {
	client, err := tclient.NewLazyClient(tclient.Options{HostPort: "127.0.0.1:1", Namespace: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, coding := range []bool{false, true} {
		for _, queue := range durable.WorkerQueues(coding) {
			t.Run(queue.Name, func(t *testing.T) {
				options := workerOptions(queue)
				if coding && options.MaxConcurrentActivityExecutionSize != 50 {
					t.Fatal("coding activity concurrency must be 50")
				}
				// The SDK panics on invalid concurrency even before polling starts.
				_ = tworker.New(client, queue.Name, options)
			})
		}
	}
}

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
