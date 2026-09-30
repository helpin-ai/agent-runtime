package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
)

const (
	a2aRunMetadataKey     = "a2a"
	a2aTaskEventType      = "a2a.task"
	a2aMaxEventTextBytes  = 64 << 10
	a2aMaxPollErrors      = 5
	a2aRequestTimeout     = time.Minute
	a2aTaskHistoryLength  = 4
	a2aStateSubmitted     = "submitted"
	a2aStateCompleted     = "completed"
	a2aStateInputRequired = "input_required"
	a2aStateAuthRequired  = "auth_required"
	a2aStateFailed        = "failed"
	a2aStateRejected      = "rejected"
	a2aStateCanceled      = "canceled"
)

// A2AAdapter runs a turn on an external agent over the A2A v1.0 protocol.
// The runtime sends one message per turn and follows the resulting task by
// polling GetTask until it finishes or asks for input. Polling survives
// proxies and activity retries better than a two-hour stream, and a retried
// activity can pick the task up again from the persisted task id.
type A2AAdapter struct {
	pollInitial time.Duration
	pollMax     time.Duration
}

func NewA2AAdapter() *A2AAdapter {
	return &A2AAdapter{pollInitial: time.Second, pollMax: 10 * time.Second}
}

func (a *A2AAdapter) Kind() string {
	return agentcore.RuntimeA2A
}

// a2aRunState is persisted in run.Input.Metadata["a2a"] so a retried activity
// follows the task it already started instead of sending the message again.
type a2aRunState struct {
	ContextID       string `json:"context_id"`
	TaskID          string `json:"task_id"`
	LastMessageID   string `json:"last_message_id"`
	State           string `json:"state"`
	StatusMessageID string `json:"status_message_id,omitempty"`
	// StaleStatusMessageID and ReplySentAt mark the question a reply answered.
	// An agent may return the task before it has read the reply; until its
	// status moves past that question the turn keeps polling instead of asking
	// the user the same question again.
	StaleStatusMessageID string `json:"stale_status_message_id,omitempty"`
	ReplySentAt          string `json:"reply_sent_at,omitempty"`
}

func loadA2ARunState(run *agentcore.AgentRun) a2aRunState {
	var state a2aRunState
	if run == nil || run.Input.Metadata == nil {
		return state
	}
	raw, ok := run.Input.Metadata[a2aRunMetadataKey]
	if !ok || raw == nil {
		return state
	}
	encoded, err := json.Marshal(raw)
	if err == nil {
		_ = json.Unmarshal(encoded, &state)
	}
	return state
}

func (s a2aRunState) metadata() map[string]interface{} {
	metadata := map[string]interface{}{
		"context_id":      s.ContextID,
		"task_id":         s.TaskID,
		"last_message_id": s.LastMessageID,
		"state":           s.State,
	}
	if s.StatusMessageID != "" {
		metadata["status_message_id"] = s.StatusMessageID
	}
	if s.StaleStatusMessageID != "" {
		metadata["stale_status_message_id"] = s.StaleStatusMessageID
	}
	if s.ReplySentAt != "" {
		metadata["reply_sent_at"] = s.ReplySentAt
	}
	return metadata
}

type a2aTurn struct {
	MessageID string
	Text      string
	// FollowOnly continues watching a task that is still running, for example
	// after a manual pause, without sending anything new.
	FollowOnly bool
}

func a2aTurnFor(run *agentcore.AgentRun, target *host.TargetContext, conn *a2aConnection, state a2aRunState) (a2aTurn, error) {
	if resume, ok := run.Input.Metadata["last_resume"].(map[string]interface{}); ok {
		content, _ := resume["content"].(string)
		resumeID, _ := resume["resume_id"].(string)
		content, resumeID = strings.TrimSpace(content), strings.TrimSpace(resumeID)
		if content != "" && resumeID != "" {
			return a2aTurn{MessageID: "helpin-" + resumeID, Text: content}, nil
		}
		if state.LastMessageID != "" {
			if state.TaskID != "" && !a2aStateFinal(state.State) {
				return a2aTurn{MessageID: state.LastMessageID, FollowOnly: true}, nil
			}
			return a2aTurn{}, errors.New("there is no new message for the external agent; resume the run with a message")
		}
	}
	var sections []string
	for _, section := range []string{run.Input.Instructions, targetSummary(target), conn.MessageAppendix} {
		if section = strings.TrimSpace(section); section != "" {
			sections = append(sections, section)
		}
	}
	if len(sections) == 0 {
		return a2aTurn{}, errors.New("the run has no instructions to send to the external agent")
	}
	return a2aTurn{MessageID: "helpin-" + run.ID + "-initial", Text: strings.Join(sections, "\n\n")}, nil
}

