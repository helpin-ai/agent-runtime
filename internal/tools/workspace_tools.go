package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const (
	defaultReadFileLimitLines  = 120
	maxReadFileLimitLines      = 240
	defaultReadFilesLimitLines = 60
	maxReadFilesPerCall        = 4
	maxReadFilesLimitLines     = 120
	maxReadFilesTotalLines     = 320
)

// RegisterWorkspaceTools registers in-process tools that operate
// on the current run's WorkspaceLease.RootPath.
func RegisterWorkspaceTools(r *Registry) {
	if r == nil {
		return
	}
	pack := newWorkspaceToolPack()
	for _, item := range []struct {
		def     Definition
		handler Handler
	}{
		{workspaceToolDefinition("read_file", "Read a bounded window of a text file at the given path (relative to the workspace root). Use ripgrep/search_files/list_symbols first, then use read_file or read_file_range for the exact section you need.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":        map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
				"repo_alias":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"offset_line": map[string]interface{}{"type": "integer", "description": "Optional 1-based line number to start reading from. Defaults to 1."},
				"limit_lines": map[string]interface{}{"type": "integer", "description": "Optional maximum number of lines to return. Defaults to 120, max 240."},
				"offset":      map[string]interface{}{"type": "integer", "description": "Deprecated 0-based line offset."},
				"limit":       map[string]interface{}{"type": "integer", "description": "Deprecated maximum line count."},
			},
			"required": []string{"path"},
		}), pack.readFile},
		{workspaceToolDefinition("read_files", "Read small bounded windows from a few specific text files in one call. Prefer ripgrep/search_files plus read_file_range first; use this only when you already know the exact files and need small excerpts.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"files": map[string]interface{}{
					"type":        "array",
					"description": "Files to read. Max 4 files per call.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"path":        map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
							"repo_alias":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
							"repository":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
							"offset_line": map[string]interface{}{"type": "integer", "description": "Optional 1-based line number to start reading from. Defaults to 1."},
							"limit_lines": map[string]interface{}{"type": "integer", "description": "Optional maximum number of lines to return for this file. Defaults to 60, max 120."},
						},
						"required": []string{"path"},
					},
				},
			},
			"required": []string{"files"},
		}), pack.readFiles},
		{workspaceToolDefinition("write_file", "Write content to a file at the given path (relative to the workspace root). Use this for new files or full rewrites after reading the current file first. Creates directories as needed.", true, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
				"content": map[string]interface{}{"type": "string", "description": "The content to write"},
			},
			"required": []string{"path", "content"},
		}), pack.writeFile},
		{workspaceToolDefinition("edit_file", "Edit an existing text file by replacing exactly one matching string. Read the file first and include enough surrounding context in old_string so the match is unique.", true, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
				"old_string": map[string]interface{}{"type": "string", "description": "Exact text to replace. Must match exactly once in the file, including whitespace."},
				"new_string": map[string]interface{}{"type": "string", "description": "Replacement text. Use an empty string to delete the matched content."},
			},
			"required": []string{"path", "old_string", "new_string"},
		}), pack.editFile},
		{workspaceToolDefinition("apply_patch", "Apply coordinated multi-file edits using the structured *** Begin Patch format. Read each existing file first, and use exact context lines so each hunk matches uniquely.", true, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"patch": map[string]interface{}{"type": "string", "description": "Patch text in the *** Begin Patch / *** End Patch format."},
			},
			"required": []string{"patch"},
		}), pack.applyPatch},
		{workspaceToolDefinition("run_command", "Run an allowlisted command in the workspace directory. Prefer program + args; shell syntax is not supported.", true, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"program": map[string]interface{}{"type": "string", "description": "The executable name, for example go, npm, git"},
				"args": map[string]interface{}{
					"type":        "array",
					"description": "Command arguments as a JSON string array",
					"items":       map[string]interface{}{"type": "string"},
				},
				"command": map[string]interface{}{"type": "string", "description": "Deprecated compatibility field. Plain commands only; shell operators are rejected."},
			},
		}), pack.runCommand},
		{workspaceToolDefinition("list_commits", "Read commit history from the checked-out repository (read-only git log). Use for changelogs, release notes, or summarizing recent changes. Filter with branch, since/until dates, path, and limit.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"branch":     map[string]interface{}{"type": "string", "description": "Branch to read. Defaults to the checked-out branch."},
				"since":      map[string]interface{}{"type": "string", "description": "Only commits after this date, for example 2026-05-05 or 1 month ago."},
				"until":      map[string]interface{}{"type": "string", "description": "Only commits before this date."},
				"path":       map[string]interface{}{"type": "string", "description": "Optional path filter."},
				"limit":      map[string]interface{}{"type": "integer", "description": "Max commits to return, default 50, max 200."},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
		}), pack.listCommits},
		{workspaceToolDefinition("create_branch", "Create a new git branch and switch to it.", true, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "Branch name"},
			},
			"required": []string{"name"},
		}), pack.createBranch},
		{workspaceToolDefinition("commit_and_push", "Stage all changes, commit with the given message, and push the current branch to origin.", true, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"message": map[string]interface{}{"type": "string", "description": "Commit message"},
			},
			"required": []string{"message"},
		}), pack.commitAndPush},
		{workspaceToolDefinition("list_directory", "List files and directories at the given path.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]interface{}{"type": "string", "description": "Directory path relative to the workspace root (empty string for root)"},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required": []string{"path"},
		}), pack.listDirectory},
		{workspaceToolDefinition("search_files", "Search for files matching a glob pattern, optionally grep for content.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern":    map[string]interface{}{"type": "string", "description": "Glob pattern, for example **/*.go"},
				"query":      map[string]interface{}{"type": "string", "description": "Optional text to search within matched files"},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required": []string{"pattern"},
		}), pack.searchFiles},
		{workspaceToolDefinition("read_file_range", "Read a specific line range from a file. Prefer this after search/ripgrep when you know the relevant span; it is much more token-efficient than broad file reads.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
				"start_line": map[string]interface{}{"type": "integer", "description": "First line number to read, 1-based"},
				"end_line":   map[string]interface{}{"type": "integer", "description": "Last line number to read, 1-based inclusive"},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required": []string{"path", "start_line", "end_line"},
		}), pack.readFileRange},
		{workspaceToolDefinition("ripgrep", "Fast regex code search using ripgrep. Preferred over search_files for content search.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern":          map[string]interface{}{"type": "string", "description": "Search pattern, regex by default"},
				"path":             map[string]interface{}{"type": "string", "description": "Optional subdirectory to search within"},
				"file_type":        map[string]interface{}{"type": "string", "description": "Restrict to file type, for example go, ts, py, js, rust, java"},
				"context_lines":    map[string]interface{}{"type": "integer", "description": "Lines of context around each match, 0-5"},
				"max_results":      map[string]interface{}{"type": "integer", "description": "Maximum result lines, default 50, max 200"},
				"case_insensitive": map[string]interface{}{"type": "boolean", "description": "Case-insensitive search"},
				"fixed_strings":    map[string]interface{}{"type": "boolean", "description": "Treat pattern as literal string"},
				"repo_alias":       map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository":       map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required": []string{"pattern"},
		}), pack.ripgrep},
		{workspaceToolDefinition("grep", "Simple text/regex search (Go-native, no external dependencies). Use ripgrep for better performance if available.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern":     map[string]interface{}{"type": "string", "description": "Search pattern, regex"},
				"path":        map[string]interface{}{"type": "string", "description": "Optional subdirectory to search within"},
				"include":     map[string]interface{}{"type": "string", "description": "Filename glob filter, for example *.go"},
				"max_results": map[string]interface{}{"type": "integer", "description": "Maximum results to return, default 50"},
				"repo_alias":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required": []string{"pattern"},
		}), pack.grep},
		{workspaceToolDefinition("list_symbols", "Extract function, type, and class declarations from a source file. Returns only signature lines with line numbers.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required": []string{"path"},
		}), pack.listSymbols},
	} {
		r.Register(item.def, item.handler)
	}
}

