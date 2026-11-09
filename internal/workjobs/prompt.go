package workjobs

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// workerCompletionContract is the child-specific completion prompt. The
// controller enforces it in code: a worker that finishes with unresolved
// items ends its attempt as partial, whatever it says.
const workerCompletionContract = `Batch contract:
- Your request is the assigned batch below, under the pinned job specification. You are one worker of a larger job; other items are handled elsewhere.
- Submit each item's result with submit_item_result as soon as it is done and check its receipt. When several items are ready at once (for example, they need no tools), submit them together as parallel submit_item_result calls in one turn. Only an "accepted" or "retry_scheduled" receipt counts. A receipt releases that item: never research or mutate it again in this attempt. If validation rejects a submission, correct the listed fields; if ownership, generation, or assignment is rejected, stop work on that item.
- When an MCP write exposes _work_item_id, set it to the assigned item being changed. Each write target must match that item's input; never combine targets from different items. If an item-bound write is refused, preserve evidence and report the blocker instead of trying an unscoped write.
- Reuse compact CRM field maps. Request refresh only after a schema change or an unknown-field error; do not repeatedly rediscover the same catalog.
- Work through every assigned item. Do not stop after a few items and do not summarize items you have not submitted.
- MCP or model authentication failure is an execution blocker, not an item exception: stop immediately and wait for authentication recovery. Never submit per-item exceptions for an expired shared connection.
- An obstacle that affects one item is that item's result, not a question for the user: if a source needs a login, is blocked, is unreachable, or the item is ambiguous, submit outcome "exception" with the matching kind and a specific detail, then continue with the next item. An unreachable or blocked source is never "no_result".
- Ask the user only when the whole batch cannot continue.
- Call finish_turn(completed) only after every assigned item has a receipt. Your summary ends this batch attempt; it never completes the job. Do not ask the user to start another batch.`

// workerInstructions builds the pinned request for one attempt.
func workerInstructions(job *agentcore.WorkJob, items []agentcore.WorkItem) (string, error) {
	spec := job.Spec
	var b strings.Builder
	fmt.Fprintf(&b, "Job objective: %s\n\n", spec.Objective)
	if spec.Instructions != "" {
		fmt.Fprintf(&b, "How to process each item:\n%s\n\n", spec.Instructions)
	}
	fmt.Fprintf(&b, "Outcomes for submit_item_result:\n- \"completed\": result must match this JSON Schema:\n%s\n", compactJSON(spec.ResultSchema))
	if len(spec.NoResultSchema) > 0 {
		fmt.Fprintf(&b, "- \"no_result\": you searched properly and nothing exists; result must match this JSON Schema:\n%s\n", compactJSON(spec.NoResultSchema))
	} else {
		b.WriteString("- \"no_result\": you searched properly and nothing exists; put what you checked in result.\n")
	}
	b.WriteString("- \"exception\": the item could not be decided; give exception.kind and exception.detail")
	if len(spec.ExceptionKinds) > 0 {
		fmt.Fprintf(&b, " (kinds: %s)", strings.Join(spec.ExceptionKinds, ", "))
	}
	b.WriteString(".\n\n")
	b.WriteString(workerCompletionContract)
	assigned := make([]map[string]json.RawMessage, 0, len(items))
	for _, item := range items {
		id, _ := json.Marshal(item.ID)
		entry := map[string]json.RawMessage{"item_id": id, "input": item.Input}
		if len(item.Result) > 0 {
			entry["previous_result"] = item.Result
		}
		if item.Exception != nil {
			prior, err := json.Marshal(item.Exception)
			if err != nil {
				return "", err
			}
			entry["previous_exception"] = prior
		}
		assigned = append(assigned, entry)
	}
	raw, err := json.MarshalIndent(assigned, "", " ")
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\n\nAssigned items (%d):\n```json\n%s\n```", len(items), raw)
	return b.String(), nil
}

func compactJSON(raw json.RawMessage) string {
	var out strings.Builder
	var value interface{}
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	encoded, _ := json.Marshal(value)
	out.Write(encoded)
	return out.String()
}
