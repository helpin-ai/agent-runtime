package workjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// workerScript decides what the scripted worker model does for one batch.
// It receives the batch's item IDs and those already accepted, and returns
// the items to submit in this response; an empty slice finishes the turn.
type workerScript func(batch, accepted []string, step int) (submit []string, finish bool)

// submitAll submits every unresolved item in one response, then finishes.
func submitAll(batch, accepted []string, _ int) ([]string, bool) {
	remaining := unresolved(batch, accepted)
	return remaining, len(remaining) == 0
}

func unresolved(batch, accepted []string) []string {
	done := map[string]bool{}
	for _, id := range accepted {
		done[id] = true
	}
	var out []string
	for _, id := range batch {
		if !done[id] {
			out = append(out, id)
		}
	}
	return out
}

// scriptedWorker is a NativeModel that behaves like a worker: it reads its
// assigned items from the request, submits results through
// submit_item_result and calls finish_turn. Results carry the item ID so the
// dataset can be checked.
type scriptedWorker struct {
	script  workerScript
	failure func(step int) error
	// extra adds tool calls (for example a mutating tool) to a response.
	extra func(batch []string, step int) []runtime.NativeBlock
	// parent answers requests that carry no assigned batch (ordinary runs).
	parent func(req runtime.NativeModelRequest) *runtime.NativeModelResponse
	// workerTools records the tool names offered to workers.
	workerTools map[string]bool
	outcome     func(itemID string) (string, json.RawMessage, *agentcore.WorkItemException)
	calls       atomic.Int64
	mu          sync.Mutex
	batches     [][]string
	beforeGo    func(batch []string, step int)
	// prompts records each worker's system prompt at its first step.
	prompts []string
	// noUsage answers like a provider that reports no token usage.
	noUsage bool
}

func (m *scriptedWorker) ResolveNativeModel(context.Context, *runtime.ExecutionContext, []tools.Definition) (runtime.NativeModel, error) {
	return m, nil
}

var assignedBlock = regexp.MustCompile("(?s)Assigned items \\(\\d+\\):\\n```json\\n(.*?)\\n```")

func (m *scriptedWorker) Generate(_ context.Context, req runtime.NativeModelRequest) (*runtime.NativeModelResponse, error) {
	m.calls.Add(1)
	var batch []string
	var accepted []string
	step := 0
	for _, message := range req.Messages {
		if match := assignedBlock.FindStringSubmatch(message.Content); match != nil {
			var items []struct {
				ItemID string `json:"item_id"`
			}
			if err := json.Unmarshal([]byte(match[1]), &items); err != nil {
				return nil, err
			}
			batch = batch[:0]
			for _, item := range items {
				batch = append(batch, item.ItemID)
			}
		}
		for _, block := range message.Blocks {
			if block.Type == "tool_call" && block.ToolName == submitToolName {
				step++
			}
			if block.Type == "tool_result" && block.ToolName == submitToolName && !block.IsError {
				var receipt agentcore.WorkReceipt
				if json.Unmarshal([]byte(block.Output), &receipt) == nil && receipt.ItemID != "" {
					accepted = append(accepted, receipt.ItemID)
				}
			}
		}
	}
	if len(batch) == 0 {
		if m.parent != nil {
			return m.parent(req), nil
		}
		return nil, fmt.Errorf("scripted worker found no assigned items")
	}
	m.mu.Lock()
	if m.workerTools == nil {
		m.workerTools = map[string]bool{}
	}
	for _, tool := range req.Tools {
		m.workerTools[tool.Name] = true
	}
	m.mu.Unlock()
	if step == 0 {
		m.mu.Lock()
		m.batches = append(m.batches, append([]string(nil), batch...))
		m.prompts = append(m.prompts, req.SystemPrompt)
		m.mu.Unlock()
	}
	if m.beforeGo != nil {
		m.beforeGo(batch, step)
	}
	if m.failure != nil {
		if err := m.failure(step); err != nil {
			return nil, err
		}
	}
	submit, finish := m.script(batch, accepted, step)
	usage := runtime.NativeUsage{InputTokens: 100, OutputTokens: 10}
	if m.noUsage {
		usage = runtime.NativeUsage{}
	}
	var extra []runtime.NativeBlock
	if m.extra != nil {
		extra = m.extra(batch, step)
	}
	if finish || (len(submit) == 0 && len(extra) == 0) {
		input, _ := json.Marshal(map[string]string{"outcome": "completed", "summary": fmt.Sprintf("Submitted %d items.", len(accepted))})
		return &runtime.NativeModelResponse{Usage: usage, Message: runtime.NativeMessage{Role: "assistant", Blocks: []runtime.NativeBlock{
			{Type: "tool_call", ToolCallID: fmt.Sprintf("finish-%d", m.calls.Load()), ToolName: "finish_turn", Input: input},
		}}}, nil
	}
	blocks := append(make([]runtime.NativeBlock, 0, len(submit)+len(extra)), extra...)
	for i, itemID := range submit {
		outcome, result, exception := agentcore.WorkItemOutcomeCompleted, json.RawMessage(fmt.Sprintf(`{"value":%q}`, itemID)), (*agentcore.WorkItemException)(nil)
		if m.outcome != nil {
			outcome, result, exception = m.outcome(itemID)
		}
		body := map[string]any{"item_id": itemID, "outcome": outcome}
		if result != nil {
			body["result"] = result
		}
		if exception != nil {
			body["exception"] = exception
		}
		input, _ := json.Marshal(body)
		blocks = append(blocks, runtime.NativeBlock{Type: "tool_call", ToolCallID: fmt.Sprintf("c%d-%d", m.calls.Load(), i), ToolName: submitToolName, Input: input})
	}
	return &runtime.NativeModelResponse{Usage: usage, Message: runtime.NativeMessage{Role: "assistant", Blocks: blocks}}, nil
}