func workspaceToolDefinition(name string, description string, mutating bool, schema map[string]interface{}) Definition {
	return Definition{Name: name, Description: description, Category: "Workspace", InputSchema: schema, Mutating: mutating}
}

type workspaceToolPack struct {
	mu     sync.Mutex
	states map[string]*workspaceToolFileState
}

func newWorkspaceToolPack() *workspaceToolPack {
	return &workspaceToolPack{states: map[string]*workspaceToolFileState{}}
}

type workspaceToolFileObservation struct {
	LastReadAt      time.Time
	LastReadModTime time.Time
	ReadVia         string
}

type workspaceToolFileState struct {
	mu    sync.Mutex
	reads map[string]workspaceToolFileObservation
}

func newWorkspaceToolFileState() *workspaceToolFileState {
	return &workspaceToolFileState{reads: map[string]workspaceToolFileObservation{}}
}

func (p *workspaceToolPack) fileState(callCtx CallContext) *workspaceToolFileState {
	if p == nil {
		return nil
	}
	key := callCtx.AppID + "/" + callCtx.RunID
	if key == "/" && callCtx.Run != nil {
		key = callCtx.Run.AppID + "/" + callCtx.Run.ID
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.states[key]
	if state == nil {
		state = newWorkspaceToolFileState()
		p.states[key] = state
	}
	return state
}

func (p *workspaceToolPack) readFile(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Path       string `json:"path"`
		OffsetLine int    `json:"offset_line"`
		LimitLines int    `json:"limit_lines"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	startLine, limitLines, err := normalizeReadFileWindow(params.OffsetLine, params.LimitLines, params.Offset, params.Limit)
	if err != nil {
		return nil, err
	}
	window, err := p.readTextFileWindow(callCtx, params.repoSelector(), params.Path, startLine, limitLines, "read_file")
	if err != nil {
		return nil, err
	}
	return workspaceToolText(formatReadFileWindow(window)), nil
}

func normalizeReadFileWindow(offsetLine, limitLines, offset, limit int) (int, int, error) {
	startLine := 1
	if offsetLine > 0 {
		startLine = offsetLine
	} else if offset > 0 {
		startLine = offset + 1
	} else if offset < 0 {
		return 0, 0, fmt.Errorf("offset must be >= 0")
	}
	if startLine < 1 {
		return 0, 0, fmt.Errorf("offset_line must be >= 1")
	}
	if limitLines <= 0 {
		limitLines = limit
	}
	if limitLines <= 0 {
		limitLines = defaultReadFileLimitLines
	}
	if limitLines > maxReadFileLimitLines {
		return 0, 0, fmt.Errorf("limit_lines too large: max %d lines per call (requested %d)", maxReadFileLimitLines, limitLines)
	}
	return startLine, limitLines, nil
}

type readFileWindow struct {
	Path           string
	StartLine      int
	Lines          []string
	HasMore        bool
	NextOffsetLine int
}

func (p *workspaceToolPack) readTextFileWindow(callCtx CallContext, repoSelector, path string, startLine, limitLines int, via string) (*readFileWindow, error) {
	root, err := requireWorkspaceRootForRepository(callCtx, via, repoSelector)
	if err != nil {
		return nil, err
	}
	absPath, err := safeWorkspacePath(root, path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	preview := make([]byte, 512)
	n, readErr := f.Read(preview)
	if readErr != nil && readErr != io.EOF {
		return nil, fmt.Errorf("read file preview: %w", readErr)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("reset file cursor: %w", err)
	}
	if isBinaryContent(preview[:n]) {
		return nil, fmt.Errorf("file appears to be binary, cannot read: %s", path)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	currentLine := 0
	for currentLine < startLine-1 && scanner.Scan() {
		currentLine++
	}
	collected := make([]string, 0, limitLines)
	for scanner.Scan() && len(collected) < limitLines {
		currentLine++
		collected = append(collected, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	hasMore := scanner.Scan()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if info, statErr := os.Stat(absPath); statErr == nil {
		p.recordFileRead(callCtx, absPath, info.ModTime(), via)
	}
	return &readFileWindow{Path: path, StartLine: startLine, Lines: collected, HasMore: hasMore, NextOffsetLine: startLine + len(collected)}, nil
}

func formatReadFileWindow(window *readFileWindow) string {
	if window == nil {
		return ""
	}
	if len(window.Lines) == 0 {
		return fmt.Sprintf("No lines available starting at line %d in %s", window.StartLine, window.Path)
	}
	var out strings.Builder
	out.WriteString(fmt.Sprintf("<file path=\"%s\" start_line=\"%d\" returned_lines=\"%d\">\n", window.Path, window.StartLine, len(window.Lines)))
	out.WriteString(strings.Join(window.Lines, "\n"))
	out.WriteString("\n</file>")
	if window.HasMore {
		out.WriteString(fmt.Sprintf("\n\nFile has more lines. Use read_file with {\"path\":\"%s\",\"offset_line\":%d} to continue, or use read_file_range for a specific span.", window.Path, window.NextOffsetLine))
	}
	return out.String()
}

func (p *workspaceToolPack) readFiles(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Files []struct {
			workspaceRepoSelector
			Path       string `json:"path"`
			OffsetLine int    `json:"offset_line"`
			LimitLines int    `json:"limit_lines"`
		} `json:"files"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if len(params.Files) == 0 {
		return nil, fmt.Errorf("files is required")
	}
	if len(params.Files) > maxReadFilesPerCall {
		return nil, fmt.Errorf("too many files: max %d per call", maxReadFilesPerCall)
	}
	totalLines := 0
	windows := make([]*readFileWindow, 0, len(params.Files))
	for _, file := range params.Files {
		if strings.TrimSpace(file.Path) == "" {
			return nil, fmt.Errorf("each file entry must include path")
		}
		startLine := 1
		if file.OffsetLine > 0 {
			startLine = file.OffsetLine
		}
		if startLine < 1 {
			return nil, fmt.Errorf("offset_line must be >= 1 for %s", file.Path)
		}
		limitLines := file.LimitLines
		if limitLines <= 0 {
			limitLines = defaultReadFilesLimitLines
		}
		if limitLines > maxReadFilesLimitLines {
			return nil, fmt.Errorf("limit_lines too large for %s: max %d lines per file", file.Path, maxReadFilesLimitLines)
		}
		totalLines += limitLines
		if totalLines > maxReadFilesTotalLines {
			return nil, fmt.Errorf("requested too many total lines across files: max %d", maxReadFilesTotalLines)
		}
		window, err := p.readTextFileWindow(callCtx, file.repoSelector(), file.Path, startLine, limitLines, "read_files")
		if err != nil {
			return nil, err
		}
		windows = append(windows, window)
	}
	var out strings.Builder
	out.WriteString(fmt.Sprintf("<files count=\"%d\">", len(windows)))
	for _, window := range windows {
		out.WriteString("\n")
		out.WriteString(formatReadFileWindow(window))
	}
	out.WriteString("\n</files>")
	return workspaceToolText(out.String()), nil
}

func (p *workspaceToolPack) writeFile(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	root, err := requireWorkspaceRoot(callCtx, "write_file")
	if err != nil {
		return nil, err
	}
	absPath, err := safeWorkspacePath(root, params.Path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return nil, fmt.Errorf("create directories: %w", err)
	}
	mode := os.FileMode(0644)
	if info, statErr := os.Stat(absPath); statErr == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("path is a directory, not a file: %s", params.Path)
		}
		if err := p.validateFileMutation(callCtx, root, absPath); err != nil {
			return nil, err
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("stat file before write: %w", statErr)
	}
	if err := os.WriteFile(absPath, []byte(params.Content), mode); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}
	if info, statErr := os.Stat(absPath); statErr == nil {
		p.recordFileWrite(callCtx, absPath, info.ModTime(), "write_file")
	}
	return workspaceToolText(fmt.Sprintf("Wrote %d bytes to %s", len(params.Content), params.Path)), nil
}

