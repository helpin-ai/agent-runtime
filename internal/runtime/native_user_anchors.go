package runtime

// nativeUserAnchors returns original messages in chronological order. Only
// confirmed human messages compete for the optional older-message budget;
// summaries and host notifications never become human instructions by role.
// Oversized older messages remain in the journal, not partial prompt excerpts.
func nativeUserAnchors(messages []NativeMessage, cut, budget int) []NativeMessage {
	latest := -1
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == "user" && !m.ContextSummary && (m.Provenance == "" || m.Provenance == "human" || m.Provenance == "host_request") {
			latest = i
			break
		}
	}
	selected := make(map[int]bool)
	if latest >= 0 && latest < cut {
		selected[latest] = true
	}
	used := 0
	for i := cut - 1; i >= 0 && budget > 0; i-- {
		m := messages[i]
		if i == latest || m.Role != "user" || m.ContextSummary || m.Provenance != "human" {
			continue
		}
		cost := nativeRequestTokens("", []NativeMessage{m}, nil)
		if cost > budget-used {
			continue
		}
		used += cost
		selected[i] = true
	}
	var anchors []NativeMessage
	for i := 0; i < cut; i++ {
		if selected[i] {
			anchors = append(anchors, messages[i])
		}
	}
	return anchors
}
