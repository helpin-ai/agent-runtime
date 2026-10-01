package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

const a2aTestToken = "secret-token-7f3a"

// fakeA2AAgent is an in-process A2A v1.0 JSON-RPC agent built on the SDK
// server. Each test scripts the events emitted for an incoming message.
type fakeA2AAgent struct {
	server *httptest.Server
	script func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool)

	mu      sync.Mutex
	methods []string
	headers []http.Header
	bodies  []string

	// staleReplies answers a reply to a paused task with the task as it stood
	// before the reply, while the real request keeps running in the background.
	staleReplies bool
	// cutStreams ends each streamed send after its first event, as a dropped
	// connection would, while the task keeps running in the background.
	cutStreams bool
	// holdSends ignores returnImmediately, holding a send open until the task
	// ends, as Hermes does.
	holdSends bool
	// forgetResults answers GetTask without artifacts or a status message, as
	// Hermes does for streamed tasks.
	forgetResults bool
	background    sync.WaitGroup
}

func newFakeA2AAgent(t *testing.T, script func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool)) *fakeA2AAgent {
	t.Helper()
	return newFakeA2AAgentWithStreaming(t, true, script)
}

// newFakeA2AAgentWithStreaming builds the fake agent; without streaming the
// adapter falls back to SendMessage and polling.
func newFakeA2AAgentWithStreaming(t *testing.T, streaming bool, script func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool)) *fakeA2AAgent {
	t.Helper()
	agent := &fakeA2AAgent{script: script}
	mux := http.NewServeMux()
	agent.server = httptest.NewServer(mux)
	t.Cleanup(agent.server.Close)
	card := &a2a.AgentCard{
		Name:                "Hermes",
		Version:             "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(agent.server.URL+"/rpc", a2a.TransportProtocolJSONRPC)},
		Capabilities:        a2a.AgentCapabilities{Streaming: streaming},
	}
	mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(agent))
	mux.Handle("/rpc", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &envelope)
		agent.mu.Lock()
		agent.methods = append(agent.methods, envelope.Method)
		agent.headers = append(agent.headers, r.Header.Clone())
		agent.bodies = append(agent.bodies, string(body))
		agent.mu.Unlock()
		if agent.holdSends {
			body = bytes.ReplaceAll(body, []byte(`"returnImmediately":true`), []byte(`"returnImmediately":false`))
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if agent.staleReplies && envelope.Method == "SendMessage" && agent.answerStale(w, r, rpc, body) {
			return
		}
		if agent.forgetResults && envelope.Method == "GetTask" {
			agent.answerForgetful(w, r, rpc)
			return
		}
		if agent.cutStreams && envelope.Method == "SendStreamingMessage" {
			agent.cutStream(w, r, rpc)
			return
		}
		rpc.ServeHTTP(w, r)
	}))
	t.Cleanup(agent.background.Wait)
	return agent
}

