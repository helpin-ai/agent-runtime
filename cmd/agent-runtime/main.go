package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/api"
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
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	persistentStore, err := openStore(context.Background())
	if err != nil {
		slog.Error("failed to configure store", "error", err)
		os.Exit(1)
	}
	codexConfig := runtime.DefaultCodexConfigFromEnv()
	codexConfig = configureCodexAuthStore(codexConfig, persistentStore)
	nativeConfig := runtime.DefaultNativeConfigFromEnv()
	openCodeConfig := runtime.DefaultOpenCodeConfigFromEnv()
	registry := runtime.NewRegistry(
		runtime.NewNativeAdapterWithConfig(nativeConfig),
		runtime.NewCodexAdapterWithConfig(codexConfig),
		runtime.NewOpenCodeAdapterWithConfig(openCodeConfig),
	)
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
	durableExecutor, closeDurable, err := openDurableExecutor()
	if err != nil {
		slog.Error("failed to configure durable executor", "error", err)
		os.Exit(1)
	}
	defer closeDurable()
	eventSink, closeEventSink, err := engine.OpenEventSinkFromEnv()
	if err != nil {
		slog.Error("failed to configure event sink", "error", err)
		os.Exit(1)
	}
	defer closeEventSink()
	// In-process broker fans events out to SSE subscribers, alongside the
	// configured (log/NATS) sink.
	eventBroker := engine.NewEventBroker()
	runner := engine.New(engine.Config{
		DefaultExecutionMode: engine.ExecutionModeLightweight,
		Store:                persistentStore,
		Runtimes:             registry,
		Tools:                toolRegistry,
		Skills:               skillRegistry,
		SkillPackages:        skillPackageStores,
		Targets:              targets,
		Workspaces:           workspaceRegistry,
		Durable:              durableExecutor,
		EventSink:            engine.MultiEventSink{eventSink, eventBroker},
	})
	reconcileCtx, stopReconciler := context.WithCancel(context.Background())
	defer stopReconciler()
	if durableExecutor != nil {
		go reconcileDurableRuns(reconcileCtx, runner, 30*time.Second, 30*time.Second)
	}

	serviceToken := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_SERVICE_TOKEN"))
	allowAnonymous := truthyEnv("AGENT_RUNTIME_ALLOW_ANONYMOUS")
	if serviceToken == "" && !allowAnonymous {
		slog.Error("AGENT_RUNTIME_SERVICE_TOKEN is required; set AGENT_RUNTIME_ALLOW_ANONYMOUS=true only for local development")
		os.Exit(1)
	}
	if allowAnonymous {
		slog.Warn("anonymous service API access enabled; do not use AGENT_RUNTIME_ALLOW_ANONYMOUS in shared environments")
	}

	handler := api.NewServer(api.Config{
		Engine:         runner,
		Store:          persistentStore,
		Tools:          toolRegistry,
		CodexAuth:      runtime.NewCodexAuthManager(persistentStore, codexConfig).SetEventSink(engine.MultiEventSink{eventSink, eventBroker}),
		ServiceToken:   serviceToken,
		AllowAnonymous: allowAnonymous,
		Capabilities:   buildCapabilities(skillRegistry),
		Events:         eventBroker,
	})

	addr := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_ADDR"))
	if addr == "" {
		addr = ":8090"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	slog.Info("agent runtime listening", "addr", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("agent runtime stopped", "error", err)
		os.Exit(1)
	}
}

func reconcileDurableRuns(ctx context.Context, runner *engine.Engine, interval, minAge time.Duration) {
	if runner == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if minAge < 0 {
		minAge = 0
	}
	reconcile := func() {
		count, err := runner.ReconcileDurableRuns(ctx, time.Now().UTC().Add(-minAge))
		if err != nil {
			slog.ErrorContext(ctx, "durable run reconciliation failed", "error", err)
		}
		if count > 0 {
			slog.InfoContext(ctx, "durable runs reconciled", "count", count)
		}
	}
	reconcile()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
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

// buildCapabilities assembles the read-only configuration snapshot served by
// GET /capabilities. It reads the same env the components were wired from, so
// it reflects the live configuration without threading state through main.
func buildCapabilities(skillRegistry *skills.Registry) api.Capabilities {
	storeCfg, _ := store.ResolveConfigFromEnv(os.Getenv)

	temporalAddress := strings.TrimSpace(os.Getenv("TEMPORAL_ADDRESS"))
	durableInfo := api.DurableInfo{Enabled: temporalAddress != ""}
	if durableInfo.Enabled {
		durableInfo.TemporalAddress = temporalAddress
		durableInfo.Namespace = temporalclient.BuildOptionsFromEnv(temporalAddress).Namespace
	}

	skillInfos := make([]api.SkillInfo, 0)
	for _, def := range skillRegistry.ListBuiltIns() {
		skillInfos = append(skillInfos, api.SkillInfo{
			Key:         def.Key,
			Title:       def.Title,
			Description: def.Description,
		})
	}

	return api.Capabilities{
		RuntimeKinds: []string{"native_sdk", "codex", "opencode"},
		Providers:    runtime.NativeProviderCapabilities(),
		Store:        api.StoreInfo{Driver: storeCfg.Driver, InMemory: storeCfg.InMemory},
		Durable:      durableInfo,
		Skills:       skillInfos,
	}
}

func openStore(_ context.Context) (agentcore.Store, error) {
	cfg, err := store.ResolveConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	if cfg.InMemory {
		slog.Info("using in-memory store")
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
	slog.Info("using sql store", "driver", cfg.Driver)
	return sqlStore, nil
}

func openDurableExecutor() (engine.DurableExecutor, func(), error) {
	address := strings.TrimSpace(os.Getenv("TEMPORAL_ADDRESS"))
	if address == "" {
		return nil, func() {}, nil
	}
	options := temporalclient.BuildOptionsFromEnv(address)
	client, err := tclient.Dial(options)
	if err != nil {
		return nil, func() {}, err
	}
	slog.Info("using temporal durable executor", "address", address, "namespace", options.Namespace)
	return durable.NewRunEngine(client), client.Close, nil
}
