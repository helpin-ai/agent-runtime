package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const modelVisibleFileReadOutputMaxRunesForTest = 2800

func TestWorkspaceToolWriteFileRequiresPriorReadForExistingFile(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "existing.txt"), []byte("original"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"existing.txt","content":"updated"}`))
	if err == nil || !strings.Contains(err.Error(), "must read existing.txt before modifying it") {
		t.Fatalf("expected prior-read error, got %v", err)
	}
}

func TestWorkspaceToolWriteFileRejectsStaleExistingFile(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	filePath := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "existing.txt")
	if err := os.WriteFile(filePath, []byte("original"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"existing.txt"}`)); err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}
	staleModTime := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(filePath, []byte("changed elsewhere"), 0644); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	if err := os.Chtimes(filePath, staleModTime, staleModTime); err != nil {
		t.Fatalf("set stale mod time: %v", err)
	}

	_, err := registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"existing.txt","content":"updated"}`))
	if err == nil || !strings.Contains(err.Error(), "changed since the last read_file call") {
		t.Fatalf("expected stale-read error, got %v", err)
	}
}

func TestWorkspaceToolWriteFileAllowsCreateWithoutPriorRead(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	output, err := registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"new.txt","content":"created"}`))
	if err != nil {
		t.Fatalf("write_file returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, output), "Wrote 7 bytes to new.txt") {
		t.Fatalf("unexpected output: %s", string(output))
	}
	data, err := os.ReadFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "new.txt"))
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(data) != "created" {
		t.Fatalf("unexpected created file content %q", string(data))
	}
}

func TestWorkspaceToolEditFileRequiresUniqueMatch(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	filePath := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt")
	if err := os.WriteFile(filePath, []byte("alpha\nshared\nomega\nshared\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"sample.txt"}`)); err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}

	_, err := registry.Execute(context.Background(), callCtx, "edit_file", json.RawMessage(`{"path":"sample.txt","old_string":"shared","new_string":"updated"}`))
	if err == nil || !strings.Contains(err.Error(), "matched 2 locations") {
		t.Fatalf("expected non-unique match error, got %v", err)
	}
}

func TestWorkspaceToolEditFileAppliesSingleReplacementAfterRead(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	filePath := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt")
	initial := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(filePath, []byte(initial), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"sample.txt"}`)); err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}

	output, err := registry.Execute(context.Background(), callCtx, "edit_file", json.RawMessage(`{"path":"sample.txt","old_string":"alpha\nbeta\ngamma\n","new_string":"alpha\nbeta-updated\ngamma\n"}`))
	if err != nil {
		t.Fatalf("edit_file returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, output), "Edited sample.txt by replacing 1 occurrence.") {
		t.Fatalf("unexpected output: %s", string(output))
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	if string(data) != "alpha\nbeta-updated\ngamma\n" {
		t.Fatalf("unexpected edited content %q", string(data))
	}
}

func TestWorkspaceToolReadFileDefaultsToBoundedWindow(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	filePath := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "large.txt")
	var builder strings.Builder
	for i := 1; i <= defaultReadFileLimitLines+10; i++ {
		builder.WriteString("line ")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteString("\n")
	}
	if err := os.WriteFile(filePath, []byte(builder.String()), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	output, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"large.txt"}`))
	if err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}
	text := workspaceToolString(t, output)
	if !strings.Contains(text, `File "large.txt", lines 1-120:`) {
		t.Fatalf("expected bounded read metadata, got %q", text)
	}
	if !strings.Contains(text, "     1 | line 1") || !strings.Contains(text, "   120 | line 120") {
		t.Fatalf("expected first window content, got %q", text)
	}
	if strings.Contains(text, "line 125") {
		t.Fatalf("did not expect lines past default window, got %q", text)
	}
	if !strings.Contains(text, `offset_line=121`) {
		t.Fatalf("expected continuation hint, got %q", text)
	}
}