// answerStale replies with the stored task snapshot when the message continues
// an existing task, then delivers the real message after responding.
func (f *fakeA2AAgent) answerStale(w http.ResponseWriter, r *http.Request, rpc http.Handler, body []byte) bool {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Params struct {
			Message struct {
				TaskID string `json:"taskId"`
			} `json:"message"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &request) != nil || request.Params.Message.TaskID == "" {
		return false
	}
	lookup := fmt.Sprintf(`{"jsonrpc":"2.0","id":"stale","method":"GetTask","params":{"id":%q}}`, request.Params.Message.TaskID)
	recorder := httptest.NewRecorder()
	rpc.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(lookup)).WithContext(r.Context()))
	var stored struct {
		Result json.RawMessage `json:"result"`
	}
	var task a2a.Task
	if json.Unmarshal(recorder.Body.Bytes(), &stored) != nil || json.Unmarshal(stored.Result, &task) != nil {
		return false
	}
	result, err := json.Marshal(a2a.StreamResponse{Event: &task})
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result)
	f.background.Add(1)
	go func() {
		defer f.background.Done()
		forward := httptest.NewRequest(http.MethodPost, "/rpc", bytes.NewReader(body))
		forward.Header = r.Header.Clone()
		rpc.ServeHTTP(httptest.NewRecorder(), forward)
	}()
	return true
}

func (f *fakeA2AAgent) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}
		f.script(ctx, execCtx, yield)
	}
}

func (f *fakeA2AAgent) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// cutStream relays the first streamed event, then ends the response while the
// agent keeps working on the task.
func (f *fakeA2AAgent) cutStream(w http.ResponseWriter, r *http.Request, rpc http.Handler) {
	reader, writer := io.Pipe()
	relay := &pipeResponseWriter{header: http.Header{}, writer: writer}
	f.background.Add(1)
	go func() {
		defer f.background.Done()
		rpc.ServeHTTP(relay, r.Clone(context.WithoutCancel(r.Context())))
		_ = writer.Close()
	}()
	var first bytes.Buffer
	buf := make([]byte, 1)
	for !bytes.Contains(first.Bytes(), []byte("\n\n")) {
		if _, err := reader.Read(buf); err != nil {
			break
		}
		first.Write(buf)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write(first.Bytes())
	f.background.Add(1)
	go func() {
		defer f.background.Done()
		_, _ = io.Copy(io.Discard, reader)
	}()
}

// answerForgetful serves GetTask with the task's results removed.
func (f *fakeA2AAgent) answerForgetful(w http.ResponseWriter, r *http.Request, rpc http.Handler) {
	recorder := httptest.NewRecorder()
	rpc.ServeHTTP(recorder, r)
	var envelope map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err == nil {
		if task, ok := envelope["result"].(map[string]any); ok {
			delete(task, "artifacts")
			delete(task, "history")
			if status, ok := task["status"].(map[string]any); ok {
				delete(status, "message")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
		return
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

type pipeResponseWriter struct {
	header http.Header
	writer *io.PipeWriter
}

func (p *pipeResponseWriter) Header() http.Header         { return p.header }
func (p *pipeResponseWriter) WriteHeader(int)             {}
func (p *pipeResponseWriter) Write(b []byte) (int, error) { return p.writer.Write(b) }
func (p *pipeResponseWriter) Flush()                      {}

func isA2ASend(method string) bool {
	return method == "SendMessage" || method == "SendStreamingMessage"
}

func (f *fakeA2AAgent) sends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.methods {
		if isA2ASend(m) {
			n++
		}
	}
	return n
}

func (f *fakeA2AAgent) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.methods {
		if m == method {
			n++
		}
	}
	return n
}

func (f *fakeA2AAgent) sentBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for i, m := range f.methods {
		if isA2ASend(m) {
			out = append(out, f.bodies[i])
		}
	}
	return out
}

func (f *fakeA2AAgent) connection(contextID string) map[string]interface{} {
	return map[string]interface{}{
		"external_agent_id":     "ext-1",
		"name":                  "Hermes",
		"card_url":              f.server.URL + "/.well-known/agent-card.json",
		"auth":                  map[string]interface{}{"type": "bearer", "token": a2aTestToken},
		"context_id":            contextID,
		"message_appendix":      "Upload files with the provided link.",
		"allow_private_network": true,
	}
}

func agentText(execCtx *a2asrv.ExecutorContext, state a2a.TaskState, text string) *a2a.TaskStatusUpdateEvent {
	return a2a.NewStatusUpdateEvent(execCtx, state, a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(text)))
}

type a2aTestHarness struct {
	adapter *A2AAdapter
	mem     *store.Memory
	sink    *testEventSink
	run     *agentcore.AgentRun
}

func newA2AHarness(t *testing.T) *a2aTestHarness {
	t.Helper()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID: "run-1", AppID: "app", AgentID: "agent", RuntimeKind: agentcore.RuntimeA2A,
		Status: agentcore.RunStatusRunning,
		Target: agentcore.TargetRef{Type: "task", ID: "T-1"},
		Input: agentcore.RunInput{
			Instructions: "Record a demo of the login flow.",
			Metadata:     map[string]interface{}{"turn_started_at": time.Now().UTC().Format(time.RFC3339Nano)},
		},
	}
	if err := mem.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	adapter := NewA2AAdapter()
	adapter.pollInitial = 5 * time.Millisecond
	adapter.pollMax = 20 * time.Millisecond
	adapter.cancelGrace = 50 * time.Millisecond
	return &a2aTestHarness{adapter: adapter, mem: mem, sink: &testEventSink{}, run: run}
}

func (h *a2aTestHarness) execCtx(ctx context.Context, conn map[string]interface{}) *ExecutionContext {
	return &ExecutionContext{
		Context:       ctx,
		AppID:         "app",
		Agent:         &agentcore.Agent{ID: "agent", AppID: "app", Name: "Hermes", RuntimeKind: agentcore.RuntimeA2A},
		Run:           h.run,
		Store:         h.mem,
		TargetContext: &host.TargetContext{Summary: "Task T-1: login demo", Data: map[string]interface{}{"a2a": conn, "task": "T-1"}},
		EventSink:     h.sink,
	}
}

func (h *a2aTestHarness) events(state string) []Event {
	var out []Event
	for _, event := range h.sink.events {
		if event.Type == a2aTaskEventType && (state == "" || event.Data["state"] == state) {
			out = append(out, event)
		}
	}
	return out
}

func (h *a2aTestHarness) storedState(t *testing.T) a2aRunState {
	t.Helper()
	stored, err := h.mem.GetRun(context.Background(), "app", h.run.ID)
	if err != nil || stored == nil {
		t.Fatalf("load run: %v", err)
	}
	return loadA2ARunState(stored)
}

func TestA2AAdapterCompletesTurnWithTextAndFiles(t *testing.T) {
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(agentText(execCtx, a2a.TaskStateWorking, "Recording"), nil) {
			return
		}
		file := a2a.NewFileURLPart("https://files.example.com/demo.mp4", "video/mp4")
		file.Filename = "demo.mp4"
		if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart("Demo recorded."), file), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	})
	h := newA2AHarness(t)
	execCtx := h.execCtx(context.Background(), agent.connection(""))

	result, err := h.adapter.Execute(execCtx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.TurnFinished || result.AwaitingInput || !strings.Contains(result.AssistantMessage, "Demo recorded.") {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, ok := execCtx.TargetContext.Data["a2a"]; ok {
		t.Fatal("connection details stayed in the target context")
	}
	if execCtx.TargetContext.Data["task"] != "T-1" {
		t.Fatal("unrelated target context data was removed")
	}
	final := h.events(a2aStateCompleted)
	if len(final) != 1 {
		t.Fatalf("expected one completed event, got %+v", h.sink.events)
	}
	data := final[0].Data
	files, _ := data["files"].([]map[string]interface{})
	if data["external_agent_id"] != "ext-1" || data["message_id"] != "helpin-run-1-initial" || data["message"] != "Demo recorded." ||
		len(files) != 1 || files[0]["url"] != "https://files.example.com/demo.mp4" || files[0]["name"] != "demo.mp4" || files[0]["media_type"] != "video/mp4" {
		t.Fatalf("unexpected completed event: %+v", data)
	}
	state := h.storedState(t)
	if state.State != a2aStateCompleted || state.TaskID == "" || state.ContextID == "" || state.LastMessageID != "helpin-run-1-initial" {
		t.Fatalf("unexpected persisted state: %+v", state)
	}
	sent := agent.sentBodies()
	if len(sent) != 1 {
		t.Fatalf("expected one SendMessage, got %d", len(sent))
	}
	for _, want := range []string{"Record a demo of the login flow.", "Task T-1: login demo", "Upload files with the provided link.", `"returnImmediately":true`} {
		if !strings.Contains(sent[0], want) {
			t.Fatalf("first message is missing %q: %s", want, sent[0])
		}
	}
	agent.mu.Lock()
	header := agent.headers[0]
	agent.mu.Unlock()
	if header.Get("Authorization") != "Bearer "+a2aTestToken || header.Get("A2A-Version") != "1.0" {
		t.Fatalf("unexpected request headers: %v", header)
	}
	assertNoA2AToken(t, h)
}

func TestA2AAdapterPausesForInputThenResumesSameTask(t *testing.T) {
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil {
			yield(agentText(execCtx, a2a.TaskStateInputRequired, "Which environment should I use?"), nil)
			return
		}
		reply := a2aPartsText(execCtx.Message.Parts)
		yield(agentText(execCtx, a2a.TaskStateCompleted, "Used "+reply+"."), nil)
	})
	h := newA2AHarness(t)

	first, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("ctx-known")))
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if !first.AwaitingInput || first.TurnFinished || first.AssistantMessage != "Which environment should I use?" {
		t.Fatalf("unexpected first result: %+v", first)
	}
	paused := h.storedState(t)
	if paused.State != a2aStateInputRequired || paused.ContextID != "ctx-known" {
		t.Fatalf("unexpected paused state: %+v", paused)
	}
	if len(h.events(a2aStateInputRequired)) != 1 {
		t.Fatalf("expected one input_required event, got %+v", h.sink.events)
	}

	h.run.Input.Metadata["last_resume"] = map[string]interface{}{"content": "staging", "resume_id": "resume-1", "intent": "reply"}
	second, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("ctx-known")))
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if !second.TurnFinished || second.AssistantMessage != "Used staging." {
		t.Fatalf("unexpected second result: %+v", second)
	}
	resumed := h.storedState(t)
	if resumed.TaskID != paused.TaskID || resumed.ContextID != "ctx-known" || resumed.LastMessageID != "helpin-resume-1" {
		t.Fatalf("resume did not continue the same task: before %+v after %+v", paused, resumed)
	}
	sent := agent.sentBodies()
	if len(sent) != 2 || !strings.Contains(sent[1], `"taskId":"`+paused.TaskID+`"`) || !strings.Contains(sent[1], `"contextId":"ctx-known"`) ||
		!strings.Contains(sent[1], `"messageId":"helpin-resume-1"`) || strings.Contains(sent[1], "Upload files") {
		t.Fatalf("unexpected resume message: %v", sent)
	}
}

// An agent may hand back the task before it has read the reply, still showing
// the question. The turn must wait for real progress instead of asking the
// user the same question again.
func TestA2AAdapterWaitsPastStaleQuestionAfterReply(t *testing.T) {
	agent := newFakeA2AAgentWithStreaming(t, false, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil {
			yield(agentText(execCtx, a2a.TaskStateInputRequired, "Which environment should I use?"), nil)
			return
		}
		time.Sleep(40 * time.Millisecond)
		yield(agentText(execCtx, a2a.TaskStateCompleted, "Used "+a2aPartsText(execCtx.Message.Parts)+"."), nil)
	})
	agent.staleReplies = true
	h := newA2AHarness(t)
	if _, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection(""))); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if h.storedState(t).StatusMessageID == "" {
		t.Fatal("the question's status message id was not recorded")
	}
	h.run.Input.Metadata["last_resume"] = map[string]interface{}{"content": "staging", "resume_id": "resume-1", "intent": "reply"}
	second, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("")))
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if second.AwaitingInput || !second.TurnFinished || second.AssistantMessage != "Used staging." {
		t.Fatalf("turn stopped at the stale question: %+v", second)
	}
	if got := len(h.events(a2aStateInputRequired)); got != 1 {
		t.Fatalf("the question was reported %d times, want once", got)
	}
	final := h.storedState(t)
	if final.StaleStatusMessageID != "" || final.ReplySentAt != "" {
		t.Fatalf("reply markers were not cleared: %+v", final)
	}
}

func TestA2AAdapterFailedTaskReturnsError(t *testing.T) {
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		// The remote error echoes the credential; the runtime must scrub it.
		yield(agentText(execCtx, a2a.TaskStateFailed, "browser crashed while using "+a2aTestToken), nil)
	})
	h := newA2AHarness(t)
	_, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("")))
	if err == nil || !strings.HasPrefix(err.Error(), "External agent Hermes failed: browser crashed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), a2aTestToken) {
		t.Fatalf("error leaked the token: %v", err)
	}
	if len(h.events(a2aStateFailed)) != 1 {
		t.Fatalf("expected one failed event, got %+v", h.sink.events)
	}
	assertNoA2AToken(t, h)
}

func TestA2AAdapterRetryFollowsTaskWithoutResending(t *testing.T) {
	release := make(chan struct{})
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(agentText(execCtx, a2a.TaskStateWorking, "Working"), nil) {
			return
		}
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		yield(agentText(execCtx, a2a.TaskStateCompleted, "Finished after the retry."), nil)
	})
	h := newA2AHarness(t)

	// The first attempt is interrupted like a worker shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.adapter.Execute(h.execCtx(ctx, agent.connection("")))
		done <- err
	}()
	waitForA2AState(t, h, "working")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted attempt returned %v, want context.Canceled", err)
	}
	if agent.count("CancelTask") != 0 {
		t.Fatal("worker shutdown cancelled the remote task")
	}

	close(release)
	retried, err := h.mem.GetRun(context.Background(), "app", h.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.run = retried
	result, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("")))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !result.TurnFinished || result.AssistantMessage != "Finished after the retry." {
		t.Fatalf("unexpected retry result: %+v", result)
	}
	if got := agent.sends(); got != 1 {
		t.Fatalf("retry resent the message: %d sends", got)
	}
}

func TestA2AAdapterDirectMessageCompletesTurn(t *testing.T) {
	h := newA2AHarness(t)
	// An executor that answers with a Message creates no task.
	direct := a2asrv.AgentExecutorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Hello from Hermes.")), nil)
		}
	})
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	card := &a2a.AgentCard{Name: "Hermes", SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(server.URL+"/rpc", a2a.TransportProtocolJSONRPC)}}
	mux.Handle("/rpc", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(direct)))
	cardJSON, _ := json.Marshal(card)
	var cached map[string]interface{}
	_ = json.Unmarshal(cardJSON, &cached)
	conn := map[string]interface{}{"name": "Hermes", "card_url": server.URL + "/missing-card.json", "agent_card": cached, "allow_private_network": true}

	result, err := h.adapter.Execute(h.execCtx(context.Background(), conn))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.TurnFinished || result.AssistantMessage != "Hello from Hermes." {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(h.events(a2aStateCompleted)) != 1 {
		t.Fatalf("expected one completed event, got %+v", h.sink.events)
	}
}

// Hermes publishes a v1.0 card whose security scheme still uses the v0.3
// shape ({"type": "http", "scheme": "bearer"}).
func TestA2AAdapterAcceptsHermesCardWithV03SecurityScheme(t *testing.T) {
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		yield(agentText(execCtx, a2a.TaskStateCompleted, "Weather checked."), nil)
	})
	card := fmt.Sprintf(`{
		"name": "Hermes", "description": "Hermes Agent", "url": %[1]q, "version": "1.0.0",
		"provider": {"organization": "Hermes Agent", "url": %[1]q},
		"supportedInterfaces": [{"url": %[1]q, "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}],
		"capabilities": {"streaming": false, "pushNotifications": true, "stateTransitionHistory": false, "extendedAgentCard": false},
		"defaultInputModes": ["text/plain"], "defaultOutputModes": ["text/plain"], "skills": [],
		"securitySchemes": {"bearer": {"type": "http", "scheme": "bearer"}},
		"security": [{"bearer": []}]
	}`, agent.server.URL+"/rpc")
	cardServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, card)
	}))
	defer cardServer.Close()
	conn := agent.connection("")
	conn["card_url"] = cardServer.URL + "/.well-known/agent-card.json"
	h := newA2AHarness(t)

	result, err := h.adapter.Execute(h.execCtx(context.Background(), conn))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.TurnFinished || result.AssistantMessage != "Weather checked." {
		t.Fatalf("unexpected result: %+v", result)
	}
	parsed, err := parseA2AAgentCard([]byte(card))
	if err != nil {
		t.Fatalf("parse card: %v", err)
	}
	if scheme, ok := parsed.SecuritySchemes["bearer"].(a2a.HTTPAuthSecurityScheme); !ok || scheme.Scheme != "bearer" {
		t.Fatalf("bearer scheme was not kept: %#v", parsed.SecuritySchemes)
	}
}

// Helpin accepts v0.3 agents, whose cards have a top-level url (transport
// defaulting to JSON-RPC) and which answer the slash-named methods.
func TestA2AAdapterTalksToV03Agent(t *testing.T) {
	executor := a2asrv.AgentExecutorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
			if !yield(agentText(execCtx, a2a.TaskStateWorking, "Looking it up"), nil) {
				return
			}
			yield(agentText(execCtx, a2a.TaskStateCompleted, "Sunny, 21°C."), nil)
		}
	})
	var mu sync.Mutex
	var methods []string
	var auth []string
	rpc := a2av0.NewJSONRPCHandler(a2asrv.NewHandler(executor))
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.Handle("/rpc", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &envelope)
		mu.Lock()
		methods = append(methods, envelope.Method)
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		rpc.ServeHTTP(w, r)
	}))
	mux.Handle("/.well-known/agent-card.json", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name": "Legacy", "url": %q, "protocolVersion": "0.3.0", "version": "1",
			"capabilities": {}, "skills": [], "defaultInputModes": ["text/plain"], "defaultOutputModes": ["text/plain"]}`, server.URL+"/rpc")
	}))
	conn := map[string]interface{}{
		"name":                  "Legacy",
		"card_url":              server.URL + "/.well-known/agent-card.json",
		"auth":                  map[string]interface{}{"type": "bearer", "token": a2aTestToken},
		"allow_private_network": true,
	}
	h := newA2AHarness(t)

	result, err := h.adapter.Execute(h.execCtx(context.Background(), conn))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.TurnFinished || !strings.Contains(result.AssistantMessage, "Sunny, 21°C.") {
		t.Fatalf("unexpected result: %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) == 0 || methods[0] != "message/send" || auth[0] != "Bearer "+a2aTestToken {
		t.Fatalf("expected v0.3 message/send with bearer auth, got methods %v auth %v", methods, auth)
	}
	if len(h.events(a2aStateCompleted)) != 1 {
		t.Fatalf("expected one completed event, got %+v", h.sink.events)
	}
}