func targetSummary(target *host.TargetContext) string {
	if target == nil {
		return ""
	}
	return target.Summary
}

// a2aTurnDeadline counts from the turn start recorded by the engine, so an
// activity retry does not restart the clock.
func a2aTurnDeadline(run *agentcore.AgentRun, limit time.Duration) time.Time {
	start := time.Now()
	if raw, ok := run.Input.Metadata["turn_started_at"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil && parsed.Before(start) {
			start = parsed
		}
	}
	return start.Add(limit)
}

func (a *A2AAdapter) Execute(execCtx *ExecutionContext) (*Result, error) {
	if execCtx == nil || execCtx.Run == nil {
		return nil, fmt.Errorf("execution context is incomplete")
	}
	ctx := execCtx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := takeA2AConnection(execCtx)
	if err != nil {
		return nil, err
	}
	run := execCtx.Run
	if run.Input.Metadata == nil {
		run.Input.Metadata = map[string]interface{}{}
	}
	state := loadA2ARunState(run)
	if state.ContextID == "" {
		state.ContextID = conn.ContextID
	}
	turn, err := a2aTurnFor(run, execCtx.TargetContext, conn, state)
	if err != nil {
		return nil, err
	}
	session := &a2aSession{adapter: a, execCtx: execCtx, conn: conn, state: state, turn: turn, name: a2aAgentName(conn, execCtx.Agent)}
	limit := conn.turnLimit()
	turnCtx, cancel := context.WithDeadline(ctx, a2aTurnDeadline(run, limit))
	defer cancel()
	result, err := session.run(turnCtx)
	if err == nil {
		return result, nil
	}
	// Worker shutdown or run cancellation: the remote task keeps running and
	// the durable retry (or Engine.CancelRun) decides what happens to it.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
		session.cancelTaskBestEffort(ctx)
		return nil, fmt.Errorf("External agent %s did not finish within %s", session.name, a2aDurationText(limit))
	}
	return nil, conn.redactErr(err)
}

func a2aAgentName(conn *a2aConnection, agent *agentcore.Agent) string {
	if conn != nil && conn.Name != "" {
		return conn.Name
	}
	if agent != nil && strings.TrimSpace(agent.Name) != "" {
		return strings.TrimSpace(agent.Name)
	}
	return "agent"
}