func (p *workspaceToolPack) editFile(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if strings.TrimSpace(params.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	if params.OldString == "" {
		return nil, fmt.Errorf("old_string is required and must not be empty")
	}
	root, err := requireWorkspaceRoot(callCtx, "edit_file")
	if err != nil {
		return nil, err
	}
	absPath, err := safeWorkspacePath(root, params.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("file does not exist: %s", params.Path)
		}
		return nil, fmt.Errorf("stat file before edit: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("path is a directory, not a file: %s", params.Path)
	}
	if err := p.validateFileMutation(callCtx, root, absPath); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read file for edit: %w", err)
	}
	if isBinaryContent(data) {
		return nil, fmt.Errorf("file appears to be binary, cannot edit: %s", params.Path)
	}
	content := string(data)
	matchCount := strings.Count(content, params.OldString)
	switch {
	case matchCount == 0:
		return nil, fmt.Errorf("old_string did not match any content in %s; re-read the file and include more exact surrounding context", params.Path)
	case matchCount > 1:
		return nil, fmt.Errorf("old_string matched %d locations in %s; include more surrounding context so the match is unique", matchCount, params.Path)
	}
	updated := strings.Replace(content, params.OldString, params.NewString, 1)
	if updated == content {
		return workspaceToolText(fmt.Sprintf("No changes made to %s.", params.Path)), nil
	}
	if err := os.WriteFile(absPath, []byte(updated), info.Mode().Perm()); err != nil {
		return nil, fmt.Errorf("write edited file: %w", err)
	}
	if updatedInfo, statErr := os.Stat(absPath); statErr == nil {
		p.recordFileWrite(callCtx, absPath, updatedInfo.ModTime(), "edit_file")
	}
	return workspaceToolText(fmt.Sprintf("Edited %s by replacing 1 occurrence.", params.Path)), nil
}

