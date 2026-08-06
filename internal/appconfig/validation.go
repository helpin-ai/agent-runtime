package appconfig

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type ComponentSummary struct {
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	Configured     bool   `json:"configured"`
	URL            string `json:"url,omitempty"`
	Transport      string `json:"transport,omitempty"`
	AuthConfigured bool   `json:"auth_configured"`
}

type AppSummary struct {
	AppID      string             `json:"app_id"`
	Components []ComponentSummary `json:"components"`
}

type ComponentHealth struct {
	ComponentSummary
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Error      string `json:"error,omitempty"`
}

type AppHealth struct {
	AppID      string            `json:"app_id"`
	Components []ComponentHealth `json:"components"`
}

func Validate(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var errs []error
	for i := range cfg.Apps {
		app := &cfg.Apps[i]
		app.AppID = strings.TrimSpace(app.AppID)
		if app.AppID == "" {
			errs = append(errs, fmt.Errorf("apps[%d].app_id is required", i))
			continue
		}
		if _, duplicate := seen[app.AppID]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate app_id %q", app.AppID))
		}
		seen[app.AppID] = struct{}{}
		app.EventProtocol = strings.ToLower(strings.TrimSpace(app.EventProtocol))
		if app.EventProtocol != "" && app.EventProtocol != "v1" && app.EventProtocol != "v2" {
			errs = append(errs, fmt.Errorf("app %q event_protocol must be v1 or v2", app.AppID))
		}
		validateURLField(&errs, app.AppID, "context_endpoint", app.ContextEndpoint)
		for j := range app.EventCallbacks {
			callback := &app.EventCallbacks[j]
			callback.URL = strings.TrimSpace(callback.URL)
			callback.Token = strings.TrimSpace(callback.Token)
			callback.TokenEnv = strings.TrimSpace(callback.TokenEnv)
			if callback.Token == "" && callback.TokenEnv != "" {
				errs = append(errs, fmt.Errorf("app %q event_callbacks[%d].token_env %q is not set", app.AppID, j, callback.TokenEnv))
			}
			if callback.URL == "" {
				errs = append(errs, fmt.Errorf("app %q event_callbacks[%d].url is required", app.AppID, j))
			} else {
				validateURLField(&errs, app.AppID, fmt.Sprintf("event_callbacks[%d].url", j), callback.URL)
			}
			seenEventTypes := make(map[string]struct{}, len(callback.EventTypes))
			normalizedEventTypes := make([]string, 0, len(callback.EventTypes))
			for _, eventType := range callback.EventTypes {
				eventType = strings.TrimSpace(eventType)
				if eventType == "" {
					continue
				}
				if _, duplicate := seenEventTypes[eventType]; duplicate {
					continue
				}
				seenEventTypes[eventType] = struct{}{}
				normalizedEventTypes = append(normalizedEventTypes, eventType)
			}
			if len(callback.EventTypes) > 0 && len(normalizedEventTypes) == 0 {
				errs = append(errs, fmt.Errorf("app %q event_callbacks[%d].event_types must contain at least one non-empty event type", app.AppID, j))
			}
			callback.EventTypes = normalizedEventTypes
		}
		if app.CommandProvider != nil {
			validateHTTPProvider(&errs, app.AppID, "command_provider", app.CommandProvider.Transport, app.CommandProvider.BaseURL, "http")
		}
		if app.SkillProvider != nil {
			validateHTTPProvider(&errs, app.AppID, "skill_provider", app.SkillProvider.Transport, app.SkillProvider.BaseURL, "http")
			validateURLField(&errs, app.AppID, "skill_provider.package_base_url", app.SkillProvider.PackageBaseURL)
		}
		if app.WorkspaceProvider != nil {
			validateHTTPProvider(&errs, app.AppID, "workspace_provider", app.WorkspaceProvider.Transport, app.WorkspaceProvider.BaseURL, "http", "repository")
		}
		if app.Browser != nil {
			app.Browser.ProfileScopeMetadataKey = strings.TrimSpace(app.Browser.ProfileScopeMetadataKey)
			if app.Browser.ProfileScopeMetadataKey == "" {
				app.Browser.ProfileScopeMetadataKey = "browser_profile_scope_id"
			}
			app.Browser.AllowedDomains = normalizeBrowserDomains(app.Browser.AllowedDomains)
			if app.Browser.Enabled && len(app.Browser.AllowedDomains) == 0 {
				errs = append(errs, fmt.Errorf("app %q browser.allowed_domains must contain at least one domain", app.AppID))
			}
			if app.Browser.Enabled {
				for _, domain := range app.Browser.AllowedDomains {
					if !validBrowserDomainPattern(domain) {
						errs = append(errs, fmt.Errorf("app %q browser.allowed_domains contains invalid pattern %q", app.AppID, domain))
					}
				}
			}
			if app.Browser.Enabled && app.Browser.ArtifactProvider != nil {
				provider := app.Browser.ArtifactProvider
				provider.Transport = strings.TrimSpace(provider.Transport)
				if provider.Transport == "" {
					provider.Transport = "http"
				}
				if provider.Transport != "http" {
					errs = append(errs, fmt.Errorf("app %q browser.artifact_provider has unsupported transport %q", app.AppID, provider.Transport))
				}
				provider.UploadEndpoint = strings.TrimSpace(provider.UploadEndpoint)
				provider.Token = strings.TrimSpace(provider.Token)
				provider.TokenEnv = strings.TrimSpace(provider.TokenEnv)
				if provider.Token == "" && provider.TokenEnv != "" {
					errs = append(errs, fmt.Errorf("app %q browser.artifact_provider.token_env %q is not set", app.AppID, provider.TokenEnv))
				}
				if provider.UploadEndpoint == "" {
					errs = append(errs, fmt.Errorf("app %q browser.artifact_provider.upload_endpoint is required", app.AppID))
				} else {
					validateURLField(&errs, app.AppID, "browser.artifact_provider.upload_endpoint", provider.UploadEndpoint)
				}
			}
		}
		for j, provider := range app.MCPProviders {
			transport := strings.TrimSpace(provider.Transport)
			if transport == "" {
				transport = "http"
			}
			switch transport {
			case "http", "streamable_http":
				validateURLField(&errs, app.AppID, fmt.Sprintf("mcp_providers[%d].url", j), provider.URL)
			case "stdio":
				if strings.TrimSpace(provider.Command) == "" {
					errs = append(errs, fmt.Errorf("app %q mcp_providers[%d].command is required for stdio", app.AppID, j))
				}
			default:
				errs = append(errs, fmt.Errorf("app %q mcp_providers[%d] has unsupported transport %q", app.AppID, j, transport))
			}
		}
	}
	return errors.Join(errs...)
}

