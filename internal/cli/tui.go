package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/helpin-ai/agent-runtime/internal/engine"
)

type tuiModel struct {
	session  *Session
	options  Options
	input    textinput.Model
	busy     bool
	cancel   context.CancelFunc
	ctx      context.Context
	result   executionDone
	width    int
	quitting bool
	live     string
}

func (m tuiModel) Init() tea.Cmd {
	if m.options.Prompt != "" || m.options.Review || m.options.Resume != "" {
		return func() tea.Msg { return startMsg{} }
	}
	return textinput.Blink
}

type startMsg struct{}
type stopMsg struct{}

func (m tuiModel) start() (tea.Model, tea.Cmd) {
	m.busy = true
	m.live = ""
	m.result = executionDone{}
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	o := m.options
	return m, func() tea.Msg { r, e := m.session.Execute(ctx, o); return executionDone{r, e} }
}
func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case stopMsg:
		if m.busy {
			m.quitting = true
			m.cancel()
			return m, nil
		}
		return m, tea.Quit
	case startMsg:
		return m.start()
	case tea.WindowSizeMsg:
		m.width = v.Width
		m.input.SetWidth(max(10, v.Width-4))
		return m, nil
	case engine.Event:
		if v.Type == "cli.sync_pending" {
			return m, tea.Printf("Host sync pending: %s", safeText(fmt.Sprint(v.Data["message"])))
		}
		if v.Type == "assistant_message_delta" || v.Type == "command.output" {
			if text, ok := v.Data["text"].(string); ok {
				m.live += safeText(text)
				if len(m.live) > 6000 {
					m.live = m.live[len(m.live)-6000:]
				}
			}
			return m, nil
		}
		if v.Type == "tool_call_started" {
			m.live = ""
			return m, tea.Printf("  → %s", safeText(fmt.Sprint(v.Data["tool_name"])))
		}
		if v.Type == "tool_call_finished" && m.live != "" {
			text := m.live
			m.live = ""
			return m, tea.Printf("%s", text)
		}
		return m, nil
	case executionDone:
		m.busy = false
		m.result = v
		m.cancel = nil
		var b strings.Builder
		printResult(context.Background(), m.session, &b, v.run)
		if v.err != nil {
			fmt.Fprintln(&b, "Error:", safeText(v.err.Error()))
		}
		if m.quitting {
			return m, tea.Sequence(tea.Printf("%s", b.String()), tea.Quit)
		}
		if v.run != nil && v.run.Status == "paused" {
			m.options.Resume = v.run.ID
			m.input.Placeholder = "Reply, /approve, /reject, or /quit"
		} else {
			m.options.Resume = ""
			m.input.Placeholder = "Describe the next task, or /quit"
		}
		m.input.SetValue("")
		return m, tea.Printf("%s", b.String())
	case tea.KeyPressMsg:
		if v.String() == "ctrl+c" {
			if m.busy {
				m.quitting = true
				m.cancel()
				return m, nil
			}
			return m, tea.Quit
		}
		if m.busy {
			return m, nil
		}
		if v.String() == "enter" {
			value := strings.TrimSpace(m.input.Value())
			if value == "/quit" || value == "/exit" {
				return m, tea.Quit
			}
			if value == "" {
				return m, nil
			}
			m.options.Intent = "reply"
			m.options.Prompt = value
			if value == "/approve" {
				if m.options.Resume == "" {
					return m, tea.Printf("No pending run to approve.")
				}
				m.options.Intent = "approve"
				m.options.Prompt = "Approved"
			}
			if value == "/reject" {
				m.options.Intent = "request_changes"
				m.options.Prompt = "Do not perform the proposed action."
			}
			model, cmd := m.start()
			return model, tea.Batch(tea.Printf("› %s", safeText(value)), cmd)
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}
func (m tuiModel) View() tea.View {
	title := lipgloss.NewStyle().Bold(true).Render("Agent Runtime") + " · " + safeText(m.options.Directory)
	body := m.input.View()
	if m.busy {
		body = "Working…  Ctrl+C to cancel"
		if m.live != "" {
			lines := strings.Split(m.live, "\n")
			if len(lines) > 8 {
				lines = lines[len(lines)-8:]
			}
			body = lipgloss.NewStyle().Width(max(10, m.width-2)).Render(strings.Join(lines, "\n")) + "\n" + body
		}
		if m.quitting {
			body = "Cancelling…"
		}
	}
	return tea.NewView(title + "\n" + body + "\n")
}
func runTUI(ctx context.Context, s *Session, o Options, in *os.File, out io.Writer) error {
	input := textinput.New()
	input.Prompt = "› "
	input.Placeholder = "Describe a coding task, or /quit"
	input.SetWidth(78)
	input.Focus()
	p := tea.NewProgram(tuiModel{session: s, options: o, input: input, ctx: ctx, width: 80}, tea.WithInput(in), tea.WithOutput(out))
	pumpCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		for {
			select {
			case e := <-s.Events:
				p.Send(e)
			case <-pumpCtx.Done():
				p.Send(stopMsg{})
				return
			}
		}
	}()
	model, err := p.Run()
	if err != nil {
		return err
	}
	if m, ok := model.(tuiModel); ok {
		return m.result.err
	}
	return nil
}
