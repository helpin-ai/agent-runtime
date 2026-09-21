package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBrowserRecordingTrimTimeout     = 90 * time.Second
	defaultBrowserRecordingTrimThreads     = 1
	defaultBrowserRecordingTrimPrePadding  = 750 * time.Millisecond
	defaultBrowserRecordingTrimPostPadding = 1500 * time.Millisecond
	minBrowserRecordingTrimSavings         = 2 * time.Second
	maxBrowserRecordingTrimWindows         = 128
	maxBrowserRecordingFFmpegOutputBytes   = 16 * 1024
)

var (
	browserRecordingTrimLimiterOnce sync.Once
	browserRecordingTrimLimiter     chan struct{}
)

type browserRecordingWindow struct {
	Start time.Duration
	End   time.Duration
}

type browserRecordingTrimRequest struct {
	InputPath    string
	OutputPath   string
	RawDuration  time.Duration
	Windows      []browserRecordingWindow
	IncludeAudio bool
	PrePadding   time.Duration
	PostPadding  time.Duration
}

type browserRecordingTrimResult struct {
	OutputDuration time.Duration
	WindowCount    int
	SizeBytes      int64
}

type browserRecordingTrimmer interface {
	Trim(context.Context, browserRecordingTrimRequest) (browserRecordingTrimResult, error)
}

type browserRecordingConverter interface {
	Convert(context.Context, string, string) (int64, error)
}

type ffmpegBrowserRecordingTrimmer struct {
	binary  string
	timeout time.Duration
	threads int
	limiter chan struct{}
}

type ffmpegBrowserRecordingConverter struct {
	binary  string
	timeout time.Duration
	threads int
	limiter chan struct{}
}

func (c *ffmpegBrowserRecordingConverter) Convert(ctx context.Context, inputPath, outputPath string) (int64, error) {
	if strings.TrimSpace(inputPath) == "" || strings.TrimSpace(outputPath) == "" {
		return 0, fmt.Errorf("browser recording conversion paths are required")
	}
	limiter := c.limiter
	if limiter == nil {
		limiter = make(chan struct{}, 1)
	}
	select {
	case limiter <- struct{}{}:
		defer func() { <-limiter }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = defaultBrowserRecordingTrimTimeout
	}
	convertCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", inputPath,
		"-an", "-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-pix_fmt", "yuv420p", "-threads", strconv.Itoa(max(c.threads, 1)),
		"-movflags", "+faststart", outputPath,
	}
	cmd := exec.CommandContext(convertCtx, firstNonEmptyString(c.binary, "ffmpeg"), args...)
	var output bytes.Buffer
	writer := boundedBufferWriter{buffer: &output, limit: maxBrowserRecordingFFmpegOutputBytes}
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Run(); err != nil {
		if convertCtx.Err() != nil {
			return 0, fmt.Errorf("browser recording conversion timed out: %w", convertCtx.Err())
		}
		message := strings.TrimSpace(output.String())
		if message == "" {
			message = err.Error()
		}
		return 0, fmt.Errorf("browser recording conversion failed: %s", message)
	}
	if err := validateMP4File(outputPath); err != nil {
		return 0, fmt.Errorf("validate converted browser recording: %w", err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return 0, fmt.Errorf("stat converted browser recording: %w", err)
	}
	if info.Size() <= 0 || info.Size() > maxBrowserRecordingBytes {
		return 0, fmt.Errorf("converted browser recording must be between 1 byte and 100 MB")
	}
	return info.Size(), nil
}

func globalBrowserRecordingTrimLimiter() chan struct{} {
	browserRecordingTrimLimiterOnce.Do(func() {
		limit := boundedEnvInt("AGENT_RUNTIME_BROWSER_RECORDING_TRIM_MAX_CONCURRENT", 1, 1, 4)
		browserRecordingTrimLimiter = make(chan struct{}, limit)
	})
	return browserRecordingTrimLimiter
}

func (t *ffmpegBrowserRecordingTrimmer) Trim(ctx context.Context, request browserRecordingTrimRequest) (browserRecordingTrimResult, error) {
	windows, outputDuration, err := normalizeBrowserRecordingWindows(
		request.Windows,
		request.RawDuration,
		request.PrePadding,
		request.PostPadding,
	)
	if err != nil {
		return browserRecordingTrimResult{}, err
	}
	if request.RawDuration-outputDuration < minBrowserRecordingTrimSavings {
		return browserRecordingTrimResult{}, fmt.Errorf("browser recording has less than 2 seconds of removable idle time")
	}
	if strings.TrimSpace(request.InputPath) == "" || strings.TrimSpace(request.OutputPath) == "" {
		return browserRecordingTrimResult{}, fmt.Errorf("browser recording trim paths are required")
	}
	limiter := t.limiter
	if limiter == nil {
		limiter = make(chan struct{}, 1)
	}
	select {
	case limiter <- struct{}{}:
		defer func() { <-limiter }()
	case <-ctx.Done():
		return browserRecordingTrimResult{}, ctx.Err()
	}

	timeout := t.timeout
	if timeout <= 0 {
		timeout = defaultBrowserRecordingTrimTimeout
	}
	trimCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := browserRecordingFFmpegArgs(request, windows, max(t.threads, 1))
	cmd := exec.CommandContext(trimCtx, firstNonEmptyString(t.binary, "ffmpeg"), args...)
	var output bytes.Buffer
	writer := boundedBufferWriter{buffer: &output, limit: maxBrowserRecordingFFmpegOutputBytes}
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Run(); err != nil {
		if trimCtx.Err() != nil {
			return browserRecordingTrimResult{}, fmt.Errorf("browser recording smart trim timed out: %w", trimCtx.Err())
		}
		message := strings.TrimSpace(output.String())
		if message == "" {
			message = err.Error()
		}
		return browserRecordingTrimResult{}, fmt.Errorf("browser recording smart trim failed: %s", message)
	}
	if err := validateMP4File(request.OutputPath); err != nil {
		return browserRecordingTrimResult{}, fmt.Errorf("validate smart-trimmed browser recording: %w", err)
	}
	info, err := os.Stat(request.OutputPath)
	if err != nil {
		return browserRecordingTrimResult{}, fmt.Errorf("stat smart-trimmed browser recording: %w", err)
	}
	if info.Size() <= 0 || info.Size() > maxBrowserRecordingBytes {
		return browserRecordingTrimResult{}, fmt.Errorf("smart-trimmed browser recording must be between 1 byte and 100 MB")
	}
	return browserRecordingTrimResult{
		OutputDuration: outputDuration,
		WindowCount:    len(windows),
		SizeBytes:      info.Size(),
	}, nil
}

