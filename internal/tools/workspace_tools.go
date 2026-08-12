package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	r.RegisterRunCloser(pack)
	for _, item := range []struct {
		def     Definition
		handler Handler
	}{
		{workspaceToolDefinition("read_file", "Read a bounded window of numbered text lines at the given path (relative to the workspace root). Truncated results include the exact offset_line to continue. When you want a named declaration, prefer read_symbol, which resolves its exact line range for you. Otherwise use ripgrep/search_files/list_symbols first, then read the exact section you need.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":        map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
				"repo_alias":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"offset_line": map[string]interface{}{"type": "integer", "minimum": 1, "description": "Optional 1-based line number to start reading from. Defaults to 1."},
				"limit_lines": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": maxReadFileLimitLines, "description": "Optional maximum number of lines to return. Defaults to 120, max 240."},
				"offset":      map[string]interface{}{"type": "integer", "minimum": 0, "description": "Deprecated 0-based line offset."},
				"limit":       map[string]interface{}{"type": "integer", "minimum": 1, "maximum": maxReadFileLimitLines, "description": "Deprecated maximum line count."},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		}), pack.readFile},
		{workspaceToolDefinition("read_files", "Read small bounded windows from a few specific text files in one call. Prefer ripgrep/search_files plus read_file_range first; use this only when you already know the exact files and need small excerpts.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"files": map[string]interface{}{
					"type":        "array",
					"description": "Files to read. Max 4 files per call.",
					"minItems":    1,
					"maxItems":    maxReadFilesPerCall,
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"path":        map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
							"repo_alias":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
							"repository":  map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
							"offset_line": map[string]interface{}{"type": "integer", "minimum": 1, "description": "Optional 1-based line number to start reading from. Defaults to 1."},
							"limit_lines": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": maxReadFilesLimitLines, "description": "Optional maximum number of lines to return for this file. Defaults to 60, max 120."},
						},
						"required":             []string{"path"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"files"},
			"additionalProperties": false,
		}), pack.readFiles},
		{workspaceToolDefinition("write_file", "Write content to a file at the given path (relative to the workspace root). Use this for new files or full rewrites only after reading the complete current file; use edit_file/apply_patch after a partial read. Creates directories as needed.", true, map[string]interface{}{
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
		{workspaceToolDefinition("read_file_range", "Read a bounded, numbered line range from a file. Use this when you know the line numbers, for example from a ripgrep hit or a stack trace; use read_symbol instead when you know a declaration's name. Truncated results include the exact continuation line. The range may span at most 240 lines.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
				"start_line": map[string]interface{}{"type": "integer", "minimum": 1, "description": "First line number to read, 1-based"},
				"end_line":   map[string]interface{}{"type": "integer", "minimum": 1, "description": "Last line number to read, 1-based inclusive. end_line - start_line must be under 240."},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required":             []string{"path", "start_line", "end_line"},
			"additionalProperties": false,
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
		{workspaceToolDefinition("list_symbols", "Outline a source file: every function, method, type, and class declaration with its name, kind, and exact start-end line range. Use this to find a declaration, then read_symbol to read it.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		}), pack.listSymbols},
		{workspaceToolDefinition("read_symbol", "Read one named declaration (function, method, type, or class) in full, by name. Preferred over read_file whenever you know the declaration's name: it resolves the exact line range for you instead of making you guess an offset. Supports .go, .ts, .tsx, .js, .jsx, .mjs, .cjs, .py, .pyi, .rs, .java.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":              map[string]interface{}{"type": "string", "description": "File path relative to the workspace root"},
				"symbol":            map[string]interface{}{"type": "string", "description": "Declaration name, e.g. a function, method, type, or class name. Match is case-sensitive first."},
				"repo_alias":        map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository":        map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"include_docstring": map[string]interface{}{"type": "boolean", "description": "Include the comment block immediately above the declaration. Defaults to true."},
			},
			"required":             []string{"path", "symbol"},
			"additionalProperties": false,
		}), pack.readSymbol},
		{workspaceToolDefinition("find_symbol", "Find where a function, method, type, or class is DECLARED across the whole workspace, without knowing its file. Returns each declaration's path, kind, and exact line range. Use this instead of ripgrep when you are looking for a definition rather than every mention; then read_symbol to read one.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":       map[string]interface{}{"type": "string", "description": "Declaration name to find. Matched case-sensitively first."},
				"kind":       map[string]interface{}{"type": "string", "description": "Optional kind filter: function, method, type, class, interface, constructor, constant, variable, module.", "enum": []string{"function", "method", "type", "class", "interface", "constructor", "constant", "variable", "module"}},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		}), pack.findSymbol},
		{workspaceToolDefinition("find_callers", "Find where a function or method is CALLED across the workspace, with the enclosing function for each call site. Use before changing or deleting a declaration to see what depends on it. Matching is by name, not by type.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"symbol":     map[string]interface{}{"type": "string", "description": "Function or method name whose call sites you want."},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required":             []string{"symbol"},
			"additionalProperties": false,
		}), pack.findCallers},
		{workspaceToolDefinition("find_callees", "List the symbols a given function or method calls. Use to understand what a declaration depends on before reading it in full. Matching is by name, not by type.", false, map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"symbol":     map[string]interface{}{"type": "string", "description": "Function or method name to inspect."},
				"path":       map[string]interface{}{"type": "string", "description": "Optional file path relative to the workspace root. Omit to search the workspace for the declaration."},
				"repo_alias": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
				"repository": map[string]interface{}{"type": "string", "description": "Optional repository alias/full name/id when multiple repositories are checked out."},
			},
			"required":             []string{"symbol"},
			"additionalProperties": false,
		}), pack.findCallees},
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
	LastReadAt     time.Time
	FileInfo       os.FileInfo
	ContentSHA256  string
	Complete       bool
	RawComplete    bool
	SeenRanges     []workspaceReadLineRange
	BoundarySHA256 map[int]string
	TotalLines     int
	TotalKnown     bool
	ReadVia        string
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
	appID, runID := callCtx.AppID, callCtx.RunID
	if strings.TrimSpace(appID) == "" && callCtx.Run != nil {
		appID = callCtx.Run.AppID
	}
	if strings.TrimSpace(runID) == "" && callCtx.Run != nil {
		runID = callCtx.Run.ID
	}
	key := workspaceToolStateKey(appID, runID)
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.states[key]
	if state == nil {
		state = newWorkspaceToolFileState()
		p.states[key] = state
	}
	return state
}

