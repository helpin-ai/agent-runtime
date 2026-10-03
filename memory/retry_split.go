package memory

// Split policy ported from pinned _split_chunk_for_output_retry, with bounded
// recursion in Provider. See upstream/LICENSE for attribution.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf16"
)

func asciiJSON(raw json.RawMessage) string {
	var out strings.Builder
	for _, r := range pythonJSON(raw) {
		if r <= 127 {
			out.WriteRune(r)
		} else if r <= 0xffff {
			fmt.Fprintf(&out, `\u%04x`, r)
		} else {
			first, second := utf16.EncodeRune(r)
			fmt.Fprintf(&out, `\u%04x\u%04x`, first, second)
		}
	}
	return out.String()
}
func serializeRetryTurns(turns []json.RawMessage) string {
	parts := make([]string, len(turns))
	for i, turn := range turns {
		parts[i] = asciiJSON(turn)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
func replaceTurnContent(turn json.RawMessage, content string) json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(turn))
	decoder.Token()
	parts := []string{}
	for decoder.More() {
		key, _ := decoder.Token()
		var value json.RawMessage
		decoder.Decode(&value)
		if key == "content" {
			value, _ = json.Marshal(content)
		}
		name, _ := json.Marshal(key)
		parts = append(parts, string(name)+":"+string(value))
	}
	return json.RawMessage("{" + strings.Join(parts, ",") + "}")
}
func splitOutputRetry(chunk string) (string, string, bool) {
	text := strings.TrimSpace(chunk)
	runes := []rune(text)
	if len(runes) <= 500 {
		return "", "", false
	}
	var turns []json.RawMessage
	if json.Unmarshal([]byte(text), &turns) == nil && turns != nil {
		if len(turns) >= 2 {
			mid := len(turns) / 2
			return serializeRetryTurns(turns[:mid]), serializeRetryTurns(turns[mid:]), true
		}
		if len(turns) == 1 {
			var turn map[string]json.RawMessage
			if json.Unmarshal(turns[0], &turn) == nil && turn != nil {
				var content string
				if json.Unmarshal(turn["content"], &content) == nil {
					chars := []rune(content)
					if len(chars) > 1 {
						mid := len(chars) / 2
						return serializeRetryTurns([]json.RawMessage{replaceTurnContent(turns[0], string(chars[:mid]))}), serializeRetryTurns([]json.RawMessage{replaceTurnContent(turns[0], string(chars[mid:]))}), true
					}
				}
			}
		}
		return "", "", false
	}
	mid, span := len(runes)/2, int(float64(len(runes))*0.2)
	start, end := max(0, mid-span), min(len(runes), mid+span)
	cut := mid
	for _, ending := range []string{". ", "! ", "? ", "\n\n"} {
		window := string(runes[start:end])
		index := strings.LastIndex(window, ending)
		if index >= 0 {
			cut = start + len([]rune(window[:index+len(ending)]))
			break
		}
	}
	left, right := strings.TrimSpace(string(runes[:cut])), strings.TrimSpace(string(runes[cut:]))
	if left == "" || right == "" || left == text || right == text {
		return "", "", false
	}
	return left, right, true
}
