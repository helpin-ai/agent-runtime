package appconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

type Config struct {
	Apps []App `json:"apps" yaml:"apps"`
}

type App struct {
	ModelCredentialCallback *ModelCredentialCallback `json:"model_credential_callback,omitempty" yaml:"model_credential_callback,omitempty"`
	AppID                   string                   `json:"app_id" yaml:"app_id"`
	EventProtocol           string                   `json:"event_protocol,omitempty" yaml:"event_protocol,omitempty"`
	ContextEndpoint         string                   `json:"context_endpoint,omitempty" yaml:"context_endpoint,omitempty"`
	ContextToken            string                   `json:"context_token,omitempty" yaml:"context_token,omitempty"`
	ContextTokenEnv         string                   `json:"context_token_env,omitempty" yaml:"context_token_env,omitempty"`
	EventCallbacks          []EventCallback          `json:"event_callbacks,omitempty" yaml:"event_callbacks,omitempty"`
	MCPProviders            []MCPProvider            `json:"mcp_providers,omitempty" yaml:"mcp_providers,omitempty"`
	WorkspaceProvider       *WorkspaceProvider       `json:"workspace_provider,omitempty" yaml:"workspace_provider,omitempty"`
	SkillProvider           *SkillProvider           `json:"skill_provider,omitempty" yaml:"skill_provider,omitempty"`
	Browser                 *BrowserConfig           `json:"browser,omitempty" yaml:"browser,omitempty"`
}

// BrowserConfig opts one host application into the shared browser runtime.
// Policy and artifact persistence are app-scoped so a multi-app deployment
// never reuses another host's domains, credentials, or storage endpoint.
type BrowserConfig struct {
	Enabled          bool              `json:"enabled" yaml:"enabled"`
	AllowedDomains   []string          `json:"allowed_domains,omitempty" yaml:"allowed_domains,omitempty"`
	ArtifactProvider *ArtifactProvider `json:"artifact_provider,omitempty" yaml:"artifact_provider,omitempty"`
}

type ArtifactProvider struct {
	Transport      string `json:"transport,omitempty" yaml:"transport,omitempty"`
	UploadEndpoint string `json:"upload_endpoint" yaml:"upload_endpoint"`
	Token          string `json:"token,omitempty" yaml:"token,omitempty"`
	TokenEnv       string `json:"token_env,omitempty" yaml:"token_env,omitempty"`
}

// UsesEventProtocolV2 reports whether an app opted into the durable ordered
// event protocol. An omitted value deliberately remains v1 for compatibility.
func UsesEventProtocolV2(cfg *Config, appID string) bool {
	appID = strings.TrimSpace(appID)
	if cfg == nil || appID == "" {
		return false
	}
	for _, app := range cfg.Apps {
		if strings.TrimSpace(app.AppID) == appID {
			return strings.EqualFold(strings.TrimSpace(app.EventProtocol), "v2")
		}
	}
	return false
}

// HasEventProtocolV2 reports whether any configured host app requires the v2
// publisher. Callers use it to reject a startup that would otherwise persist
// v2 events without delivering them to the host projection.
func HasEventProtocolV2(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, app := range cfg.Apps {
		if strings.EqualFold(strings.TrimSpace(app.EventProtocol), "v2") {
			return true
		}
	}
	return false
}

type EventCallback struct {
	URL        string   `json:"url" yaml:"url"`
	Token      string   `json:"token,omitempty" yaml:"token,omitempty"`
	TokenEnv   string   `json:"token_env,omitempty" yaml:"token_env,omitempty"`
	EventTypes []string `json:"event_types,omitempty" yaml:"event_types,omitempty"`
}

