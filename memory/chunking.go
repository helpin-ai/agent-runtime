package memory

// Text-only chunking ported from the pinned Hindsight fact_extraction.py.
// See upstream/LICENSE and upstream/README.md for attribution.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

var textSeparators = []string{"\n\n", "\n", ". ", "! ", "? ", "; ", ", ", " ", ""}

func splitChunks(content string, size int) []string {
	if utf8.RuneCountInString(content) <= size {
		return []string{content}
	}
	var turns []json.RawMessage
	if json.Unmarshal([]byte(content), &turns) == nil && turns != nil {
		objects := true
		for _, turn := range turns {
			if len(bytes.TrimSpace(turn)) == 0 || bytes.TrimSpace(turn)[0] != '{' {
				objects = false
				break
			}
		}
		if objects {
			return conversationChunks(turns, size)
		}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &object) == nil && object != nil {
		return recursiveChunks(content, size, textSeparators)
	}
	lines := []string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var item map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &item) != nil || item == nil {
			return recursiveChunks(content, size, textSeparators)
		}
		lines = append(lines, line)
	}
	if len(lines) < 2 {
		return recursiveChunks(content, size, textSeparators)
	}
	return packStructured(lines, size, 0, "\n", "", "")
}

func conversationChunks(turns []json.RawMessage, size int) []string {
	units := make([]string, len(turns))
	for i, turn := range turns {
		units[i] = pythonJSON(turn)
	}
	return packStructured(units, size, 2, ", ", "[", "]")
}

// Upstream estimates each structured unit with one separator character,
// including conversation arrays whose actual comma separator has two.
func packStructured(units []string, size, initial int, separator, prefix, suffix string) []string {
	result, buffered := []string{}, []string{}
	length := initial
	flush := func() {
		if len(buffered) > 0 {
			result = append(result, prefix+strings.Join(buffered, separator)+suffix)
			buffered = nil
			length = initial
		}
	}
	for _, unit := range units {
		n := utf8.RuneCountInString(unit)
		if n > size {
			flush()
			result = append(result, recursiveChunks(unit, size, textSeparators)...)
			continue
		}
		if len(buffered) > 0 && length+n+1 > size {
			flush()
		}
		buffered = append(buffered, unit)
		length += n + 1
	}
	flush()
	if len(result) == 0 {
		return []string{prefix + suffix}
	}
	return result
}

func recursiveChunks(text string, size int, separators []string) []string {
	separator, remaining := separators[len(separators)-1], []string(nil)
	for i, candidate := range separators {
		if candidate == "" || strings.Contains(text, candidate) {
			separator, remaining = candidate, separators[i+1:]
			break
		}
	}
	pieces := []string{}
	if separator == "" {
		for _, r := range text {
			pieces = append(pieces, string(r))
		}
	} else {
		previous, search := 0, 0
		for search < len(text) {
			index := strings.Index(text[search:], separator)
			if index < 0 {
				break
			}
			index += search
			if index > previous {
				pieces = append(pieces, text[previous:index])
			}
			previous, search = index, index+len(separator)
		}
		if previous < len(text) {
			pieces = append(pieces, text[previous:])
		}
	}
	result, buffered := []string{}, []string{}
	length := 0
	flush := func() {
		if text := strings.TrimSpace(strings.Join(buffered, "")); text != "" {
			result = append(result, text)
		}
		buffered, length = nil, 0
	}
	for _, piece := range pieces {
		n := utf8.RuneCountInString(piece)
		if n < size {
			if length+n > size && len(buffered) > 0 {
				flush()
			}
			buffered = append(buffered, piece)
			length += n
		} else {
			flush()
			if len(remaining) > 0 {
				result = append(result, recursiveChunks(piece, size, remaining)...)
			} else {
				result = append(result, piece)
			}
		}
	}
	flush()
	return result
}

// Preserve JSON object key order, Unicode, and Python json.dumps spacing.
// Input is already validated JSON; decoding only its strings avoids Go map
// sorting changing the chunk boundaries of conversation turns.
func pythonJSON(raw json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		panic(err) // caller validated JSON
	}
	input := compact.Bytes()
	var out strings.Builder
	for i := 0; i < len(input); i++ {
		switch input[i] {
		case '"':
			end := i + 1
			for ; end < len(input); end++ {
				if input[end] == '\\' {
					end++
				} else if input[end] == '"' {
					break
				}
			}
			var value string
			if err := json.Unmarshal(input[i:end+1], &value); err != nil {
				panic(err)
			}
			out.WriteByte('"')
			for _, r := range value {
				switch r {
				case '"', '\\':
					out.WriteByte('\\')
					out.WriteRune(r)
				case '\b':
					out.WriteString(`\b`)
				case '\f':
					out.WriteString(`\f`)
				case '\n':
					out.WriteString(`\n`)
				case '\r':
					out.WriteString(`\r`)
				case '\t':
					out.WriteString(`\t`)
				default:
					if r < 32 {
						fmt.Fprintf(&out, `\u%04x`, r)
					} else {
						out.WriteRune(r)
					}
				}
			}
			out.WriteByte('"')
			i = end
		case ',', ':':
			out.WriteByte(input[i])
			out.WriteByte(' ')
		default:
			out.WriteByte(input[i])
		}
	}
	return out.String()
}