type harness struct {
	t       *testing.T
	tools   *tools.Registry
	store   *store.SQL
	engine  *engine.Engine
	service *Service
	model   *scriptedWorker
	cancel  context.CancelFunc
	done    chan struct{}
}

const testApp = "app-a"

var testTarget = agentcore.TargetRef{Type: "batch", ID: "B-1"}

func openStore(t *testing.T, path string) *store.SQL {
	t.Helper()
	s, err := store.OpenSQL(store.SQLConfig{Driver: "sqlite", DSN: "file:" + path + "?_busy_timeout=10000&_journal_mode=WAL"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	return s
}

// newHarness starts a "process": an engine, a job service and its
// controller over the SQLite file at path.
func newHarness(t *testing.T, path string, model *scriptedWorker) *harness {
	t.Helper()
	return newHarnessWithSkills(t, path, model, nil, nil)
}

func newHarnessWithSkills(t *testing.T, path string, model *scriptedWorker, skillRegistry *skills.Registry, packages *skills.PackageStoreRegistry) *harness {
	t.Helper()
	s := openStore(t, path)
	targets := host.NewStaticContextProvider()
	targets.Register(testApp, testTarget, host.TargetContext{Summary: "batch"})
	registry := tools.NewRegistry()
	eng := engine.New(engine.Config{
		DefaultExecutionMode: engine.ExecutionModeLightweight,
		AutomaticRecovery:    true,
		Store:                s,
		Runtimes:             runtime.NewRegistry(runtime.NewNativeAdapterWithConfig(runtime.NativeConfig{ModelFactory: model, MaxToolSteps: 40})),
		Tools:                registry,
		Targets:              targets,
		Skills:               skillRegistry,
		SkillPackages:        packages,
	})
	service := New(Config{Engine: eng, Store: s, Tools: registry, Enabled: func(appID string) bool { return appID == testApp }, Interval: 20 * time.Millisecond, RetryBackoff: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, tools: registry, store: s, engine: eng, service: service, model: model, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		service.Run(ctx)
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	<-h.done
}

func (h *harness) agent() string {
	h.t.Helper()
	agent := agentcore.Agent{AppID: testApp, ID: "researcher", Name: "Researcher", RuntimeKind: agentcore.RuntimeNativeSDK}
	if existing, _ := h.store.GetAgent(context.Background(), testApp, agent.ID); existing == nil {
		if err := h.store.CreateAgent(context.Background(), &agent); err != nil {
			h.t.Fatal(err)
		}
	}
	return agent.ID
}

func testSpec() agentcore.WorkJobSpec {
	return agentcore.WorkJobSpec{
		Objective:    "Look up each item",
		ResultSchema: json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"string","minLength":1}}}`),
	}
}

func items(n int) []agentcore.WorkItemInput {
	out := make([]agentcore.WorkItemInput, n)
	for i := range out {
		out[i] = agentcore.WorkItemInput{ID: fmt.Sprintf("item-%04d", i+1), Input: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i+1))}
	}
	return out
}

func (h *harness) create(spec agentcore.WorkJobSpec, n int) *agentcore.WorkJob {
	h.t.Helper()
	job, err := h.service.Create(context.Background(), agentcore.CreateWorkJobRequest{
		AppID: testApp, IdempotencyKey: "job-" + h.t.Name(), Spec: spec,
		Worker: agentcore.WorkJobWorker{AgentID: h.agent(), Target: testTarget}, Items: items(n),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return job
}

func (h *harness) waitState(jobID string, timeout time.Duration, states ...string) *agentcore.WorkJob {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		job, err := h.store.GetWorkJob(context.Background(), testApp, jobID)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, state := range states {
			if job.State == state {
				return job
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("job did not reach %v; state=%s reason=%s counts=%+v error=%q", states, job.State, job.WaitingReason, job.Counts, job.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) attempts(jobID string) []agentcore.WorkAttempt {
	h.t.Helper()
	attempts, err := h.store.ListWorkAttempts(context.Background(), testApp, jobID, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return attempts
}

func tempDB(t *testing.T) string { return filepath.Join(t.TempDir(), "runtime.db") }

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}
