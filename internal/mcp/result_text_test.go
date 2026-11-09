package mcp

import (
	"encoding/json"
	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
	"testing"
)

func TestRunResultTextDeduplicatesOnlyEquivalentJSON(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		wantCount  int
	}{{"same", "{ \"id\":42 }", 1}, {"different", "{\"id\":43}", 1}, {"warning", "Write succeeded; do not retry.", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			result := &protocol.CallToolResult{StructuredContent: map[string]any{"id": 42}, Content: []protocol.Content{&protocol.TextContent{Text: tc.text}}}
			got := runResultText(result)
			if strings.Count(got, `"id":42`) != tc.wantCount {
				t.Fatalf("lost or duplicated data: %s", got)
			}
			if tc.name != "same" && !strings.Contains(got, tc.text) {
				t.Fatalf("lost complementary text: %s", got)
			}
		})
	}
}

func TestRunResultTextKeepsDifferentLargeIdentifiers(t *testing.T) {
	result := &protocol.CallToolResult{StructuredContent: map[string]any{"id": json.Number("9007199254740993")}, Content: []protocol.Content{&protocol.TextContent{Text: `{"id":9007199254740992}`}}}
	got := runResultText(result)
	if !strings.Contains(got, "9007199254740993") || !strings.Contains(got, "9007199254740992") {
		t.Fatalf("different identifiers deduplicated: %s", got)
	}
}
