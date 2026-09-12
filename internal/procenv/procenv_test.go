package procenv

import (
	"slices"
	"strings"
	"testing"
)

// The leak this package exists to prevent: worker credentials must never reach
// a child that an agent can drive.
func TestSanitizedFromDropsRuntimeCredentials(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/node",
		"DATABASE_URL=postgres://user:pw@db/agent",
		"AGENT_RUNTIME_SERVICE_TOKEN=super-secret",
		"AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY=0123456789abcdef",
		"TEMPORAL_API_KEY=temporal-secret",
		"AGENT_RUNTIME_NATS_URL=nats://nats:4222",
	}

	env := SanitizedFrom(base)

	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "DATABASE_URL",
			"AGENT_RUNTIME_SERVICE_TOKEN",
			"AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY",
			"TEMPORAL_API_KEY",
			"AGENT_RUNTIME_NATS_URL":
			t.Fatalf("credential %q leaked into subprocess env: %v", key, env)
		}
	}
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Fatalf("PATH = missing, want passthrough; got %v", env)
	}
	if !slices.Contains(env, "HOME=/home/node") {
		t.Fatalf("HOME = missing, want passthrough; got %v", env)
	}
}

func TestSanitizedFromKeepsToolchainAndProviderKeys(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"LC_ALL=C.UTF-8",
		"NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca.pem",
		"HTTPS_PROXY=http://proxy:3128",
		"OPENAI_API_KEY=sk-test",
		"UNRELATED=nope",
	}

	env := SanitizedFrom(base)

	for _, want := range []string{
		"PATH=/usr/bin",
		"LC_ALL=C.UTF-8",
		"NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca.pem",
		"HTTPS_PROXY=http://proxy:3128",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("%q = missing, want kept; got %v", want, env)
		}
	}
	if slices.Contains(env, "UNRELATED=nope") {
		t.Fatalf("UNRELATED = kept, want dropped; got %v", env)
	}
}

func TestSanitizedFromOverridesReplaceAllowlistedValues(t *testing.T) {
	base := []string{"HOME=/home/node", "CODEX_HOME=/home/node/.codex"}

	env := SanitizedFrom(base, "CODEX_HOME=/work/.codex", "AGENT_RUNTIME_RUN_TOKEN=run-scoped")

	if !slices.Contains(env, "CODEX_HOME=/work/.codex") {
		t.Fatalf("CODEX_HOME = not overridden; got %v", env)
	}
	if slices.Contains(env, "CODEX_HOME=/home/node/.codex") {
		t.Fatalf("CODEX_HOME = original retained alongside override; got %v", env)
	}
	// Overrides bypass the allowlist so a caller can inject a credential that
	// only this child should see.
	if !slices.Contains(env, "AGENT_RUNTIME_RUN_TOKEN=run-scoped") {
		t.Fatalf("override for non-allowlisted key = dropped; got %v", env)
	}
}

func TestSanitizedFromHonoursDeploymentAllowlist(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"CUSTOM_TOOL_HOME=/opt/tool",
		"STILL_SECRET=nope",
		AllowlistEnvVar + "=CUSTOM_TOOL_HOME, SOMETHING_ABSENT",
	}

	env := SanitizedFrom(base)

	if !slices.Contains(env, "CUSTOM_TOOL_HOME=/opt/tool") {
		t.Fatalf("deployment allowlist ignored; got %v", env)
	}
	if slices.Contains(env, "STILL_SECRET=nope") {
		t.Fatalf("STILL_SECRET = kept, want dropped; got %v", env)
	}
}

func TestSanitizedFromIsDeterministic(t *testing.T) {
	base := []string{"TZ=UTC", "PATH=/usr/bin", "HOME=/home/node"}

	first := SanitizedFrom(base)
	second := SanitizedFrom(base)

	if !slices.Equal(first, second) {
		t.Fatalf("SanitizedFrom is not deterministic: %v vs %v", first, second)
	}
	if !slices.IsSorted(first) {
		t.Fatalf("SanitizedFrom = %v, want sorted", first)
	}
}

func TestSanitizedFromSkipsMalformedEntries(t *testing.T) {
	env := SanitizedFrom([]string{"PATH=/usr/bin", "MALFORMED"}, "ALSO_MALFORMED", "=novalue")

	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Fatalf("PATH = missing; got %v", env)
	}
	if len(env) != 1 {
		t.Fatalf("env = %v, want only PATH", env)
	}
}

func TestCommandCannotOptProviderOrServiceKeysIntoEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "provider-secret")
	t.Setenv("AGENT_RUNTIME_SERVICE_TOKEN", "service-secret")
	t.Setenv(AllowlistEnvVar, "OPENAI_API_KEY,AGENT_RUNTIME_SERVICE_TOKEN")
	for _, entry := range Command() {
		if strings.Contains(entry, "provider-secret") || strings.Contains(entry, "service-secret") {
			t.Fatal("credential exposed to command")
		}
	}
}