func validBrowserDomainPattern(value string) bool {
	value = strings.TrimSpace(value)
	if value == "*" {
		return true
	}
	if strings.HasPrefix(value, "*.") {
		value = strings.TrimPrefix(value, "*.")
	}
	if value == "" || strings.ContainsAny(value, "/?#@ ") {
		return false
	}
	parsed, err := url.Parse("https://" + value)
	return err == nil && parsed.Host == value && parsed.Hostname() == value
}

func normalizeBrowserDomains(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(value, ".")))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func validateURLField(errs *[]error, appID, field, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		*errs = append(*errs, fmt.Errorf("app %q %s must be an absolute http(s) URL", appID, field))
	}
}

func validateHTTPProvider(errs *[]error, appID, field, transport, baseURL string, supported ...string) {
	transport = strings.TrimSpace(transport)
	if transport == "" {
		transport = "http"
	}
	valid := false
	for _, candidate := range supported {
		if transport == candidate {
			valid = true
			break
		}
	}
	if !valid {
		*errs = append(*errs, fmt.Errorf("app %q %s has unsupported transport %q", appID, field, transport))
	}
	if strings.TrimSpace(baseURL) == "" {
		*errs = append(*errs, fmt.Errorf("app %q %s.base_url is required", appID, field))
		return
	}
	validateURLField(errs, appID, field+".base_url", baseURL)
}

