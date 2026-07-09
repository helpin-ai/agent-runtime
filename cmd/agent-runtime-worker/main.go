package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/appconfig"
	"github.com/helpin-ai/agent-runtime/internal/durable"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/temporalclient"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
	tclient "go.temporal.io/sdk/client"
	tworker "go.temporal.io/sdk/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

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
	if err := appconfig.Apply(context.Background(), appCfg, targets, toolRegistry, workspaceRegistry); err != nil {
		slog.Error("failed to apply app config", "error", err)
		os.Exit(1)
	}
	if err := appconfig.ApplySkillProviders(context.Background(), appCfg, skillRegistry, skillPackageStores); err != nil {
		slog.Error("failed to apply skill lookup config", "error", err)
		os.Exit(1)
	}
	codexConfig := runtime.DefaultCodexConfigFromEnv()
	codexConfig = configureCodexAuthStore(codexConfig, persistentStore)
	nativeConfig := runtime.DefaultNativeConfigFromEnv()
	openCodeConfig := runtime.DefaultOpenCodeConfigFromEnv()
	eventSink, closeEventSink, err := engine.OpenEventSinkFromEnv()
	if err != nil {
		slog.Error("failed to configure event sink", "error", err)
		os.Exit(1)
	}
	defer closeEventSink()
	runner := engine.New(engine.Config{
		DefaultExecutionMode: engine.ExecutionModeDurable,
		Store:                persistentStore,
		Runtimes:             runtime.NewRegistry(runtime.NewNativeAdapterWithConfig(nativeConfig), runtime.NewCodexAdapterWithConfig(codexConfig), runtime.NewOpenCodeAdapterWithConfig(openCodeConfig)),
		Tools:                toolRegistry,
		Skills:               skillRegistry,
		SkillPackages:        skillPackageStores,
		Targets:              targets,
		Workspaces:           workspaceRegistry,
		EventSink:            engine.MultiEventSink{engine.PersistedEventSink{Store: persistentStore}, eventSink},
	})
	activities := durable.NewAgentRunActivities(persistentStore, runner)

	var workers []tworker.Worker
	for _, queue := range durable.SharedQueues() {
		options := tworker.Options{
			MaxConcurrentActivityExecutionSize:     queue.Concurrency,
			MaxConcurrentWorkflowTaskExecutionSize: queue.Concurrency,
		}
		w := tworker.New(temporalClient, queue.Name, options)
		durable.RegisterAgentRunWorker(w, activities)
		if err := w.Start(); err != nil {
			slog.Error("failed to start temporal worker", "queue", queue.Name, "error", err)
			os.Exit(1)
		}
		workers = append(workers, w)
	}
	slog.Info("agent runtime temporal workers started", "queues", len(workers))

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, os.Interrupt, syscall.SIGTERM)
	<-stopCh
	slog.Info("stopping agent runtime temporal workers")
	for _, w := range workers {
		w.Stop()
	}
}

func openTemporalClient() (tclient.Client, error) {
	address := strings.TrimSpace(os.Getenv("TEMPORAL_ADDRESS"))
	if address == "" {
		return nil, fmt.Errorf("TEMPORAL_ADDRESS is required")
	}
	return tclient.Dial(temporalclient.BuildOptionsFromEnv(address))
}

func configureCodexAuthStore(cfg runtime.CodexConfig, persistentStore agentcore.Store) runtime.CodexConfig {
	if sqlStore, ok := persistentStore.(*store.SQL); ok && sqlStore.DB() != nil {
		keyValue := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY"))
		if keyValue == "" {
			keyValue = strings.TrimSpace(os.Getenv("CODEX_AUTH_ENCRYPTION_KEY"))
		}
		key, err := runtime.ParseCodexAuthEncryptionKey(keyValue)
		if err == nil && len(key) == 32 {
			cfg.AuthStore = runtime.NewStoreBackedCodexAuthStore(sqlStore.DB(), key)
			slog.Info("codex auth store configured", "store", "store_backed")
			return cfg
		}
		if keyValue != "" && err != nil {
			slog.Warn("codex auth store encryption key is invalid; falling back", "error", err)
		}
	}
	switch cfg.AuthStore.(type) {
	case *runtime.FileCodexAuthStore:
		slog.Info("codex auth store configured", "store", "file")
	default:
		slog.Info("codex auth store configured", "store", "none")
	}
	return cfg
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
