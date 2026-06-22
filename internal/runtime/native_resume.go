package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
)

type nativeOutputSummary struct {
	Messages []NativeMessage `json:"native_messages,omitempty"`
}

type nativeResumePayload struct {
	Intent          string          `json:"intent"`
	Content         string          `json:"content,omitempty"`
	ResponsePayload json.RawMessage `json:"response_payload,omitempty"`
	ExternalActorID string          `json:"external_actor_id,omitempty"`
}

func nativeInitialMessages(execCtx *ExecutionContext) []NativeMessage {
	if resumed := nativeResumedMessages(execCtx); len(resumed) > 0 {
		return resumed
	}
	return []NativeMessage{{
		Role:    "user",
		Content: nativeInitialUserPrompt(execCtx),
	}}
}

func nativeResumedMessages(execCtx *ExecutionContext) []NativeMessage {
	if execCtx == nil || execCtx.Run == nil || len(execCtx.Run.OutputSummary) == 0 {
		return nil
	}
	resume, ok := nativeLastResumePayload(execCtx)
	if !ok {
		return nil
	}
	var summary nativeOutputSummary
	if err := json.Unmarshal(execCtx.Run.OutputSummary, &summary); err != nil || len(summary.Messages) == 0 {
		return nil
	}
	messages := append([]NativeMessage(nil), summary.Messages...)
	messages = append(messages, nativeResumeMessage(resume))
	return messages
}

func nativeLastResumePayload(execCtx *ExecutionContext) (nativeResumePayload, bool) {
	if execCtx == nil || execCtx.Run == nil || execCtx.Run.Input.Metadata == nil {
		return nativeResumePayload{}, false
	}
	raw, ok := execCtx.Run.Input.Metadata["last_resume"]
	if !ok {
		return nativeResumePayload{}, false
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nativeResumePayload{}, false
	}
	var payload nativeResumePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nativeResumePayload{}, false
	}
	if strings.TrimSpace(payload.Intent) == "" && strings.TrimSpace(payload.Content) == "" && len(payload.ResponsePayload) == 0 {
		return nativeResumePayload{}, false
	}
	payload.Intent = strings.TrimSpace(payload.Intent)
	payload.Content = strings.TrimSpace(payload.Content)
	payload.ExternalActorID = strings.TrimSpace(payload.ExternalActorID)
	return payload, true
}

func nativeResumeMessage(payload nativeResumePayload) NativeMessage {
	content := nativeResumeContent(payload)
	return NativeMessage{
		Role:    "user",
		Content: content,
		Blocks:  []NativeBlock{{Type: nativeBlockTypeText, Text: content}},
	}
}

func nativeResumeContent(payload nativeResumePayload) string {
	intent := strings.TrimSpace(payload.Intent)
	if intent == "" {
		intent = "reply"
	}
	parts := []string{fmt.Sprintf("Human resumed the paused run with intent %q.", intent)}
	if strings.TrimSpace(payload.Content) != "" {
		parts = append(parts, "Human message:\n"+strings.TrimSpace(payload.Content))
	}
	if len(payload.ResponsePayload) > 0 && strings.TrimSpace(string(payload.ResponsePayload)) != "null" {
		parts = append(parts, "Structured response payload:\n"+strings.TrimSpace(string(payload.ResponsePayload)))
	}
	if strings.TrimSpace(payload.ExternalActorID) != "" {
		parts = append(parts, "External actor ID: "+strings.TrimSpace(payload.ExternalActorID))
	}
	return strings.Join(parts, "\n\n")
}
