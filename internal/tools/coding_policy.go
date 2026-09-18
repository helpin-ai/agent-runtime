package tools

// RequiresCoding reports whether effective permissions allow local code
// execution or repository mutation. Host business mutations stay on support
// queues. CanonicalName also handles the MCP aliases of built-in tools.
func RequiresCoding(allowed map[string]bool) bool {
	for name, enabled := range allowed {
		if !enabled {
			continue
		}
		switch CanonicalName(name) {
		case "run_python", "run_command", "write_file", "edit_file", "apply_patch", "create_branch", "commit_and_push", "start_preview":
			return true
		}
	}
	return false
}
