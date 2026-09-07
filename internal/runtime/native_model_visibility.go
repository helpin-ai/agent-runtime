package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	modelVisibleToolOutputMaxRunes     = 4000
	modelVisibleToolOutputHeadRunes    = 2700
	modelVisibleToolOutputTailRunes    = 800
	modelVisibleToolOutputMaxSmallTool = 8000

	modelVisibleFileReadOutputMaxRunes  = 2800
	modelVisibleFileReadOutputHeadRunes = 1800
	modelVisibleFileReadOutputTailRunes = 600

	modelVisibleReadRangeOutputMaxRunes  = 2600
	modelVisibleReadRangeOutputHeadRunes = 1700
	modelVisibleReadRangeOutputTailRunes = 500

	modelVisibleRipgrepOutputMaxRunes  = 3000
	modelVisibleRipgrepOutputHeadRunes = 2100
	modelVisibleRipgrepOutputTailRunes = 500

	boundedToolOutputCompactionMaxRunes = 30000

	toolResultNoOutputPlaceholder      = "[tool returned no output]"
	toolResultNoOutputErrorPlaceholder = "[tool returned no output; tool reported an error]"
)

type nativeModelVisibleToolOutput struct {
	Content       string
	OriginalRunes int
	VisibleRunes  int
	OriginalLines int
	VisibleLines  int
	Compacted     bool
}

