package engine

import (
	"encoding/json"
	"fmt"

	"github.com/helpin-ai/agent-runtime/internal/skills"
)

func runtimePolicyFromExecutionConfig(config json.RawMessage) (skills.Policy, error) {
	if len(config) == 0 || string(config) == "null" {
		return skills.Policy{}, nil
	}
	var envelope struct {
		RuntimePolicy json.RawMessage `json:"runtime_policy"`
	}
	if err := json.Unmarshal(config, &envelope); err != nil {
		return skills.Policy{}, fmt.Errorf("parse agent execution config: %w", err)
	}
	if len(envelope.RuntimePolicy) == 0 || string(envelope.RuntimePolicy) == "null" {
		return skills.Policy{}, nil
	}
	var policy skills.Policy
	if err := json.Unmarshal(envelope.RuntimePolicy, &policy); err != nil {
		return skills.Policy{}, fmt.Errorf("parse runtime_policy: %w", err)
	}
	return skills.AggregatePolicy([]skills.Definition{{Policy: policy}}), nil
}

func mergeRuntimePolicy(base, configured skills.Policy) skills.Policy {
	return skills.AggregatePolicy([]skills.Definition{{Policy: base}, {Policy: configured}})
}
