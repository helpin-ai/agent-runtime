package engine

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// hiddenRunCacheLimit bounds the per-run visibility cache. Clearing it only
// costs a membership lookup per run.
const hiddenRunCacheLimit = 4096

// HiddenRuns reports whether a run was created by the runtime itself (a
// work-job worker or a model-spawned child). Such runs keep their persisted
// events and explicit SSE, but never reach the app's event sink or event
// callbacks: hosts that resolve every event against their own records, such
// as Helpin's NATS consumer, would retry events for runs they never started.
type HiddenRuns struct {
	Store agentcore.Store
	mu    sync.Mutex
	cache map[string]bool
}

// Hidden is safe for concurrent use. A run's visibility is fixed when its
// membership is created in the run-creation transaction, before any event,
// so a cached answer never goes stale. A lookup error is reported as hidden:
// withholding a host notification is recoverable, delivering one for a run
// the host never started is not.
func (h *HiddenRuns) Hidden(ctx context.Context, appID, runID string) bool {
	if h == nil || h.Store == nil || runID == "" {
		return false
	}
	groups, ok := h.Store.(agentcore.RunGroupStore)
	if !ok || !groups.RunGroupsSupported() {
		return false
	}
	key := strings.TrimSpace(appID) + "\x00" + runID
	h.mu.Lock()
	hidden, cached := h.cache[key]
	h.mu.Unlock()
	if cached {
		return hidden
	}
	member, err := groups.GetRunGroupMember(ctx, appID, runID)
	if err != nil {
		slog.WarnContext(ctx, "run visibility lookup failed; withholding host event delivery", "app_id", appID, "run_id", runID, "error", err)
		return true
	}
	hidden = member != nil && member.Hidden
	h.mu.Lock()
	if h.cache == nil || len(h.cache) >= hiddenRunCacheLimit {
		h.cache = map[string]bool{}
	}
	h.cache[key] = hidden
	h.mu.Unlock()
	return hidden
}

// HostDeliverySink forwards events to a host-facing sink (the global NATS or
// callback sink, or app event callbacks) except events of hidden runs.
type HostDeliverySink struct {
	Sink   EventSink
	Hidden *HiddenRuns
}

func (s HostDeliverySink) Emit(ctx context.Context, event Event) {
	if s.Sink == nil || s.Hidden.Hidden(ctx, event.AppID, event.RunID) {
		return
	}
	s.Sink.Emit(ctx, event)
}

var _ EventSink = HostDeliverySink{}
