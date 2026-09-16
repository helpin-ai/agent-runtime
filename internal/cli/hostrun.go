package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/id"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Admission is an immutable host policy snapshot. All local execution still
// passes the CLI's own tool and approval policy before reaching the OS.
type Admission struct {
	Execution    *ExecutionGrant `json:"execution,omitempty"`
	RunID        string          `json:"run_id"`
	Agent        agentcore.Agent `json:"agent"`
	Context      string          `json:"context"`
	AllowedTools []string        `json:"allowed_tools"`
}
type hostFactory struct {
	home     string
	fallback runtime.NativeModelFactory
}

func (f hostFactory) ResolveNativeModel(ctx context.Context, x *runtime.ExecutionContext, defs []tools.Definition) (runtime.NativeModel, error) {
	name, _ := x.Run.Input.Metadata["cli_connection"].(string)
	if name == "" {
		if f.fallback == nil {
			return nil, errors.New("no model configured: connect a compatible host and use --connection, or set OPENAI_API_KEY, ANTHROPIC_API_KEY, or OPENROUTER_API_KEY")
		}
		return f.fallback.ResolveNativeModel(ctx, x, defs)
	}
	runID, _ := x.Run.Input.Metadata["cli_host_run_id"].(string)
	if runID == "" {
		return nil, errors.New("missing host run admission")
	}
	c, e := loadConnection(f.home, name)
	if e != nil {
		return nil, e
	}
	return hostModel{home: f.home, name: name, connection: c, runID: runID, localID: x.Run.ID, epoch: grantEpoch(x.Run)}, nil
}

type hostModel struct {
	home, name, runID string
	localID           string
	epoch             int64
	connection        Connection
}

func (m hostModel) Generate(ctx context.Context, request runtime.NativeModelRequest) (*runtime.NativeModelResponse, error) {
	t, e := accessToken(ctx, m.home, m.name, m.connection)
	if e != nil {
		return nil, e
	}
	b, e := json.Marshal(request)
	if e != nil {
		return nil, e
	}
	if hasLeases(m.connection) {
		sum := sha256.Sum256(append([]byte(m.localID), b...))
		b, e = json.Marshal(map[string]any{"request_id": hex.EncodeToString(sum[:]), "epoch": m.epoch, "local_run_id": m.localID, "request": request})
		if e != nil {
			return nil, e
		}
	}
	var result runtime.NativeModelResponse
	e = readJSONWithClient(ctx, modelHTTPClient, "POST", strings.TrimSuffix(m.connection.Descriptor.APIBaseURL, "/")+"/runs/"+url.PathEscape(m.runID)+"/model", t, bytes.NewReader(b), &result)
	return &result, e
}
func admit(ctx context.Context, home string, o Options) (*Admission, error) {
	c, e := loadConnection(home, o.Connection)
	if e != nil {
		return nil, e
	}
	t, e := accessToken(ctx, home, o.Connection, c)
	if e != nil {
		return nil, e
	}
	if o.Agent == "" {
		return nil, errors.New("--agent is required for a host-connected run")
	}
	b, _ := json.Marshal(map[string]interface{}{"request_id": admissionRequestID(o), "agent_id": o.Agent, "target": o.Target, "instructions": o.Prompt, "execution_location": "local", "review": o.Review})
	var a Admission
	e = readJSON(ctx, "POST", strings.TrimSuffix(c.Descriptor.APIBaseURL, "/")+"/runs", t, bytes.NewReader(b), &a)
	if e != nil {
		return nil, e
	}
	if a.RunID == "" || a.Agent.ID == "" || a.AllowedTools == nil {
		return nil, fmt.Errorf("host returned incomplete run admission")
	}
	if supports(c, "execution_leases") && c.Descriptor.Capabilities != nil {
		if a.Execution == nil || a.Execution.ID == "" || a.Execution.RunID != a.RunID || a.Execution.Epoch < 1 || a.Execution.PolicyHash == "" {
			return nil, errors.New("host returned an incomplete execution grant")
		}
	}
	return &a, nil
}

// Sync uses durable event IDs. A host must deduplicate event_id; retries send
// the same IDs and never create a second host run.
func (s *Session) Sync(ctx context.Context, runID string) error {
	run, e := s.Store.GetRun(ctx, localApp, runID)
	if e != nil {
		return e
	}
	if run == nil {
		return errors.New("run not found")
	}
	name, _ := run.Input.Metadata["cli_connection"].(string)
	if name == "" {
		return errors.New("run has no host connection")
	}
	hostID, _ := run.Input.Metadata["cli_host_run_id"].(string)
	c, e := loadConnection(s.Home, name)
	if e != nil {
		return e
	}
	t, e := accessToken(ctx, s.Home, name, c)
	if e != nil {
		return e
	}
	events, e := s.Store.ListEvents(ctx, localApp, runID)
	if e != nil {
		return e
	}
	messages, e := s.Store.ListMessages(ctx, localApp, runID)
	if e != nil {
		return e
	}
	reported := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		reported = append(reported, map[string]any{"id": msg.ID, "role": msg.Role, "content": msg.Content, "content_blocks": msg.ContentBlocks, "tool_invocations": msg.ToolInvocations})
	}
	body := map[string]interface{}{"local_run_id": run.ID, "status": run.Status, "events": events, "messages": reported, "output_summary": run.OutputSummary}
	if hasLeases(c) {
		body["epoch"] = grantEpoch(run)
	}
	b, e := json.Marshal(body)
	if e != nil {
		return e
	}
	if e = readJSON(ctx, "POST", strings.TrimSuffix(c.Descriptor.APIBaseURL, "/")+"/runs/"+url.PathEscape(hostID)+"/events", t, bytes.NewReader(b), nil); e != nil {
		return e
	}
	if c.Descriptor.Capabilities != nil && supports(c, "artifacts") {
		cmd := exec.CommandContext(ctx, "git", "diff", "--no-ext-diff", "--no-color")
		cmd.Dir = run.Target.ID
		cmd.Env = procenv.Command()
		if patch, err := cmd.Output(); err == nil && len(patch) > 0 && len(patch) <= 256<<10 {
			sum := sha256.Sum256(patch)
			raw, err := json.Marshal(map[string]any{"epoch": grantEpoch(run), "local_run_id": run.ID, "request_id": hex.EncodeToString(sum[:]), "kind": "patch", "content": string(patch)})
			if err != nil {
				return err
			}
			if err = readJSON(ctx, "POST", c.Descriptor.APIBaseURL+"/runs/"+url.PathEscape(hostID)+"/artifacts", t, bytes.NewReader(raw), nil); err != nil {
				return err
			}
		}
	}
	run.Input.Metadata["cli_sync_pending"] = false
	run.Input.Metadata["cli_synced_at"] = time.Now().UTC().Format(time.RFC3339)
	return s.Store.UpdateRun(ctx, run)
}

func admissionRequestID(o Options) string {
	if o.RequestID != "" {
		return o.RequestID
	}
	return id.New("cli_request")
}
