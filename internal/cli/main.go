package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/term"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
)

const Version = "0.1.0-dev"
const usage = `Agent Runtime CLI — local coding and review

Usage:
  agent-runtime-cli                         Open the terminal assistant
  agent-runtime-cli run [flags] PROMPT       Run a coding agent locally
  agent-runtime-cli review [flags] [PROMPT]  Review without modifying files
  agent-runtime-cli runs list                List saved runs
  agent-runtime-cli runs show ID             Show a run and its messages
  agent-runtime-cli runs resume ID [flags]    Continue a paused/interrupted run
  agent-runtime-cli runs sync ID             Retry sending saved events to its host
  agent-runtime-cli runs logs ID             Show persisted events
  agent-runtime-cli runs diff ID             Show the workspace diff
  agent-runtime-cli connect URL --name NAME  Discover a compatible host
  agent-runtime-cli login NAME               Authenticate using OAuth + PKCE
  agent-runtime-cli connections              List saved connections
  agent-runtime-cli logout NAME              Remove saved credentials
  agent-runtime-cli agents NAME              List a host's available agents
  agent-runtime-cli doctor                   Inspect local prerequisites

Run flags:
  --connection NAME  Use a saved OAuth host connection
  --agent ID         Host agent ID (required with --connection)
  --target REF       Opaque host target, such as task:123
  --dir PATH       Workspace (default: current directory)
  --provider NAME  Native provider (openai, anthropic, openrouter)
  --model NAME     Model override
  --yes            Allow local tool mutations without approval prompts
  --json           Write newline-delimited JSON events
  --env NAME       Expose one additional environment variable to commands (repeatable)
  --intent VALUE   Resume reply, approve, or request_changes (default: reply)

API keys use the native runtime's environment configuration. Run data is stored
outside the repository; set AGENT_RUNTIME_CLI_HOME to override its location.
`