func a2aDurationText(limit time.Duration) string {
	if limit%time.Minute == 0 {
		minutes := int(limit / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	return limit.String()
}

type a2aSession struct {
	adapter *A2AAdapter
	execCtx *ExecutionContext
	conn    *a2aConnection
	state   a2aRunState
	turn    a2aTurn
	name    string
	client  *a2aclient.Client
}

var errA2ARunTerminal = errors.New("run is already terminal")

func (s *a2aSession) run(ctx context.Context) (*Result, error) {
	client, err := newA2AClient(ctx, s.conn)
	if err != nil {
		return nil, err
	}
	s.client = client
	defer func() { _ = client.Destroy() }()

	var task *a2a.Task
	if s.state.LastMessageID == s.turn.MessageID && s.state.TaskID != "" {
		// A previous attempt of this turn already delivered the message.
		task, err = s.getTask(ctx)
		if err != nil {
			return nil, err
		}
	} else if s.turn.FollowOnly {
		return nil, errors.New("the external agent task to follow is unknown")
	} else {
		if s.state.TaskID != "" && a2aStateInterrupted(s.state.State) {
			s.state.StaleStatusMessageID = s.state.StatusMessageID
			s.state.ReplySentAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		sent, err := client.SendMessage(ctx, s.sendRequest())
		if err != nil {
			return nil, fmt.Errorf("send message to external agent %s: %w", s.name, err)
		}
		switch value := sent.(type) {
		case *a2a.Message:
			return s.finishWithMessage(ctx, value)
		case *a2a.Task:
			task = value
		}
		if task == nil {
			return nil, fmt.Errorf("external agent %s returned neither a task nor a message", s.name)
		}
	}
	if err := s.observe(ctx, task); err != nil {
		return nil, err
	}
	delay := s.adapter.pollInitial
	failures := 0
	for !a2aStateFinal(a2aStateName(task.Status.State)) || s.awaitingReplyProgress(task) {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		next, err := s.getTask(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, a2a.ErrTaskNotFound) {
				return nil, err
			}
			failures++
			if failures >= a2aMaxPollErrors {
				return nil, err
			}
			delay = min(delay*2, s.adapter.pollMax)
			continue
		}
		failures = 0
		if a2aStateName(next.Status.State) != a2aStateName(task.Status.State) {
			delay = s.adapter.pollInitial
		} else {
			delay = min(delay*2, s.adapter.pollMax)
		}
		task = next
		if err := s.observe(ctx, task); err != nil {
			return nil, err
		}
	}
	return s.finish(ctx, task)
}

// awaitingReplyProgress reports whether the task still shows the question the
// turn's reply answered. Without a status message id or timestamp to compare,
// any state counts as progress so the turn cannot wait forever.
func (s *a2aSession) awaitingReplyProgress(task *a2a.Task) bool {
	if s.state.StaleStatusMessageID == "" && s.state.ReplySentAt == "" {
		return false
	}
	if task == nil || !a2aStateInterrupted(a2aStateName(task.Status.State)) {
		return false
	}
	if task.Status.Message != nil && task.Status.Message.ID != "" && s.state.StaleStatusMessageID != "" {
		return task.Status.Message.ID == s.state.StaleStatusMessageID
	}
	if task.Status.Timestamp != nil && s.state.ReplySentAt != "" {
		if sentAt, err := time.Parse(time.RFC3339Nano, s.state.ReplySentAt); err == nil {
			return task.Status.Timestamp.Before(sentAt)
		}
	}
	return false
}

func (s *a2aSession) sendRequest() *a2a.SendMessageRequest {
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(s.turn.Text))
	message.ID = s.turn.MessageID
	message.ContextID = s.state.ContextID
	if s.state.TaskID != "" && a2aStateInterrupted(s.state.State) {
		message.TaskID = a2a.TaskID(s.state.TaskID)
	}
	// ReturnImmediately asks the agent to answer with the created task instead
	// of holding the request open until the task finishes.
	return &a2a.SendMessageRequest{Message: message, Config: &a2a.SendMessageConfig{ReturnImmediately: true}}
}

func (s *a2aSession) getTask(ctx context.Context) (*a2a.Task, error) {
	callCtx, cancel := context.WithTimeout(ctx, a2aRequestTimeout)
	defer cancel()
	history := a2aTaskHistoryLength
	task, err := s.client.GetTask(callCtx, &a2a.GetTaskRequest{ID: a2a.TaskID(s.state.TaskID), HistoryLength: &history})
	if err != nil {
		return nil, fmt.Errorf("read external agent %s task: %w", s.name, err)
	}
	if task == nil {
		return nil, fmt.Errorf("external agent %s returned an empty task", s.name)
	}
	return task, nil
}

// observe records the task position and reports a state change. The final
// state is reported once by finish, with the answer and files.
func (s *a2aSession) observe(ctx context.Context, task *a2a.Task) error {
	next := s.state
	next.TaskID = string(task.ID)
	if task.ContextID != "" {
		next.ContextID = task.ContextID
	}
	next.LastMessageID = s.turn.MessageID
	next.State = a2aStateName(task.Status.State)
	next.StatusMessageID = ""
	if task.Status.Message != nil {
		next.StatusMessageID = task.Status.Message.ID
	}
	if !s.awaitingReplyProgress(task) {
		next.StaleStatusMessageID, next.ReplySentAt = "", ""
	}
	if next == s.state {
		return nil
	}
	s.state = next
	if err := s.persist(ctx); err != nil {
		return err
	}
	if !a2aStateFinal(next.State) {
		s.emit(ctx, a2aStatusText(task), nil)
	}
	return nil
}

func (s *a2aSession) finish(ctx context.Context, task *a2a.Task) (*Result, error) {
	state := a2aStateName(task.Status.State)
	message := a2aStatusText(task)
	if state == a2aStateCompleted {
		if artifacts := a2aArtifactText(task); artifacts != "" {
			message = artifacts
		}
	}
	if message == "" && (state == a2aStateCompleted || a2aStateInterrupted(state)) {
		message = a2aLastAgentText(task)
	}
	files := a2aTaskFiles(task)
	s.emit(ctx, message, files)
	return s.result(state, message, files)
}

// finishWithMessage handles agents that answer directly with a Message and
// create no task. Such an answer is final for the turn.
func (s *a2aSession) finishWithMessage(ctx context.Context, message *a2a.Message) (*Result, error) {
	if message == nil {
		return nil, fmt.Errorf("external agent %s returned an empty message", s.name)
	}
	if message.ContextID != "" {
		s.state.ContextID = message.ContextID
	}
	s.state.TaskID = string(message.TaskID)
	s.state.LastMessageID = s.turn.MessageID
	s.state.State = a2aStateCompleted
	if err := s.persist(ctx); err != nil {
		return nil, err
	}
	text := a2aPartsText(message.Parts)
	files := a2aURLFiles(nil, message.Parts)
	s.emit(ctx, text, files)
	return s.result(a2aStateCompleted, text, files)
}

func (s *a2aSession) result(state, message string, files []map[string]interface{}) (*Result, error) {
	message = s.conn.redact(message)
	switch state {
	case a2aStateCompleted:
		if message == "" {
			message = fmt.Sprintf("%s completed the task.", s.name)
		}
		if len(files) > 0 {
			message += "\n\n" + a2aFileList(files)
		}
		return &Result{AssistantMessage: message, AssistantMessageID: s.turn.MessageID + "-reply", TurnFinished: true, TurnOutcome: "completed"}, nil
	case a2aStateInputRequired, a2aStateAuthRequired:
		if message == "" {
			message = fmt.Sprintf("%s needs more input to continue.", s.name)
		}
		return &Result{AssistantMessage: message, AssistantMessageID: s.turn.MessageID + "-reply", AwaitingInput: true}, nil
	case a2aStateCanceled:
		return nil, fmt.Errorf("External agent %s canceled the task", s.name)
	default:
		if message == "" {
			return nil, fmt.Errorf("External agent %s %s", s.name, state)
		}
		return nil, fmt.Errorf("External agent %s %s: %s", s.name, state, message)
	}
}

func (s *a2aSession) persist(ctx context.Context) error {
	run := s.execCtx.Run
	if run.Input.Metadata == nil {
		run.Input.Metadata = map[string]interface{}{}
	}
	run.Input.Metadata[a2aRunMetadataKey] = s.state.metadata()
	store := s.execCtx.Store
	if store == nil {
		return nil
	}
	// A cancellation may have terminalized the run while the task was polled;
	// writing the in-memory copy back would resurrect it.
	if stored, err := store.GetRun(ctx, run.AppID, run.ID); err == nil && stored != nil && agentcore.IsTerminalStatus(stored.Status) {
		return errA2ARunTerminal
	}
	if err := store.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("record external agent task: %w", err)
	}
	return nil
}

