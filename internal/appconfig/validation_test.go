package appconfig

import (
	"strings"
	"testing"
)

func TestDecodeYAMLAndResolveTokenEnvironment(t *testing.T) {
	cfg, err := Decode(`
apps:
  - app_id: helpin
    context_endpoint: https://helpin.test/target-context
    context_token_env: HELPIN_TOKEN
    command_provider:
      transport: http
      base_url: https://helpin.test/commands
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
	if len(cfg.Apps) != 1 || cfg.Apps[0].ContextToken != "secret" || cfg.Apps[0].CommandProvider.Token != "secret" || cfg.Apps[0].MCPProviders[0].Token != "secret" {
		t.Fatalf("unexpected decoded config: %#v", cfg)
	}
}

func TestValidateRejectsIncompleteAndUnsupportedProviders(t *testing.T) {
	err := Validate(&Config{Apps: []App{{
		AppID:             "app-a",
		CommandProvider:   &CommandProvider{Transport: "stdio"},
		SkillProvider:     &SkillProvider{},
		WorkspaceProvider: &WorkspaceProvider{Transport: "unknown", BaseURL: "https://host.test"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "command_provider.base_url is required") || !strings.Contains(err.Error(), "unsupported transport") {
		t.Fatalf("expected provider validation errors, got %v", err)
	}
}

func TestValidateRejectsDuplicateAppsAndInvalidURLs(t *testing.T) {
	err := Validate(&Config{Apps: []App{
		{AppID: "same", ContextEndpoint: "not-a-url"},
		{AppID: "same"},
	}})
	if err == nil || !strings.Contains(err.Error(), "absolute http(s) URL") || !strings.Contains(err.Error(), "duplicate app_id") {
		t.Fatalf("expected combined validation errors, got %v", err)
	}
}

func TestSummariesNeverExposeTokens(t *testing.T) {
	summaries := Summaries(&Config{Apps: []App{{
		AppID: "app-a", ContextEndpoint: "https://host.test/context", ContextToken: "top-secret",
		CommandProvider: &CommandProvider{BaseURL: "https://host.test/commands", Token: "top-secret"},
	}}})
	if len(summaries) != 1 || len(summaries[0].Components) != 2 {
		t.Fatalf("unexpected summaries: %#v", summaries)
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(summaries[0].Components[0].URL)), "secret") {
		t.Fatalf("token leaked into summary: %#v", summaries)
	}
	if !summaries[0].Components[0].AuthConfigured || !summaries[0].Components[1].AuthConfigured {
		t.Fatalf("expected auth configuration flags: %#v", summaries)
	}
}