type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }
func Main(args []string, in *os.File, out, errout io.Writer) error {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "help" || args[0] == "-h") {
		_, err := fmt.Fprint(out, usage)
		return err
	}
	if len(args) > 0 && (args[0] == "--version" || args[0] == "version") {
		fmt.Fprintln(out, Version)
		return nil
	}
	home, err := DataDir()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) > 0 {
		switch args[0] {
		case "doctor":
			fmt.Fprintln(out, "Agent Runtime CLI", Version)
			fmt.Fprintln(out, "Data:", home)
			for _, p := range []string{"git", "go", "node", "python3"} {
				path, e := exec.LookPath(p)
				if e != nil {
					path = "not installed"
				}
				fmt.Fprintf(out, "%s: %s\n", p, path)
			}
			configured := false
			for _, p := range runtime.NativeProviderCapabilities() {
				if p.Configured {
					configured = true
					fmt.Fprintf(out, "Provider: %s (%s)\n", p.Name, p.DefaultModel)
				}
			}
			if !configured {
				fmt.Fprintln(out, "No local model credentials. Use a host connection or configure a native provider.")
			}
			return nil
		case "connect", "login", "logout", "connections", "agents", "whoami":
			return connectionCommand(ctx, home, args, out)
		}
	}
	s, err := OpenSession(home, nil)
	if err != nil {
		return err
	}
	defer s.Close()
	logFile, e := os.OpenFile(filepath.Join(home, "cli.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	defer logFile.Close()
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logFile, nil)))
	defer slog.SetDefault(previousLogger)

	o := Options{Directory: "."}
	jsonMode := false
	interactive := len(args) == 0
	if len(args) > 0 && args[0] == "runs" {
		if len(args) < 2 {
			return errors.New("expected runs list, show, resume, or diff")
		}
		if args[1] == "list" {
			runs, e := s.Store.ListRuns(ctx, localApp)
			if e != nil {
				return e
			}
			for _, r := range runs {
				fmt.Fprintf(out, "%s  %-10s  %s\n", r.ID, r.Status, r.Target.ID)
			}
			return nil
		}
		if len(args) < 3 {
			return errors.New("run ID required")
		}
		run, e := s.Store.GetRun(ctx, localApp, args[2])
		if e != nil {
			return e
		}
		if run == nil {
			return errors.New("run not found")
		}
		switch args[1] {
		case "show":
			messages, e := s.Store.ListMessages(ctx, localApp, run.ID)
			if e != nil {
				return e
			}
			return json.NewEncoder(out).Encode(map[string]interface{}{"run": run, "messages": messages})
		case "sync":
			unlock, e := lockWorkspace(home, run.Target.ID)
			if e != nil {
				return e
			}
			defer unlock()
			return s.Sync(ctx, run.ID)
		case "logs":
			events, e := s.Store.ListEvents(ctx, localApp, run.ID)
			if e != nil {
				return e
			}
			return json.NewEncoder(out).Encode(events)
		case "diff":
			cmd := exec.CommandContext(ctx, "git", "diff", "--no-ext-diff")
			cmd.Dir = run.Target.ID
			cmd.Env = procenv.Command()
			cmd.Stdout = out
			cmd.Stderr = errout
			return cmd.Run()
		case "resume":
			o.Resume = run.ID
			o.Directory = run.Target.ID
			args = append([]string{"run"}, args[3:]...)
		default:
			return fmt.Errorf("unknown runs command %q", args[1])
		}
	}
	if !interactive {
		if args[0] != "run" && args[0] != "review" {
			return fmt.Errorf("unknown command %q; use --help", args[0])
		}
		o.Review = args[0] == "review"
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		fs.SetOutput(errout)
		fs.StringVar(&o.Connection, "connection", "", "host connection")
		fs.StringVar(&o.Agent, "agent", "", "host agent")
		fs.StringVar(&o.Target, "target", "", "opaque host target")
		fs.StringVar(&o.Directory, "dir", o.Directory, "workspace")
		fs.StringVar(&o.Provider, "provider", "", "provider")
		fs.StringVar(&o.Model, "model", "", "model")
		fs.StringVar(&o.Intent, "intent", "reply", "resume intent")
		fs.BoolVar(&o.Yes, "yes", false, "allow mutations")
		fs.BoolVar(&jsonMode, "json", false, "JSON events")
		var names stringsFlag
		fs.Var(&names, "env", "environment variable")
		if e := fs.Parse(args[1:]); e != nil {
			return e
		}
		o.Prompt = strings.Join(fs.Args(), " ")
		o.Env = procenv.Command()
		for _, n := range names {
			if strings.Contains(n, "=") || n == "" {
				return fmt.Errorf("--env expects a variable name")
			}
			if v, ok := os.LookupEnv(n); ok {
				o.Env = append(o.Env, n+"="+v)
			}
		}
		if o.Prompt == "" && !o.Review && o.Resume == "" {
			return errors.New("prompt required; use no arguments for the terminal assistant")
		}
	}
	if interactive || (!jsonMode && term.IsTerminal(in.Fd()) && isTerminalWriter(out)) {
		return runTUI(ctx, s, o, in, out)
	}
	done := make(chan executionDone, 1)
	go func() { r, e := s.Execute(ctx, o); done <- executionDone{r, e} }()
	for {
		select {
		case ev := <-s.Events:
			if jsonMode {
				if err = json.NewEncoder(out).Encode(ev); err != nil {
					return err
				}
			} else {
				renderEvent(out, ev)
			}
		case d := <-done:
			for len(s.Events) > 0 {
				ev := <-s.Events
				if jsonMode {
					_ = json.NewEncoder(out).Encode(ev)
				} else {
					renderEvent(out, ev)
				}
			}
			if jsonMode {
				_ = json.NewEncoder(out).Encode(map[string]interface{}{"type": "cli.result", "run": d.run, "error": errorText(d.err)})
			} else {
				printResult(ctx, s, out, d.run)
			}
			return d.err
		}
	}
}
func isTerminalWriter(w io.Writer) bool { f, ok := w.(*os.File); return ok && term.IsTerminal(f.Fd()) }
func errorText(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

type executionDone struct {
	run *agentcore.AgentRun
	err error
}

func renderEvent(w io.Writer, e engine.Event) {
	if e.Type == "cli.sync_pending" {
		fmt.Fprintln(w, "\nHost sync pending:", safeText(fmt.Sprint(e.Data["message"])))
		return
	}
	if e.Type == "assistant_message_delta" {
		if t, ok := e.Data["text"].(string); ok {
			fmt.Fprint(w, safeText(t))
		}
	} else if e.Type == "command.output" {
		fmt.Fprint(w, safeText(fmt.Sprint(e.Data["text"])))
	} else if strings.Contains(e.Type, "tool_call") && !strings.Contains(e.Type, "delta") {
		fmt.Fprintf(w, "\n[%s] %s\n", e.Type, safeText(fmt.Sprint(e.Data["tool_name"])))
	}
}
func printResult(ctx context.Context, s *Session, w io.Writer, r *agentcore.AgentRun) {
	if r == nil {
		return
	}
	messages, _ := s.Store.ListMessages(ctx, localApp, r.ID)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && messages[i].Content != "" {
			fmt.Fprintln(w, "\n"+safeText(messages[i].Content))
			break
		}
	}
	fmt.Fprintf(w, "\nRun %s: %s\n", r.ID, r.Status)
	if r.Status == "paused" {
		items, _ := s.Store.ListInteractions(ctx, localApp, r.ID)
		for _, v := range items {
			if v.Status == "pending" {
				fmt.Fprintln(w, safeText(v.Title+"\n"+v.Summary+"\n"+string(v.RequestPayload)))
			}
		}
		intent := "reply 'your response'"
		if r.PauseReason == "human_approval" {
			intent = "approve"
		}
		fmt.Fprintf(w, "Resume: agent-runtime-cli runs resume %s --intent %s\n", r.ID, intent)
	}
}

// Treat all model, repository, and process text as untrusted terminal content.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 32 && r != 127 && !(r >= 128 && r <= 159) {
			return r
		}
		return -1
	}, s)
}
