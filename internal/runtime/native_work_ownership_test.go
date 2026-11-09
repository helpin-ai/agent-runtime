package runtime

import (
	"context"
	"encoding/json"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"strings"
	"testing"
)

type ownershipStore struct {
	agentcore.Store
	agentcore.WorkJobStore
	agentcore.NativeStateStore
	attempt *agentcore.WorkAttempt
	job     *agentcore.WorkJob
	items   []agentcore.WorkItem
}

func (s ownershipStore) GetWorkAttemptByRun(context.Context, string, string) (*agentcore.WorkAttempt, error) {
	return s.attempt, nil
}
func (s ownershipStore) GetWorkJob(context.Context, string, string) (*agentcore.WorkJob, error) {
	return s.job, nil
}
func (s ownershipStore) GetWorkItems(context.Context, string, string, []string) ([]agentcore.WorkItem, error) {
	return s.items, nil
}

func TestWorkerAdmissionHonorsAssignmentsAndPause(t *testing.T) {
	for _, tc := range []struct {
		name, jobState, itemState, attemptID string
		generation                           int64
		allow                                bool
	}{
		{"owned", "running", "running", "attempt", 1, true},
		{"surrendered", "running", "retry_wait", "attempt", 1, false},
		{"reassigned", "running", "running", "replacement", 1, false},
		{"accepted", "running", "completed", "attempt", 1, false},
		{"paused batch drains", "paused", "running", "attempt", 1, true},
		{"cancelled", "cancelled", "running", "attempt", 1, false},
		{"stale generation", "running", "running", "attempt", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := contextTestExec(t)
			x.Run.Input.Metadata = map[string]any{"work_attempt_id": "attempt", "worker_write_scope": "item"}
			x.Store = ownershipStore{Store: x.Store, attempt: &agentcore.WorkAttempt{ID: "attempt", State: "open", Generation: 1, JobID: "job", ItemIDs: []string{"item"}}, job: &agentcore.WorkJob{State: tc.jobState}, items: []agentcore.WorkItem{{ID: "item", State: tc.itemState, AttemptID: tc.attemptID}}}
			err := nativeWorkAllowsTool(agentcore.WithExecutionGeneration(x.Context, tc.generation), x, "write_crm")
			if (err == nil) != tc.allow {
				t.Fatalf("admission=%v, wanted allowed=%v", err, tc.allow)
			}
		})
	}
}

func TestReleasedWorkerCompletesWithoutModelOrMutation(t *testing.T) {
	x := contextTestExec(t)
	x.Run.Input.Metadata = map[string]any{"work_attempt_id": "attempt", "worker_write_scope": "item"}
	x.Context = agentcore.WithExecutionGeneration(x.Context, 1)
	x.Store = ownershipStore{Store: x.Store, NativeStateStore: x.Store.(agentcore.NativeStateStore), attempt: &agentcore.WorkAttempt{ID: "attempt", State: "open", Generation: 1, JobID: "job"}, job: &agentcore.WorkJob{State: "running"}}
	m := &contextTestModel{}
	result, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}})
	if err != nil || !result.TurnFinished || len(m.requests) != 0 {
		t.Fatalf("released worker ran: result=%+v requests=%d err=%v", result, len(m.requests), err)
	}
}