func (s *a2aSession) emit(ctx context.Context, message string, files []map[string]interface{}) {
	if files == nil {
		files = []map[string]interface{}{}
	}
	emitNativeEvent(ctx, s.execCtx, a2aTaskEventType, map[string]interface{}{
		"external_agent_id": s.conn.ExternalAgentID,
		"context_id":        s.state.ContextID,
		"task_id":           s.state.TaskID,
		"state":             s.state.State,
		"message":           truncateNativeText(s.conn.redact(message), a2aMaxEventTextBytes),
		"files":             files,
		"message_id":        s.turn.MessageID,
	})
}

// cancelTaskBestEffort stops a task that outlived the turn limit. It runs on
// a fresh context because the turn context has already expired.
func (s *a2aSession) cancelTaskBestEffort(ctx context.Context) {
	if s.client == nil || s.state.TaskID == "" || a2aStateTerminal(s.state.State) {
		return
	}
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, _ = s.client.CancelTask(cancelCtx, &a2a.CancelTaskRequest{ID: a2a.TaskID(s.state.TaskID)})
}

// NeedsRemoteCancel reports whether the run has a remote task that is still
// alive, including one paused for input.
func (a *A2AAdapter) NeedsRemoteCancel(run *agentcore.AgentRun) bool {
	state := loadA2ARunState(run)
	return state.TaskID != "" && !a2aStateTerminal(state.State)
}

