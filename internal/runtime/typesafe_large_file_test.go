package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func largeFileContext(t *testing.T, name string, content string, call NativeBlock) (map[string]any, error) {
	t.Helper()
	x := contextTestExec(t)
	x.Run.ExternalActorID = "owner"
	root := t.TempDir()
	if content != "" {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	x.Run.WorkspaceLease = &agentcore.WorkspaceLease{RootPath: root}
	messages := []NativeMessage{{Role: "user", Provenance: "human", Content: "Update the blog post with the new turnout figures."}}
	return typeSafeContext(x, messages, call)
}

func reviewedFiles(t *testing.T, state map[string]any) map[string]string {
	t.Helper()
	files, ok := state["context"].(map[string]any)["untrusted_repository_files"].(map[string]string)
	if !ok {
		t.Fatalf("files=%#v", state["context"])
	}
	return files
}

// A blog post larger than the whole-file limit is still reviewed: the
// reviewer sees the text around the edit instead of the review failing.
func TestTypeSafeLargeEditedFileIsExcerpted(t *testing.T) {
	var page strings.Builder
	for i := 0; i < 2000; i++ {
		page.WriteString("<p>Paragraph about the election, line filler text.</p>\n")
	}
	content := page.String()[:30000] + "<p>Turnout fell to 81.5%.</p>\n" + page.String()[30000:]
	if len(content) <= typeSafeReviewFileLimit {
		t.Fatalf("test page too small: %d", len(content))
	}
	input, _ := json.Marshal(map[string]string{"path": "outputs/blog.html", "old_string": "<p>Turnout fell to 81.5%.</p>", "new_string": "<p>Turnout rose to 84.9%.</p>"})
	state, err := largeFileContext(t, "outputs/blog.html", content, NativeBlock{ToolName: "edit_file", ToolCallID: "edit", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	excerpt := reviewedFiles(t, state)["outputs/blog.html"]
	if !strings.Contains(excerpt, "<p>Turnout fell to 81.5%.</p>") {
		t.Fatal("excerpt misses the edited text")
	}
	if len(excerpt) > 2*typeSafeExcerptContext+2048 || !strings.Contains(excerpt, "bytes of this file not shown before") || !strings.Contains(excerpt, "not shown after") {
		t.Fatalf("excerpt length %d, markers missing or too long", len(excerpt))
	}
	again, _ := largeFileContext(t, "outputs/blog.html", content, NativeBlock{ToolName: "edit_file", ToolCallID: "edit", Input: input})
	if reviewedFiles(t, again)["outputs/blog.html"] != excerpt {
		t.Fatal("excerpt is not stable for the same file and call")
	}
}

func TestTypeSafeLargeReplacedFileShowsStartAndEnd(t *testing.T) {
	content := "FIRST LINE\n" + strings.Repeat("middle line of an old file\n", 3000) + "LAST LINE\n"
	input, _ := json.Marshal(map[string]string{"path": "notes.md", "content": "new notes"})
	state, err := largeFileContext(t, "notes.md", content, NativeBlock{ToolName: "write_file", ToolCallID: "write", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	excerpt := reviewedFiles(t, state)["notes.md"]
	if !strings.HasPrefix(excerpt, "FIRST LINE\n") || !strings.HasSuffix(excerpt, "LAST LINE\n") || !strings.Contains(excerpt, "not shown here") {
		t.Fatalf("excerpt=%.200q", excerpt)
	}
}

func TestTypeSafeEditOfMissingFileHasNoFileEvidence(t *testing.T) {
	input, _ := json.Marshal(map[string]string{"path": "outputs/new.js", "old_string": "a", "new_string": "b"})
	state, err := largeFileContext(t, "", "", NativeBlock{ToolName: "edit_file", ToolCallID: "edit", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if files := reviewedFiles(t, state); len(files) != 0 {
		t.Fatalf("files=%v", files)
	}
}

// Scripts that a command runs are still reviewed whole, or not at all.
func TestTypeSafeLargeScriptStillRequiresWholeReview(t *testing.T) {
	script := strings.Repeat("print('step')\n", 4000)
	input, _ := json.Marshal(map[string]any{"program": "python3", "args": []string{"big.py"}})
	if _, err := largeFileContext(t, "big.py", script, NativeBlock{ToolName: "run_command", ToolCallID: "run", Input: input}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err=%v, want too large", err)
	}
}

func TestTypeSafeFileExcerptFallsBackWhenEditTextIsAmbiguous(t *testing.T) {
	data := []byte("start\n" + strings.Repeat("same line\n", 4000) + "end\n")
	excerpt := typeSafeFileExcerpt(data, "same line")
	if !strings.HasPrefix(excerpt, "start\n") || !strings.HasSuffix(excerpt, "end\n") {
		t.Fatalf("excerpt=%.120q", excerpt)
	}
}