type nativeCompactionHint struct {
	Exempt   bool   `json:"exempt"`
	MaxRunes int    `json:"max_runes,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

func prepareNativeToolResultForModel(toolName, output string, isError bool) nativeModelVisibleToolOutput {
	analysis := analyzeNativeToolOutputForModel(toolName, output)
	if strings.TrimSpace(analysis.Content) != "" {
		return analysis
	}
	placeholder := toolResultNoOutputPlaceholder
	if isError {
		placeholder = toolResultNoOutputErrorPlaceholder
	}
	analysis.Content = placeholder
	analysis.VisibleRunes = len([]rune(placeholder))
	analysis.VisibleLines = countNativeToolOutputLines(placeholder)
	return analysis
}

func analyzeNativeToolOutputForModel(toolName, output string) nativeModelVisibleToolOutput {
	if strings.TrimSpace(output) == "" {
		return nativeModelVisibleToolOutput{Content: output}
	}

	runes := []rune(output)
	maxRunes, headRunes, tailRunes := nativeToolOutputCompactionLimits(toolName)
	analysis := nativeModelVisibleToolOutput{
		Content:       output,
		OriginalRunes: len(runes),
		VisibleRunes:  len(runes),
		OriginalLines: countNativeToolOutputLines(output),
		VisibleLines:  countNativeToolOutputLines(output),
	}
	// read_files already enforces a shared content budget and carries exact
	// continuation cursors. Blindly clipping its envelope corrupts JSON and can
	// discard those cursors. Preserve the bounded structured result intact.
	// Allow JSON escaping (up to six bytes per source rune) and four envelopes.
	if strings.TrimSpace(toolName) == "read_files" && len(runes) <= 128*1024 && json.Valid([]byte(output)) {
		return analysis
	}
	if nativeOutputHasCompactionExemption(output, len(runes)) || len(runes) <= maxRunes || headRunes+tailRunes >= len(runes) {
		return analysis
	}

	omittedRunes := len(runes) - headRunes - tailRunes
	head := string(runes[:headRunes])
	tail := string(runes[len(runes)-tailRunes:])
	label := strings.TrimSpace(toolName)
	if label == "" {
		label = "tool"
	}

	compacted := head + fmt.Sprintf(
		"\n\n[agent-runtime truncated %d characters from previous %s output to reduce model token usage. For read tools, request a narrower range. Do not repeat a mutating action just to retrieve its output.]\n\n",
		omittedRunes,
		label,
	) + tail
	return nativeModelVisibleToolOutput{
		Content:       compacted,
		OriginalRunes: len(runes),
		VisibleRunes:  len([]rune(compacted)),
		OriginalLines: countNativeToolOutputLines(output),
		VisibleLines:  countNativeToolOutputLines(compacted),
		Compacted:     true,
	}
}

func nativeOutputHasCompactionExemption(output string, runeCount int) bool {
	const (
		agentRuntimeCompactionKey = "_agent_runtime_compaction"
		legacyCompactionKey       = "_" + "hel" + "pin" + "_compaction"
	)
	if !strings.Contains(output, legacyCompactionKey) && !strings.Contains(output, agentRuntimeCompactionKey) {
		return false
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		return false
	}
	rawHint := envelope[agentRuntimeCompactionKey]
	if len(rawHint) == 0 {
		rawHint = envelope[legacyCompactionKey]
	}
	if len(rawHint) == 0 {
		return false
	}
	var hint nativeCompactionHint
	if err := json.Unmarshal(rawHint, &hint); err != nil {
		return false
	}
	if !hint.Exempt {
		return false
	}
	maxRunes := hint.MaxRunes
	if maxRunes <= 0 || maxRunes > boundedToolOutputCompactionMaxRunes {
		maxRunes = boundedToolOutputCompactionMaxRunes
	}
	return runeCount <= maxRunes
}

// nativeToolOutputCompactionLimits and isHighVolumeNativeToolOutput below are
// matched against the tool name recorded on a call, which for replayed history
// may predate a consolidation. The retired names (read_file, read_file_range,
// search_files, ripgrep, grep, find_symbol, find_callers, find_callees) are
// listed alongside their replacements on purpose: dropping one would fall
// through to the small-tool budget and truncate an old high-volume result.
func nativeToolOutputCompactionLimits(toolName string) (maxRunes, headRunes, tailRunes int) {
	switch strings.TrimSpace(toolName) {
	case "read_file", "read_files":
		return modelVisibleFileReadOutputMaxRunes, modelVisibleFileReadOutputHeadRunes, modelVisibleFileReadOutputTailRunes
	case "read_file_range":
		return modelVisibleReadRangeOutputMaxRunes, modelVisibleReadRangeOutputHeadRunes, modelVisibleReadRangeOutputTailRunes
	case "ripgrep", "repository_search":
		return modelVisibleRipgrepOutputMaxRunes, modelVisibleRipgrepOutputHeadRunes, modelVisibleRipgrepOutputTailRunes
	default:
		if isHighVolumeNativeToolOutput(toolName) {
			return modelVisibleToolOutputMaxRunes, modelVisibleToolOutputHeadRunes, modelVisibleToolOutputTailRunes
		}
		return modelVisibleToolOutputMaxSmallTool, modelVisibleToolOutputMaxSmallTool - 800, 400
	}
}

func isHighVolumeNativeToolOutput(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "read_file",
		"read_files",
		"read_file_range",
		"read_symbol",
		"trace_symbol",
		"repository_search",
		"find_symbol",
		"find_callers",
		"find_callees",
		"list_directory",
		"search_files",
		"ripgrep",
		"grep",
		"list_symbols",
		"run_command",
		"read_document",
		"get_document_blocks",
		"get_release_context",
		"get_task_context",
		"search_documents",
		"find_tasks_for_git_changes",
		"web_search_brave",
		"web_search_exa",
		"web_search",
		"fetch_url",
		"crawl_url":
		return true
	default:
		return false
	}
}

func countNativeToolOutputLines(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}

func sanitizeNativeMessagesForReplay(history []NativeMessage) []NativeMessage {
	if len(history) == 0 {
		return nil
	}
	sanitized := make([]NativeMessage, 0, len(history))
	for i := 0; i < len(history); i++ {
		msg := history[i]
		switch strings.TrimSpace(msg.Role) {
		case "assistant":
			if !nativeMessageHasToolCalls(msg) {
				sanitized = append(sanitized, msg)
				continue
			}

			groupEnd := i + 1
			for groupEnd < len(history) && strings.TrimSpace(history[groupEnd].Role) == "tool" {
				groupEnd++
			}
			if groupEnd == i+1 {
				sanitized = append(sanitized, msg)
				continue
			}

			matchedCalls := matchedNativeAssistantToolCallIDs(msg, history[i+1:groupEnd])
			if assistant, keep := sanitizeNativeAssistantReplayMessage(msg, matchedCalls); keep {
				sanitized = append(sanitized, assistant)
			}
			for _, toolMsg := range history[i+1 : groupEnd] {
				if tool, keep := sanitizeNativeToolReplayMessage(toolMsg, matchedCalls); keep {
					sanitized = append(sanitized, tool)
				}
			}
			i = groupEnd - 1
		case "tool":
			continue
		default:
			sanitized = append(sanitized, msg)
		}
	}
	return sanitized
}

func nativeMessageHasToolCalls(msg NativeMessage) bool {
	for _, block := range msg.Blocks {
		if strings.TrimSpace(block.Type) == nativeBlockTypeToolCall && strings.TrimSpace(block.ToolCallID) != "" {
			return true
		}
	}
	return false
}

func matchedNativeAssistantToolCallIDs(assistant NativeMessage, toolMessages []NativeMessage) map[string]bool {
	allowed := make(map[string]bool)
	for _, block := range assistant.Blocks {
		if strings.TrimSpace(block.Type) != nativeBlockTypeToolCall || strings.TrimSpace(block.ToolCallID) == "" {
			continue
		}
		allowed[strings.TrimSpace(block.ToolCallID)] = false
	}
	for _, toolMsg := range toolMessages {
		for _, block := range toolMsg.Blocks {
			if strings.TrimSpace(block.Type) != nativeBlockTypeToolResult {
				continue
			}
			toolCallID := strings.TrimSpace(block.ToolCallID)
			if _, ok := allowed[toolCallID]; ok {
				allowed[toolCallID] = true
			}
		}
	}
	matched := make(map[string]bool)
	for toolCallID, ok := range allowed {
		if ok {
			matched[toolCallID] = true
		}
	}
	return matched
}

func sanitizeNativeAssistantReplayMessage(msg NativeMessage, matchedCalls map[string]bool) (NativeMessage, bool) {
	sanitized := msg
	sanitized.Blocks = filterNativeBlocksForReplay(msg.Blocks, matchedCalls)
	if strings.TrimSpace(sanitized.Content) == "" && len(sanitized.Blocks) == 0 {
		return NativeMessage{}, false
	}
	return sanitized, true
}

func sanitizeNativeToolReplayMessage(msg NativeMessage, matchedCalls map[string]bool) (NativeMessage, bool) {
	if len(matchedCalls) == 0 {
		return NativeMessage{}, false
	}
	sanitized := msg
	sanitized.Blocks = filterNativeBlocksForReplay(msg.Blocks, matchedCalls)
	if len(sanitized.Blocks) == 0 {
		return NativeMessage{}, false
	}
	sanitized.Content = nativePersistedContentFromBlocks(sanitized.Blocks)
	return sanitized, true
}

func filterNativeBlocksForReplay(blocks []NativeBlock, matchedCalls map[string]bool) []NativeBlock {
	if len(blocks) == 0 {
		return nil
	}
	filtered := make([]NativeBlock, 0, len(blocks))
	for _, block := range blocks {
		switch strings.TrimSpace(block.Type) {
		case nativeBlockTypeText:
			filtered = append(filtered, block)
		case nativeBlockTypeToolCall, nativeBlockTypeToolResult:
			if matchedCalls[strings.TrimSpace(block.ToolCallID)] {
				filtered = append(filtered, block)
			}
		}
	}
	return filtered
}

func nativePersistedContentFromBlocks(blocks []NativeBlock) string {
	if text := strings.TrimSpace(nativeTextFromBlocks(blocks)); text != "" {
		return text
	}
	for _, block := range blocks {
		if strings.TrimSpace(block.Type) == nativeBlockTypeToolResult && strings.TrimSpace(block.Output) != "" {
			return strings.TrimSpace(block.Output)
		}
	}
	return ""
}

func nativeTextFromBlocks(blocks []NativeBlock) string {
	var parts []string
	for _, block := range blocks {
		if strings.TrimSpace(block.Type) == nativeBlockTypeText && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}
