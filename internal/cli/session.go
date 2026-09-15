package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/id"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const localApp = "local-cli"

type Options struct {
	Connection, Agent, Target, RequestID string

	Directory, Provider, Model, Prompt, Resume, Intent string
	Review, Yes                                        bool
	Env                                                []string
}
type Session struct {
	mu           sync.Mutex
	activeCancel context.CancelFunc
	activeDone   chan struct{}
	closed       bool

	Home string

	Store  *store.SQL
	Engine *engine.Engine
	Events chan engine.Event
	close  func() error
}
type eventSink struct{ events chan engine.Event }

func (s eventSink) Emit(ctx context.Context, e engine.Event) {
	select {
	case s.events <- e:
	case <-ctx.Done():
	default:
	}
}

type commandStream struct{ sink eventSink }

func (s commandStream) Write(p []byte) (int, error) {
	s.sink.Emit(context.Background(), engine.Event{Type: "command.output", Data: map[string]interface{}{"text": string(p)}})
	return len(p), nil
}
func DataDir() (string, error) {
	if d := os.Getenv("AGENT_RUNTIME_CLI_HOME"); d != "" {
		return filepath.Abs(d)
	}
	d, err := os.UserConfigDir()
	return filepath.Join(d, "agent-runtime-cli"), err
}
func OpenSession(dir string, factory runtime.NativeModelFactory) (*Session, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "sessions.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := gorm.Open(sqlite.Open(path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	st := store.NewSQL(db)
	if err = st.AutoMigrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	events := make(chan engine.Event, 256)
	cfg := runtime.DefaultNativeConfigFromEnv()
	if f, ok := cfg.ModelFactory.(runtime.EinoProviderFactory); ok {
		p := providerDefaults(dir)
		f.DefaultProvider, f.DefaultModel = p.Provider, ""
		cfg.ModelFactory = f
	}
	if factory != nil {
		cfg.ModelFactory = factory
	}
	cfg.ModelFactory = hostFactory{home: dir, fallback: cfg.ModelFactory}
	cfg.MaxToolSteps = 100
	wr := workspace.NewRegistry()
	if err = wr.Register(localApp, localWorkspace{}); err != nil {
		return nil, err
	}
	eng := engine.New(engine.Config{
		Store:                      st,
		Runtimes:                   runtime.NewRegistry(runtime.NewNativeAdapterWithConfig(cfg)),
		Tools:                      tools.NewRegistry(),
		Targets:                    host.NewStaticContextProvider(),
		Workspaces:                 wr,
		EventSink:                  engine.MultiEventSink{engine.PersistedEventSink{Store: st}, eventSink{events}},
		DefaultExecutionMode:       engine.ExecutionModeLightweight,
		CodingWorker:               true,
		ManualLightweightExecution: true,
	})
	return &Session{Home: dir, Store: st, Engine: eng, Events: events, close: sqlDB.Close}, nil
}
func (s *Session) Close() error {
	s.mu.Lock()
	s.closed = true
	cancel, done := s.activeCancel, s.activeDone
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return s.close()
}
func (s *Session) Execute(ctx context.Context, o Options) (*agentcore.AgentRun, error) {
	s.mu.Lock()
	if s.closed || s.activeDone != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("session is closed or already running")
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.activeCancel = cancel
	s.activeDone = done
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.activeCancel = nil
		s.activeDone = nil
		close(done)
		s.mu.Unlock()
	}()

	var run *agentcore.AgentRun
	var err error
	root := o.Directory
	if o.Resume != "" {
		saved, e := s.Store.GetRun(ctx, localApp, o.Resume)
		if e != nil {
			return nil, e
		}
		if saved == nil {
			return nil, fmt.Errorf("run not found")
		}
		root = saved.Target.ID
	}
	if root == "" {
		root = "."
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	unlock, err := lockWorkspace(s.Home, root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if o.Resume != "" {
		run, err = s.Store.GetRun(ctx, localApp, o.Resume)
		if err != nil {
			return nil, err
		}
		if run == nil {
			return nil, fmt.Errorf("run not found")
		}
		if run.Status == "paused" {
			intent := o.Intent
			if intent == "" {
				intent = "reply"
			}
			run, err = s.Engine.ResumeRun(ctx, localApp, run.ID, engine.ResumePayload{Intent: intent, Content: o.Prompt, ResumeID: id.New("resume")})
		} else if run.Status != "queued" && run.Status != "running" {
			return nil, fmt.Errorf("run %s is %s and cannot resume", run.ID, run.Status)
		}
	} else {
		root, e := filepath.Abs(o.Directory)
		if e != nil {
			return nil, e
		}
		root, e = filepath.EvalSymlinks(root)
		if e != nil {
			return nil, e
		}
		info, e := os.Stat(root)
		if e != nil {
			return nil, e
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("workspace must be a directory")
		}
		allowed := []string{"read_files", "list_directory", "repository_search", "run_command", "request_user_input", "request_approval"}
		access := "read_write"
		prompt := o.Prompt
		if o.Review {
			access = "read_only"
			prompt = "Review the working tree for correctness and regressions. Report actionable findings with file paths and lines. Do not edit files.\n" + prompt
		} else {
			allowed = append(allowed, "edit_file", "apply_patch", "write_file")
		}
		approval := "mutating_tools"
		if o.Yes {
			approval = "never"
		}
		metadata := map[string]interface{}{"delivery_mode": "preview"}
		var admission *Admission
		if o.Connection != "" {
			c, e := loadConnection(s.Home, o.Connection)
			if e != nil {
				return nil, e
			}
			if !supports(c, "model_gateway") {
				return nil, fmt.Errorf("host supports admission but has no model gateway yet; use agent-runtime-cli admit to prepare a local execution")
			}
			admission, err = admit(ctx, s.Home, o)
			if err != nil {
				return nil, err
			}
			switch admission.Agent.ApprovalMode {
			case "", "never", "mutating_tools", "always":
			default:
				return nil, fmt.Errorf("unsupported host approval mode %q", admission.Agent.ApprovalMode)
			}
			if workspace.AccessMode(&admission.Agent) == workspace.AccessReadOnly {
				access = workspace.AccessReadOnly
				allowed = []string{"read_files", "list_directory", "repository_search", "run_command", "request_user_input", "request_approval"}
			}
			permitted := map[string]bool{}
			for _, n := range admission.AllowedTools {
				permitted[n] = true
			}
			filtered := make([]string, 0, len(allowed))
			for _, n := range allowed {
				if permitted[n] {
					filtered = append(filtered, n)
				}
			}
			allowed = filtered
			metadata["cli_connection"] = o.Connection
			metadata["cli_host_run_id"] = admission.RunID
			metadata["cli_target"] = o.Target
			metadata["cli_sync_pending"] = true
			prompt = admission.Context + "\n" + prompt
			if admission.Agent.ApprovalMode == "always" {
				approval = "always"
			} else if admission.Agent.ApprovalMode == "mutating_tools" {
				approval = "mutating_tools"
			}
		}
		config, _ := json.Marshal(map[string]interface{}{"workspace": map[string]string{"mode": "host_prepared", "access": access}})
		agent := &agentcore.Agent{ID: id.New("cli_agent"), AppID: localApp, Name: "Local coding agent", RuntimeKind: "native_sdk", Provider: o.Provider, Model: o.Model, SystemPrompt: "You are a local coding assistant. Read repository instructions. Inspect before editing, keep changes focused, and validate with local tests. Never publish or commit unless explicitly requested.", AllowedTools: allowed, AllowedTargets: []string{"workspace"}, ApprovalMode: approval, ExecutionConfig: config, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if admission != nil {
			agent.SystemPrompt = admission.Agent.SystemPrompt
			agent.Name = admission.Agent.Name
		}
		if err = s.Store.CreateAgent(ctx, agent); err != nil {
			return nil, err
		}
		run, err = s.Engine.StartRun(ctx, engine.StartRunRequest{AppID: localApp, AgentID: agent.ID, Target: agentcore.TargetRef{Type: "workspace", ID: root}, Instructions: prompt, AllowedTools: allowed, Metadata: metadata})
	}
	if err != nil {
		return run, err
	}
	if err = s.prepareLease(ctx, run); err != nil {
		return run, err
	}
	stopLease := s.maintainLease(ctx, run, cancel)
	defer stopLease()
	ctx = tools.WithLocalCommandOptions(ctx, tools.LocalCommandOptions{Env: o.Env, Output: commandStream{eventSink{s.Events}}})
	_, err = s.Engine.ExecuteRunOnce(ctx, localApp, run.ID)
	if ctx.Err() != nil {
		_, _ = s.Engine.CancelRun(context.Background(), localApp, run.ID)
	}
	updated, e := s.Store.GetRun(context.Background(), localApp, run.ID)
	if e == nil {
		run = updated
	}
	if run != nil && run.Input.Metadata["cli_connection"] != nil {
		syncCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		if syncErr := s.Sync(syncCtx, run.ID); syncErr != nil {
			eventSink{s.Events}.Emit(context.Background(), engine.Event{Type: "cli.sync_pending", RunID: run.ID, Data: map[string]interface{}{"message": syncErr.Error()}})
		}
		stop()
		if latest, e := s.Store.GetRun(context.Background(), localApp, run.ID); e == nil && latest != nil {
			run = latest
		}
	}
	return run, err
}

type localWorkspace struct{}

func (localWorkspace) PrepareWorkspace(_ context.Context, r workspace.PrepareRequest) (*agentcore.WorkspaceLease, error) {
	root := r.Target.ID
	if !filepath.IsAbs(root) || strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("invalid local workspace")
	}
	return &agentcore.WorkspaceLease{ID: id.New("local"), Provider: "local-cli", RootPath: root, CleanupPolicy: workspace.CleanupManual}, nil
}
func (localWorkspace) FinalizeWorkspace(context.Context, workspace.FinalizeRequest) (*workspace.FinalizeResult, error) {
	return &workspace.FinalizeResult{}, nil
}
func (localWorkspace) CleanupWorkspace(context.Context, workspace.CleanupRequest) error { return nil }
