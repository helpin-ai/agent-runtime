package runtime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const (
	nativeNoProgressRecoveryLimit = 3
	nativeNoProgressStopLimit     = 5
)

// nativeNoProgressState is checkpointed so a worker restart cannot reset the
// count for a run that keeps repeating the same unproductive call.
type nativeNoProgressState struct {
	History      []string `json:"history,omitempty"`
	Fingerprint  string   `json:"fingerprint"`
	Count        int      `json:"count"`
	RecoverySent bool     `json:"recovery_sent,omitempty"`
}

func (r *nativeRecorder) observeNoProgress(executed nativeExecutedToolCall) (int, bool) {
	fingerprint := nativeNoProgressFingerprint(executed)
	if fingerprint == "" {
		r.state.NoProgress = nil
		return 0, false
	}
	if r.state.NoProgress == nil {
		r.state.NoProgress = &nativeNoProgressState{}
	}
	state := r.state.NoProgress
	// Old checkpoints have only a consecutive count.
	if len(state.History) == 0 && state.Fingerprint != "" {
		for i := 0; i < min(state.Count, 20); i++ {
			state.History = append(state.History, state.Fingerprint)
		}
	}
	state.History = append(state.History, fingerprint)
	if len(state.History) > 20 {
		state.History = state.History[len(state.History)-20:]
	}
	count := 1
	for period := 1; period <= 4 && period*2 <= len(state.History); period++ {
		repeats := 1
		for end := len(state.History) - period; end >= period; end -= period {
			equal := true
			for j := 0; j < period; j++ {
				if state.History[end-period+j] != state.History[len(state.History)-period+j] {
					equal = false
					break
				}
			}
			if !equal {
				break
			}
			repeats++
		}
		count = max(count, repeats)
	}
	if count < state.Count {
		state.RecoverySent = false
	}
	state.Count = count
	state.Fingerprint = fingerprint

	return r.state.NoProgress.Count, r.state.NoProgress.Count >= nativeNoProgressStopLimit
}

func nativeNoProgressCorrection(toolName string, count int) NativeMessage {
	return NativeMessage{
		Role:       "user",
		Provenance: "runtime_correction",
		Content:    fmt.Sprintf("Runtime recovery notice: the %s tool has been called %d times in a repeating sequence with unchanged arguments and results. This action is not making progress. Do not repeat that call. Inspect its latest result and choose a different useful step, such as a different file range, a real edit, or completing the task with the evidence already available. If progress genuinely requires external input, explain the specific blocker using the normal completion path.", toolName, count),
	}
}

func nativeNoProgressFingerprint(executed nativeExecutedToolCall) string {
	if executed.ApprovalRequired {
		return ""
	}
	name := tools.CanonicalName(executed.ToolName)
	kind := ""
	switch name {
	case nativeToolReadResult, "read_files", "list_directory", "repository_search", "list_symbols", "read_symbol", "trace_symbol":
		if executed.IsError {
			kind = "error"
		} else {
			kind = "unchanged_read"
		}
	case "edit_file":
		if executed.IsError {
			kind = "error"
		} else if strings.HasPrefix(executed.Output, "No changes made to ") {
			kind = "unchanged_edit"
		}
	}
	if kind == "" && !executed.Mutating && !strings.Contains(name, "poll") && !strings.Contains(name, "wait") && !strings.Contains(name, "status") {
		if executed.IsError {
			kind = "error"
		} else {
			for _, fragment := range []string{"describe_", "search", "fetch", "crawl", "get_crm_", "list_crm_", "list_contacts"} {
				if strings.Contains(name, fragment) {
					kind = "unchanged_read"
					break
				}
			}
		}
	}
	if kind == "" {
		return ""
	}
	input := executed.Input
	var decoded any
	if json.Unmarshal(input, &decoded) == nil {
		if canonical, err := json.Marshal(decoded); err == nil {
			input = canonical
		}
	}
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s", name, kind, input)
	// A changed result starts a fresh count even when arguments are identical.
	_, _ = fmt.Fprintf(hash, "\x00%s", executed.Output)
	return fmt.Sprintf("%x", hash.Sum(nil))
}
