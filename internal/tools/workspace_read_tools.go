package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	maxReadFileContentRunes       = 2100
	maxReadFilesTotalContentRunes = maxReadFileContentRunes
	maxReadFileLineRunes          = 1000
	maxReadLineCaptureBytes       = maxReadFileLineRunes*utf8.UTFMax + utf8.UTFMax
	maxReadDisplayedPathRunes     = 160
	readStreamBufferBytes         = 32 * 1024
	readLineTruncationMarker      = " ... [line truncated]"
)

type workspaceReadLineRange struct {
	Start int
	End   int
}

type readFileWindow struct {
	Path              string
	Via               string
	StartLine         int
	Lines             []string
	HasMore           bool
	NextOffsetLine    int
	TotalLines        int
	TotalLinesKnown   bool
	AnyLineClamped    bool
	TruncatedByBudget bool
	AbsPath           string
	FileInfo          os.FileInfo
	ContentSHA256     string
	RawComplete       bool
	SeenRanges        []workspaceReadLineRange
	BoundarySHA256    map[int]string
}

func normalizeReadFileWindow(offsetLine, limitLines, offset, limit *int) (int, int, error) {
	if offsetLine != nil && offset != nil {
		return 0, 0, fmt.Errorf("use offset_line or deprecated offset, not both")
	}
	if limitLines != nil && limit != nil {
		return 0, 0, fmt.Errorf("use limit_lines or deprecated limit, not both")
	}

	startLine := 1
	switch {
	case offsetLine != nil:
		if *offsetLine < 1 {
			return 0, 0, fmt.Errorf("offset_line must be >= 1")
		}
		startLine = *offsetLine
	case offset != nil:
		if *offset < 0 {
			return 0, 0, fmt.Errorf("offset must be >= 0")
		}
		if *offset == int(^uint(0)>>1) {
			return 0, 0, fmt.Errorf("offset is too large")
		}
		startLine = *offset + 1
	}

	lineLimit := defaultReadFileLimitLines
	switch {
	case limitLines != nil:
		lineLimit = *limitLines
	case limit != nil:
		lineLimit = *limit
	}
	if lineLimit < 1 {
		return 0, 0, fmt.Errorf("limit_lines must be >= 1")
	}
	if lineLimit > maxReadFileLimitLines {
		return 0, 0, fmt.Errorf(
			"limit_lines too large: max %d lines per call (requested %d)",
			maxReadFileLimitLines,
			lineLimit,
		)
	}
	return startLine, lineLimit, nil
}

func (p *workspaceToolPack) readTextFileWindow(
	ctx context.Context,
	callCtx CallContext,
	repoSelector,
	path string,
	startLine,
	limitLines int,
	via string,
	contentBudget int,
) (*readFileWindow, error) {
	if err := contextReadError(ctx); err != nil {
		return nil, err
	}
	root, err := requireWorkspaceRootForRepository(callCtx, via, repoSelector)
	if err != nil {
		return nil, err
	}
	f, absPath, err := openWorkspaceReadFile(root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	beforeInfo, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened file: %w", err)
	}
	if !beforeInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file: %s", displayReadPath(path, maxReadDisplayedPathRunes))
	}

	preview := make([]byte, 512)
	n, readErr := f.Read(preview)
	if readErr != nil && readErr != io.EOF {
		return nil, fmt.Errorf("read file preview: %w", readErr)
	}
	if isBinaryContent(preview[:n]) {
		return nil, fmt.Errorf("file appears to be binary, cannot read: %s", displayReadPath(path, maxReadDisplayedPathRunes))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("reset file cursor: %w", err)
	}

	window := &readFileWindow{Path: path, Via: via, StartLine: startLine, AbsPath: absPath}
	hasher := sha256.New()
	reader := bufio.NewReaderSize(io.TeeReader(f, hasher), readStreamBufferBytes)
	currentLine := 0
	reachedEOF := false
	window.BoundarySHA256 = make(map[int]string, 2)
	for currentLine < startLine-1 {
		_, exists, lineEOF, _, err := readNextBoundedLine(ctx, reader, false)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
		if !exists {
			reachedEOF = true
			break
		}
		currentLine++
		if lineEOF {
			reachedEOF = true
			break
		}
	}
	lastDisplayedDigest := hashDigestString(hasher)
	window.BoundarySHA256[currentLine] = lastDisplayedDigest

	if contentBudget <= 0 {
		return nil, fmt.Errorf("content budget must be positive")
	}
	contentRunes := 0
	for !reachedEOF && len(window.Lines) < limitLines {
		line, exists, lineEOF, clamped, err := readNextBoundedLine(ctx, reader, true)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
		if !exists {
			reachedEOF = true
			break
		}
		lineNumber := currentLine + 1
		if lineNumber == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		line, budgetClamped := clampReadLineToBudget(lineNumber, line, contentBudget)
		clamped = clamped || budgetClamped
		lineRunes := readRenderedLineRunes(lineNumber, line)
		if len(window.Lines) > 0 && contentRunes+lineRunes > contentBudget {
			window.HasMore = true
			window.NextOffsetLine = lineNumber
			window.TruncatedByBudget = true
			break
		}
		currentLine = lineNumber
		window.Lines = append(window.Lines, line)
		window.SeenRanges = appendReadLineRange(window.SeenRanges, lineNumber, lineNumber)
		lastDisplayedDigest = hashDigestString(hasher)
		window.AnyLineClamped = window.AnyLineClamped || clamped
		contentRunes += lineRunes
		if lineEOF {
			reachedEOF = true
		}
	}
	window.BoundarySHA256[currentLine] = lastDisplayedDigest

	if !window.HasMore && !reachedEOF && len(window.Lines) >= limitLines {
		exists, err := readStreamHasMore(ctx, reader)
		if err != nil {
			return nil, fmt.Errorf("read file lookahead: %w", err)
		}
		if exists {
			window.HasMore = true
			window.NextOffsetLine = currentLine + 1
		} else {
			reachedEOF = true
		}
	}
	if reachedEOF {
		window.TotalLines = currentLine
		window.TotalLinesKnown = true
	}
	window.RawComplete = startLine == 1 && reachedEOF

	afterInfo, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect file after read: %w", err)
	}
	if !sameWorkspaceFileVersion(beforeInfo, afterInfo) {
		return nil, fmt.Errorf("file changed while it was being read: %s; retry the read", displayReadPath(path, maxReadDisplayedPathRunes))
	}
	if reachedEOF {
		window.ContentSHA256 = hashDigestString(hasher)
	}
	window.FileInfo = afterInfo
	return window, nil
}

