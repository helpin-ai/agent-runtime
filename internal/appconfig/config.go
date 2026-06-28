package appconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

type Config struct {
	Apps []App `json:"apps"`
}

type App struct {
	AppID             string             `json:"app_id"`
	ContextEndpoint   string             `json:"context_endpoint,omitempty"`
	ContextToken      string             `json:"context_token,omitempty"`
	MCPProviders      []MCPProvider      `json:"mcp_providers,omitempty"`
	CommandProvider   *CommandProvider   `json:"command_provider,omitempty"`
	WorkspaceProvider *WorkspaceProvider `json:"workspace_provider,omitempty"`
	SkillProvider     *SkillProvider     `json:"skill_provider,omitempty"`
}

type MCPProvider struct {
	Name         string            `json:"name"`
	Transport    string            `json:"transport"`
	URL          string            `json:"url,omitempty"`
	Token        string            `json:"token,omitempty"`
	Command      string            `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	ToolPrefix   string            `json:"tool_prefix,omitempty"`
	AllowedTools []string          `json:"allowed_tools,omitempty"`
}

type WorkspaceProvider struct {
	Transport string `json:"transport"`
	BaseURL   string `json:"base_url"`
	Token     string `json:"token,omitempty"`
	RootDir   string `json:"root_dir,omitempty"`
}

type CommandProvider struct {
	Transport string `json:"transport"`
	BaseURL   string `json:"base_url"`
	Token     string `json:"token,omitempty"`
}

type SkillProvider struct {
	Transport      string `json:"transport"`
	BaseURL        string `json:"base_url"`
	Token          string `json:"token,omitempty"`
	PackageBaseURL string `json:"package_base_url,omitempty"`
	PackageToken   string `json:"package_token,omitempty"`
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
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("decode AGENT_RUNTIME_APP_CONFIG: %w", err)
	}
	return &cfg, nil
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

func Apply(ctx context.Context, cfg *Config, adapters *host.AdapterRegistry, registry *tools.Registry, workspaces *workspace.Registry) error {
	if cfg == nil {
		return nil
	}
	for _, app := range cfg.Apps {
		appID := strings.TrimSpace(app.AppID)
		if appID == "" {
			return fmt.Errorf("app_id is required in app config")
		}
		adapter := host.ConfiguredAppAdapter{
			ID: appID,
			Register: func(ctx context.Context, registry *tools.Registry) error {
				for _, providerCfg := range app.MCPProviders {
					provider, prefix, err := providerFromConfig(providerCfg)
					if err != nil {
						return err
					}
					if _, err := mcp.RegisterProviderTools(ctx, registry, provider, prefix); err != nil {
						return err
					}
				}
				return nil
			},
		}
		if app.CommandProvider != nil {
			executor, err := commandExecutorFromConfig(*app.CommandProvider)
			if err != nil {
				return err
			}
			adapter.CommandExecutor = executor
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
	}
	return nil
}

func commandExecutorFromConfig(cfg CommandProvider) (tools.CommandToolExecutor, error) {
	transport := strings.TrimSpace(cfg.Transport)
	if transport == "" {
		transport = "http"
	}
	switch transport {
	case "http":
		return tools.HTTPCommandExecutor{
			BaseURL: strings.TrimSpace(cfg.BaseURL),
			Token:   strings.TrimSpace(cfg.Token),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported command provider transport %q", transport)
	}
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
	if prefix == "" {
		prefix = strings.TrimSpace(cfg.Name)
	}
	return provider, prefix, nil
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