func (p *workspaceToolPack) CloseRun(_ context.Context, appID, runID string) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	delete(p.states, workspaceToolStateKey(appID, runID))
	p.mu.Unlock()
	return nil
}

func workspaceToolStateKey(appID, runID string) string {
	return strings.TrimSpace(appID) + "/" + strings.TrimSpace(runID)
}

func (p *workspaceToolPack) readFile(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Path       string `json:"path"`
		OffsetLine *int   `json:"offset_line"`
		LimitLines *int   `json:"limit_lines"`
		Offset     *int   `json:"offset"`
		Limit      *int   `json:"limit"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	startLine, limitLines, err := normalizeReadFileWindow(params.OffsetLine, params.LimitLines, params.Offset, params.Limit)
	if err != nil {
		return nil, err
	}
	window, err := p.readTextFileWindow(ctx, callCtx, params.repoSelector(), params.Path, startLine, limitLines, "read_file")
	if err != nil {
		return nil, err
	}
	output := workspaceToolText(formatReadFileWindow(window))
	p.recordFileReads(callCtx, window)
	return output, nil
}

func (p *workspaceToolPack) readFiles(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Files []struct {
			workspaceRepoSelector
			Path       string `json:"path"`
			OffsetLine *int   `json:"offset_line"`
			LimitLines *int   `json:"limit_lines"`
		} `json:"files"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
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
		if err := contextReadError(ctx); err != nil {
			return nil, err
		}
		if strings.TrimSpace(file.Path) == "" {
			return nil, fmt.Errorf("each file entry must include path")
		}
		startLine := 1
		if file.OffsetLine != nil {
			startLine = *file.OffsetLine
		}
		if startLine < 1 {
			return nil, fmt.Errorf("offset_line must be >= 1 for %s", file.Path)
		}
		limitLines := defaultReadFilesLimitLines
		if file.LimitLines != nil {
			limitLines = *file.LimitLines
			if limitLines < 1 {
				return nil, fmt.Errorf("limit_lines must be >= 1 for %s", file.Path)
			}
		}
		if limitLines > maxReadFilesLimitLines {
			return nil, fmt.Errorf("limit_lines too large for %s: max %d lines per file", file.Path, maxReadFilesLimitLines)
		}
		totalLines += limitLines
		if totalLines > maxReadFilesTotalLines {
			return nil, fmt.Errorf("requested too many total lines across files: max %d", maxReadFilesTotalLines)
		}
		window, err := p.readTextFileWindow(ctx, callCtx, file.repoSelector(), file.Path, startLine, limitLines, "read_files")
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
	output := workspaceToolText(out.String())
	p.recordFileReads(callCtx, windows...)
	return output, nil
}