func TestWorkerCannotWriteReleasedItemUsingOwnedItemLabel(t *testing.T) {
	x := contextTestExec(t)
	x.Run.Input.Metadata = map[string]any{"work_attempt_id": "attempt", "worker_write_scope": "item"}
	x.Context = agentcore.WithExecutionGeneration(x.Context, 1)
	x.Store = ownershipStore{Store: x.Store, NativeStateStore: x.Store.(agentcore.NativeStateStore), attempt: &agentcore.WorkAttempt{ID: "attempt", State: "open", Generation: 1, JobID: "job", ItemIDs: []string{"released", "owned"}}, job: &agentcore.WorkJob{State: "running"}, items: []agentcore.WorkItem{
		{ID: "released", AttemptID: "attempt", State: "retry_wait", Input: json.RawMessage(`{"company_id":"company-a","domain":"a.example"}`)},
		{ID: "owned", AttemptID: "attempt", State: "running", Input: json.RawMessage(`{"company_id":"company-b","domain":"b.example"}`)},
	}}
	writes := 0
	x.AllowedTools["mcp__crm__upsert_crm_records"] = true
	x.Tools.Register(tools.Definition{Name: "mcp__crm__upsert_crm_records", Category: "MCP / crm", Mutating: true, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		writes++
		return json.RawMessage(`{}`), nil
	})
	call := NativeBlock{Type: nativeBlockTypeToolCall, ToolCallID: "write", ToolName: "mcp__crm__upsert_crm_records", Input: json.RawMessage(`{"_work_item_id":"owned","object_type":"company","match_on":"id","records":[{"id":"company-a","fields":{"name":"Wrong item"}}]}`)}
	x.Agent.ExecutionConfig = nil
	model := &fakeNativeModel{responses: []NativeModelResponse{{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{call}}}}}
	result, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: model}})
	if err != nil {
		t.Fatal(err)
	}
	rejected := len(result.ToolSummaries) > 0 && result.ToolSummaries[0].Error != ""
	t.Logf("external_writes=%d rejected=%v", writes, rejected)
	if writes != 0 || !rejected {
		t.Fatalf("released target escaped its fence: writes=%d rejected=%v", writes, rejected)
	}
}