func TestA2AAdapterRemoteCancel(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(agentText(execCtx, a2a.TaskStateWorking, "Working"), nil) {
			return
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	h := newA2AHarness(t)
	done := make(chan error, 1)
	go func() {
		_, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("")))
		done <- err
	}()
	waitForA2AState(t, h, "working")
	stored, err := h.mem.GetRun(context.Background(), "app", h.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !h.adapter.NeedsRemoteCancel(stored) {
		t.Fatal("a working task should need a remote cancel")
	}
	target := &host.TargetContext{Data: map[string]interface{}{"a2a": agent.connection("")}}
	if err := h.adapter.CancelRemote(context.Background(), stored, target); err != nil {
		t.Fatalf("cancel remote: %v", err)
	}
	if agent.count("CancelTask") != 1 {
		t.Fatal("CancelTask was not called")
	}
	if _, ok := target.Data["a2a"]; ok {
		t.Fatal("cancel left connection details in the target context")
	}
	select {
	case err := <-done:
		if err == nil || err.Error() != "External agent Hermes canceled the task" {
			t.Fatalf("unexpected turn error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not observe the remote cancellation")
	}
}

// Hermes holds a plain send open until it finishes, so only a stream reveals
// the task ID while it works; a cancellation needs that ID.
func TestA2AAdapterStreamsSoCancelReachesAgentThatHoldsSends(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(agentText(execCtx, a2a.TaskStateWorking, "Recording"), nil) {
			return
		}
		select {
		case <-release:
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	})
	agent.holdSends = true
	h := newA2AHarness(t)
	done := make(chan error, 1)
	go func() {
		_, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("")))
		done <- err
	}()
	waitForA2AState(t, h, "working")
	if agent.count("SendStreamingMessage") != 1 || agent.count("SendMessage") != 0 {
		t.Fatalf("expected one streamed send, got methods %v", agent.methods)
	}
	stored, err := h.mem.GetRun(context.Background(), "app", h.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := &host.TargetContext{Data: map[string]interface{}{"a2a": agent.connection("")}}
	if err := h.adapter.CancelRemote(context.Background(), stored, target); err != nil {
		t.Fatalf("cancel remote: %v", err)
	}
	if agent.count("CancelTask") != 1 {
		t.Fatal("CancelTask was not called")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceled the task") {
			t.Fatalf("unexpected turn error after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not observe the remote cancellation")
	}
}

