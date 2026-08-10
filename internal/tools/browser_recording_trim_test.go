package tools

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNormalizeBrowserRecordingWindowsPadsClampsAndMerges(t *testing.T) {
	windows, duration, err := normalizeBrowserRecordingWindows([]browserRecordingWindow{
		{Start: 8 * time.Second, End: 9 * time.Second},
		{Start: 2 * time.Second, End: 3 * time.Second},
		{Start: 3500 * time.Millisecond, End: 4 * time.Second},
		{Start: 19 * time.Second, End: 22 * time.Second},
	}, 20*time.Second, time.Second, time.Second)
	if err != nil {
		t.Fatalf("normalize windows: %v", err)
	}
	want := []browserRecordingWindow{
		{Start: time.Second, End: 5 * time.Second},
		{Start: 7 * time.Second, End: 10 * time.Second},
		{Start: 18 * time.Second, End: 20 * time.Second},
	}
	if !slices.Equal(windows, want) {
		t.Fatalf("windows = %#v, want %#v", windows, want)
	}
	if duration != 9*time.Second {
		t.Fatalf("duration = %s, want 9s", duration)
	}
}

func TestBrowserRecordingFFmpegArgsSeekOnlyRetainedWindows(t *testing.T) {
	request := browserRecordingTrimRequest{
		InputPath: "/tmp/raw.mp4", OutputPath: "/tmp/demo.mp4", IncludeAudio: true,
	}
	args := browserRecordingFFmpegArgs(request, []browserRecordingWindow{
		{Start: 1500 * time.Millisecond, End: 4 * time.Second},
		{Start: 10 * time.Second, End: 12 * time.Second},
	}, 1)
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-ss 1.500 -t 2.500 -i /tmp/raw.mp4",
		"-ss 10.000 -t 2.000 -i /tmp/raw.mp4",
		"concat=n=2:v=1:a=1[vout][aout]",
		"-map [vout] -map [aout]",
		"-preset veryfast",
		"-threads 1",
		"-movflags +faststart /tmp/demo.mp4",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("ffmpeg args %q do not contain %q", joined, expected)
		}
	}
}

type fakeBrowserRecordingTrimmer struct {
	requests []browserRecordingTrimRequest
	err      error
	result   browserRecordingTrimResult
	payload  []byte
}

func (t *fakeBrowserRecordingTrimmer) Trim(_ context.Context, request browserRecordingTrimRequest) (browserRecordingTrimResult, error) {
	t.requests = append(t.requests, request)
	if t.err != nil {
		return browserRecordingTrimResult{}, t.err
	}
	payload := t.payload
	if len(payload) == 0 {
		payload = []byte("\x00\x00\x00\x18ftypisomtrimmed")
	}
	if err := os.WriteFile(request.OutputPath, payload, 0600); err != nil {
		return browserRecordingTrimResult{}, err
	}
	result := t.result
	if result.OutputDuration <= 0 {
		result.OutputDuration = 2 * time.Second
	}
	if result.WindowCount <= 0 {
		result.WindowCount = 1
	}
	result.SizeBytes = int64(len(payload))
	return result, nil
}
