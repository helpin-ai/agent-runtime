package runtime

import (
	"encoding/json"
	"testing"
)

// TestNativeUsageWatermarkIsCumulativeAndIdempotent fixes the contract that
// work-job budget admission relies on: the persisted native checkpoint usage
// is a cumulative watermark. Reading it repeatedly, replaying a completed
// checkpoint, and resuming a run whose summary already carries the same
// cumulative usage never count tokens twice. Duplicate usage.checkpoint
// events are irrelevant because admission reads the store, not events.
func TestNativeUsageWatermarkIsCumulativeAndIdempotent(t *testing.T) {
	x := contextTestExec(t)
	x.Agent.ExecutionConfig = json.RawMessage(`{}`)
	m := &contextTestModel{}
	watermark := func() NativeUsage {
		t.Helper()
		raw, err := NativeCheckpointUsage(x.Context, x.Store, x.AppID, x.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return nativeUsageFromSummary(raw)
	}
	if _, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}}); err != nil {
		t.Fatal(err)
	}
	first := watermark()
	if first.InputTokens != 100 || first.OutputTokens != 10 {
		t.Fatalf("first watermark = %+v", first)
	}
	if again := watermark(); again != first {
		t.Fatalf("watermark read is not idempotent: %+v then %+v", first, again)
	}

	// Replaying a completed checkpoint does not sample or count again.
	if _, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}}); err != nil {
		t.Fatal(err)
	}
	if replay := watermark(); replay != first || len(m.requests) != 1 {
		t.Fatalf("replay changed usage: %+v after %d requests", replay, len(m.requests))
	}

	// The engine stores the cumulative summary on the run. Resuming takes the
	// maximum of run summary and checkpoint, then adds only new provider work.
	raw, _ := NativeCheckpointUsage(x.Context, x.Store, x.AppID, x.Run.ID)
	x.Run.OutputSummary = raw
	x.Run.Input.Metadata = map[string]any{"last_resume": map[string]any{"intent": "reply", "content": "continue"}}
	if _, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}}); err != nil {
		t.Fatal(err)
	}
	if resumed := watermark(); resumed.InputTokens != 200 || resumed.OutputTokens != 20 {
		t.Fatalf("resume double counted or lost usage: %+v", resumed)
	}
}

// TestNativeUsageWatermarkKeepsBilledUsageWithoutFinalResult covers missing
// final usage: a provider call that fails after billing still advances the
// watermark, so budget admission never under-counts an interrupted child.
func TestNativeUsageWatermarkKeepsBilledUsageWithoutFinalResult(t *testing.T) {
	x := contextTestExec(t)
	x.Agent.ExecutionConfig = json.RawMessage(`{}`)
	m := &contextTestModel{failAt: 1}
	if _, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}}); err == nil {
		t.Fatal("expected provider failure")
	}
	raw, err := NativeCheckpointUsage(x.Context, x.Store, x.AppID, x.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage := nativeUsageFromSummary(raw); usage.InputTokens != 7 {
		t.Fatalf("billed usage lost on failure: %s", raw)
	}
}