func TestWorkspaceToolReadFilesReadsMultipleFiles(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("a\nb\nc\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.txt"), []byte("x\ny\nz\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	output, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"one.txt","limit_lines":2},{"path":"two.txt","offset_line":2,"limit_lines":2}]}`))
	if err != nil {
		t.Fatalf("read_files returned error: %v", err)
	}
	text := workspaceToolString(t, output)
	if !strings.Contains(text, `<files count="2">`) ||
		!strings.Contains(text, "     1 | a\n     2 | b") ||
		!strings.Contains(text, "     2 | y\n     3 | z") {
		t.Fatalf("unexpected output: %q", text)
	}
}

func TestWorkspaceToolReadFileRejectsPathTraversal(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	_, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"../outside.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "path traversal not allowed") {
		t.Fatalf("expected traversal error, got %v", err)
	}
}

func TestWorkspaceToolReadFileRejectsOversizedLimit(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	limit := strconv.Itoa(maxReadFileLimitLines + 1)
	_, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"sample.txt","limit_lines":`+limit+`}`))
	if err == nil || !strings.Contains(err.Error(), "limit_lines too large") {
		t.Fatalf("expected limit error, got %v", err)
	}
}

func TestWorkspaceToolReadFileReportsSingleRemainingLine(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	var content strings.Builder
	for line := 1; line <= defaultReadFileLimitLines+1; line++ {
		fmt.Fprintf(&content, "line %d\n", line)
	}
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "boundary.txt")
	if err := os.WriteFile(path, []byte(content.String()), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	first, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"boundary.txt"}`))
	if err != nil {
		t.Fatalf("read first window: %v", err)
	}
	firstText := workspaceToolString(t, first)
	if !strings.Contains(firstText, "offset_line=121") {
		t.Fatalf("expected continuation for the single remaining line, got %q", firstText)
	}
	second, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"boundary.txt","offset_line":121}`))
	if err != nil {
		t.Fatalf("read second window: %v", err)
	}
	if got := workspaceToolString(t, second); !strings.Contains(got, "   121 | line 121") {
		t.Fatalf("expected final line in continuation, got %q", got)
	}
}

func TestWorkspaceToolReadFileBoundsAndClampsHostileLineWithoutBlockingFullReplacement(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "bundle.js")
	content := strings.Repeat("x", 2*1024*1024) + "\nsecond\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	output, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"bundle.js"}`))
	if err != nil {
		t.Fatalf("read hostile line: %v", err)
	}
	text := workspaceToolString(t, output)
	if utf8.RuneCountInString(text) > modelVisibleFileReadOutputMaxRunesForTest {
		t.Fatalf("read output exceeded model-safe bound: %d runes", utf8.RuneCountInString(text))
	}
	if !strings.Contains(text, "[line truncated]") || !strings.Contains(text, "     2 | second") {
		t.Fatalf("expected clamped first line and intact second line, got %q", text)
	}
	if _, err = registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"bundle.js","content":"replacement"}`)); err != nil {
		t.Fatalf("clamped full scan should authorize replacement using its raw fingerprint: %v", err)
	}
}

func TestWorkspaceToolReadFileOutputBudgetHasExactResumeLine(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "wide.txt")
	var content strings.Builder
	for line := 1; line <= 10; line++ {
		fmt.Fprintf(&content, "%02d-%s\n", line, strings.Repeat("x", 500))
	}
	if err := os.WriteFile(path, []byte(content.String()), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	first, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"wide.txt"}`))
	if err != nil {
		t.Fatalf("read first window: %v", err)
	}
	firstText := workspaceToolString(t, first)
	if utf8.RuneCountInString(firstText) > modelVisibleFileReadOutputMaxRunesForTest {
		t.Fatalf("read output exceeded model-safe bound: %d runes", utf8.RuneCountInString(firstText))
	}
	if !strings.Contains(firstText, "offset_line=5") {
		t.Fatalf("expected output-budget continuation at line 5, got %q", firstText)
	}
	second, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"wide.txt","offset_line":5}`))
	if err != nil {
		t.Fatalf("read continuation: %v", err)
	}
	if got := workspaceToolString(t, second); !strings.Contains(got, "     5 | 05-") {
		t.Fatalf("expected continuation to start at undisplayed line, got %q", got)
	}
}

func TestWorkspaceToolReadFilesCombinedOutputIsModelBounded(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	for index := 1; index <= maxReadFilesPerCall; index++ {
		name := fmt.Sprintf("wide-%d.txt", index)
		if err := os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("x", 10_000)), 0644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	input := `{"files":[{"path":"wide-1.txt"},{"path":"wide-2.txt"},{"path":"wide-3.txt"},{"path":"wide-4.txt"}]}`
	output, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(input))
	if err != nil {
		t.Fatalf("read files: %v", err)
	}
	text := workspaceToolString(t, output)
	if utf8.RuneCountInString(text) > modelVisibleFileReadOutputMaxRunesForTest {
		t.Fatalf("combined read output exceeded model-safe bound: %d runes", utf8.RuneCountInString(text))
	}
}

func TestWorkspaceToolPartialReadBlocksFullReplacement(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "partial.txt")
	content := strings.Repeat("line\n", defaultReadFileLimitLines+1)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"partial.txt"}`)); err != nil {
		t.Fatalf("read partial file: %v", err)
	}

	_, err := registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"partial.txt","content":"replacement"}`))
	if err == nil || !strings.Contains(err.Error(), "only part of partial.txt has been read") {
		t.Fatalf("expected partial-read error, got %v", err)
	}
}

func TestWorkspaceToolPagedCoverageAllowsFullReplacement(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "paged.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("line\n", defaultReadFileLimitLines+1)), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"paged.txt"}`)); err != nil {
		t.Fatalf("read first page: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"paged.txt","offset_line":121}`)); err != nil {
		t.Fatalf("read final page: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"paged.txt","content":"replacement"}`)); err != nil {
		t.Fatalf("fully paged file should allow replacement: %v", err)
	}
}

func TestWorkspaceToolPagedCoverageRejectsMixedFileVersions(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "paged-change.txt")
	original := "aaaa\n" + strings.Repeat("line\n", defaultReadFileLimitLines)
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"paged-change.txt"}`)); err != nil {
		t.Fatalf("read first page: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	changed := "bbbb\n" + strings.Repeat("line\n", defaultReadFileLimitLines)
	if err := os.WriteFile(path, []byte(changed), 0644); err != nil {
		t.Fatalf("change fixture: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore timestamp: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"paged-change.txt","offset_line":121}`)); err != nil {
		t.Fatalf("read final page: %v", err)
	}
	_, err = registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"paged-change.txt","content":"replacement"}`))
	if err == nil || !strings.Contains(err.Error(), "only part of paged-change.txt has been read") {
		t.Fatalf("mixed file versions were incorrectly merged: %v", err)
	}
}

func TestWorkspaceToolWeakerReadPreservesCompleteObservation(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "complete.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"complete.txt"}`)); err != nil {
		t.Fatalf("read complete file: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"complete.txt","offset_line":99}`)); err != nil {
		t.Fatalf("read past EOF: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"complete.txt","content":"replacement"}`)); err != nil {
		t.Fatalf("weaker read erased complete observation: %v", err)
	}
}

func TestWorkspaceToolReadFilesFailureDoesNotCommitEarlierObservations(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "unreturned.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"unreturned.txt"},{"path":"missing.txt"}]}`))
	if err == nil {
		t.Fatal("expected batch read failure")
	}
	_, err = registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"unreturned.txt","content":"replacement"}`))
	if err == nil || !strings.Contains(err.Error(), "must read unreturned.txt") {
		t.Fatalf("failed batch read authorized unseen write: %v", err)
	}
}

func TestWorkspaceToolReadFileRejectsLegacyOffsetOverflow(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "offset.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	input := json.RawMessage(fmt.Sprintf(`{"path":"offset.txt","offset":%d}`, int64(^uint64(0)>>1)))
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", input); err == nil || !strings.Contains(err.Error(), "offset is too large") {
		t.Fatalf("expected overflowing offset rejection, got %v", err)
	}
}

func TestWorkspaceToolReadFileHonorsCanceledContext(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "cancel.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.Execute(ctx, callCtx, "read_file", json.RawMessage(`{"path":"cancel.txt"}`)); err == nil || !strings.Contains(err.Error(), "read canceled") {
		t.Fatalf("expected cancellation error, got %v", err)
	}
}

func TestWorkspaceToolReadFileBoundsQuotedUnicodePath(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	segment := strings.Repeat("\u2028", 70)
	fileName := strings.Repeat("\u2028", 20) + ".txt"
	relPath := filepath.Join(segment, segment, fileName)
	absPath := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		t.Fatalf("create fixture directories: %v", err)
	}
	content := strings.Repeat("x", 990) + "\n" + strings.Repeat("y", 990)
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	input, err := json.Marshal(map[string]any{"path": relPath})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	output, err := registry.Execute(context.Background(), callCtx, "read_file", input)
	if err != nil {
		t.Fatalf("read unicode path: %v", err)
	}
	if count := utf8.RuneCountInString(workspaceToolString(t, output)); count > modelVisibleFileReadOutputMaxRunesForTest {
		t.Fatalf("quoted path expanded output beyond bound: %d runes", count)
	}
}

func TestWorkspaceToolReadFileDefersExactStreamBoundaryDecision(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	line := strings.Repeat("x", readStreamBufferBytes-1) + "\n"
	if err := os.WriteFile(filepath.Join(root, "boundary-only.txt"), []byte(line), 0644); err != nil {
		t.Fatalf("write boundary fixture: %v", err)
	}
	output, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"boundary-only.txt","limit_lines":1}`))
	if err != nil {
		t.Fatalf("read boundary fixture: %v", err)
	}
	if got := workspaceToolString(t, output); strings.Contains(got, "Continue reading") {
		t.Fatalf("exact stream boundary falsely reported more content: %q", got)
	}
	if err := os.WriteFile(filepath.Join(root, "boundary-more.txt"), []byte(line+"next\n"), 0644); err != nil {
		t.Fatalf("write continuation fixture: %v", err)
	}
	output, err = registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"boundary-more.txt","limit_lines":1}`))
	if err != nil {
		t.Fatalf("read continuation fixture: %v", err)
	}
	if got := workspaceToolString(t, output); !strings.Contains(got, "offset_line=2") {
		t.Fatalf("exact stream boundary missed remaining content: %q", got)
	}
}

func TestReadStreamLookaheadDoesNotConsumeHostileNextLine(t *testing.T) {
	source := &countingReadSource{Reader: strings.NewReader(strings.Repeat("x", 2*1024*1024))}
	reader := bufio.NewReaderSize(source, readStreamBufferBytes)
	hasMore, err := readStreamHasMore(context.Background(), reader)
	if err != nil || !hasMore {
		t.Fatalf("look ahead: has_more=%t err=%v", hasMore, err)
	}
	if source.BytesRead > readStreamBufferBytes {
		t.Fatalf("lookahead consumed %d bytes of the next hostile line", source.BytesRead)
	}
}

type countingReadSource struct {
	io.Reader
	BytesRead int
}

func (r *countingReadSource) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	r.BytesRead += n
	return n, err
}

func TestWorkspaceToolCompleteReadDetectsSameTimestampContentChange(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "same-time.txt")
	if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"same-time.txt"}`)); err != nil {
		t.Fatalf("read file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte("modified"), 0644); err != nil {
		t.Fatalf("modify fixture: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore fixture timestamp: %v", err)
	}

	_, err = registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"same-time.txt","content":"replacement"}`))
	if err == nil || !strings.Contains(err.Error(), "content changed since the last read_file call") {
		t.Fatalf("expected content fingerprint rejection, got %v", err)
	}
}

func TestWorkspaceToolReadFileRejectsWorkspaceSymlink(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatalf("write outside fixture: %v", err)
	}
	link := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	_, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"escape.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "cannot open workspace file") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestWorkspaceToolReadFileAllowsSymlinkThatStaysInsideWorkspace(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("inside\n"), 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	output, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"link.txt"}`))
	if err != nil {
		t.Fatalf("read safe symlink: %v", err)
	}
	if got := workspaceToolString(t, output); !strings.Contains(got, "inside") {
		t.Fatalf("unexpected safe symlink output %q", got)
	}
}