// Engine.CancelRun cancels the remote task before it marks the run cancelled;
// the agent's "canceled" must not turn the cancellation into a failure.
func TestA2AAdapterRemoteCancelOfLocallyCancelledRunIsNotAFailure(t *testing.T) {
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(agentText(execCtx, a2a.TaskStateWorking, "Working"), nil) {
			return
		}
		time.Sleep(20 * time.Millisecond)
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	})
	h := newA2AHarness(t)
	h.adapter.cancelGrace = 5 * time.Second
	go func() {
		waitForA2AState(t, h, "working")
		time.Sleep(60 * time.Millisecond)
		stored, _ := h.mem.GetRun(context.Background(), "app", h.run.ID)
		stored.Status = agentcore.RunStatusCancelled
		_ = h.mem.UpdateRun(context.Background(), stored)
	}()

	_, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("")))
	if !errors.Is(err, errA2ARunTerminal) {
		t.Fatalf("expected the locally cancelled run to end quietly, got %v", err)
	}
}

func TestA2AAdapterKeepsStreamedReplyWhenStoredTaskHasNone(t *testing.T) {
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart("7")), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	})
	agent.forgetResults = true
	h := newA2AHarness(t)

	result, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("")))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.AssistantMessage != "7" {
		t.Fatalf("streamed reply was lost: %q", result.AssistantMessage)
	}
}

