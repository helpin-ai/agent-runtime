package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
)

const terminalHelp = `Commands
 /provider NAME   Select openrouter, openai, or anthropic (saved)
 /model MODEL     Select a model ID (saved)
 /models          Show configured providers and default models
 /connection NAME Select a saved host; /connection local for standalone
 /agents          List the connected host's agents
 /agent ID        Choose a host agent
 /target REF      Set its target, e.g. task:123
 /runs            Browse saved runs
 /resume ID       Open a paused/interrupted run
 /diff            Inspect current tracked changes
 /details         Inspect the last run's durable events and tool results
 /approve         Approve the pending action
 /reject          Reject the pending action
 /review TEXT     Start a read-only review
 /new             Start a new task
 /clear           Clear the visible transcript
 /quit            Exit

PageUp/PageDown scroll · Ctrl+Home/End jump · Ctrl+C cancels, then exits when idle
Host login: agent-runtime-cli connect URL --name NAME; agent-runtime-cli login NAME`

type tuiModel struct {
	session                  *Session
	options                  Options
	input                    textinput.Model
	viewport                 viewport.Model
	busy                     bool
	cancel                   context.CancelFunc
	ctx                      context.Context
	result                   executionDone
	width, height            int
	quitting                 bool
	live, transcript, status string
}
type startMsg struct{}
type stopMsg struct{}
type panelMsg struct {
	text string
	err  error
}