type MCPProvider struct {
	Name                   string            `json:"name" yaml:"name"`
	Transport              string            `json:"transport" yaml:"transport"`
	URL                    string            `json:"url,omitempty" yaml:"url,omitempty"`
	Token                  string            `json:"token,omitempty" yaml:"token,omitempty"`
	TokenEnv               string            `json:"token_env,omitempty" yaml:"token_env,omitempty"`
	Command                string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args                   []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env                    map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	ToolPrefix             string            `json:"tool_prefix,omitempty" yaml:"tool_prefix,omitempty"`
	ToolNamespace          string            `json:"tool_namespace,omitempty" yaml:"tool_namespace,omitempty"`
	AllowedTools           []string          `json:"allowed_tools,omitempty" yaml:"allowed_tools,omitempty"`
	RefreshInterval        string            `json:"refresh_interval,omitempty" yaml:"refresh_interval,omitempty"`
	StartupPolicy          string            `json:"startup_policy,omitempty" yaml:"startup_policy,omitempty"`
	UnknownRefreshCooldown string            `json:"unknown_refresh_cooldown,omitempty" yaml:"unknown_refresh_cooldown,omitempty"`
}

type WorkspaceProvider struct {
	Transport string `json:"transport" yaml:"transport"`
	BaseURL   string `json:"base_url" yaml:"base_url"`
	Token     string `json:"token,omitempty" yaml:"token,omitempty"`
	TokenEnv  string `json:"token_env,omitempty" yaml:"token_env,omitempty"`
	RootDir   string `json:"root_dir,omitempty" yaml:"root_dir,omitempty"`
}

type SkillProvider struct {
	Transport       string `json:"transport" yaml:"transport"`
	BaseURL         string `json:"base_url" yaml:"base_url"`
	Token           string `json:"token,omitempty" yaml:"token,omitempty"`
	TokenEnv        string `json:"token_env,omitempty" yaml:"token_env,omitempty"`
	PackageBaseURL  string `json:"package_base_url,omitempty" yaml:"package_base_url,omitempty"`
	PackageToken    string `json:"package_token,omitempty" yaml:"package_token,omitempty"`
	PackageTokenEnv string `json:"package_token_env,omitempty" yaml:"package_token_env,omitempty"`
}

