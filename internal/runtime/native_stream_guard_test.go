package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeToolArgumentGuard(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fragments []string
		accepted  bool
	}{
		{"runaway outside string", []string{`{"files":[{"repository":""`, strings.Repeat(" \r\n\t", 2048), " "}, false},
		{"threshold allowed", []string{`{"x":`, strings.Repeat(" ", 8192), `1}`}, true},
		{"whitespace resets", []string{`{"x":`, strings.Repeat(" ", 8192), `1,`, strings.Repeat(" ", 8192), `"y":2}`}, true},
		{"string spaces preserved", []string{`{"x":"`, strings.Repeat(" ", 20000), `"}`}, true},
		{"escaped quote across chunks", []string{`{"x":"\`, `"`, strings.Repeat(" ", 20000), `"}`}, true},
		{"escaped slash closes string", []string{`{"x":"\`, `\"`, strings.Repeat(" ", 8193)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var guard nativeToolArgumentGuard
			accepted := true
			for _, fragment := range tc.fragments {
				if !guard.accept(fragment) {
					accepted = false
					break
				}
			}
			if accepted != tc.accepted {
				t.Fatalf("accepted=%v want=%v", accepted, tc.accepted)
			}
		})
	}
}

type guardedNativeStream struct {
	fakeNativeModelStream
	closed bool
}

func (s *guardedNativeStream) Close() { s.closed = true }

func TestCollectNativeModelStreamWhitespaceGuard(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		stream := &guardedNativeStream{}
		add := func(id, input string) {
			stream.chunks = append(stream.chunks, NativeModelResponse{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: id, ToolName: "read_files", Input: json.RawMessage(input)}}}})
		}
		add("a", `{"x":`)
		add("a", strings.Repeat(" ", 4096))
		add("b", `{"x":`)
		add("b", strings.Repeat(" ", 4096))
		add("b", `1}`)
		if stalled {
			add("a", strings.Repeat(" ", 4097))
		} else {
			add("a", `2}`)
		}
		response, _, err := collectNativeModelStream(t.Context(), contextTestExec(t), stream)
		if !stream.closed {
			t.Fatal("stream was not closed")
		}
		if stalled {
			if err == nil || !strings.Contains(err.Error(), "excessive whitespace") {
				t.Fatalf("err=%v", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range nativeRawToolCallBlocks(response.Message) {
				if !json.Valid(call.Input) {
					t.Fatalf("arguments corrupted: %q", call.Input)
				}
			}
		}
	}
}
