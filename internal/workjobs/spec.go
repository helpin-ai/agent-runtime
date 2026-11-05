// Package workjobs runs one contract over a list of items. The job is owned
// by the runtime, not by a conversation: items, assignments, accepted
// results, usage and progress live in the main store, a controller
// dispatches bounded batches to fresh worker runs of an existing agent, and
// a worker's final message never completes the job.
package workjobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const (
	DefaultBatchSize         = 10
	MaxBatchSize             = 50
	DefaultMaxAttempts       = 3
	DefaultMaxNoProgress     = 3
	DefaultMaxExceptionRatio = 0.1
	DefaultReservePerAttempt = 200_000
	MaxConcurrency           = 8
	MaxItemsPerRequest       = 10_000
	MaxItemInputBytes        = 16 << 10
	MaxResultBytes           = 64 << 10
	maxItemIDLength          = 200
	maxSchemaBytes           = 64 << 10
	maxInstructionsBytes     = 32 << 10
	maxBatchInputBytes       = 96 << 10
	maxExceptionDetailLength = 2000
	maxExceptionKindLength   = 64
	maxObjectiveLength       = 4000
	defaultRetryBackoff      = 30 * time.Second
)

// normalizeSpec applies defaults and rejects specifications the runtime
// cannot enforce. Schemas are compiled here so a job never starts with a
// contract its validator cannot evaluate.
func normalizeSpec(spec agentcore.WorkJobSpec) (agentcore.WorkJobSpec, error) {
	spec.Objective = strings.TrimSpace(spec.Objective)
	spec.Instructions = strings.TrimSpace(spec.Instructions)
	if spec.Objective == "" {
		return spec, errors.New("spec.objective is required")
	}
	if len(spec.Objective) > maxObjectiveLength || len(spec.Instructions) > maxInstructionsBytes {
		return spec, errors.New("spec.objective or spec.instructions is too long")
	}
	if isJSONNull(spec.ResultSchema) {
		spec.ResultSchema = nil
	}
	if isJSONNull(spec.NoResultSchema) {
		spec.NoResultSchema = nil
	}
	if len(spec.ResultSchema) == 0 {
		return spec, errors.New("spec.result_schema is required")
	}
	if _, err := compileSchema(spec.ResultSchema); err != nil {
		return spec, fmt.Errorf("spec.result_schema: %w", err)
	}
	if len(spec.NoResultSchema) > 0 {
		if _, err := compileSchema(spec.NoResultSchema); err != nil {
			return spec, fmt.Errorf("spec.no_result_schema: %w", err)
		}
	}
	if spec.BatchSize == 0 {
		spec.BatchSize = DefaultBatchSize
	}
	if spec.BatchSize < 1 || spec.BatchSize > MaxBatchSize {
		return spec, fmt.Errorf("spec.batch_size must be between 1 and %d", MaxBatchSize)
	}
	if spec.MaxAttemptsPerItem == 0 {
		spec.MaxAttemptsPerItem = DefaultMaxAttempts
	}
	if spec.MaxAttemptsPerItem < 1 || spec.MaxAttemptsPerItem > 10 {
		return spec, errors.New("spec.max_attempts_per_item must be between 1 and 10")
	}
	if spec.MaxNoProgressAttempts == 0 {
		spec.MaxNoProgressAttempts = DefaultMaxNoProgress
	}
	if spec.MaxNoProgressAttempts < 1 {
		return spec, errors.New("spec.max_no_progress_attempts must be positive")
	}
	if spec.MaxExceptionRatio == nil {
		ratio := DefaultMaxExceptionRatio
		spec.MaxExceptionRatio = &ratio
	}
	if *spec.MaxExceptionRatio < 0 || *spec.MaxExceptionRatio > 1 {
		return spec, errors.New("spec.max_exception_ratio must be between 0 and 1")
	}
	if spec.MaxConcurrency == 0 {
		spec.MaxConcurrency = 1
	}
	if spec.MaxConcurrency < 1 || spec.MaxConcurrency > MaxConcurrency {
		return spec, fmt.Errorf("spec.max_concurrency must be between 1 and %d", MaxConcurrency)
	}
	if spec.Budget.MaxTokens < 0 || spec.Budget.ReserveTokensPerAttempt < 0 {
		return spec, errors.New("spec.budget values must not be negative")
	}
	if spec.Budget.MaxTokens > 0 && spec.Budget.ReserveTokensPerAttempt == 0 {
		spec.Budget.ReserveTokensPerAttempt = min(DefaultReservePerAttempt, spec.Budget.MaxTokens)
	}
	for _, kinds := range [][]string{spec.ExceptionKinds, spec.RetryableExceptionKinds} {
		for _, kind := range kinds {
			if kind = strings.TrimSpace(kind); kind == "" || len(kind) > maxExceptionKindLength {
				return spec, errors.New("exception kinds must be non-empty short names")
			}
		}
	}
	if len(spec.ExceptionKinds) > 0 {
		for _, kind := range spec.RetryableExceptionKinds {
			if !slices.Contains(spec.ExceptionKinds, kind) {
				return spec, fmt.Errorf("retryable exception kind %q is not in exception_kinds", kind)
			}
		}
	}
	return spec, nil
}

func compileSchema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	if len(raw) > maxSchemaBytes {
		return nil, errors.New("schema is too large")
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("schema must be a JSON object")
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	// No loader: a schema cannot make the runtime fetch remote references.
	return schema.Resolve(nil)
}

func validateItems(items []agentcore.WorkItemInput, existing int) error {
	if len(items) == 0 && existing == 0 {
		return errors.New("items are required")
	}
	if len(items) > MaxItemsPerRequest {
		return fmt.Errorf("at most %d items per request", MaxItemsPerRequest)
	}
	seen := make(map[string]bool, len(items))
	for i := range items {
		item := &items[i]
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" || len(item.ID) > maxItemIDLength {
			return fmt.Errorf("item %d: id is required and at most %d characters", i, maxItemIDLength)
		}
		if seen[item.ID] {
			return fmt.Errorf("item id %q is duplicated", item.ID)
		}
		seen[item.ID] = true
		if len(item.Input) == 0 {
			item.Input = json.RawMessage(`{}`)
		}
		if len(item.Input) > MaxItemInputBytes || !json.Valid(item.Input) {
			return fmt.Errorf("item %q: input must be valid JSON of at most %d bytes", item.ID, MaxItemInputBytes)
		}
	}
	return nil
}

// checkDependencies rejects unknown dependencies and cycles among items.
func checkDependencies(items []agentcore.WorkItemInput, known func(string) bool) error {
	graph := make(map[string][]string, len(items))
	for _, item := range items {
		for _, dep := range item.DependsOn {
			if dep == item.ID {
				return fmt.Errorf("item %q depends on itself", item.ID)
			}
			if _, inRequest := graph[dep]; !inRequest && !known(dep) && !slices.ContainsFunc(items, func(other agentcore.WorkItemInput) bool { return other.ID == dep }) {
				return fmt.Errorf("item %q depends on unknown item %q", item.ID, dep)
			}
		}
		graph[item.ID] = item.DependsOn
	}
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			return fmt.Errorf("item dependencies form a cycle through %q", id)
		case done:
			return nil
		}
		state[id] = visiting
		for _, dep := range graph[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = done
		return nil
	}
	for _, item := range items {
		if err := visit(item.ID); err != nil {
			return err
		}
	}
	return nil
}