func TestA2AAdapterFollowsTaskAfterStreamDrops(t *testing.T) {
	agent := newFakeA2AAgent(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(agentText(execCtx, a2a.TaskStateWorking, "Recording"), nil) {
			return
		}
		time.Sleep(30 * time.Millisecond)
		yield(agentText(execCtx, a2a.TaskStateCompleted, "Recorded after the stream dropped."), nil)
	})
	agent.cutStreams = true
	h := newA2AHarness(t)

	result, err := h.adapter.Execute(h.execCtx(context.Background(), agent.connection("")))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.TurnFinished || result.AssistantMessage != "Recorded after the stream dropped." {
		t.Fatalf("unexpected result: %+v", result)
	}
	if agent.sends() != 1 || agent.count("GetTask") == 0 {
		t.Fatalf("expected one send then polling, got methods %v", agent.methods)
	}
}

func TestA2AAdapterBlocksPrivateNetworkUnlessAllowed(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tlsServer.Close()

	if _, err := newA2AHTTPClient(false).Get(tlsServer.URL); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("loopback dial was not blocked: %v", err)
	}
	if _, err := newA2AHTTPClient(true).Get(tlsServer.URL); err == nil || strings.Contains(err.Error(), "not allowed") {
		// The test certificate is untrusted, so the allowed dial still fails,
		// but only at TLS verification.
		t.Fatalf("allowed dial was blocked or unexpectedly trusted: %v", err)
	}

	h := newA2AHarness(t)
	for name, conn := range map[string]map[string]interface{}{
		"https loopback": {"name": "Hermes", "card_url": tlsServer.URL + "/card.json", "auth": map[string]interface{}{"type": "bearer", "token": a2aTestToken}},
		"plain http":     {"name": "Hermes", "card_url": "http://127.0.0.1:1/card.json"},
		"cached card": {"name": "Hermes", "card_url": "https://agent.example.com/card.json", "agent_card": map[string]interface{}{
			"name": "Hermes", "supportedInterfaces": []interface{}{map[string]interface{}{"url": "http://127.0.0.1:1/rpc", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}},
		}},
	} {
		_, err := h.adapter.Execute(h.execCtx(context.Background(), conn))
		if err == nil || !(strings.Contains(err.Error(), "not allowed") || strings.Contains(err.Error(), "HTTPS is required") || strings.Contains(err.Error(), "no interface with an allowed URL")) {
			t.Fatalf("%s: private network was reachable: %v", name, err)
		}
		if strings.Contains(err.Error(), a2aTestToken) {
			t.Fatalf("%s: error leaked the token: %v", name, err)
		}
	}
}

