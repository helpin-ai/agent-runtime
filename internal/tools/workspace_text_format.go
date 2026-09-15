package tools

import "strings"

// Preserve the file's BOM separately from the text shown by the read tools.
func splitWorkspaceBOM(content string) (bom, text string) {
	if strings.HasPrefix(content, "\ufeff") {
		return "\ufeff", strings.TrimPrefix(content, "\ufeff")
	}
	return "", content
}

// Mixed line endings must be matched exactly, without normalizing unrelated bytes.
func workspaceUniformCRLF(content string) bool {
	return strings.Contains(content, "\r\n") && !strings.Contains(strings.ReplaceAll(content, "\r\n", ""), "\n")
}
