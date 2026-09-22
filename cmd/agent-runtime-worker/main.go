package main

import (
	"context"
	"flag"
	"fmt"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/appconfig"
	"github.com/helpin-ai/agent-runtime/internal/durable"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/sandbox"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/temporalclient"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
	tclient "go.temporal.io/sdk/client"
	tworker "go.temporal.io/sdk/worker"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == landlockExecCommand {
		os.Exit(runLandlockExec(os.Args[2:]))
	}
	coding := flag.Bool("coding", false, "Serve only the isolated execution (coding) queue")
	allQueues := flag.Bool("all-queues", false, "Serve shared and execution queues from one process; for single-tenant installs that accept shared execution and chat workloads")
	flag.Parse()
	role := durable.WorkerRoleShared
	switch {
	case *coding && *allQueues:
		slog.Error("--coding and --all-queues are mutually exclusive")
		os.Exit(1)
	case *coding:
		role = durable.WorkerRoleExecution
	case *allQueues:
		role = durable.WorkerRoleAll
	}
	execution := role != durable.WorkerRoleShared
	if execution {
		if strings.HasPrefix(strings.TrimSpace(os.Getenv("AGENT_RUNTIME_APP_CONFIG")), "@") {
			slog.Error("execution workers require inline AGENT_RUNTIME_APP_CONFIG; remove the mounted app config")
			os.Exit(1)
		}
		if err := hardenExecutionProcess(); err != nil {
			slog.Error("execution process hardening failed", "error", err)
			os.Exit(1)
		}
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if execution {
		if err := workspace.ValidateToolCacheConfig(); err != nil {
			slog.Error("invalid execution cache configuration", "error", err)
			os.Exit(1)
		}
		if err := workspace.ResetEphemeralRoot(); err != nil {
			slog.Error("failed to prepare ephemeral tool state", "error", err)
			os.Exit(1)
		}
	}
	sandboxMode, err := commandSandboxMode(execution, os.Getenv("AGENT_RUNTIME_EXECUTION_ISOLATION"))
	if err != nil {
		slog.Error("execution isolation is unavailable", "error", err)
		os.Exit(1)
	}
	tools.SetCommandSandbox(sandboxMode)

	persistentStore, err := openStore(context.Background())
	if err != nil {
		slog.Error("failed to configure store", "error", err)
		os.Exit(1)
	}
	temporalClient, err := openTemporalClient()
	if err != nil {
		slog.Error("failed to connect to temporal", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()

	toolRegistry := tools.NewRegistry()
	skillRegistry := skills.NewDefaultRegistry()
	skillPackageStores := skills.NewPackageStoreRegistry()
	targets := host.NewAdapterRegistry(host.NewStaticContextProvider())
	workspaceRegistry := workspace.NewRegistry()
	appCfg, err := appconfig.LoadFromEnv()
	if err != nil {
		slog.Error("failed to load app config", "error", err)
		os.Exit(1)
	}
	runMCPConfig, err := mcp.RunConfigFromEnv(os.Getenv)
	if err != nil {
		slog.Error("failed to configure run-scoped MCP", "error", err)
		os.Exit(1)
	}
	if err := appconfig.ApplyWithOptions(context.Background(), appCfg, targets, toolRegistry, workspaceRegistry, appconfig.ApplyOptions{ContinueOnRequiredProviderFailure: true}); err != nil {
		slog.Error("failed to apply app config", "error", err)
		os.Exit(1)
	}
	if err := appconfig.ApplySkillProviders(context.Background(), appCfg, skillRegistry, skillPackageStores); err != nil {
		slog.Error("failed to apply skill lookup config", "error", err)
		os.Exit(1)
	}
	nativeConfig := runtime.DefaultNativeConfigFromEnv()
	globalEventSink, closeEventSink, err := engine.OpenEventSinkFromEnv()
	if err != nil {
		slog.Error("failed to configure event sink", "error", err)
		os.Exit(1)
	}
	defer closeEventSink()
	v2EventPublisher, closeV2EventPublisher, err := engine.OpenV2EventPublisherFromEnv()
	if err != nil {
		slog.Error("failed to configure v2 event publisher", "error", err)
		os.Exit(1)
	}
	if appconfig.HasEventProtocolV2(appCfg) && v2EventPublisher == nil {
		slog.Error("invalid v2 event configuration", "error", "an app uses event_protocol=v2 but AGENT_RUNTIME_EVENT_SINK does not include nats")
		os.Exit(1)
	}
	defer closeV2EventPublisher()
	modelCredentials, err := appconfig.ModelCredentialManager(appCfg, persistentStore)
	if err != nil {
		slog.Error("failed to configure model credentials", "error", err)
		os.Exit(1)
	}
	appEventSink := appconfig.EventCallbackSink(appCfg, nil)
	runner := engine.New(engine.Config{
		RequireRunModelCredentials: func(appID string) bool { return appconfig.RequiresRunModelCredentials(appCfg, appID) },
		ValidateRunModelEndpoint: func(appID string, model *sdk.RunModel) error {
			return appconfig.ValidateRunModelEndpoint(appCfg, appID, model)
		},
		ModelCredentials:     modelCredentials,
		DefaultExecutionMode: engine.ExecutionModeDurable,
		Store:                persistentStore,
		Runtimes:             runtime.NewRegistry(runtime.NewNativeAdapterWithConfig(nativeConfig)),
		Tools:                toolRegistry,
		Skills:               skillRegistry,
		SkillPackages:        skillPackageStores,
		Targets:              targets,
		Workspaces:           workspaceRegistry,
		EventSink: engine.MultiEventSink{engine.PersistedEventSink{
			Store:       persistentStore,
			V2Enabled:   func(appID string) bool { return appconfig.UsesEventProtocolV2(appCfg, appID) },
			V2Publisher: v2EventPublisher,
		}, globalEventSink, appEventSink},
		RunMCP:       runMCPConfig,
		CodingWorker: execution,
	})
	activities := durable.NewAgentRunActivities(persistentStore, runner)
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, os.Interrupt, syscall.SIGTERM)
	healthServer, err := startWorkerHealthServer(toolRegistry)
	if err != nil {
		slog.Error("failed to start worker health server", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = healthServer.Shutdown(shutdownCtx)
	}()
	for !toolRegistry.Ready() {
		slog.Warn("temporal polling is waiting for required tool providers", "providers", toolRegistry.ProviderHealth())
		select {
		case <-stopCh:
			slog.Info("stopping agent runtime temporal worker before polling started")
			return
		case <-time.After(5 * time.Second):
		}
	}

	var workers []tworker.Worker
	for _, queue := range durable.WorkerQueuesForRole(role) {
		w := tworker.New(temporalClient, queue.Name, workerOptions(queue))
		durable.RegisterAgentRunWorker(w, activities)
		if err := w.Start(); err != nil {
			slog.Error("failed to start temporal worker", "queue", queue.Name, "error", err)
			os.Exit(1)
		}
		workers = append(workers, w)
	}
	slog.Info("agent runtime temporal workers started", "queues", len(workers))

	<-stopCh
	slog.Info("stopping agent runtime temporal workers")
	var stopGroup sync.WaitGroup
	for _, w := range workers {
		stopGroup.Add(1)
		go func(worker tworker.Worker) {
			defer stopGroup.Done()
			worker.Stop()
		}(w)
	}
	stopGroup.Wait()
}

// commandSandboxMode resolves AGENT_RUNTIME_EXECUTION_ISOLATION against the
// kernel. "landlock" (the default) refuses to start below ABI 2 so a pod on an
// old node stops instead of running commands unconfined; "best_effort" and
// "none" log once and run unconfined.
func commandSandboxMode(execution bool, isolation string) (string, error) {
	if !execution {
		return tools.CommandSandboxNone, nil
	}
	isolation = strings.TrimSpace(isolation)
	if isolation == "" {
		isolation = tools.CommandSandboxLandlock
	}
	switch isolation {
	case tools.CommandSandboxNone:
		slog.Warn("AGENT_RUNTIME_EXECUTION_ISOLATION=none; agent commands run unconfined")
		return tools.CommandSandboxNone, nil
	case tools.CommandSandboxLandlock, tools.CommandSandboxBestEffort:
	default:
		return "", fmt.Errorf("unknown AGENT_RUNTIME_EXECUTION_ISOLATION %q; use landlock, best_effort or none", isolation)
	}
	abi, err := sandbox.ABI()
	if err != nil {
		return "", err
	}
	if abi >= 2 {
		return tools.CommandSandboxLandlock, nil
	}
	if isolation == tools.CommandSandboxBestEffort {
		slog.Warn("landlock unavailable; agent commands run unconfined", "landlock_abi", abi, "required_abi", 2)
		return tools.CommandSandboxNone, nil
	}
	return "", fmt.Errorf("landlock abi %d found, abi 2 or newer required (kernel 5.19+ with landlock enabled); set AGENT_RUNTIME_EXECUTION_ISOLATION=none to run unconfined", abi)
}

func workerOptions(queue durable.QueueConfig) tworker.Options {
	return tworker.Options{
		MaxConcurrentActivityExecutionSize: queue.Concurrency,
		// Temporal needs slots for both sticky and regular workflow polling.
		MaxConcurrentWorkflowTaskExecutionSize: max(2, queue.Concurrency),
		WorkerStopTimeout:                      workerStopTimeout(),
	}
}

func startWorkerHealthServer(registry *tools.Registry) (*http.Server, error) {
	address := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_WORKER_HEALTH_ADDR"))
	if address == "" {
		address = ":8091"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Addr: address, Handler: workerHealthHandler(registry), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("worker health server stopped", "error", err)
		}
	}()
	return server, nil
}

func workerHealthHandler(registry *tools.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if registry != nil && !registry.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not_ready"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	return mux
}

func workerStopTimeout() time.Duration {
	const defaultTimeout = 2 * time.Minute
	raw := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_WORKER_STOP_TIMEOUT"))
	if raw == "" {
		return defaultTimeout
	}
	if duration, err := time.ParseDuration(raw); err == nil && duration > 0 {
		return duration
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	slog.Warn("invalid AGENT_RUNTIME_WORKER_STOP_TIMEOUT; using default", "value", raw, "default", defaultTimeout)
	return defaultTimeout
}

func openTemporalClient() (tclient.Client, error) {
	address := strings.TrimSpace(os.Getenv("TEMPORAL_ADDRESS"))
	if address == "" {
		return nil, fmt.Errorf("TEMPORAL_ADDRESS is required")
	}
	return tclient.Dial(temporalclient.BuildOptionsFromEnv(address))
}

func openStore(_ context.Context) (agentcore.Store, error) {
	cfg, err := store.ResolveConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	if cfg.InMemory {
		slog.Warn("using in-memory store for temporal worker; API and worker must share one process for this to work")
		return store.NewMemory(), nil
	}

	sqlStore, err := store.OpenSQL(store.SQLConfig{Driver: cfg.Driver, DSN: cfg.DSN})
	if err != nil {
		return nil, err
	}
	switch cfg.Driver {
	case "postgres", "postgresql":
		if err := sqlStore.MigratePostgres(context.Background()); err != nil {
			return nil, err
		}
	default:
		if err := sqlStore.AutoMigrate(); err != nil {
			return nil, err
		}
	}
	return sqlStore, nil
}
