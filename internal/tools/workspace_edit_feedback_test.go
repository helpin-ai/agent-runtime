package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEditFileFeedback(t *testing.T) {
	for _, tt := range []struct {
		name, before, old, replacement, after, excerpt string
	}{
		{"replacement", "zero\nalpha\nbeta\ngamma\nlast\n", "beta", "BETTER", "zero\nalpha\nBETTER\ngamma\nlast\n", "@@ -1,5 +1,5 @@\n zero\n alpha\n-beta\n+BETTER\n gamma\n last"},
		{"deletion", "first\nremove\nlast\n", "remove\n", "", "first\nlast\n", "@@ -1,3 +1,2 @@\n first\n-remove\n last"},
		{"insertion", "start\nend\n", "\nend", "\none\ntwo\nend", "start\none\ntwo\nend\n", "@@ -1,2 +1,4 @@\n start\n+one\n+two\n end"},
		{"partial line", "call(alpha)\n", "alpha", "beta", "call(beta)\n", "@@ -1,1 +1,1 @@\n-call(alpha)\n+call(beta)"},
		{"remove final newline", "last\n", "last\n", "last", "last", "@@ -1,1 +1,1 @@\n-last\n+last\n\\ No newline at end of file"},
		{"delete whole file", "only\n", "only\n", "", "", "@@ -1,1 +0,0 @@\n-only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			registry, callCtx := workspaceToolTestRegistry(t)
			path := writeWorkspaceFixture(t, callCtx, "sample.txt", tt.before)
			if _, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"sample.txt"}]}`)); err != nil {
				t.Fatal(err)
			}
			input, err := json.Marshal(map[string]string{"path": "sample.txt", "old_string": tt.old, "new_string": tt.replacement})
			if err != nil {
				t.Fatal(err)
			}
			out, err := registry.Execute(context.Background(), callCtx, "edit_file", input)
			if err != nil {
				t.Fatal(err)
			}
			header, excerpt, ok := strings.Cut(ToolResultText(out), "\n")
			if !ok || !strings.HasPrefix(header, "Edited sample.txt at line ") || !strings.HasSuffix(header, " by replacing 1 occurrence.") || excerpt != tt.excerpt {
				t.Fatalf("unexpected feedback:\n%s\nwant excerpt:\n%s", ToolResultText(out), tt.excerpt)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != tt.after {
				t.Fatalf("unexpected file bytes: %q, %v", got, err)
			}
		})
	}
}

func TestEditFileFeedbackIsBounded(t *testing.T) {
	for _, replacement := range []string{strings.Repeat("new line\n", 100), strings.Repeat("界", 4000)} {
		registry, callCtx := workspaceToolTestRegistry(t)
		path := writeWorkspaceFixture(t, callCtx, "sample.txt", "old\n")
		if _, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"sample.txt"}]}`)); err != nil {
			t.Fatal(err)
		}
		input, err := json.Marshal(map[string]string{"path": "sample.txt", "old_string": "old\n", "new_string": replacement})
		if err != nil {
			t.Fatal(err)
		}
		out, err := registry.Execute(context.Background(), callCtx, "edit_file", input)
		if err != nil {
			t.Fatal(err)
		}
		_, excerpt, ok := strings.Cut(ToolResultText(out), "\n")
		if !ok || !utf8.ValidString(excerpt) || utf8.RuneCountInString(excerpt) > 3000 || len(strings.Split(excerpt, "\n")) > 60 || !strings.HasSuffix(excerpt, "[diff truncated]") {
			t.Fatalf("invalid bounded excerpt: %q", excerpt)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != replacement {
			t.Fatalf("feedback cap truncated file: %v", err)
		}
	}
}

func TestEditFeedbackLimitsContext(t *testing.T) {
	excerpt := workspaceEditDiff("outside before\na\nb\nc\nold\nd\ne\nf\noutside after\n", "outside before\na\nb\nc\nnew\nd\ne\nf\noutside after\n")
	want := "@@ -2,7 +2,7 @@\n a\n b\n c\n-old\n+new\n d\n e\n f"
	if excerpt != want {
		t.Fatalf("unexpected context:\n%s", excerpt)
	}
}

func TestEditFileNoChangeFeedback(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := writeWorkspaceFixture(t, callCtx, "sample.txt", "\ufeffsame\r\n")
	if _, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"sample.txt"}]}`)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := registry.Execute(context.Background(), callCtx, "edit_file", json.RawMessage(`{"path":"sample.txt","old_string":"same\n","new_string":"same\n"}`))
	if err != nil || ToolResultText(out) != "No changes made to sample.txt." {
		t.Fatalf("no-change response: %s, %v", out, err)
	}
	after, err := os.Stat(path)
	if err != nil || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("no-change edit rewrote file: %v", err)
	}
}
