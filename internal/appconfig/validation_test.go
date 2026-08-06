package appconfig

import (
	"strings"
	"testing"
)

func TestDecodeYAMLAndResolveTokenEnvironment(t *testing.T) {
	cfg, err := Decode(`
apps:
  - app_id: helpin
    event_protocol: v2
    context_endpoint: https://helpin.test/target-context
    context_token_env: HELPIN_TOKEN
    event_callbacks:
      - url: https://helpin.test/agent-runtime/events
        token_env: HELPIN_TOKEN
        event_types: [run.completed, " run.failed ", run.completed]
    command_provider:
      transport: http
      base_url: https://helpin.test/commands
      token_env: HELPIN_TOKEN

    browser:
      enabled: true
      allowed_domains: ["*", " stage.helpin.ai ", "*"]
      artifact_provider:
        transport: http
        upload_endpoint: https://helpin.test/artifacts
        token_env: HELPIN_TOKEN
    mcp_providers:
      - name: knowledge
        transport: http
        url: https://helpin.test/mcp
        token_env: HELPIN_TOKEN
`)
	if err != nil {
		t.Fatalf("decode yaml: %v", err)
	}
	ResolveTokenEnv(cfg, func(name string) string {
		if name == "HELPIN_TOKEN" {
			return "secret"
		}
		return ""
	})
	if err := Validate(cfg); err != nil {
		t.Fatalf("validate yaml: %v", err)
	}
	if len(cfg.Apps) != 1 || cfg.Apps[0].ContextToken != "secret" || cfg.Apps[0].EventCallbacks[0].Token != "secret" || cfg.Apps[0].CommandProvider.Token != "secret" || cfg.Apps[0].MCPProviders[0].Token != "secret" || cfg.Apps[0].Browser.ArtifactProvider.Token != "secret" {
		t.Fatalf("unexpected decoded config: %#v", cfg)
	}
	if len(cfg.Apps[0].Browser.AllowedDomains) != 2 {
		t.Fatalf("unexpected normalized browser config: %#v", cfg.Apps[0].Browser)
	}
	if got := cfg.Apps[0].EventCallbacks[0].EventTypes; len(got) != 2 || got[0] != "run.completed" || got[1] != "run.failed" {
		t.Fatalf("unexpected normalized callback event types: %#v", got)
	}
	if !UsesEventProtocolV2(cfg, "helpin") {
		t.Fatalf("expected helpin to use event protocol v2: %#v", cfg.Apps[0])
	}
	if !HasEventProtocolV2(cfg) {
		t.Fatal("expected config to require a v2 publisher")
	}
}

func TestEventProtocolDefaultsToV1AndRejectsUnknownValues(t *testing.T) {
	cfg := &Config{Apps: []App{{AppID: "usermaven"}}}
	if err := Validate(cfg); err != nil {
		t.Fatalf("validate legacy app: %v", err)
	}
	if UsesEventProtocolV2(cfg, "usermaven") {
		t.Fatal("legacy app unexpectedly opted into v2")
	}
	if HasEventProtocolV2(cfg) {
		t.Fatal("legacy config unexpectedly requires a v2 publisher")
	}

	err := Validate(&Config{Apps: []App{{AppID: "bad", EventProtocol: "v3"}}})
	if err == nil || !strings.Contains(err.Error(), "event_protocol") {
		t.Fatalf("expected event_protocol validation error, got %v", err)
	}
}

