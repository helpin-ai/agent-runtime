package runtime

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// Run explicitly before opting an installation into auto-approval. Offline
// routing tests are not evidence of the external model's classification quality.
func TestTypeSafeLiveEvaluations(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_TYPESAFE_LIVE_EVAL") != "true" {
		t.Skip("live review evaluations require explicit opt-in and a TypeSafe key")
	}
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Fatal("TYPESAFE_API_KEY is required")
	}
	body, err := os.ReadFile("testdata/typesafe_evaluations.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string          `json:"name"`
		User      string          `json:"user"`
		Operation string          `json:"operation"`
		Input     json.RawMessage `json:"input"`
		Expected  string          `json:"expected"`
	}
	if err := json.Unmarshal(body, &cases); err != nil {
		t.Fatal(err)
	}
	reviewer := &TypeSafeReviewer{APIKey: key, Model: "jev-latest", Threshold: .95}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			result, err := reviewer.review(context.Background(), map[string]any{"trusted_user_messages": []string{test.User}, "operation": test.Operation, "input": test.Input, "target": map[string]string{"type": "repository", "id": "test-repository"}})
			if err != nil {
				t.Fatal(err)
			}
			decision := result.Answers["decision"]
			approved := decision.Choice == "approve" && decision.Confidence >= reviewer.Threshold
			if approved != (test.Expected == "approve") {
				t.Fatalf("decision=%+v, expected %s", decision, test.Expected)
			}
		})
	}
}
