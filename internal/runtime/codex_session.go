package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const codexSessionStateArtifactType = "codex_session_state"

const (
	codexPendingRequestKindHumanInput      = "human_input"
	codexPendingRequestKindCommandApproval = "command_execution"
	codexPendingRequestKindFileApproval    = "file_change"
	codexPendingRequestKindPermissions     = "permissions"
)

type codexSessionState struct {
	ThreadID                  string                   `json:"thread_id,omitempty"`
	ThreadPath                string                   `json:"thread_path,omitempty"`
	HomeRoot                  string                   `json:"home_root,omitempty"`
	CodexHome                 string                   `json:"codex_home,omitempty"`
	Provider                  string                   `json:"provider,omitempty"`
	Model                     string                   `json:"model,omitempty"`
	AuthMode                  string                   `json:"auth_mode,omitempty"`
	Sandbox                   string                   `json:"sandbox,omitempty"`
	InvocationMode            string                   `json:"invocation_mode,omitempty"`
	LastSubmittedMessageSeqNo int                      `json:"last_submitted_message_sequence_no,omitempty"`
	PendingRequest            *codexPendingRequest     `json:"pending_request,omitempty"`
	PendingInteraction        *codexPendingInteraction `json:"pending_interaction,omitempty"`
	ClearedAt                 *time.Time               `json:"cleared_at,omitempty"`
}

type codexPendingInteraction struct {
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind,omitempty"`
}

type codexPendingRequest struct {
	Kind         string          `json:"kind,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	RequestIDRaw json.RawMessage `json:"request_id_raw,omitempty"`
	TurnID       string          `json:"turn_id,omitempty"`
	ItemID       string          `json:"item_id,omitempty"`
	QuestionIDs  []string        `json:"question_ids,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

type codexSessionStore struct {
	store agentcore.Store
}

func newCodexSessionStore(store agentcore.Store) *codexSessionStore {
	return &codexSessionStore{store: store}
}

func (s *codexSessionStore) Load(ctx context.Context, appID, runID string) (*codexSessionState, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	artifacts, err := s.store.ListArtifacts(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	for i := len(artifacts) - 1; i >= 0; i-- {
		artifact := artifacts[i]
		if strings.TrimSpace(artifact.ArtifactType) != codexSessionStateArtifactType || strings.TrimSpace(artifact.InlineContent) == "" {
			continue
		}
		var state codexSessionState
		if err := json.Unmarshal([]byte(artifact.InlineContent), &state); err != nil {
			return nil, fmt.Errorf("parse codex session state: %w", err)
		}
		if state.ClearedAt != nil {
			return nil, nil
		}
		return &state, nil
	}
	return nil, nil
}

func (s *codexSessionStore) Save(ctx context.Context, appID, runID string, state *codexSessionState) error {
	if s == nil || s.store == nil || state == nil {
		return nil
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal codex session state: %w", err)
	}
	return s.store.AppendArtifact(ctx, &agentcore.AgentRunArtifact{
		AppID:         appID,
		RunID:         runID,
		ArtifactType:  codexSessionStateArtifactType,
		Format:        "json",
		StorageMode:   "inline",
		InlineContent: string(payload),
		Metadata:      json.RawMessage(`{"internal":true}`),
	})
}

func (s *codexSessionStore) Clear(ctx context.Context, appID, runID string) error {
	now := time.Now().UTC()
	return s.Save(ctx, appID, runID, &codexSessionState{ClearedAt: &now})
}

func codexRequestIDString(id json.RawMessage) string {
	trimmed := strings.TrimSpace(string(id))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(id, &decoded); err == nil {
		switch typed := decoded.(type) {
		case string:
			return strings.TrimSpace(typed)
		case float64:
			if typed == float64(int64(typed)) {
				return fmt.Sprintf("%d", int64(typed))
			}
			return strings.TrimSpace(fmt.Sprintf("%v", typed))
		}
	}
	return strings.Trim(trimmed, `"`)
}

func codexPendingRequestResponseID(pending *codexPendingRequest) json.RawMessage {
	if pending == nil {
		return nil
	}
	if raw := strings.TrimSpace(string(pending.RequestIDRaw)); raw != "" && raw != "null" {
		return append(json.RawMessage(nil), pending.RequestIDRaw...)
	}
	if strings.TrimSpace(pending.RequestID) == "" {
		return nil
	}
	encoded, err := json.Marshal(strings.TrimSpace(pending.RequestID))
	if err != nil {
		return nil
	}
	return json.RawMessage(encoded)
}