func Summaries(cfg *Config) []AppSummary {
	if cfg == nil {
		return []AppSummary{}
	}
	out := make([]AppSummary, 0, len(cfg.Apps))
	for _, app := range cfg.Apps {
		out = append(out, AppSummary{AppID: app.AppID, Components: appComponents(app)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AppID < out[j].AppID })
	return out
}

func SummaryForApp(cfg *Config, appID string) *AppSummary {
	appID = strings.TrimSpace(appID)
	for _, summary := range Summaries(cfg) {
		if summary.AppID == appID {
			copy := summary
			return &copy
		}
	}
	return nil
}

func CheckApp(ctx context.Context, cfg *Config, appID string, client *http.Client) (*AppHealth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("app configuration is unavailable")
	}
	var selected *App
	for i := range cfg.Apps {
		if strings.TrimSpace(cfg.Apps[i].AppID) == strings.TrimSpace(appID) {
			selected = &cfg.Apps[i]
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("app %q is not configured", strings.TrimSpace(appID))
	}
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	targets := appHealthTargets(*selected)
	health := &AppHealth{AppID: selected.AppID, Components: make([]ComponentHealth, len(targets))}
	var wg sync.WaitGroup
	for i, target := range targets {
		i, target := i, target
		wg.Add(1)
		go func() {
			defer wg.Done()
			health.Components[i] = checkComponent(ctx, client, target.summary, target.token)
		}()
	}
	wg.Wait()
	return health, nil
}

func checkComponent(ctx context.Context, client *http.Client, component ComponentSummary, token string) ComponentHealth {
	result := ComponentHealth{ComponentSummary: component, Status: "configured"}
	if component.URL == "" {
		return result
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, component.URL, nil)
	if err != nil {
		result.Status, result.Error = "error", err.Error()
		return result
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := client.Do(req)
	if err != nil {
		result.Status, result.Error = "error", err.Error()
		return result
	}
	defer resp.Body.Close()
	result.HTTPStatus = resp.StatusCode
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.Status = "error"
		result.Error = http.StatusText(resp.StatusCode)
		return result
	}
	result.Status = "reachable"
	return result
}

type appHealthTarget struct {
	summary ComponentSummary
	token   string
}

func appHealthTargets(app App) []appHealthTarget {
	components := appComponents(app)
	targets := make([]appHealthTarget, 0, len(components))
	callbackIndex := 0
	for _, component := range components {
		token := ""
		switch component.Kind {
		case "context":
			token = app.ContextToken
		case "event_callback":
			if callbackIndex < len(app.EventCallbacks) {
				token = app.EventCallbacks[callbackIndex].Token
			}
			callbackIndex++
		case "commands":
			token = app.CommandProvider.Token
		case "skills":
			token = app.SkillProvider.Token
		case "skill_packages":
			token = firstNonEmpty(app.SkillProvider.PackageToken, app.SkillProvider.Token)
		case "workspace":
			token = app.WorkspaceProvider.Token
		case "browser_artifacts":
			if app.Browser != nil && app.Browser.ArtifactProvider != nil {
				token = app.Browser.ArtifactProvider.Token
			}
		case "mcp":
			for _, provider := range app.MCPProviders {
				if component.Name == "MCP: "+firstNonEmpty(provider.Name, provider.ToolPrefix, "unnamed") {
					token = provider.Token
					break
				}
			}
		}
		targets = append(targets, appHealthTarget{summary: component, token: token})
	}
	return targets
}

func appComponents(app App) []ComponentSummary {
	components := make([]ComponentSummary, 0, 4+len(app.EventCallbacks)+len(app.MCPProviders))
	if app.ContextEndpoint != "" {
		components = append(components, ComponentSummary{Name: "Target context", Kind: "context", Configured: true, URL: app.ContextEndpoint, Transport: "http", AuthConfigured: app.ContextToken != ""})
	}
	for i, callback := range app.EventCallbacks {
		components = append(components, ComponentSummary{Name: fmt.Sprintf("Event callback %d", i+1), Kind: "event_callback", Configured: true, URL: callback.URL, Transport: "http", AuthConfigured: callback.Token != ""})
	}
	if app.CommandProvider != nil {
		components = append(components, ComponentSummary{Name: "Commands", Kind: "commands", Configured: true, URL: endpointWithSuffix(app.CommandProvider.BaseURL, "execute"), Transport: firstNonEmpty(app.CommandProvider.Transport, "http"), AuthConfigured: app.CommandProvider.Token != ""})
	}
	if app.SkillProvider != nil {
		components = append(components, ComponentSummary{Name: "Skills", Kind: "skills", Configured: true, URL: endpointWithSuffix(app.SkillProvider.BaseURL, "active-by-key"), Transport: firstNonEmpty(app.SkillProvider.Transport, "http"), AuthConfigured: app.SkillProvider.Token != ""})
		if app.SkillProvider.PackageBaseURL != "" {
			components = append(components, ComponentSummary{Name: "Skill packages", Kind: "skill_packages", Configured: true, URL: app.SkillProvider.PackageBaseURL, Transport: "http", AuthConfigured: firstNonEmpty(app.SkillProvider.PackageToken, app.SkillProvider.Token) != ""})
		}
	}
	if app.WorkspaceProvider != nil {
		components = append(components, ComponentSummary{Name: "Workspace", Kind: "workspace", Configured: true, URL: endpointWithSuffix(app.WorkspaceProvider.BaseURL, "repository-spec"), Transport: firstNonEmpty(app.WorkspaceProvider.Transport, "http"), AuthConfigured: app.WorkspaceProvider.Token != ""})
	}
	if app.Browser != nil && app.Browser.Enabled && app.Browser.ArtifactProvider != nil {
		provider := app.Browser.ArtifactProvider
		components = append(components, ComponentSummary{Name: "Browser artifacts", Kind: "browser_artifacts", Configured: true, URL: provider.UploadEndpoint, Transport: firstNonEmpty(provider.Transport, "http"), AuthConfigured: provider.Token != ""})
	}
	for _, provider := range app.MCPProviders {
		components = append(components, ComponentSummary{Name: "MCP: " + firstNonEmpty(provider.Name, provider.ToolPrefix, "unnamed"), Kind: "mcp", Configured: true, URL: provider.URL, Transport: firstNonEmpty(provider.Transport, "http"), AuthConfigured: provider.Token != ""})
	}
	return components
}

func endpointWithSuffix(baseURL, suffix string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	return baseURL + "/" + strings.TrimLeft(suffix, "/")
}
