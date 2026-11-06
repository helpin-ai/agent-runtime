package workjobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const submitToolName = engine.WorkJobSubmitTool

func submitToolDefinition() tools.Definition {
	return tools.Definition{
		Name:        submitToolName,
		Description: "Submit the result of one assigned work item. The runtime validates it against the job contract and returns a receipt; only an accepted receipt counts. Submit each item as soon as it is done.",
		Category:    "Runtime control",
		// A runtime control, like finish_turn: it records progress in the job
		// and has no external effect, so it never waits for approval.
		Mutating:  false,
		RiskLevel: tools.RiskLevelRead,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id": map[string]any{"type": "string", "description": "The assigned item's item_id."},
				"outcome": map[string]any{"type": "string", "enum": []string{agentcore.WorkItemOutcomeCompleted, agentcore.WorkItemOutcomeNoResult, agentcore.WorkItemOutcomeException},
					"description": "completed: result matches the job's result schema. no_result: searched properly and nothing exists. exception: could not decide (login needed, blocked, unreachable, ambiguous)."},
				"result": map[string]any{"type": "object", "description": "The item result for completed and no_result outcomes."},
				"exception": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind":   map[string]any{"type": "string"},
						"detail": map[string]any{"type": "string"},
					},
					"required": []string{"kind", "detail"},
				},
			},
			"required":             []string{"item_id", "outcome"},
			"additionalProperties": false,
		},
	}
}

type submitInput struct {
	ItemID    string                       `json:"item_id"`
	Outcome   string                       `json:"outcome"`
	Result    json.RawMessage              `json:"result,omitempty"`
	Exception *agentcore.WorkItemException `json:"exception,omitempty"`
}

// handleSubmit is the submit_item_result handler. App and run come from the
// trusted call context; the attempt is found from the run, so a model cannot
// name another job, attempt or generation.
func (s *Service) handleSubmit(ctx context.Context, call tools.CallContext, raw json.RawMessage) (json.RawMessage, error) {
	attempt, err := s.store.GetWorkAttemptByRun(ctx, call.AppID, call.RunID)
	if err != nil {
		return nil, err
	}
	if attempt == nil {
		return nil, errors.New("submit_item_result is only available to work job workers")
	}
	job, err := s.store.GetWorkJob(ctx, call.AppID, attempt.JobID)
	if err != nil {
		return nil, err
	}
	var input submitInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, fmt.Errorf("invalid submit_item_result input: %v", err)
	}
	input.ItemID = strings.TrimSpace(input.ItemID)
	if problems := validateSubmission(job.Spec, &input); len(problems) > 0 {
		return nil, fmt.Errorf("result for %q was not accepted: %s", input.ItemID, strings.Join(problems, "; "))
	}
	retryable := input.Outcome == agentcore.WorkItemOutcomeException && slices.Contains(job.Spec.RetryableExceptionKinds, input.Exception.Kind)
	// The engine records the execution's authority when it starts; without
	// it the submission cannot be fenced and is refused.
	generation, ok := agentcore.ExecutionGenerationFromContext(ctx)
	if !ok {
		return nil, errors.New("this execution has no work job authority; stop and finish the turn")
	}
	receipt, err := s.store.AcceptWorkResult(ctx, agentcore.WorkResult{
		AppID: call.AppID, AttemptID: attempt.ID, RunID: call.RunID, ItemID: input.ItemID, Outcome: input.Outcome,
		Result: input.Result, Exception: input.Exception, Digest: submissionDigest(input),
		Retryable: retryable, RetryAfter: s.cfg.RetryBackoff, Generation: generation, Now: s.now(),
	})
	var rejected *agentcore.WorkRejectedError
	if errors.As(err, &rejected) {
		return nil, fmt.Errorf("result for %q was not accepted: %s", input.ItemID, rejected.Reason)
	}
	if err != nil {
		return nil, err
	}
	s.Wake()
	return json.Marshal(receipt)
}

// validateSubmission is the deterministic phase 1a validator: outcome,
// schema, exception policy and size. It returns actionable field errors.
func validateSubmission(spec agentcore.WorkJobSpec, input *submitInput) []string {
	var problems []string
	if input.ItemID == "" {
		problems = append(problems, "item_id is required")
	}
	if len(input.Result) > MaxResultBytes {
		problems = append(problems, fmt.Sprintf("result is larger than %d bytes", MaxResultBytes))
		return problems
	}
	if isJSONNull(input.Result) {
		input.Result = nil
	}
	switch input.Outcome {
	case agentcore.WorkItemOutcomeCompleted:
		if input.Exception != nil {
			problems = append(problems, "exception is only allowed with outcome exception")
		}
		if len(input.Result) == 0 {
			problems = append(problems, "result is required for outcome completed")
		} else if err := validateAgainst(spec.ResultSchema, input.Result); err != nil {
			problems = append(problems, "result: "+err.Error())
		}
	case agentcore.WorkItemOutcomeNoResult:
		if input.Exception != nil {
			problems = append(problems, "exception is only allowed with outcome exception; an unreachable or blocked source is an exception, not no_result")
		}
		if len(spec.NoResultSchema) > 0 {
			if len(input.Result) == 0 {
				problems = append(problems, "result is required for outcome no_result")
			} else if err := validateAgainst(spec.NoResultSchema, input.Result); err != nil {
				problems = append(problems, "result: "+err.Error())
			}
		}
	case agentcore.WorkItemOutcomeException:
		if input.Exception == nil {
			problems = append(problems, "exception.kind and exception.detail are required for outcome exception")
			break
		}
		input.Exception.Kind = strings.TrimSpace(input.Exception.Kind)
		input.Exception.Detail = strings.TrimSpace(input.Exception.Detail)
		if input.Exception.Kind == "" || input.Exception.Detail == "" {
			problems = append(problems, "exception.kind and exception.detail are required")
		}
		if len(spec.ExceptionKinds) > 0 && input.Exception.Kind != "" && !slices.Contains(spec.ExceptionKinds, input.Exception.Kind) {
			problems = append(problems, fmt.Sprintf("exception.kind must be one of %s", strings.Join(spec.ExceptionKinds, ", ")))
		}
		if len(input.Exception.Kind) > maxExceptionKindLength || len(input.Exception.Detail) > maxExceptionDetailLength {
			problems = append(problems, "exception is too long")
		}
	default:
		problems = append(problems, "outcome must be completed, no_result or exception")
	}
	return problems
}

func validateAgainst(schema, value json.RawMessage) error {
	resolved, err := compileSchema(schema)
	if err != nil {
		return err
	}
	var instance any
	if err := json.Unmarshal(value, &instance); err != nil {
		return err
	}
	return resolved.Validate(instance)
}

func isJSONNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// submissionDigest identifies submitted content independent of key order,
// so an identical replay is recognized.
func submissionDigest(input submitInput) string {
	normalized := map[string]any{"item_id": input.ItemID, "outcome": input.Outcome}
	if len(input.Result) > 0 {
		var result any
		_ = json.Unmarshal(input.Result, &result)
		normalized["result"] = result
	}
	if input.Exception != nil {
		normalized["exception"] = input.Exception
	}
	raw, _ := json.Marshal(normalized)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