func (p *workspaceToolPack) listDirectory(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "list_directory", params.repoSelector())
	if err != nil {
		return nil, err
	}
	dirPath := root
	if params.Path != "" {
		dirPath, err = safeWorkspacePath(root, params.Path)
		if err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("list directory: %w", err)
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return workspaceToolText("Directory is empty."), nil
	}
	return workspaceToolText(strings.Join(lines, "\n")), nil
}

func (p *workspaceToolPack) searchFiles(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Pattern string `json:"pattern"`
		Query   string `json:"query"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "search_files", params.repoSelector())
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(root, params.Pattern))
	if err != nil {
		return nil, fmt.Errorf("glob: %w", err)
	}
	if params.Query == "" {
		results := make([]string, 0, len(matches))
		for _, match := range matches {
			rel, _ := filepath.Rel(root, match)
			results = append(results, rel)
		}
		sort.Strings(results)
		if len(results) > 200 {
			results = append(results[:200], "... (truncated)")
		}
		if len(results) == 0 {
			return workspaceToolText("No matches found."), nil
		}
		return workspaceToolText(strings.Join(results, "\n")), nil
	}
	var results []string
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil || info.IsDir() || info.Size() > 2*1024*1024 {
			continue
		}
		data, err := os.ReadFile(match)
		if err != nil || isBinaryContent(data) {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if strings.Contains(line, params.Query) {
				rel, _ := filepath.Rel(root, match)
				results = append(results, fmt.Sprintf("%s:%d: %s", rel, i+1, line))
				if len(results) >= 100 {
					results = append(results, "... (truncated)")
					return workspaceToolText(strings.Join(results, "\n")), nil
				}
			}
		}
	}
	if len(results) == 0 {
		return workspaceToolText("No matches found."), nil
	}
	return workspaceToolText(strings.Join(results, "\n")), nil
}

func (p *workspaceToolPack) readFileRange(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if params.StartLine < 1 || params.EndLine < 1 {
		return nil, fmt.Errorf("start_line and end_line must be >= 1")
	}
	if params.EndLine < params.StartLine {
		return nil, fmt.Errorf("end_line must be >= start_line")
	}
	if params.EndLine-params.StartLine+1 > 250 {
		return nil, fmt.Errorf("range too large: max 250 lines per call (requested %d)", params.EndLine-params.StartLine+1)
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "read_file_range", params.repoSelector())
	if err != nil {
		return nil, err
	}
	absPath, err := safeWorkspacePath(root, params.Path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	var lines []string
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum > params.EndLine {
			break
		}
		if lineNum >= params.StartLine {
			lines = append(lines, fmt.Sprintf("%4d | %s", lineNum, scanner.Text()))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if info, statErr := os.Stat(absPath); statErr == nil {
		p.recordFileRead(callCtx, absPath, info.ModTime(), "read_file_range")
	}
	if len(lines) == 0 {
		return workspaceToolText(fmt.Sprintf("No lines in range %d-%d (file has %d lines)", params.StartLine, params.EndLine, lineNum)), nil
	}
	return workspaceToolText(strings.Join(lines, "\n")), nil
}

func (p *workspaceToolPack) ripgrep(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		FileType        string `json:"file_type"`
		ContextLines    int    `json:"context_lines"`
		MaxResults      int    `json:"max_results"`
		CaseInsensitive bool   `json:"case_insensitive"`
		FixedStrings    bool   `json:"fixed_strings"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if params.Pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "ripgrep", params.repoSelector())
	if err != nil {
		return nil, err
	}
	if params.MaxResults <= 0 {
		params.MaxResults = 50
	}
	if params.MaxResults > 200 {
		params.MaxResults = 200
	}
	if params.ContextLines < 0 {
		params.ContextLines = 0
	}
	if params.ContextLines > 5 {
		params.ContextLines = 5
	}
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		return nil, fmt.Errorf("ripgrep (rg) not found in PATH; use the 'grep' tool as a fallback")
	}
	searchDir := root
	if params.Path != "" {
		searchDir, err = safeWorkspacePath(root, params.Path)
		if err != nil {
			return nil, err
		}
	}
	args := []string{"--no-heading", "--line-number", "--color", "never", "--max-columns", "500", "--max-columns-preview", "--glob", "!.git", "--glob", "!node_modules", "--glob", "!vendor", "--glob", "!dist", "--glob", "!__pycache__"}
	if params.FileType != "" {
		args = append(args, "--type", params.FileType)
	}
	if params.ContextLines > 0 {
		args = append(args, "-C", strconv.Itoa(params.ContextLines))
	}
	if params.CaseInsensitive {
		args = append(args, "-i")
	}
	if params.FixedStrings {
		args = append(args, "-F")
	}
	args = append(args, "--", params.Pattern, searchDir)
	timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(timeout, rgPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return workspaceToolText("No matches found."), nil
		}
		if timeout.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("ripgrep timed out after 30s")
		}
		return nil, fmt.Errorf("ripgrep error: %w", err)
	}
	result := strings.ReplaceAll(string(out), root+string(os.PathSeparator), "")
	outputLines := strings.Split(strings.TrimRight(result, "\n"), "\n")
	if len(outputLines) == 1 && outputLines[0] == "" {
		return workspaceToolText("No matches found."), nil
	}
	if len(outputLines) > params.MaxResults {
		outputLines = append(outputLines[:params.MaxResults], fmt.Sprintf("... (%d+ results, truncated)", params.MaxResults))
	}
	return workspaceToolText(strings.Join(outputLines, "\n")), nil
}

var excludedWorkspaceDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true, "__pycache__": true}

func (p *workspaceToolPack) grep(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Include    string `json:"include"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if params.Pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "grep", params.repoSelector())
	if err != nil {
		return nil, err
	}
	if params.MaxResults <= 0 {
		params.MaxResults = 50
	}
	re, err := regexp.Compile(params.Pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}
	searchDir := root
	if params.Path != "" {
		searchDir, err = safeWorkspacePath(root, params.Path)
		if err != nil {
			return nil, err
		}
	}
	var results []string
	limitReached := false
	walkErr := filepath.WalkDir(searchDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if excludedWorkspaceDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if params.Include != "" {
			matched, _ := filepath.Match(params.Include, d.Name())
			if !matched {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > 2*1024*1024 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || isBinaryContent(data) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				results = append(results, fmt.Sprintf("%s:%d: %s", rel, i+1, line))
				if len(results) >= params.MaxResults {
					limitReached = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("search error: %w", walkErr)
	}
	if len(results) == 0 {
		return workspaceToolText("No matches found."), nil
	}
	if limitReached {
		results = append(results, fmt.Sprintf("... (truncated at %d results)", params.MaxResults))
	}
	return workspaceToolText(strings.Join(results, "\n")), nil
}

type symbolPattern struct {
	exts     []string
	patterns []*regexp.Regexp
}

var workspaceSymbolPatterns = []symbolPattern{
	{exts: []string{".go"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^func\s`), regexp.MustCompile(`^type\s+\w+\s+(struct|interface)`), regexp.MustCompile(`^type\s+\w+\s`), regexp.MustCompile(`^var\s+\w+`), regexp.MustCompile(`^const\s+\w+`)}},
	{exts: []string{".ts", ".tsx"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^export\s+(function|const|class|type|interface|enum)\s`), regexp.MustCompile(`^\s*(public|private|protected|async)\s+\w+\(`), regexp.MustCompile(`^function\s+\w+`)}},
	{exts: []string{".js", ".jsx"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^export\s+(function|const|class)\s`), regexp.MustCompile(`^function\s+\w+`), regexp.MustCompile(`^class\s+\w+`), regexp.MustCompile(`module\.exports`)}},
	{exts: []string{".py"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^def\s+\w+`), regexp.MustCompile(`^class\s+\w+`), regexp.MustCompile(`^async\s+def\s+\w+`)}},
	{exts: []string{".rs"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^pub\s+(fn|struct|enum|trait|type|impl|mod)\s`), regexp.MustCompile(`^fn\s+`), regexp.MustCompile(`^struct\s+`), regexp.MustCompile(`^enum\s+`), regexp.MustCompile(`^trait\s+`), regexp.MustCompile(`^impl\s`)}},
	{exts: []string{".java"}, patterns: []*regexp.Regexp{regexp.MustCompile(`(public|private|protected).*\s+(class|interface|enum)\s+`), regexp.MustCompile(`(public|private|protected)\s+.*\w+\s*\([^)]*\)\s*\{`)}},
}