func TestA2AAdapterRequiresConnectionDetails(t *testing.T) {
	h := newA2AHarness(t)
	execCtx := h.execCtx(context.Background(), nil)
	delete(execCtx.TargetContext.Data, "a2a")
	if _, err := h.adapter.Execute(execCtx); !errors.Is(err, errA2AConnectionMissing) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestA2ATurnDeadlineUsesTurnStart(t *testing.T) {
	started := time.Now().Add(-100 * time.Minute)
	run := &agentcore.AgentRun{Input: agentcore.RunInput{Metadata: map[string]interface{}{"turn_started_at": started.Format(time.RFC3339Nano)}}}
	deadline := a2aTurnDeadline(run, 110*time.Minute)
	if got := deadline.Sub(started); got != 110*time.Minute {
		t.Fatalf("deadline measured from the wrong start: %s", got)
	}
	if text := a2aDurationText(110 * time.Minute); text != "110 minutes" {
		t.Fatalf("unexpected duration text %q", text)
	}
}

func waitForA2AState(t *testing.T, h *a2aTestHarness, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.storedState(t).State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task never reached %s; stored %+v", state, h.storedState(t))
}

func assertNoA2AToken(t *testing.T, h *a2aTestHarness) {
	t.Helper()
	events, _ := json.Marshal(h.sink.events)
	stored, _ := h.mem.GetRun(context.Background(), "app", h.run.ID)
	metadata, _ := json.Marshal(stored.Input.Metadata)
	if strings.Contains(string(events), a2aTestToken) || strings.Contains(string(metadata), a2aTestToken) {
		t.Fatalf("token leaked into events or run metadata")
	}
}