func TestWorkerWriteTargetsAndStrippedItemLabel(t *testing.T) {
	x := contextTestExec(t)
	x.Run.Input.Metadata = map[string]any{"work_attempt_id": "attempt", "worker_write_scope": "item"}
	x.Context = agentcore.WithExecutionGeneration(x.Context, 1)
	x.Store = ownershipStore{Store: x.Store, attempt: &agentcore.WorkAttempt{ID: "attempt", State: "open", Generation: 1, JobID: "job", ItemIDs: []string{"a"}}, job: &agentcore.WorkJob{State: "running"}, items: []agentcore.WorkItem{{ID: "a", AttemptID: "attempt", State: "running", Input: json.RawMessage(`{"company_id":"company-a","domain":"a.example"}`)}}}
	schema := json.RawMessage(`{"type":"object","properties":{},"x-runtime-work-item-targets":[{"when":{"object_type":"company"},"records":"records/*","targets":{"id":["company_id"],"fields/domain":["domain"]}}]}`)
	def := tools.Definition{Name: "upsert", Category: "MCP / crm", Mutating: true, InputSchema: schema}
	for _, tc := range []struct {
		name, input string
		allowed     bool
	}{
		{"owned id", `{"_work_item_id":"a","object_type":"company","records":[{"id":"company-a"}]}`, true},
		{"owned domain", `{"_work_item_id":"a","object_type":"company","records":[{"fields":{"domain":"a.example"}}]}`, true},
		{"wrong target", `{"_work_item_id":"a","object_type":"company","records":[{"id":"company-b"}]}`, false},
		{"mixed batch", `{"_work_item_id":"a","object_type":"company","records":[{"id":"company-a"},{"id":"company-b"}]}`, false},
		{"conflicting domain", `{"_work_item_id":"a","object_type":"company","records":[{"id":"company-a","fields":{"domain":"b.example"}}]}`, false},
		{"missing target", `{"_work_item_id":"a","object_type":"company","records":[{"fields":{"name":"a"}}]}`, false},
		{"different entity", `{"_work_item_id":"a","object_type":"contact","records":[{"id":"company-a"}]}`, false},
		{"missing item", `{"object_type":"company","records":[{"id":"company-a"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clean, err := nativeWorkWriteInput(x.Context, x, def, json.RawMessage(tc.input))
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v err=%v", tc.allowed, err)
			}
			if err == nil && strings.Contains(string(clean), nativeWorkItemArgument) {
				t.Fatal("internal item label leaked to provider arguments")
			}
		})
	}
	normal := *x.Run
	normal.Input.Metadata = nil
	x.Run = &normal
	raw := json.RawMessage(`{"object_type":"company","records":[{"id":"other"}]}`)
	if clean, err := nativeWorkWriteInput(x.Context, x, def, raw); err != nil || string(clean) != string(raw) {
		t.Fatalf("ordinary integration changed: %s %v", clean, err)
	}
}

func TestLegacyWorkerWriteContractIsUnchanged(t *testing.T) {
	x := contextTestExec(t)
	x.Run.Input.Metadata = map[string]any{"work_attempt_id": "legacy"}
	def := tools.Definition{Name: "legacy_write", Category: "MCP / legacy", Mutating: true}
	input := json.RawMessage(`{"id":"existing"}`)
	output, err := nativeWorkWriteInput(x.Context, x, def, input)
	if err != nil || string(output) != string(input) {
		t.Fatalf("legacy worker contract changed: %v", err)
	}
}

func TestApprovedWorkerWriteRechecksReleasedItem(t *testing.T) {
	x := contextTestExec(t)
	x.Agent.ExecutionConfig = nil
	x.Agent.ApprovalMode = agentcore.ApprovalModeAlways
	x.Run.Input.Metadata = map[string]any{"work_attempt_id": "attempt", "worker_write_scope": "item"}
	x.Context = agentcore.WithExecutionGeneration(x.Context, 1)
	owned := []agentcore.WorkItem{{ID: "a", AttemptID: "attempt", State: "running", Input: json.RawMessage(`{"company_id":"company-a"}`)}, {ID: "b", AttemptID: "attempt", State: "running", Input: json.RawMessage(`{"company_id":"company-b"}`)}}
	x.Store = ownershipStore{Store: x.Store, NativeStateStore: x.Store.(agentcore.NativeStateStore), attempt: &agentcore.WorkAttempt{ID: "attempt", JobID: "job", State: "open", Generation: 1, ItemIDs: []string{"a", "b"}}, job: &agentcore.WorkJob{State: "running"}, items: owned}
	if err := x.Store.CreateRun(x.Context, x.Run); err != nil {
		t.Fatal(err)
	}
	writes := 0
	name := "mcp__crm__write"
	x.AllowedTools[name] = true
	x.Tools.Register(tools.Definition{Name: name, Category: "MCP / crm", Mutating: true, InputSchema: json.RawMessage(`{"type":"object","properties":{},"x-runtime-work-item-targets":[{"targets":{"company_id":["company_id"]}}]}`)}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		writes++
		return json.RawMessage(`{}`), nil
	})
	model := &fakeNativeModel{responses: []NativeModelResponse{{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "write-a", ToolName: name, Input: json.RawMessage(`{"_work_item_id":"a","company_id":"company-a"}`)}}}}}}
	result, err := NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: model}}).Execute(x)
	if err != nil || !result.WaitForApproval || writes != 0 {
		t.Fatalf("pause failed: %v", err)
	}
	interactions, err := x.Store.ListInteractions(x.Context, x.AppID, x.Run.ID)
	if err != nil || len(interactions) != 1 {
		t.Fatalf("interaction missing: %v", err)
	}
	resolveInteraction(t, x.Store, x.AppID, x.Run.ID, interactions[0].ID, "approve", "")
	owned[0].State = "completed"
	x.Run.OutputSummary = result.OutputSummary
	x.Run.Input.Metadata["last_resume"] = map[string]any{"intent": "approve"}
	_, err = NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: &fakeNativeModel{}}}).Execute(x)
	if err == nil || writes != 0 {
		t.Fatalf("released approved write escaped: writes=%d err=%v", writes, err)
	}
}