// CancelRemote asks the external agent to cancel the run's task. The caller
// supplies a freshly resolved target context with the connection details.
func (a *A2AAdapter) CancelRemote(ctx context.Context, run *agentcore.AgentRun, targetContext *host.TargetContext) error {
	state := loadA2ARunState(run)
	if state.TaskID == "" {
		return nil
	}
	conn, err := takeA2AConnection(&ExecutionContext{TargetContext: targetContext})
	if err != nil {
		return err
	}
	client, err := newA2AClient(ctx, conn)
	if err != nil {
		return err
	}
	defer func() { _ = client.Destroy() }()
	if _, err := client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(state.TaskID)}); err != nil {
		return fmt.Errorf("cancel external agent task: %w", conn.redactErr(err))
	}
	return nil
}

// a2aStateName converts TASK_STATE_INPUT_REQUIRED to input_required.
func a2aStateName(state a2a.TaskState) string {
	name := strings.ToLower(strings.TrimPrefix(string(state), "TASK_STATE_"))
	if name == "" {
		return a2aStateSubmitted
	}
	return name
}

func a2aStateTerminal(state string) bool {
	switch state {
	case a2aStateCompleted, a2aStateFailed, a2aStateRejected, a2aStateCanceled:
		return true
	}
	return false
}

func a2aStateInterrupted(state string) bool {
	return state == a2aStateInputRequired || state == a2aStateAuthRequired
}

// a2aStateFinal reports whether the turn ends in this state.
func a2aStateFinal(state string) bool {
	return a2aStateTerminal(state) || a2aStateInterrupted(state)
}

func a2aStatusText(task *a2a.Task) string {
	if task == nil || task.Status.Message == nil {
		return ""
	}
	return a2aPartsText(task.Status.Message.Parts)
}

func a2aArtifactText(task *a2a.Task) string {
	var texts []string
	for _, artifact := range task.Artifacts {
		if artifact == nil {
			continue
		}
		if text := a2aPartsText(artifact.Parts); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func a2aLastAgentText(task *a2a.Task) string {
	for i := len(task.History) - 1; i >= 0; i-- {
		message := task.History[i]
		if message != nil && message.Role == a2a.MessageRoleAgent {
			if text := a2aPartsText(message.Parts); text != "" {
				return text
			}
		}
	}
	return ""
}

func a2aPartsText(parts a2a.ContentParts) string {
	var texts []string
	for _, part := range parts {
		if part == nil {
			continue
		}
		if text, ok := part.Content.(a2a.Text); ok && strings.TrimSpace(string(text)) != "" {
			texts = append(texts, strings.TrimSpace(string(text)))
		}
	}
	return strings.Join(texts, "\n")
}

func a2aTaskFiles(task *a2a.Task) []map[string]interface{} {
	var files []map[string]interface{}
	for _, artifact := range task.Artifacts {
		if artifact != nil {
			files = a2aURLFiles(files, artifact.Parts)
		}
	}
	if task.Status.Message != nil {
		files = a2aURLFiles(files, task.Status.Message.Parts)
	}
	return files
}

// a2aURLFiles collects HTTP(S) URL parts. Inline bytes are never forwarded;
// the host fetches each URL through its own size- and type-capped client.
func a2aURLFiles(files []map[string]interface{}, parts a2a.ContentParts) []map[string]interface{} {
	for _, part := range parts {
		if part == nil {
			continue
		}
		link, ok := part.Content.(a2a.URL)
		value := strings.TrimSpace(string(link))
		lower := strings.ToLower(value)
		if !ok || !(strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")) {
			continue
		}
		duplicate := false
		for _, existing := range files {
			if existing["url"] == value {
				duplicate = true
				break
			}
		}
		if !duplicate {
			files = append(files, map[string]interface{}{"name": part.Filename, "media_type": part.MediaType, "url": value})
		}
	}
	return files
}

func a2aFileList(files []map[string]interface{}) string {
	lines := make([]string, 0, len(files)+1)
	lines = append(lines, "Files:")
	for _, file := range files {
		name, _ := file["name"].(string)
		link, _ := file["url"].(string)
		if name == "" {
			lines = append(lines, "- "+link)
		} else {
			lines = append(lines, fmt.Sprintf("- %s: %s", name, link))
		}
	}
	return strings.Join(lines, "\n")
}
