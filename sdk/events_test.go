package sdk

import (
	"encoding/json"
	"testing"
)

func TestParseEventEnvelopeAndUsageData(t *testing.T) {
	payload, _ := json.Marshal(EventEnvelope{
		EventID: "event-1",
		AppID:   "helpin",
		RunID:   "run-1",
		Type:    EventUsageCheckpoint,
		Data: map[string]interface{}{
			"usage_semantic": UsageSemanticCumulative,
			"usage": map[string]interface{}{
				"total_tokens":            float64(12),
				"input_tokens":            float64(4),
				"cached_input_tokens":     float64(1),
				"output_tokens":           float64(2),
				"reasoning_output_tokens": float64(3),
			},
		},
	})
	envelope, err := ParseEventEnvelope(payload)
	if err != nil {
		t.Fatalf("ParseEventEnvelope: %v", err)
	}
	data, ok, err := envelope.UsageCheckpoint()
	if err != nil || !ok {
		t.Fatalf("UsageCheckpoint ok=%v err=%v", ok, err)
	}
	if data.UsageSemantic != UsageSemanticCumulative || data.Usage.TotalTokens != 12 || data.Usage.CachedInputTokens != 1 {
		t.Fatalf("unexpected usage data: %#v", data)
	}
}

func TestParseEventEnvelopeRequiresIdentity(t *testing.T) {
	if _, err := ParseEventEnvelope([]byte(`{"type":"run.completed"}`)); err == nil {
		t.Fatal("expected identity validation error")
	}
}