func normalizeBrowserRecordingWindows(windows []browserRecordingWindow, rawDuration, prePadding, postPadding time.Duration) ([]browserRecordingWindow, time.Duration, error) {
	if rawDuration <= 0 {
		return nil, 0, fmt.Errorf("browser recording duration must be positive")
	}
	if len(windows) == 0 {
		return nil, 0, fmt.Errorf("browser recording has no browser actions to retain")
	}
	if len(windows) > maxBrowserRecordingTrimWindows {
		return nil, 0, fmt.Errorf("browser recording has more than %d action windows", maxBrowserRecordingTrimWindows)
	}
	if prePadding < 0 || postPadding < 0 {
		return nil, 0, fmt.Errorf("browser recording trim padding cannot be negative")
	}

	normalized := make([]browserRecordingWindow, 0, len(windows))
	for _, window := range windows {
		start := max(window.Start-prePadding, 0)
		end := min(window.End+postPadding, rawDuration)
		if end <= start {
			continue
		}
		normalized = append(normalized, browserRecordingWindow{Start: start, End: end})
	}
	if len(normalized) == 0 {
		return nil, 0, fmt.Errorf("browser recording has no valid browser action windows")
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Start == normalized[j].Start {
			return normalized[i].End < normalized[j].End
		}
		return normalized[i].Start < normalized[j].Start
	})
	merged := make([]browserRecordingWindow, 0, len(normalized))
	for _, window := range normalized {
		if len(merged) == 0 || window.Start > merged[len(merged)-1].End {
			merged = append(merged, window)
			continue
		}
		merged[len(merged)-1].End = max(merged[len(merged)-1].End, window.End)
	}
	var duration time.Duration
	for _, window := range merged {
		duration += window.End - window.Start
	}
	return merged, duration, nil
}

func browserRecordingFFmpegArgs(request browserRecordingTrimRequest, windows []browserRecordingWindow, threads int) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	for _, window := range windows {
		args = append(args,
			"-ss", ffmpegDuration(window.Start),
			"-t", ffmpegDuration(window.End-window.Start),
			"-i", request.InputPath,
		)
	}

	filters := make([]string, 0, len(windows)*2+1)
	concatInputs := strings.Builder{}
	for index := range windows {
		filters = append(filters, fmt.Sprintf("[%d:v]setpts=PTS-STARTPTS[v%d]", index, index))
		concatInputs.WriteString(fmt.Sprintf("[v%d]", index))
		if request.IncludeAudio {
			filters = append(filters, fmt.Sprintf("[%d:a]asetpts=PTS-STARTPTS[a%d]", index, index))
			concatInputs.WriteString(fmt.Sprintf("[a%d]", index))
		}
	}
	if len(windows) == 1 {
		filters = append(filters, "[v0]null[vout]")
		if request.IncludeAudio {
			filters = append(filters, "[a0]anull[aout]")
		}
	} else {
		filters = append(filters, fmt.Sprintf("%sconcat=n=%d:v=1:a=%d[vout]%s",
			concatInputs.String(), len(windows), boolInt(request.IncludeAudio), audioOutputLabel(request.IncludeAudio)))
	}
	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "[vout]")
	if request.IncludeAudio {
		args = append(args, "-map", "[aout]", "-c:a", "aac", "-b:a", "128k")
	} else {
		args = append(args, "-an")
	}
	return append(args,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-pix_fmt", "yuv420p",
		"-threads", strconv.Itoa(max(threads, 1)),
		"-movflags", "+faststart",
		request.OutputPath,
	)
}

func ffmpegDuration(value time.Duration) string {
	return strconv.FormatFloat(value.Seconds(), 'f', 3, 64)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func audioOutputLabel(includeAudio bool) string {
	if includeAudio {
		return "[aout]"
	}
	return ""
}

type boundedBufferWriter struct {
	buffer *bytes.Buffer
	limit  int
}

func (w boundedBufferWriter) Write(body []byte) (int, error) {
	length := len(body)
	if w.buffer == nil || w.limit <= w.buffer.Len() {
		return length, nil
	}
	remaining := w.limit - w.buffer.Len()
	_, _ = w.buffer.Write(body[:min(length, remaining)])
	return length, nil
}
