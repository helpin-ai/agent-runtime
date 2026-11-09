package mcp

import (
	"encoding/json"
	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"reflect"
	"strings"
)

// Prefer one representation only when the JSON values are actually equivalent.
// Explanations, warnings and other non-equivalent content remain visible.
func runResultText(result *protocol.CallToolResult) string {
	parts := make([]string, 0, len(result.Content)+1)
	var structured any
	structuredJSON, err := json.Marshal(result.StructuredContent)
	hasStructured := err == nil && result.StructuredContent != nil
	if hasStructured {
		structured, _ = decodeResultJSON(string(structuredJSON))
	}
	equivalent := false
	for _, content := range result.Content {
		if text, ok := content.(*protocol.TextContent); ok {
			decoded, valid := decodeResultJSON(text.Text)
			same := hasStructured && valid && reflect.DeepEqual(decoded, structured)
			if same {
				if !equivalent {
					parts = append(parts, string(structuredJSON))
				}
				equivalent = true
			} else {
				parts = append(parts, text.Text)
			}
		} else if raw, err := json.Marshal(content); err == nil {
			parts = append(parts, string(raw))
		}
	}
	if hasStructured && !equivalent {
		parts = append(parts, string(structuredJSON))
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func decodeResultJSON(text string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	var extra any
	return value, decoder.Decode(&extra) == io.EOF
}