func (p *workspaceToolPack) writeFile(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
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
		if err := p.validateFileMutation(ctx, callCtx, root, absPath, true); err != nil {
			return nil, err
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("stat file before write: %w", statErr)
	}
	if err := os.WriteFile(absPath, []byte(params.Content), mode); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}
	p.recordFileWrite(ctx, callCtx, absPath, "write_file")
	return workspaceToolText(fmt.Sprintf("Wrote %d bytes to %s", len(params.Content), params.Path)), nil
}

func (p *workspaceToolPack) editFile(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
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
	if err := p.validateFileMutation(ctx, callCtx, root, absPath, false); err != nil {
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
	p.recordFileWrite(ctx, callCtx, absPath, "edit_file")
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

func (p *workspaceToolPack) readFileRange(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	if params.StartLine < 1 || params.EndLine < 1 {
		return nil, fmt.Errorf("start_line and end_line must be >= 1")
	}
	if params.EndLine < params.StartLine {
		return nil, fmt.Errorf("end_line must be >= start_line")
	}
	// Share read_file's ceiling rather than carrying a separate literal: this
	// was the one read path with its own bound, so a request of 241-250 lines
	// succeeded here and failed on read_file for no reason a caller could see.
	if params.EndLine-params.StartLine+1 > maxReadFileLimitLines {
		return nil, fmt.Errorf(
			"range too large: max %d lines per call (requested %d)",
			maxReadFileLimitLines,
			params.EndLine-params.StartLine+1,
		)
	}
	window, err := p.readTextFileWindow(
		ctx,
		callCtx,
		params.repoSelector(),
		params.Path,
		params.StartLine,
		params.EndLine-params.StartLine+1,
		"read_file_range",
	)
	if err != nil {
		return nil, err
	}
	output := workspaceToolText(formatReadFileWindow(window))
	p.recordFileReads(callCtx, window)
	return output, nil
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
	root, _, absPath, err := cleanWorkspacePath(root, relPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return "", err
	}
	current := root
	if rel != "." {
		for _, component := range strings.Split(rel, string(os.PathSeparator)) {
			current = filepath.Join(current, component)
			info, lstatErr := os.Lstat(current)
			if os.IsNotExist(lstatErr) {
				break
			}
			if lstatErr != nil {
				return "", fmt.Errorf("inspect workspace path %s: %w", relPath, lstatErr)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("workspace mutation path must not contain symlinks: %s", relPath)
			}
		}
	}
	return absPath, nil
}

func cleanWorkspacePath(root, relPath string) (string, string, string, error) {
	displayPath := displayReadPath(relPath, maxReadDisplayedPathRunes)
	if strings.ContainsRune(relPath, 0) {
		return "", "", "", fmt.Errorf("path contains a NUL byte")
	}
	if filepath.IsAbs(relPath) {
		return "", "", "", fmt.Errorf("absolute paths are not allowed: %s", displayPath)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve workspace root: %w", err)
	}
	cleanPath := filepath.Clean(relPath)
	absPath, err := filepath.Abs(filepath.Join(root, cleanPath))
	if err != nil {
		return "", "", "", err
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return "", "", "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", "", fmt.Errorf("path traversal not allowed: %s", displayPath)
	}
	return root, filepath.ToSlash(rel), absPath, nil
}

func (p *workspaceToolPack) recordFileReads(callCtx CallContext, windows ...*readFileWindow) {
	state := p.fileState(callCtx)
	if state == nil || len(windows) == 0 {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, window := range windows {
		if window == nil || window.FileInfo == nil || strings.TrimSpace(window.AbsPath) == "" {
			continue
		}
		candidate := workspaceToolFileObservation{
			LastReadAt:     time.Now().UTC(),
			FileInfo:       window.FileInfo,
			ContentSHA256:  window.ContentSHA256,
			RawComplete:    window.RawComplete,
			SeenRanges:     append([]workspaceReadLineRange(nil), window.SeenRanges...),
			BoundarySHA256: cloneReadBoundaryHashes(window.BoundarySHA256),
			TotalLines:     window.TotalLines,
			TotalKnown:     window.TotalLinesKnown,
			ReadVia:        window.Via,
		}
		state.reads[window.AbsPath] = mergeFileObservation(state.reads[window.AbsPath], candidate)
	}
}

func mergeFileObservation(existing, candidate workspaceToolFileObservation) workspaceToolFileObservation {
	if existing.FileInfo == nil || !sameWorkspaceFileVersion(existing.FileInfo, candidate.FileInfo) {
		candidate.Complete = observationIsComplete(candidate)
		return candidate
	}
	if existing.ContentSHA256 != "" && candidate.ContentSHA256 != "" && existing.ContentSHA256 != candidate.ContentSHA256 {
		candidate.Complete = observationIsComplete(candidate)
		return candidate
	}
	compatible, boundaryCompared := readObservationBoundariesMatch(existing.BoundarySHA256, candidate.BoundarySHA256)
	if !compatible {
		candidate.Complete = observationIsComplete(candidate)
		return candidate
	}
	if !boundaryCompared && existing.Complete && candidate.ContentSHA256 == "" {
		return existing
	}
	if !boundaryCompared && len(existing.SeenRanges) > 0 && len(candidate.SeenRanges) > 0 {
		candidate.Complete = observationIsComplete(candidate)
		return candidate
	}
	merged := existing
	merged.LastReadAt = candidate.LastReadAt
	merged.FileInfo = candidate.FileInfo
	if candidate.ContentSHA256 != "" {
		merged.ContentSHA256 = candidate.ContentSHA256
	}
	merged.RawComplete = existing.RawComplete || candidate.RawComplete
	merged.SeenRanges = mergeReadLineRanges(existing.SeenRanges, candidate.SeenRanges)
	merged.BoundarySHA256 = mergeReadBoundaryHashes(existing.BoundarySHA256, candidate.BoundarySHA256)
	if candidate.TotalKnown {
		merged.TotalKnown = true
		merged.TotalLines = candidate.TotalLines
	}
	if candidate.ReadVia != "" {
		merged.ReadVia = candidate.ReadVia
	}
	merged.Complete = observationIsComplete(merged)
	return merged
}

func cloneReadBoundaryHashes(input map[int]string) map[int]string {
	if len(input) == 0 {
		return nil
	}
	cloned := make(map[int]string, len(input))
	for line, digest := range input {
		cloned[line] = digest
	}
	return cloned
}

func readObservationBoundariesMatch(left, right map[int]string) (compatible, compared bool) {
	for line, leftDigest := range left {
		rightDigest, ok := right[line]
		if !ok {
			continue
		}
		compared = true
		if leftDigest != rightDigest {
			return false, true
		}
	}
	return true, compared
}

func mergeReadBoundaryHashes(left, right map[int]string) map[int]string {
	merged := cloneReadBoundaryHashes(left)
	if merged == nil && len(right) > 0 {
		merged = make(map[int]string, len(right))
	}
	for line, digest := range right {
		merged[line] = digest
	}
	return merged
}

func mergeReadLineRanges(left, right []workspaceReadLineRange) []workspaceReadLineRange {
	all := make([]workspaceReadLineRange, 0, len(left)+len(right))
	all = append(all, left...)
	all = append(all, right...)
	if len(all) == 0 {
		return nil
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Start == all[j].Start {
			return all[i].End < all[j].End
		}
		return all[i].Start < all[j].Start
	})
	merged := make([]workspaceReadLineRange, 0, len(all))
	for _, item := range all {
		merged = appendReadLineRange(merged, item.Start, item.End)
	}
	return merged
}

func observationIsComplete(observation workspaceToolFileObservation) bool {
	if observation.ContentSHA256 == "" {
		return false
	}
	if observation.RawComplete {
		return true
	}
	return observation.TotalKnown && observation.TotalLines > 0 && len(observation.SeenRanges) == 1 &&
		observation.SeenRanges[0].Start == 1 && observation.SeenRanges[0].End >= observation.TotalLines
}

func (p *workspaceToolPack) recordFileWrite(ctx context.Context, callCtx CallContext, absPath, via string) {
	state := p.fileState(callCtx)
	if state == nil {
		return
	}
	info, err := os.Lstat(absPath)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	state.mu.Lock()
	obs := state.reads[absPath]
	complete := obs.Complete
	if obs.FileInfo == nil {
		complete = true
	}
	state.mu.Unlock()
	contentHash := ""
	if complete {
		var hashedInfo os.FileInfo
		contentHash, hashedInfo, err = hashWorkspaceFile(ctx, absPath, info)
		if err != nil {
			complete = false
			contentHash = ""
		} else {
			info = hashedInfo
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	obs.LastReadAt = time.Now().UTC()
	obs.FileInfo = info
	obs.ContentSHA256 = contentHash
	obs.Complete = complete
	obs.RawComplete = complete
	if complete {
		obs.SeenRanges = nil
		obs.BoundarySHA256 = nil
		obs.TotalKnown = false
		obs.TotalLines = 0
	}
	if obs.ReadVia == "" {
		obs.ReadVia = via
	}
	state.reads[absPath] = obs
}

func (p *workspaceToolPack) validateFileMutation(
	ctx context.Context,
	callCtx CallContext,
	root,
	absPath string,
	requireComplete bool,
) error {
	info, err := os.Lstat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat file before edit: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("path is not a regular file: %s", relativeWorkspaceToolPath(root, absPath))
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
	if requireComplete && !obs.Complete {
		return fmt.Errorf(
			"only part of %s has been read; continue from the reported offset_line before full replacement or deletion, or use edit_file/apply_patch for a targeted change",
			relativeWorkspaceToolPath(root, absPath),
		)
	}
	if !sameWorkspaceFileVersion(obs.FileInfo, info) {
		return fmt.Errorf(
			"refusing to modify %s because it changed since the last %s call; re-read the file and try again",
			relativeWorkspaceToolPath(root, absPath),
			obs.ReadVia,
		)
	}
	if requireComplete && obs.ContentSHA256 != "" {
		currentHash, currentInfo, hashErr := hashWorkspaceFile(ctx, absPath, info)
		if hashErr != nil {
			return fmt.Errorf("fingerprint %s before modification: %w", relativeWorkspaceToolPath(root, absPath), hashErr)
		}
		if currentHash != obs.ContentSHA256 || !sameWorkspaceFileVersion(info, currentInfo) {
			return fmt.Errorf(
				"refusing to modify %s because its content changed since the last %s call; re-read the file and try again",
				relativeWorkspaceToolPath(root, absPath),
				obs.ReadVia,
			)
		}
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
