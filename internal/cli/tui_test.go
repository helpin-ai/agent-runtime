package cli

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/helpin-ai/agent-runtime/internal/engine"
)

func TestProviderDefaultsDetectOpenRouter(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_NATIVE_PROVIDER", "")
	t.Setenv("AGENT_RUNTIME_NATIVE_MODEL", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "fixture-key")
	p := providerDefaults(t.TempDir())
	if p.Provider != "openrouter" || p.Model == "" {
		t.Fatalf("wrong default: %+v", p)
	}
}
func TestPreferencesPreserveExplicitSelection(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_NATIVE_PROVIDER", "")
	t.Setenv("AGENT_RUNTIME_NATIVE_MODEL", "")
	home := t.TempDir()
	if err := savePreferences(home, "openrouter", "example/model"); err != nil {
		t.Fatal(err)
	}
	if got := providerDefaults(home); got.Provider != "openrouter" || got.Model != "example/model" {
		t.Fatal(got)
	}
	t.Setenv("AGENT_RUNTIME_NATIVE_PROVIDER", "openai")
	if got := providerDefaults(home); got.Provider != "openai" || got.Model == "example/model" {
		t.Fatal(got)
	}
}
func TestFullScreenTranscriptResizesAndBoundsOutput(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(78), viewport.WithHeight(17))
	vp.FillHeight = true
	vp.SoftWrap = true
	m := tuiModel{ctx: context.Background(), input: textinput.New(), viewport: vp, width: 80, height: 24, options: Options{Directory: "/project", Provider: "openrouter"}, status: "Ready"}
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 42, Height: 12}, {Width: 120, Height: 40}} {
		updated, _ := m.Update(size)
		m = updated.(tuiModel)
		m.append("A transcript line")
		view := m.View()
		if !view.AltScreen {
			t.Fatal("not full screen")
		}
		if h := lipgloss.Height(view.Content); h != size.Height {
			t.Fatalf("height=%d want %d", h, size.Height)
		}
	}
	for i := 0; i < 100; i++ {
		m.append(strings.Repeat("output ", 1000))
	}
	if len(m.transcript) > 128<<10 {
		t.Fatal("unbounded transcript")
	}
	updated, _ := m.Update(engine.Event{Type: "command.output", Data: map[string]any{"text": strings.Repeat("x", 1<<20)}})
	m = updated.(tuiModel)
	if len(m.live) > 16000 {
		t.Fatal("unbounded live output")
	}
}