func readStreamHasMore(ctx context.Context, reader *bufio.Reader) (bool, error) {
	if err := contextReadError(ctx); err != nil {
		return false, err
	}
	_, err := reader.Peek(1)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, io.EOF):
		return false, nil
	default:
		return false, err
	}
}

func hashDigestString(hasher interface{ Sum([]byte) []byte }) string {
	if hasher == nil {
		return ""
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func readNextBoundedLine(ctx context.Context, reader *bufio.Reader, capture bool) (string, bool, bool, bool, error) {
	if reader == nil {
		return "", false, false, false, fmt.Errorf("reader is not configured")
	}
	var prefix []byte
	sawData := false
	clamped := false
	for {
		if err := contextReadError(ctx); err != nil {
			return "", false, false, false, err
		}
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			sawData = true
			content := fragment
			if err == nil && content[len(content)-1] == '\n' {
				content = content[:len(content)-1]
			}
			if capture && len(content) > 0 {
				remaining := maxReadLineCaptureBytes - len(prefix)
				if remaining > 0 {
					take := len(content)
					if take > remaining {
						take = remaining
					}
					prefix = append(prefix, content[:take]...)
				}
				if len(content) > remaining {
					clamped = true
				}
			}
		}
		switch {
		case err == nil:
			line, wasClamped := normalizeBoundedReadLine(prefix, clamped)
			return line, true, false, wasClamped, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if capture && len(prefix) >= maxReadLineCaptureBytes {
				clamped = true
			}
			continue
		case errors.Is(err, io.EOF):
			if !sawData {
				return "", false, true, false, nil
			}
			line, wasClamped := normalizeBoundedReadLine(prefix, clamped)
			return line, true, true, wasClamped, nil
		default:
			return "", false, false, false, err
		}
	}
}

func contextReadError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("read canceled: %w", ctx.Err())
	default:
		return nil
	}
}

func appendReadLineRange(ranges []workspaceReadLineRange, start, end int) []workspaceReadLineRange {
	if start < 1 || end < start {
		return ranges
	}
	if len(ranges) > 0 && start <= ranges[len(ranges)-1].End+1 {
		if end > ranges[len(ranges)-1].End {
			ranges[len(ranges)-1].End = end
		}
		return ranges
	}
	return append(ranges, workspaceReadLineRange{Start: start, End: end})
}

func normalizeBoundedReadLine(prefix []byte, clamped bool) (string, bool) {
	line := strings.TrimSuffix(strings.ToValidUTF8(string(prefix), "\ufffd"), "\r")
	runes := []rune(line)
	if len(runes) > maxReadFileLineRunes {
		clamped = true
	}
	if !clamped {
		return line, false
	}
	marker := []rune(readLineTruncationMarker)
	keep := maxReadFileLineRunes - len(marker)
	if keep < 0 {
		keep = 0
	}
	if len(runes) > keep {
		runes = runes[:keep]
	}
	return string(runes) + string(marker), true
}

func readRenderedLineRunes(lineNumber int, line string) int {
	return utf8.RuneCountInString(fmt.Sprintf("%6d | ", lineNumber)) + utf8.RuneCountInString(line) + 1
}