func (m tuiModel) Init() tea.Cmd {
	if m.options.Prompt != "" || m.options.Review || m.options.Resume != "" {
		return func() tea.Msg { return startMsg{} }
	}
	return textinput.Blink
}
func (m *tuiModel) append(text string) {
	m.transcript += safeText(text) + "\n\n"
	if len(m.transcript) > 128<<10 {
		m.transcript = "[Earlier output is saved in run history]\n" + m.transcript[len(m.transcript)-(96<<10):]
	}
	m.refresh(true)
}
func (m *tuiModel) refresh(bottom bool) {
	m.viewport.SetContent(m.transcript + m.live)
	if bottom {
		m.viewport.GotoBottom()
	}
}
func (m tuiModel) start() (tea.Model, tea.Cmd) {
	m.busy = true
	m.live = ""
	m.result = executionDone{}
	m.status = "Working"
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	o := m.options
	m.append("› " + o.Prompt)
	return m, func() tea.Msg { r, e := m.session.Execute(ctx, o); return executionDone{r, e} }
}
func (m tuiModel) command(value string) (tea.Model, tea.Cmd) {
	name, arg, _ := strings.Cut(value, " ")
	arg = strings.TrimSpace(arg)
	m.input.SetValue("")
	switch name {
	case "/quit", "/exit":
		return m, tea.Quit
	case "/help":
		m.append(terminalHelp)
	case "/clear":
		m.transcript = ""
		m.live = ""
		m.refresh(true)
	case "/new":
		m.options.Resume = ""
		m.options.Review = false
		m.status = "Ready"
		m.append("New task. Existing runs remain saved.")
	case "/provider", "/model":
		if m.options.Resume != "" {
			m.append("Start /new before changing a saved run's model.")
			break
		}
		provider, model := m.options.Provider, m.options.Model
		if name == "/provider" {
			provider = arg
			model = ""
			for _, p := range runtime.NativeProviderCapabilities() {
				if p.Name == provider {
					model = p.DefaultModel
				}
			}
		} else {
			model = arg
		}
		if err := savePreferences(m.session.Home, provider, model); err != nil {
			m.append(err.Error())
			break
		}
		m.options.Provider, m.options.Model = provider, model
		m.append("Model selection saved: " + provider + " / " + model)
	case "/models":
		for _, p := range runtime.NativeProviderCapabilities() {
			state := "key missing"
			if p.Configured {
				state = "configured"
			}
			m.append(fmt.Sprintf("%s · %s · %s", p.Name, state, p.DefaultModel))
		}
		m.append("Use /provider NAME and /model MODEL_ID. Keys are read from your shell environment.")
	case "/connection":
		if arg == "local" {
			arg = ""
		}
		m.options.Connection = arg
		m.options.Resume = ""
		m.options.Agent = ""
		m.options.Target = ""
		m.append("Connection: " + arg)
	case "/agent":
		m.options.Agent = arg
		m.append("Agent: " + arg)
	case "/target":
		m.options.Target = arg
		m.append("Target: " + arg)
	case "/agents":
		home, name := m.session.Home, m.options.Connection
		return m, func() tea.Msg {
			var b strings.Builder
			err := connectionCommand(m.ctx, home, []string{"agents", name}, &b)
			return panelMsg{b.String(), err}
		}
	case "/runs":
		return m, func() tea.Msg {
			runs, err := m.session.Store.ListRuns(m.ctx, localApp)
			var b strings.Builder
			for _, r := range runs {
				fmt.Fprintf(&b, "%s  %s  %s\n", r.ID, r.Status, r.Target.ID)
			}
			return panelMsg{b.String(), err}
		}
	case "/resume":
		run, err := m.session.Store.GetRun(m.ctx, localApp, arg)
		if err != nil {
			m.append(err.Error())
			break
		}
		if run == nil {
			m.append("Run not found.")
			break
		}
		if run.Status != "paused" && run.Status != "running" && run.Status != "queued" {
			m.append("This run is terminal. Use /new for another task.")
			break
		}
		m.options.Resume = arg
		m.result.run = run
		m.options.Directory = run.Target.ID
		m.options.Prompt = ""
		m.status = run.Status
		var b strings.Builder
		printResult(m.ctx, m.session, &b, run)
		m.append(b.String())
		m.append("Run selected. Reply or use /approve to continue.")
	case "/diff":
		dir := m.options.Directory
		return m, func() tea.Msg {
			cmd := exec.CommandContext(m.ctx, "git", "diff", "--no-ext-diff", "--no-color")
			cmd.Dir = dir
			cmd.Env = procenv.Command()
			out, err := cmd.Output()
			text := string(out)
			if text == "" && err == nil {
				text = "No tracked unstaged changes."
			}
			return panelMsg{text, err}
		}
	case "/details":
		if m.result.run == nil {
			m.append("No run selected. Use /runs and /resume ID.")
			break
		}
		id := m.result.run.ID
		return m, func() tea.Msg {
			events, err := m.session.Store.ListEvents(m.ctx, localApp, id)
			var b strings.Builder
			for _, e := range events {
				fmt.Fprintf(&b, "%s %v\n", e.Type, e.Data)
			}
			return panelMsg{b.String(), err}
		}
	case "/approve", "/reject":
		if m.options.Resume == "" {
			m.append("No pending run. Use /runs and /resume ID.")
			break
		}
		m.options.Intent = "approve"
		m.options.Prompt = "Approved"
		if name == "/reject" {
			m.options.Intent = "request_changes"
			m.options.Prompt = "Do not perform the proposed action."
		}
		return m.start()
	case "/review":
		m.options.Resume = ""
		m.options.Review = true
		m.options.Prompt = arg
		return m.start()
	default:
		m.append("Unknown command. Type /help to see available commands.")
	}
	return m, nil
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
		m.width, m.height = v.Width, v.Height
		m.input.SetWidth(max(1, v.Width-4))
		m.viewport.SetWidth(max(1, v.Width-2))
		m.viewport.SetHeight(max(1, v.Height-6))
		m.refresh(false)
		return m, nil
	case panelMsg:
		if v.err != nil {
			m.append("Error: " + v.err.Error())
		} else {
			m.append(v.text)
		}
		return m, nil
	case engine.Event:
		switch v.Type {
		case "cli.sync_pending":
			m.append("Host sync pending: " + fmt.Sprint(v.Data["message"]))
		case "assistant_message_delta", "command.output":
			if text, ok := v.Data["text"].(string); ok {
				follow := m.viewport.AtBottom()
				m.live += safeText(text)
				if len(m.live) > 16000 {
					m.live = m.live[len(m.live)-12000:]
				}
				m.refresh(follow)
			}
		case "tool_call_started":
			m.live = ""
			m.append("→ " + fmt.Sprint(v.Data["tool_name"]))
			m.status = "Tool: " + fmt.Sprint(v.Data["tool_name"])
		case "tool_call_finished":
			if m.live != "" {
				text := m.live
				m.live = ""
				m.append(text)
			}
		}
		return m, nil
	case executionDone:
		m.busy = false
		m.result = v
		m.cancel = nil
		m.live = ""
		m.status = "Ready"
		var b strings.Builder
		printResult(context.Background(), m.session, &b, v.run)
		if v.err != nil {
			fmt.Fprintln(&b, "Error:", v.err.Error())
			m.status = "Error — /help for options"
		}
		m.append(b.String())
		if m.quitting {
			return m, tea.Quit
		}
		if v.run != nil && v.run.Status == "paused" {
			m.options.Resume = v.run.ID
			m.status = "Needs input — reply, /approve or /reject"
		} else {
			m.options.Resume = ""
			m.options.Review = false
		}
		m.input.SetValue("")
		return m, nil
	case tea.KeyPressMsg:
		key := v.String()
		if key == "ctrl+c" {
			if m.busy {
				m.cancel()
				m.status = "Cancelling…"
				return m, nil
			}
			return m, tea.Quit
		}
		if key == "pgup" || key == "pgdown" || key == "ctrl+home" || key == "ctrl+end" {
			if key == "ctrl+home" {
				m.viewport.GotoTop()
			} else if key == "ctrl+end" {
				m.viewport.GotoBottom()
			} else {
				m.viewport, _ = m.viewport.Update(msg)
			}
			return m, nil
		}
		if m.busy {
			return m, nil
		}
		if key == "enter" {
			value := strings.TrimSpace(m.input.Value())
			if value == "" {
				return m, nil
			}
			if strings.HasPrefix(value, "/") {
				return m.command(value)
			}
			m.options.Intent = "reply"
			m.options.Prompt = value
			return m.start()
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}
func (m tuiModel) View() tea.View {
	width := max(1, m.width-2)
	line := func(s string) string { return ansi.Truncate(safeText(s), width, "…") }
	provider := m.options.Provider
	if provider == "" {
		provider = "No provider — /models"
	}
	model := m.options.Model
	if model == "" {
		model = "provider default"
	}
	connection := "local"
	if m.options.Connection != "" {
		connection = m.options.Connection + " · " + m.options.Agent + " · " + m.options.Target
		provider = "host-managed"
		model = ""
	}
	title := lipgloss.NewStyle().Bold(true).Render(line("Agent Runtime  ·  " + connection))
	composer := m.input.View()
	if m.busy {
		composer = "Working…  Ctrl+C cancels this run"
	}
	body := title + "\n" + line(m.options.Directory+" · "+provider+" / "+model) + "\n" + strings.Repeat("─", width) + "\n" + m.viewport.View() + "\n" + line(m.status) + "\n" + composer + "\n" + line("/help commands · /models · /runs · PageUp/Down scroll · Ctrl+C cancel/exit")
	view := tea.NewView(body)
	view.AltScreen = true
	return view
}
func runTUI(ctx context.Context, s *Session, o Options, in *os.File, out io.Writer) error {
	input := textinput.New()
	input.Prompt = "› "
	input.Placeholder = "Describe a task, or /help"
	input.SetWidth(76)
	input.Focus()
	vp := viewport.New(viewport.WithWidth(78), viewport.WithHeight(18))
	vp.SoftWrap = true
	vp.FillHeight = true
	m := tuiModel{session: s, options: o, input: input, viewport: vp, ctx: ctx, width: 80, height: 24, status: "Ready"}
	m.append("Work in your current checkout. Changes and command execution may require approval.\nType /help for commands, /models to configure a provider, or enter a task.")
	p := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out))
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