func TestWorkspaceToolReadFileRejectsFIFOWithoutBlocking(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatalf("create FIFO: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := registry.Execute(ctx, callCtx, "read_file", json.RawMessage(`{"path":"pipe"}`))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected non-regular-file rejection, got %v", err)
	}
}

func TestWorkspaceToolReadFileRejectsInvalidWindowInputs(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	for _, input := range []string{
		`{"path":"sample.txt","offset_line":-1}`,
		`{"path":"sample.txt","limit_lines":-1}`,
		`{"path":"sample.txt","offset_line":1,"offset":0}`,
		`{"path":"sample.txt","unknown":true}`,
	} {
		if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(input)); err == nil {
			t.Fatalf("expected invalid input %s to be rejected", input)
		}
	}
}

func TestWorkspaceToolReadFileDistinguishesEmptyAndPastEOF(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0644); err != nil {
		t.Fatalf("write empty fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "short.txt"), []byte("one\ntwo\n"), 0644); err != nil {
		t.Fatalf("write short fixture: %v", err)
	}

	empty, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"empty.txt"}`))
	if err != nil {
		t.Fatalf("read empty file: %v", err)
	}
	if got := workspaceToolString(t, empty); got != `File "empty.txt" is empty.` {
		t.Fatalf("unexpected empty-file result %q", got)
	}
	pastEOF, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"short.txt","offset_line":9}`))
	if err != nil {
		t.Fatalf("read past EOF: %v", err)
	}
	if got := workspaceToolString(t, pastEOF); !strings.Contains(got, `beyond the end of "short.txt" (2 lines)`) {
		t.Fatalf("unexpected past-EOF result %q", got)
	}
}