func (p *workspaceToolPack) listSymbols(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "list_symbols", params.repoSelector())
	if err != nil {
		return nil, err
	}
	absPath, err := safeWorkspacePath(root, params.Path)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(absPath))
	var patterns []*regexp.Regexp
	for _, sp := range workspaceSymbolPatterns {
		for _, e := range sp.exts {
			if e == ext {
				patterns = sp.patterns
				break
			}
		}
		if patterns != nil {
			break
		}
	}
	if patterns == nil {
		return nil, fmt.Errorf("unsupported file type: %s (supported: .go, .ts, .tsx, .js, .jsx, .py, .rs, .java)", ext)
	}
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	var symbols []string
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		for _, pattern := range patterns {
			if pattern.MatchString(line) {
				symbols = append(symbols, fmt.Sprintf("%4d | %s", lineNum, line))
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(symbols) == 0 {
		return workspaceToolText("No symbols found."), nil
	}
	return workspaceToolText(strings.Join(symbols, "\n")), nil
}

type workspaceRepoSelector struct {
	Repository string `json:"repository"`
	RepoAlias  string `json:"repo_alias"`
}

func (s workspaceRepoSelector) repoSelector() string {
	return firstNonEmptyString(s.RepoAlias, s.Repository)
}

func requireWorkspaceRoot(callCtx CallContext, toolName string) (string, error) {
	return requireWorkspaceRootForRepository(callCtx, toolName, "")
}

func requireWorkspaceRootForRepository(callCtx CallContext, toolName, repoSelector string) (string, error) {
	if callCtx.Run == nil || callCtx.Run.WorkspaceLease == nil || strings.TrimSpace(callCtx.Run.WorkspaceLease.RootPath) == "" {
		return "", fmt.Errorf("%s requires a workspace lease with a root path for this run", toolName)
	}
	rootPath := strings.TrimSpace(callCtx.Run.WorkspaceLease.RootPath)
	if selector := normalizeRepositorySelector(repoSelector); selector != "" {
		selectedRoot, ok := repositoryRootForSelector(callCtx.Run.WorkspaceLease, selector)
		if !ok {
			return "", fmt.Errorf("%s repository %q is not checked out for this run", toolName, strings.TrimSpace(repoSelector))
		}
		rootPath = selectedRoot
	}
	root, err := filepath.Abs(rootPath)
	if err != nil {
		return "", fmt.Errorf("%s workspace path is invalid: %w", toolName, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("%s workspace is unavailable: %w", toolName, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s workspace path is not a directory", toolName)
	}
	return root, nil
}

func normalizeRepositorySelector(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.Trim(value, "/")
	return value
}

func repositoryRootForSelector(lease *agentcore.WorkspaceLease, selector string) (string, bool) {
	if lease == nil {
		return "", false
	}
	if repositoryLeaseMatchesSelector(lease, selector) {
		return strings.TrimSpace(lease.RootPath), true
	}
	for key, raw := range repositoryWorkspaceEntries(lease) {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if normalizeRepositorySelector(key) == selector || repositoryWorkspaceEntryMatchesSelector(entry, selector) {
			root := stringFromAnyMap(entry, "root_path")
			if root != "" {
				return root, true
			}
		}
	}
	return "", false
}

func repositoryLeaseMatchesSelector(lease *agentcore.WorkspaceLease, selector string) bool {
	if lease == nil {
		return false
	}
	meta := lease.Metadata
	for _, key := range []string{"repo_alias", "repository_id", "repo_full_name", "clone_url"} {
		if normalizeRepositorySelector(stringFromAnyMap(meta, key)) == selector {
			return true
		}
	}
	return false
}

func repositoryWorkspaceEntryMatchesSelector(entry map[string]interface{}, selector string) bool {
	for _, key := range []string{"alias", "repo_alias", "repository_id", "repo_full_name", "clone_url"} {
		if normalizeRepositorySelector(stringFromAnyMap(entry, key)) == selector {
			return true
		}
	}
	if metadata, ok := entry["metadata"].(map[string]interface{}); ok {
		for _, key := range []string{"repo_alias", "repository_id", "repo_full_name", "clone_url"} {
			if normalizeRepositorySelector(stringFromAnyMap(metadata, key)) == selector {
				return true
			}
		}
	}
	return false
}

func repositoryWorkspaceEntries(lease *agentcore.WorkspaceLease) map[string]interface{} {
	if lease == nil || lease.Metadata == nil {
		return nil
	}
	raw, ok := lease.Metadata["repository_workspaces"]
	if !ok {
		return nil
	}
	entries, _ := raw.(map[string]interface{})
	return entries
}

func safeWorkspacePath(root, relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", relPath)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(filepath.Join(root, filepath.Clean(relPath)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path traversal not allowed: %s", relPath)
	}
	return absPath, nil
}

func (p *workspaceToolPack) recordFileRead(callCtx CallContext, absPath string, modTime time.Time, via string) {
	state := p.fileState(callCtx)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.reads[absPath] = workspaceToolFileObservation{LastReadAt: time.Now().UTC(), LastReadModTime: modTime.UTC(), ReadVia: via}
}

func (p *workspaceToolPack) recordFileWrite(callCtx CallContext, absPath string, modTime time.Time, via string) {
	state := p.fileState(callCtx)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	obs := state.reads[absPath]
	obs.LastReadAt = time.Now().UTC()
	obs.LastReadModTime = modTime.UTC()
	if obs.ReadVia == "" {
		obs.ReadVia = via
	}
	state.reads[absPath] = obs
}

func (p *workspaceToolPack) validateFileMutation(callCtx CallContext, root, absPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat file before edit: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory, not a file: %s", relativeWorkspaceToolPath(root, absPath))
	}
	state := p.fileState(callCtx)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	obs, ok := state.reads[absPath]
	state.mu.Unlock()
	if !ok {
		return fmt.Errorf("must read %s before modifying it; use read_file or read_file_range first", relativeWorkspaceToolPath(root, absPath))
	}
	currentModTime := info.ModTime().UTC()
	if currentModTime.After(obs.LastReadModTime) {
		return fmt.Errorf(
			"refusing to modify %s because it changed since the last %s call (last seen mod time %s, current mod time %s); re-read the file and try again",
			relativeWorkspaceToolPath(root, absPath),
			obs.ReadVia,
			obs.LastReadModTime.Format(time.RFC3339Nano),
			currentModTime.Format(time.RFC3339Nano),
		)
	}
	return nil
}

func relativeWorkspaceToolPath(root, absPath string) string {
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return absPath
	}
	return rel
}

func isBinaryContent(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return true
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	ct := http.DetectContentType(sniff)
	return !strings.HasPrefix(ct, "text/") && ct != "application/json" && ct != "application/xml"
}

func workspaceToolText(text string) json.RawMessage {
	payload, _ := json.Marshal(text)
	return payload
}