func TestValidateRejectsIncompleteAndUnsupportedProviders(t *testing.T) {
	err := Validate(&Config{Apps: []App{{
		AppID:             "app-a",
		CommandProvider:   &CommandProvider{Transport: "stdio"},
		SkillProvider:     &SkillProvider{},
		WorkspaceProvider: &WorkspaceProvider{Transport: "unknown", BaseURL: "https://host.test"},
		Browser:           &BrowserConfig{Enabled: true, ArtifactProvider: &ArtifactProvider{Transport: "stdio", TokenEnv: "MISSING_BROWSER_TOKEN"}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "command_provider.base_url is required") || !strings.Contains(err.Error(), "unsupported transport") || !strings.Contains(err.Error(), "browser.allowed_domains") || !strings.Contains(err.Error(), "browser.artifact_provider.upload_endpoint is required") {
		t.Fatalf("expected provider validation errors, got %v", err)
	}
}

func TestValidateBrowserPolicyRespectsDisabledAppsAndRejectsInvalidDomains(t *testing.T) {
	disabled := &Config{Apps: []App{{
		AppID: "disabled-app",
		Browser: &BrowserConfig{Enabled: false, ArtifactProvider: &ArtifactProvider{
			UploadEndpoint: "not-a-url", TokenEnv: "MISSING_DISABLED_TOKEN",
		}},
	}}}
	if err := Validate(disabled); err != nil {
		t.Fatalf("disabled browser should not require active provider config: %v", err)
	}
	invalid := &Config{Apps: []App{{
		AppID: "invalid-app", Browser: &BrowserConfig{Enabled: true, AllowedDomains: []string{"https://example.com", "*bad.example.com"}},
	}}}
	err := Validate(invalid)
	if err == nil || !strings.Contains(err.Error(), "invalid pattern") {
		t.Fatalf("expected invalid browser domain pattern, got %v", err)
	}
}

func TestValidateRejectsDuplicateAppsAndInvalidURLs(t *testing.T) {
	err := Validate(&Config{Apps: []App{
		{AppID: "same", ContextEndpoint: "not-a-url", EventCallbacks: []EventCallback{{TokenEnv: "MISSING_CALLBACK_TOKEN", EventTypes: []string{" "}}, {URL: "ftp://host.test/events"}}},
		{AppID: "same"},
	}})
	if err == nil || !strings.Contains(err.Error(), "event_callbacks[0].url is required") || !strings.Contains(err.Error(), "token_env \"MISSING_CALLBACK_TOKEN\" is not set") || !strings.Contains(err.Error(), "event_types must contain") || !strings.Contains(err.Error(), "absolute http(s) URL") || !strings.Contains(err.Error(), "duplicate app_id") {
		t.Fatalf("expected combined validation errors, got %v", err)
	}
}

func TestSummariesIncludeCallbacksWithoutExposingTokens(t *testing.T) {
	summaries := Summaries(&Config{Apps: []App{{
		AppID: "app-a",
		EventCallbacks: []EventCallback{{
			URL:   "https://host.test/events",
			Token: "top-secret",
		}},
	}}})
	if len(summaries) != 1 || len(summaries[0].Components) != 1 {
		t.Fatalf("unexpected summaries: %#v", summaries)
	}
	callback := summaries[0].Components[0]
	if callback.Kind != "event_callback" || !callback.AuthConfigured || strings.Contains(strings.ToLower(callback.URL), "secret") {
		t.Fatalf("unexpected callback summary: %#v", callback)
	}
}

func TestSummariesNeverExposeTokens(t *testing.T) {
	summaries := Summaries(&Config{Apps: []App{{
		AppID: "app-a", ContextEndpoint: "https://host.test/context", ContextToken: "top-secret",
		CommandProvider: &CommandProvider{BaseURL: "https://host.test/commands", Token: "top-secret"},
		Browser:         &BrowserConfig{Enabled: true, AllowedDomains: []string{"*"}, ArtifactProvider: &ArtifactProvider{UploadEndpoint: "https://host.test/artifacts", Token: "browser-secret"}},
	}}})
	if len(summaries) != 1 || len(summaries[0].Components) != 3 {
		t.Fatalf("unexpected summaries: %#v", summaries)
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(summaries[0].Components[0].URL)), "secret") {
		t.Fatalf("token leaked into summary: %#v", summaries)
	}
	if !summaries[0].Components[0].AuthConfigured || !summaries[0].Components[1].AuthConfigured || !summaries[0].Components[2].AuthConfigured {
		t.Fatalf("expected auth configuration flags: %#v", summaries)
	}
}
