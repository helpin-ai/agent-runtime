package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type failingCompactionStore struct{ agentcore.NativeStateStore }

func (s failingCompactionStore) SaveNativeState(ctx context.Context, state *agentcore.NativeState, records []json.RawMessage) error {
	for _, record := range records {
		var event struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(record, &event); err != nil {
			return err
		}
		if event.Kind == "compaction" {
			return errors.New("archive commit failed")
		}
	}
	return s.NativeStateStore.SaveNativeState(ctx, state, records)
}

func TestNativeCompactionFailedCommitRestoresMemoryButRetainsUsage(t *testing.T) {
	x := contextTestExec(t)
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	messages, _, err := r.initialMessages(true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		messages = append(messages, NativeMessage{Role: "assistant", Content: strings.Repeat("source text ", 200)})
	}
	result := &nativeExecutionResult{Messages: messages}
	r.state.InputAdjustment = 200
	if err := r.save(x.Context, "ready", result); err != nil {
		t.Fatal(err)
	}
	r.store = failingCompactionStore{r.store}
	p, err := nativeContextPolicy(x)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := nativeCompact(x.Context, r, &contextTestModel{}, p, "", nil, result, true)
	if ok || !errors.Is(err, errNativeCheckpoint) {
		t.Fatalf("commit failure: %v %v", ok, err)
	}
	if r.state.Generation != 0 || r.state.InputAdjustment != 200 || !reflect.DeepEqual(r.state.Messages, messages) || !reflect.DeepEqual(result.Messages, messages) {
		t.Fatal("failed commit changed live checkpoint")
	}
	durable, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	if durable.record.Version != r.record.Version || !reflect.DeepEqual(durable.state, r.state) {
		t.Fatal("live and durable state diverged")
	}
	if result.Usage.InputTokens != 30 || durable.state.Usage.InputTokens != 30 {
		t.Fatal("summary usage was rolled back")
	}
}
