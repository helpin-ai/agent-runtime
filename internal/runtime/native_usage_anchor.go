package runtime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Only append-only growth can reuse a provider's priced prefix. Rewriting any
// priced message, model, system instruction or schema invalidates the anchor.
type nativeUsageAnchor struct {
	PrefixLength int    `json:"prefix_length"`
	Fingerprint  string `json:"fingerprint"`
	Estimated    int    `json:"estimated"`
	Actual       int    `json:"actual"`
}

func nativeUsageFingerprint(x *ExecutionContext, system string, messages []NativeMessage, defs []tools.Definition, prefixLength int) string {
	names := newNativeToolNameMapper(defs)
	visible := nativeBudgetToolResults(messages, names)
	priced, err := nativeMessagesToEino(system, visible[:prefixLength], names)
	if err != nil {
		return "invalid"
	}
	raw, _ := json.Marshal(struct {
		Model         any
		AgentModel    string
		AgentProvider string
		System        string
		Messages      any
		Tools         []tools.Definition
	}{x.Run.Input.Model, x.Agent.Model, x.Agent.Provider, system, priced, defs})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (r *nativeRecorder) adjustedInput(system string, messages []NativeMessage, defs []tools.Definition) int {
	estimated := nativeRequestTokens(system, messages, defs)
	anchor := r.state.InputAnchor
	r.state.InputAdjustment = 0
	if anchor == nil {
		return estimated
	}
	if anchor.PrefixLength < 0 || anchor.PrefixLength > len(messages) || anchor.Fingerprint != nativeUsageFingerprint(r.execCtx, system, messages, defs, anchor.PrefixLength) {
		r.state.InputAnchor = nil
		return estimated
	}
	r.state.InputAdjustment = anchor.Actual - anchor.Estimated
	return max(0, estimated+r.state.InputAdjustment)
}

func (r *nativeRecorder) anchorInput(system string, messages []NativeMessage, defs []tools.Definition, actual int64) {
	if actual <= 0 || actual > int64(^uint(0)>>1) {
		return
	}
	r.state.InputAnchor = &nativeUsageAnchor{PrefixLength: len(messages), Fingerprint: nativeUsageFingerprint(r.execCtx, system, messages, defs, len(messages)), Estimated: nativeRequestTokens(system, messages, defs), Actual: int(actual)}
}