func TestWorkspaceToolPackCloseRunReleasesOnlySelectedState(t *testing.T) {
	pack := newWorkspaceToolPack()
	pack.fileState(CallContext{AppID: "app-a", RunID: "run-a"})
	pack.fileState(CallContext{AppID: "app-a", RunID: "run-b"})
	if err := pack.CloseRun(context.Background(), "app-a", "run-a"); err != nil {
		t.Fatalf("close run: %v", err)
	}
	pack.mu.Lock()
	defer pack.mu.Unlock()
	if _, ok := pack.states["app-a/run-a"]; ok {
		t.Fatal("expected closed run state to be deleted")
	}
	if _, ok := pack.states["app-a/run-b"]; !ok {
		t.Fatal("expected unrelated run state to remain")
	}
}

func TestWorkspaceToolPackIsRegisteredForRunCleanup(t *testing.T) {
	registry := NewRegistry()
	found := false
	for _, closer := range registry.state.runClosers {
		if _, ok := closer.(*workspaceToolPack); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected workspace tool pack to be registered as a run closer")
	}
	clone := registry.CloneForApp("app-a")
	if len(clone.state.runClosers) != len(registry.state.runClosers) {
		t.Fatalf("clone lost run closers: got %d want %d", len(clone.state.runClosers), len(registry.state.runClosers))
	}
}

