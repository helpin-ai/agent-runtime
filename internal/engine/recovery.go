package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	openai "github.com/openai/openai-go/v3"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// Stored in run metadata so existing SDKs and databases preserve recovery across restarts.
const recoveryKey = "runtime_recovery"

type recovery struct {
	Reason      string    `json:"reason"`
	Attempts    int       `json:"attempts"`
	NextRetryAt time.Time `json:"next_retry_at"`
}

func recoveryState(run *agentcore.AgentRun) *recovery {
	if run == nil {
		return nil
	}
	value := run.Input.Metadata[recoveryKey]
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	var state recovery
	if err != nil || json.Unmarshal(data, &state) != nil {
		return nil
	}
	return &state
}

func transientFailure(err error) string {
	if err == nil || errors.Is(err, context.Canceled) {
		return ""
	}
	message := strings.ToLower(err.Error())
	var oe *openai.Error
	var ae *anthropic.Error
	status := 0
	if errors.As(err, &oe) {
		status = oe.StatusCode
		message += " " + strings.ToLower(oe.Code+" "+oe.Message)
	}
	if errors.As(err, &ae) {
		status = ae.StatusCode
	}
	for _, permanent := range []string{"insufficient_quota", "usage_limit", "quota exceeded", "insufficient credits", "exhausted", "billing", "invalid api key", "unauthorized", "authentication", "context length", "max_output_tokens", "checkpoint", "outcome unknown", "ambiguous"} {
		if strings.Contains(message, permanent) {
			return ""
		}
	}
	switch status {
	case 408, 429, 500, 502, 503, 504, 529:
		return "provider_busy"
	}
	if status >= 400 {
		return ""
	}
	for _, busy := range []string{"429", "rate_limit", "rate limit", "too many requests", "overloaded", "server_error", "service_unavailable", "status code: 500", "status code: 502", "status code: 503", "status code: 504", "status code: 408"} {
		if strings.Contains(message, busy) {
			return "provider_busy"
		}
	}
	var network net.Error
	if errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return "connection_lost"
	}
	// Some provider SDKs flatten transport and stream errors instead of wrapping them.
	for _, marker := range []string{"connection reset", "connection refused", "network is unreachable", "no such host", "dial tcp", "i/o timeout", "unexpected eof", "unexpected end of json", "failed to read stream", "stream ended before completion", "stream chunk"} {
		if strings.Contains(message, marker) {
			return "connection_lost"
		}
	}
	return ""
}

func recoveryDelay(attempt int) time.Duration {
	return min(5*time.Second<<min(max(attempt-1, 0), 6), 30*time.Second)
}

// The conditional metadata write cannot overwrite a concurrent cancellation or pause.
type recoveryStore interface {
	SetRunRecovery(context.Context, string, string, map[string]interface{}) (bool, error)
}

func (e *Engine) deferRecovery(ctx context.Context, run *agentcore.AgentRun, cause error) bool {
	if !e.cfg.AutomaticRecovery || e.cfg.ManualLightweightExecution || run.ExecutionMode != ExecutionModeLightweight || run.RuntimeKind != agentcore.RuntimeNativeSDK || ctx.Err() != nil {
		return false
	}
	reason := transientFailure(cause)
	persistence, ok := e.cfg.Store.(recoveryStore)
	if reason == "" || !ok {
		return false
	}
	previous := recoveryState(run)
	var state *recovery
	if previous != nil {
		copy := *previous
		state = &copy
	}
	if state == nil {
		state = &recovery{}
	}
	state.Reason, state.Attempts = reason, state.Attempts+1
	state.NextRetryAt = time.Now().UTC().Add(max(recoveryDelay(state.Attempts), providerRetryAfter(cause)))
	if run.Input.Metadata == nil {
		run.Input.Metadata = map[string]interface{}{}
	}
	run.Input.Metadata[recoveryKey] = state
	saved, err := persistence.SetRunRecovery(ctx, run.AppID, run.ID, run.Input.Metadata)
	for err != nil {
		// Retain execution ownership until the durable wait is recorded.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return true
		case <-timer.C:
		}
		saved, err = persistence.SetRunRecovery(ctx, run.AppID, run.ID, run.Input.Metadata)
	}
	if saved {
		e.emitRunEvent(ctx, run, "run.recovery_wait", map[string]interface{}{"reason": reason, "next_retry_at": state.NextRetryAt, "attempts": state.Attempts})
	}
	return true
}

// One lightweight execution slot owns the wait as well as execution. Restart recovery
// reclaims that same run; job workers remain owned by their existing controller.
func (e *Engine) waitForRecovery(ctx context.Context, appID, runID string) bool {
	for {
		run, err := e.cfg.Store.GetRun(ctx, appID, runID)
		if err != nil || run == nil || agentcore.IsTerminalStatus(run.Status) {
			return false
		}
		state := recoveryState(run)
		if state == nil {
			return true
		}
		if run.Status != agentcore.RunStatusRunning {
			return false
		}
		if !e.cfg.AutomaticRecovery {
			return false
		}
		if groups, ok := e.runGroupStore(); ok {
			member, err := groups.GetRunGroupMember(ctx, appID, runID)
			if err != nil {
				return false
			}
			if member != nil {
				group, err := groups.GetRunGroup(ctx, appID, member.GroupID)
				if err != nil || group == nil || group.State != agentcore.RunGroupStateActive {
					return false
				}
			}
		}
		remaining := time.Until(state.NextRetryAt)
		if remaining <= 0 {
			e.emitRunEvent(ctx, run, "run.recovery_started", nil)
			return true
		}
		timer := time.NewTimer(min(remaining, time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func providerRetryAfter(err error) time.Duration {
	var response *http.Response
	var openaiError *openai.Error
	var anthropicError *anthropic.Error
	if errors.As(err, &openaiError) {
		response = openaiError.Response
	}
	if errors.As(err, &anthropicError) {
		response = anthropicError.Response
	}
	if response == nil {
		return 0
	}
	raw := response.Header.Get("Retry-After")
	if seconds, err := strconv.ParseInt(raw, 10, 32); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(raw); err == nil {
		return max(0, time.Until(deadline))
	}
	return 0
}
