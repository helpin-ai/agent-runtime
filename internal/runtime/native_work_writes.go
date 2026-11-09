package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const nativeWorkItemArgument = "_work_item_id"
const nativeWorkTargetsExtension = "x-runtime-work-item-targets"

// MCP write contracts declare the target selectors, never the worker. A selector
// is a slash-separated property path; '*' expands every record in a batch.
type nativeWorkTargetPolicy struct {
	When    map[string]string   `json:"when"`
	Records string              `json:"records,omitempty"`
	Targets map[string][]string `json:"targets"`
}

func nativeWorkerMCPWrite(x *ExecutionContext, def tools.Definition) bool {
	// Hosts opt in when their providers declare target bindings; legacy jobs retain their contract.
	return x.Run.Input.Metadata["worker_write_scope"] == "item" && x.Run.Input.Metadata["work_attempt_id"] != nil && def.Mutating && strings.HasPrefix(def.Category, "MCP / ")
}

func nativeWorkWriteDefinition(x *ExecutionContext, def tools.Definition) tools.Definition {
	if !nativeWorkerMCPWrite(x, def) {
		return def
	}
	raw, _ := json.Marshal(def.InputSchema)
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return def
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props[nativeWorkItemArgument] = map[string]any{"type": "string", "description": "Exact assigned item_id whose target this write affects. Released items cannot be written."}
	schema["properties"] = props
	required, _ := schema["required"].([]any)
	schema["required"] = append(required, nativeWorkItemArgument)
	def.InputSchema = schema
	return def
}

func nativeWorkWriteInput(ctx context.Context, x *ExecutionContext, def tools.Definition, input json.RawMessage) (json.RawMessage, error) {
	if !nativeWorkerMCPWrite(x, def) {
		return input, nil
	}
	var args map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(input)))
	decoder.UseNumber()
	if decoder.Decode(&args) != nil {
		return nil, fmt.Errorf("invalid worker write arguments")
	}
	itemID, _ := args[nativeWorkItemArgument].(string)
	owned, _, err := nativeOwnedWorkItems(ctx, x)
	if err != nil {
		return nil, err
	}
	found := false
	for _, id := range owned {
		if id == itemID {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("write refused: %s must name a currently owned item; released items cannot be written", nativeWorkItemArgument)
	}
	ws := x.Store.(agentcore.WorkJobStore)
	attempt, err := ws.GetWorkAttemptByRun(ctx, x.AppID, x.Run.ID)
	if err != nil {
		return nil, err
	}
	if attempt == nil {
		return nil, fmt.Errorf("worker assignment no longer exists")
	}
	items, err := ws.GetWorkItems(ctx, x.AppID, attempt.JobID, []string{itemID})
	if err != nil {
		return nil, err
	}
	var source map[string]any
	for _, item := range items {
		if item.ID == itemID && item.AttemptID == attempt.ID && item.State == agentcore.WorkItemStateRunning {
			if err := json.Unmarshal(item.Input, &source); err != nil {
				return nil, fmt.Errorf("item has no object target data")
			}
		}
	}
	if source == nil {
		return nil, fmt.Errorf("item assignment changed before write")
	}
	raw, _ := json.Marshal(def.InputSchema)
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil {
		return nil, fmt.Errorf("MCP write has no target contract")
	}
	var policies []nativeWorkTargetPolicy
	if json.Unmarshal(schema[nativeWorkTargetsExtension], &policies) != nil || len(policies) == 0 {
		return nil, fmt.Errorf("MCP write refused: provider does not declare item-target bindings; update the provider before enabling worker writes")
	}
	matched := false
	for _, policy := range policies {
		applies := true
		for path, want := range policy.When {
			values := nativeWorkPath(args, path)
			if len(values) != 1 || fmt.Sprint(values[0]) != want {
				applies = false
				break
			}
		}
		if !applies {
			continue
		}
		matched = true
		records := []any{args}
		if policy.Records != "" {
			records = nativeWorkPath(args, policy.Records)
		}
		if len(records) == 0 {
			return nil, fmt.Errorf("write target records are missing")
		}
		for _, record := range records {
			bound := false
			for argument, paths := range policy.Targets {
				values := nativeWorkPath(record, argument)
				for _, value := range values {
					text, ok := value.(string)
					if !ok || strings.TrimSpace(text) == "" {
						return nil, fmt.Errorf("write target %s must be a nonempty identifier", argument)
					}
					allowed := false
					for _, path := range paths {
						for _, candidate := range nativeWorkPath(source, path) {
							if candidate == text {
								allowed = true
							}
						}
					}
					if !allowed {
						return nil, fmt.Errorf("write target %s does not belong to assigned item %q", argument, itemID)
					}
					bound = true
				}
			}
			if !bound {
				return nil, fmt.Errorf("write has no target bound to assigned item %q", itemID)
			}
		}
	}
	if !matched {
		return nil, fmt.Errorf("this write operation has no item-target binding")
	}
	delete(args, nativeWorkItemArgument)
	return json.Marshal(args)
}

func nativeWorkPath(value any, path string) []any {
	path = strings.Trim(path, "/")
	if path == "" {
		return []any{value}
	}
	parts := strings.SplitN(path, "/", 2)
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	if parts[0] == "*" {
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		var out []any
		for _, item := range items {
			out = append(out, nativeWorkPath(item, rest)...)
		}
		return out
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	child, ok := object[parts[0]]
	if !ok {
		return nil
	}
	return nativeWorkPath(child, rest)
}