func TestToolResultTextDecodesTextAndPreservesStructuredJSON(t *testing.T) {
	if got := ToolResultText(json.RawMessage(`"line one\nline two"`)); got != "line one\nline two" {
		t.Fatalf("decoded text result = %q", got)
	}
	if got := ToolResultText(json.RawMessage(`{"ok":true}`)); got != `{"ok":true}` {
		t.Fatalf("structured result changed to %q", got)
	}
	if got := ToolResultText(json.RawMessage(`null`)); got != `null` {
		t.Fatalf("null result changed to %q", got)
	}
}

func TestWorkspaceToolListDirectoryReturnsContents(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	if err := os.Mkdir(filepath.Join(root, "nested"), 0755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	output, err := registry.Execute(context.Background(), callCtx, "list_directory", json.RawMessage(`{"path":""}`))
	if err != nil {
		t.Fatalf("list_directory returned error: %v", err)
	}
	text := workspaceToolString(t, output)
	if !strings.Contains(text, "nested/") || !strings.Contains(text, "sample.txt") {
		t.Fatalf("expected directory contents, got %q", text)
	}
}

func TestWorkspaceToolGrepAndSymbols(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	source := "package sample\n\ntype Thing struct{}\n\nfunc Run() {}\n"
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.go"), []byte(source), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	grepOut, err := registry.Execute(context.Background(), callCtx, "grep", json.RawMessage(`{"pattern":"Thing","include":"*.go"}`))
	if err != nil {
		t.Fatalf("grep returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, grepOut), "sample.go:3: type Thing struct{}") {
		t.Fatalf("unexpected grep output: %s", string(grepOut))
	}
	symbolsOut, err := registry.Execute(context.Background(), callCtx, "list_symbols", json.RawMessage(`{"path":"sample.go"}`))
	if err != nil {
		t.Fatalf("list_symbols returned error: %v", err)
	}
	symbols := workspaceToolString(t, symbolsOut)
	if !strings.Contains(symbols, "type Thing struct{}") || !strings.Contains(symbols, "func Run() {}") {
		t.Fatalf("unexpected symbols output: %q", symbols)
	}
}

func TestWorkspaceToolDefinitionsAreRegisteredWithMutatingFlags(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"read_file", "read_files", "list_directory", "search_files", "read_file_range", "ripgrep", "grep", "list_symbols"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s definition", name)
		}
		if def.Mutating {
			t.Fatalf("expected %s to be read-only", name)
		}
		if name == "read_file" || name == "read_files" || name == "read_file_range" {
			schema, ok := def.InputSchema.(map[string]interface{})
			if !ok || schema["additionalProperties"] != false {
				t.Fatalf("expected strict schema for %s, got %#v", name, def.InputSchema)
			}
			if name == "read_files" {
				properties := schema["properties"].(map[string]interface{})
				files := properties["files"].(map[string]interface{})
				if files["minItems"] != 1 || files["maxItems"] != maxReadFilesPerCall {
					t.Fatalf("expected bounded files array, got %#v", files)
				}
			}
		}
	}
	for _, name := range []string{"write_file", "edit_file"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s definition", name)
		}
		if !def.Mutating {
			t.Fatalf("expected %s to be mutating", name)
		}
	}
}

func workspaceToolTestRegistry(t *testing.T) (*Registry, CallContext) {
	t.Helper()
	root := t.TempDir()
	registry := NewRegistry()
	run := &agentcore.AgentRun{
		ID:    "run-1",
		AppID: "app-a",
		WorkspaceLease: &agentcore.WorkspaceLease{
			ID:       "lease-1",
			RootPath: root,
		},
	}
	return registry, CallContext{AppID: run.AppID, RunID: run.ID, Run: run}
}

func workspaceToolString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode tool output: %v; raw=%s", err, string(raw))
	}
	return out
}
