package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	openai "github.com/openai/openai-go/v3"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestTransientFailureClassification(t *testing.T) {
	for _, test := range []struct {
		err   error
		retry bool
	}{
		{io.ErrUnexpectedEOF, true}, {&net.DNSError{Err: "no such host", Name: "provider"}, true},
		{fmt.Errorf("stream: %w", io.EOF), true}, {errors.New("429 rate_limit_exceeded"), true},
		{errors.New("429 insufficient_quota"), false}, {errors.New("usage_limit reached"), false},
		{errors.New("invalid api key"), false}, {errors.New("checkpoint: unexpected EOF"), false},
		{context.Canceled, false}, {errors.New("context length exceeded"), false}, {errors.New("invalid arguments"), false},
	} {
		if got := transientFailure(test.err) != ""; got != test.retry {
			t.Errorf("%v: retry=%v", test.err, got)
		}
	}
}

type recoveringAdapter struct {
	calls atomic.Int32
	fail  bool
}

func (*recoveringAdapter) Kind() string { return agentcore.RuntimeNativeSDK }
func (a *recoveringAdapter) Execute(x *runtime.ExecutionContext) (*runtime.Result, error) {
	if a.calls.Add(1) == 1 || a.fail {
		return nil, io.ErrUnexpectedEOF
	}
	return &runtime.Result{AssistantMessage: "continued", OutputSummary: json.RawMessage(`{}`)}, nil
}

func TestRecoveryKeepsRunAndTurn(t *testing.T) {
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(t.Context(), &agent); err != nil {
		t.Fatal(err)
	}
	adapter := &recoveringAdapter{}
	eng := adapterEngine(mem, adapter)
	eng.cfg.AutomaticRecovery = true
	run, err := eng.StartRun(t.Context(), StartRunRequest{AppID: "app-a", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"}, Instructions: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		saved, _ := mem.GetRun(t.Context(), run.AppID, run.ID)
		if state := recoveryState(saved); state != nil {
			if saved.Status != "running" || saved.CompletedAt != nil || agentcore.TurnIdentity(saved) != agentcore.TurnIdentity(run) {
				t.Fatalf("lost identity: %+v", saved)
			}
			state.NextRetryAt = time.Now().Add(-time.Second)
			saved.Input.Metadata[recoveryKey] = state
			if _, err := mem.SetRunRecovery(t.Context(), run.AppID, run.ID, saved.Input.Metadata); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no persisted recovery")
		}
		time.Sleep(10 * time.Millisecond)
	}
	completed := waitForRunStatus(t, mem, run.AppID, run.ID, agentcore.RunStatusCompleted)
	if recoveryState(completed) != nil || adapter.calls.Load() != 2 {
		t.Fatalf("recovery=%+v calls=%d", recoveryState(completed), adapter.calls.Load())
	}
}

func TestRecoveryWaitStopsOnCancellation(t *testing.T) {
	mem := store.NewMemory()
	run := &agentcore.AgentRun{ID: "waiting", AppID: "app-a", Status: "running", Input: agentcore.RunInput{Metadata: map[string]interface{}{recoveryKey: &recovery{NextRetryAt: time.Now().Add(time.Hour)}}}}
	if err := mem.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	eng := New(Config{Store: mem, AutomaticRecovery: true})
	done := make(chan bool, 1)
	go func() { done <- eng.waitForRecovery(context.Background(), run.AppID, run.ID) }()
	run.Status = agentcore.RunStatusCancelled
	if err := mem.UpdateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	select {
	case resumed := <-done:
		if resumed {
			t.Fatal("cancelled run resumed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait ignored cancellation")
	}
	if saved, _ := mem.SetRunRecovery(t.Context(), run.AppID, run.ID, run.Input.Metadata); saved {
		t.Fatal("recovery resurrected cancelled run")
	}
}

func TestRecoveryDelayBounded(t *testing.T) {
	if recoveryDelay(1) != 5*time.Second || recoveryDelay(10000) != 30*time.Second {
		t.Fatal("invalid backoff")
	}
}

func TestRestartHonorsPersistedRecoveryDeadline(t *testing.T) {
	mem := store.NewMemory()
	run := &agentcore.AgentRun{ID: "restart-wait", AppID: "app-a", Status: "running", ExecutionMode: ExecutionModeLightweight, Input: agentcore.RunInput{Metadata: map[string]interface{}{recoveryKey: &recovery{Attempts: 9, NextRetryAt: time.Now().Add(time.Hour)}}}}
	if err := mem.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	eng := New(Config{Store: mem, AutomaticRecovery: true})
	// Cancelling the recovery context proves it did not dispatch before the persisted deadline.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if eng.waitForRecovery(ctx, run.AppID, run.ID) {
		t.Fatal("restart ignored future retry deadline")
	}
	saved, _ := mem.GetRun(t.Context(), run.AppID, run.ID)
	if recoveryState(saved).Attempts != 9 {
		t.Fatal("restart reset backoff")
	}
}

func TestProviderRetryAfterAndQuota(t *testing.T) {
	request, _ := http.NewRequest("POST", "https://example.invalid", nil)
	cause := &openai.Error{StatusCode: 429, Code: "rate_limit_exceeded", Request: request, Response: &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"120"}}}}
	if transientFailure(cause) != "provider_busy" || providerRetryAfter(cause) != 2*time.Minute {
		t.Fatal("provider delay not honored")
	}
	cause.Code = "insufficient_quota"
	if transientFailure(cause) != "" {
		t.Fatal("exhausted quota was retried")
	}
}