func formatReadFileWindow(window *readFileWindow) string {
	if window == nil {
		return ""
	}
	displayPathLimit := maxReadDisplayedPathRunes
	if window.Via == "read_files" {
		displayPathLimit = 60
	}
	displayPath := displayReadPath(window.Path, displayPathLimit)
	if len(window.Lines) == 0 {
		if window.TotalLinesKnown && window.TotalLines == 0 {
			return fmt.Sprintf("File %s is empty.", displayPath)
		}
		if window.TotalLinesKnown && window.StartLine > window.TotalLines {
			return fmt.Sprintf(
				"offset_line %d is beyond the end of %s (%d lines). Retry with a smaller offset_line.",
				window.StartLine,
				displayPath,
				window.TotalLines,
			)
		}
		return fmt.Sprintf("No lines available starting at line %d in %s.", window.StartLine, displayPath)
	}

	endLine := window.StartLine + len(window.Lines) - 1
	var out strings.Builder
	out.WriteString(fmt.Sprintf("File %s, lines %d-%d:\n", displayPath, window.StartLine, endLine))
	for index, line := range window.Lines {
		out.WriteString(fmt.Sprintf("%6d | %s\n", window.StartLine+index, line))
	}
	if window.AnyLineClamped {
		out.WriteString("Note: one or more long lines were truncated; use repository_search for targeted content.\n")
	}
	if window.HasMore {
		reason := "line limit reached"
		if window.TruncatedByBudget {
			reason = "output limit reached"
		}
		out.WriteString(fmt.Sprintf(
			"Note: %s. Continue reading the same path with start_line=%d; do not restart the same range or increase limit_lines.",
			reason,
			window.NextOffsetLine,
		))
	}
	return strings.TrimSpace(out.String())
}

func truncateReadRunes(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

func clampReadLineToBudget(lineNumber int, line string, budget int) (string, bool) {
	prefixRunes := utf8.RuneCountInString(fmt.Sprintf("%6d | ", lineNumber)) + 1
	maxContentRunes := budget - prefixRunes
	lineRunes := []rune(line)
	if maxContentRunes <= 0 {
		return "", len(lineRunes) > 0
	}
	if len(lineRunes) <= maxContentRunes {
		return line, false
	}
	marker := []rune(readLineTruncationMarker)
	keep := maxContentRunes - len(marker)
	if keep < 0 {
		keep = 0
	}
	return string(lineRunes[:keep]) + string(marker), true
}

func displayReadPath(path string, limit int) string {
	sanitized := strings.Map(func(value rune) rune {
		if value < 0x20 || value == 0x7f {
			return '?'
		}
		return value
	}, path)
	quoted := strconv.Quote(sanitized)
	if utf8.RuneCountInString(quoted) <= limit {
		return quoted
	}
	runes := []rune(sanitized)
	low, high := 0, len(runes)
	for low < high {
		mid := low + (high-low+1)/2
		candidate := strconv.Quote(string(runes[:mid]) + "...")
		if utf8.RuneCountInString(candidate) <= limit {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return strconv.Quote(string(runes[:low]) + "...")
}

func openWorkspaceReadFile(root, path string) (*os.File, string, error) {
	rootPath, cleanPath, absPath, err := cleanWorkspacePath(root, path)
	if err != nil {
		return nil, "", err
	}
	workspaceRoot, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", fmt.Errorf("open workspace root: %w", err)
	}
	defer workspaceRoot.Close()
	info, err := workspaceRoot.Stat(cleanPath)
	if err != nil {
		return nil, "", readableWorkspaceOpenError(path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("path is not a regular file: %s", displayReadPath(path, maxReadDisplayedPathRunes))
	}
	f, err := workspaceRoot.OpenFile(cleanPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", readableWorkspaceOpenError(path, err)
	}
	return f, absPath, nil
}

func readableWorkspaceOpenError(path string, err error) error {
	displayPath := displayReadPath(path, maxReadDisplayedPathRunes)
	switch {
	case os.IsNotExist(err):
		return fmt.Errorf("file does not exist: %s", displayPath)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("file is not readable: %s", displayPath)
	default:
		return fmt.Errorf("cannot open workspace file %s", displayPath)
	}
}

func hashOpenedWorkspaceFile(ctx context.Context, f *os.File, expected os.FileInfo) (string, os.FileInfo, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", nil, err
	}
	hasher := sha256.New()
	buffer := make([]byte, readStreamBufferBytes)
	for {
		if err := contextReadError(ctx); err != nil {
			return "", nil, err
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			if _, err := hasher.Write(buffer[:n]); err != nil {
				return "", nil, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", nil, readErr
		}
	}
	info, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	if !sameWorkspaceFileVersion(expected, info) {
		return "", nil, fmt.Errorf("file changed while its fingerprint was calculated")
	}
	return hex.EncodeToString(hasher.Sum(nil)), info, nil
}

func hashWorkspaceFile(ctx context.Context, path string, expected os.FileInfo) (string, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	if !sameWorkspaceFileVersion(expected, openedInfo) {
		return "", nil, fmt.Errorf("file changed before its fingerprint was calculated")
	}
	return hashOpenedWorkspaceFile(ctx, f, openedInfo)
}

func sameWorkspaceFileVersion(left, right os.FileInfo) bool {
	if left == nil || right == nil {
		return false
	}
	return os.SameFile(left, right) &&
		left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime())
}

func decodeStrictWorkspaceInput(input json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("parse input: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("parse input: expected one JSON object")
	}
	return nil
}