func LoadFromEnv() (*Config, error) {
	raw := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_APP_CONFIG"))
	if raw == "" {
		return &Config{}, nil
	}
	if strings.HasPrefix(raw, "@") {
		path := strings.TrimSpace(strings.TrimPrefix(raw, "@"))
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		raw = string(body)
	}
	cfg, err := Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("decode AGENT_RUNTIME_APP_CONFIG: %w", err)
	}
	resolveTokenEnv(cfg, os.Getenv)
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Decode accepts JSON or YAML. JSON remains the canonical wire format while
// YAML is convenient for reviewed deployment files.
func Decode(raw string) (*Config, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &Config{}, nil
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err == nil {
		return &cfg, nil
	}
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func resolveTokenEnv(cfg *Config, getenv func(string) string) {
	if cfg == nil || getenv == nil {
		return
	}
	for i := range cfg.Apps {
		app := &cfg.Apps[i]
		if app.ContextToken == "" && app.ContextTokenEnv != "" {
			app.ContextToken = strings.TrimSpace(getenv(app.ContextTokenEnv))
		}
		for j := range app.EventCallbacks {
			callback := &app.EventCallbacks[j]
			if callback.Token == "" && callback.TokenEnv != "" {
				callback.Token = strings.TrimSpace(getenv(callback.TokenEnv))
			}
		}
		for j := range app.MCPProviders {
			provider := &app.MCPProviders[j]
			if provider.Token == "" && provider.TokenEnv != "" {
				provider.Token = strings.TrimSpace(getenv(provider.TokenEnv))
			}
		}
		if app.SkillProvider != nil {
			if app.SkillProvider.Token == "" && app.SkillProvider.TokenEnv != "" {
				app.SkillProvider.Token = strings.TrimSpace(getenv(app.SkillProvider.TokenEnv))
			}
			if app.SkillProvider.PackageToken == "" && app.SkillProvider.PackageTokenEnv != "" {
				app.SkillProvider.PackageToken = strings.TrimSpace(getenv(app.SkillProvider.PackageTokenEnv))
			}
		}
		if app.WorkspaceProvider != nil && app.WorkspaceProvider.Token == "" && app.WorkspaceProvider.TokenEnv != "" {
			app.WorkspaceProvider.Token = strings.TrimSpace(getenv(app.WorkspaceProvider.TokenEnv))
		}
		if app.Browser != nil && app.Browser.ArtifactProvider != nil {
			provider := app.Browser.ArtifactProvider
			if provider.Token == "" && provider.TokenEnv != "" {
				provider.Token = strings.TrimSpace(getenv(provider.TokenEnv))
			}
		}
	}
}

func ResolveTokenEnv(cfg *Config, getenv func(string) string) {
	resolveTokenEnv(cfg, getenv)
}

func ApplySkillLookups(_ context.Context, cfg *Config, registry *skills.Registry) error {
	return ApplySkillProviders(context.Background(), cfg, registry, nil)
}

func ApplySkillProviders(_ context.Context, cfg *Config, registry *skills.Registry, packageStores *skills.PackageStoreRegistry) error {
	if cfg == nil || registry == nil {
		return nil
	}
	for _, app := range cfg.Apps {
		appID := strings.TrimSpace(app.AppID)
		if appID == "" {
			return fmt.Errorf("app_id is required in app config")
		}
		if app.SkillProvider == nil {
			continue
		}
		lookup, err := skillLookupFromConfig(*app.SkillProvider)
		if err != nil {
			return fmt.Errorf("configure skill provider for app %q: %w", appID, err)
		}
		registry.SetWorkspaceLookupForApp(appID, lookup)
		if packageStores != nil && strings.TrimSpace(app.SkillProvider.PackageBaseURL) != "" {
			store := skills.HTTPPackageStore{
				BaseURL: strings.TrimSpace(app.SkillProvider.PackageBaseURL),
				Token:   strings.TrimSpace(firstNonEmpty(app.SkillProvider.PackageToken, app.SkillProvider.Token)),
			}
			if err := packageStores.Register(appID, store); err != nil {
				return fmt.Errorf("configure skill package store for app %q: %w", appID, err)
			}
		}
	}
	return nil
}

type ApplyOptions struct {
	// ContinueOnRequiredProviderFailure lets a process boot while background
	// discovery works to make the provider catalog ready.
	ContinueOnRequiredProviderFailure bool
	RequiredProviderStartupTimeout    time.Duration
}

func Apply(ctx context.Context, cfg *Config, adapters *host.AdapterRegistry, registry *tools.Registry, workspaces *workspace.Registry) error {
	return ApplyWithOptions(ctx, cfg, adapters, registry, workspaces, ApplyOptions{})
}

func ApplyWithOptions(ctx context.Context, cfg *Config, adapters *host.AdapterRegistry, registry *tools.Registry, workspaces *workspace.Registry, options ApplyOptions) error {
	if cfg == nil {
		return nil
	}
	startupTimeout := options.RequiredProviderStartupTimeout
	if startupTimeout <= 0 {
		startupTimeout = 90 * time.Second
	}
	for _, app := range cfg.Apps {
		appID := strings.TrimSpace(app.AppID)
		if appID == "" {
			return fmt.Errorf("app_id is required in app config")
		}
		adapter := host.ConfiguredAppAdapter{
			ID: appID,
			Register: func(ctx context.Context, registry *tools.Registry) error {
				for index, providerCfg := range app.MCPProviders {
					provider, prefix, err := providerFromConfig(providerCfg)
					if err != nil {
						return err
					}
					name := strings.TrimSpace(providerCfg.Name)
					interval := providerRefreshInterval(providerCfg)
					registry.RegisterProviderRefresher(appID, name, providerUnknownRefreshCooldown(providerCfg), func(refreshCtx context.Context) error {
						registrations, names, err := mcp.ProviderRegistrations(refreshCtx, provider, prefix)
						if err != nil {
							markProviderRefreshFailure(registry, appID, name, err)
							slog.WarnContext(refreshCtx, "MCP provider refresh failed", "app_id", appID, "provider", name, "error", err)
							return err
						}
						if err := registry.ReplaceAppProvider(appID, name, index, registrations, strings.TrimSpace(providerCfg.ToolNamespace) == "none"); err != nil {
							markProviderRefreshFailure(registry, appID, name, err)
							slog.WarnContext(refreshCtx, "MCP provider catalog was rejected", "app_id", appID, "provider", name, "error", err)
							return err
						}
						previous := currentProviderHealth(registry, appID, name)
						if previous.ToolCount > 0 && len(names) < previous.ToolCount {
							slog.WarnContext(refreshCtx, "MCP provider catalog shrank", "app_id", appID, "provider", name, "previous_count", previous.ToolCount, "tool_count", len(names))
						}
						registry.SetProviderHealth(tools.ProviderHealth{AppID: appID, Provider: name, Ready: true, Source: "mcp", ToolCount: len(names), LastSuccess: time.Now().UTC()})
						return nil
					})
					discoverErr := registry.RefreshProvider(ctx, appID, name)
					if discoverErr != nil && !options.ContinueOnRequiredProviderFailure {
						discoverErr = retryRequiredProvider(ctx, registry, appID, name, startupTimeout, discoverErr)
						if discoverErr != nil {
							return fmt.Errorf("required MCP provider %q for app %q did not become ready within %s: %w", name, appID, startupTimeout, discoverErr)
						}
					}
					if interval > 0 || discoverErr != nil {
						go maintainProvider(ctx, registry, appID, name, interval, discoverErr != nil)
					}
				}
				return nil
			},
		}
		if strings.TrimSpace(app.ContextEndpoint) != "" {
			adapter.ContextProvider = host.HTTPContextProvider{
				Endpoint: strings.TrimSpace(app.ContextEndpoint),
				Token:    strings.TrimSpace(app.ContextToken),
			}
		}
		if err := adapters.Register(ctx, adapter, registry); err != nil {
			return err
		}
		if app.WorkspaceProvider != nil {
			if workspaces == nil {
				return fmt.Errorf("workspace registry is required when workspace_provider is configured for app %q", appID)
			}
			provider, err := workspaceProviderFromConfig(*app.WorkspaceProvider)
			if err != nil {
				return err
			}
			if err := workspaces.Register(appID, provider); err != nil {
				return err
			}
		}
		if app.Browser != nil && app.Browser.Enabled {
			browserCfg := tools.BrowserToolsConfigFromEnv()
			if !browserCfg.Enabled {
				return fmt.Errorf("configure browser for app %q: AGENT_RUNTIME_BROWSER_ENABLED and KERNEL_API_KEY are required", appID)
			}
			browserCfg.AppID = appID
			browserCfg.AllowedDomains = append([]string(nil), app.Browser.AllowedDomains...)
			if app.Browser.ArtifactProvider != nil {
				browserCfg.ArtifactUploadURL = strings.TrimSpace(app.Browser.ArtifactProvider.UploadEndpoint)
				browserCfg.ArtifactUploadToken = strings.TrimSpace(app.Browser.ArtifactProvider.Token)
			}
			tools.RegisterBrowserTools(registry.ForApp(appID), browserCfg)
		}
	}
	return nil
}

func providerFromConfig(cfg MCPProvider) (mcp.ToolProvider, string, error) {
	transport := strings.TrimSpace(cfg.Transport)
	if transport == "" {
		transport = "http"
	}
	var provider mcp.ToolProvider
	switch transport {
	case "http":
		provider = mcp.HTTPProvider{BaseURL: strings.TrimSpace(cfg.URL), Token: strings.TrimSpace(cfg.Token)}
	case "streamable_http":
		provider = &mcp.StreamableHTTPProvider{Endpoint: strings.TrimSpace(cfg.URL), Token: strings.TrimSpace(cfg.Token)}
	case "stdio":
		provider = &mcp.StdioProvider{Command: strings.TrimSpace(cfg.Command), Args: cfg.Args, Env: cfg.Env}
	default:
		return nil, "", fmt.Errorf("unsupported mcp transport %q", transport)
	}
	if len(cfg.AllowedTools) > 0 {
		allowed := make(map[string]bool, len(cfg.AllowedTools))
		for _, tool := range cfg.AllowedTools {
			if tool = strings.TrimSpace(tool); tool != "" {
				allowed[tool] = true
			}
		}
		provider = mcp.FilteringProvider{Provider: provider, Allowed: allowed}
	}
	prefix := strings.TrimSpace(cfg.ToolPrefix)
	if strings.TrimSpace(cfg.ToolNamespace) == "none" {
		prefix = ""
	} else if prefix == "" {
		prefix = strings.TrimSpace(cfg.Name)
	}
	return provider, prefix, nil
}

func providerRefreshInterval(cfg MCPProvider) time.Duration {
	value := strings.TrimSpace(cfg.RefreshInterval)
	if value == "" {
		return 0
	}
	duration, _ := time.ParseDuration(value)
	return duration
}

func providerUnknownRefreshCooldown(cfg MCPProvider) time.Duration {
	value := strings.TrimSpace(cfg.UnknownRefreshCooldown)
	if value == "" {
		return 30 * time.Second
	}
	duration, _ := time.ParseDuration(value)
	return duration
}

func retryRequiredProvider(ctx context.Context, registry *tools.Registry, appID, name string, timeout time.Duration, lastErr error) error {
	retryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	delay := time.Second
	for {
		if err := waitForRetry(retryCtx, jitter(delay)); err != nil {
			return lastErr
		}
		if err := registry.RefreshProvider(retryCtx, appID, name); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func maintainProvider(ctx context.Context, registry *tools.Registry, appID, name string, interval time.Duration, recovering bool) {
	delay := interval
	if recovering {
		delay = time.Second
	}
	for delay > 0 {
		if err := waitForRetry(ctx, jitter(delay)); err != nil {
			return
		}
		err := registry.RefreshProvider(ctx, appID, name)
		if err == nil {
			recovering = false
			if interval <= 0 {
				return
			}
			delay = interval
			continue
		}
		if recovering {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		} else {
			delay = interval
		}
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	span := base / 5
	if span <= 0 {
		return base
	}
	return base - span + time.Duration(rand.Int64N(int64(2*span)+1))
}

func currentProviderHealth(registry *tools.Registry, appID, provider string) tools.ProviderHealth {
	for _, health := range registry.ProviderHealth() {
		if health.AppID == appID && health.Provider == provider {
			return health
		}
	}
	return tools.ProviderHealth{AppID: appID, Provider: provider}
}

func markProviderRefreshFailure(registry *tools.Registry, appID, provider string, err error) {
	previous := currentProviderHealth(registry, appID, provider)
	source := "unavailable"
	ready := false
	if !previous.LastSuccess.IsZero() {
		source, ready = "last_known_good", true
	}
	registry.SetProviderHealth(tools.ProviderHealth{
		AppID: appID, Provider: provider, Ready: ready, Degraded: true, Source: source,
		ToolCount: previous.ToolCount, LastSuccess: previous.LastSuccess, LastError: err.Error(),
	})
}

func workspaceProviderFromConfig(cfg WorkspaceProvider) (workspace.Provider, error) {
	transport := strings.TrimSpace(cfg.Transport)
	if transport == "" {
		transport = "http"
	}
	switch transport {
	case "http":
		return workspace.HTTPProvider{
			BaseURL: strings.TrimSpace(cfg.BaseURL),
			Token:   strings.TrimSpace(cfg.Token),
		}, nil
	case "repository":
		return workspace.RepositoryProvider{
			RootDir: strings.TrimSpace(cfg.RootDir),
			SpecProvider: workspace.HTTPProvider{
				BaseURL: strings.TrimSpace(cfg.BaseURL),
				Token:   strings.TrimSpace(cfg.Token),
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported workspace provider transport %q", transport)
	}
}

func skillLookupFromConfig(cfg SkillProvider) (skills.WorkspaceLookup, error) {
	transport := strings.TrimSpace(cfg.Transport)
	if transport == "" {
		transport = "http"
	}
	switch transport {
	case "http":
		return skills.HTTPWorkspaceLookup{
			BaseURL: strings.TrimSpace(cfg.BaseURL),
			Token:   strings.TrimSpace(cfg.Token),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported skill provider transport %q", transport)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
