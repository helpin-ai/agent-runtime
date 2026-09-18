package workspace

import (
	"slices"
	"strings"
	"testing"
)

func TestGitEnvPassesHostTransportSettingsThrough(t *testing.T) {
	t.Setenv("GIT_SSL_CAINFO", "/etc/ssl/corp-ca.pem")
	t.Setenv("SSH_AUTH_SOCK", "/run/ssh-agent.sock")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.postBuffer")
	t.Setenv("GIT_CONFIG_VALUE_0", "1048576")
	t.Setenv("OPENAI_API_KEY", "provider-secret")
	t.Setenv("AGENT_RUNTIME_SERVICE_TOKEN", "service-secret")
	env := gitEnv(&RepositoryAuth{Type: "github", Token: "installation-token", Env: map[string]string{"GIT_SSL_CAINFO": "/auth/override.pem"}})
	for _, want := range []string{"SSH_AUTH_SOCK=/run/ssh-agent.sock", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.postBuffer", "GIT_CONFIG_VALUE_0=1048576", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(env, want) {
			t.Fatalf("git environment lacks %q: %v", want, env)
		}
	}
	// Per-auth overrides still win over the host value.
	if !slices.Contains(env, "GIT_SSL_CAINFO=/auth/override.pem") || slices.Contains(env, "GIT_SSL_CAINFO=/etc/ssl/corp-ca.pem") {
		t.Fatalf("auth override did not replace host GIT_SSL_CAINFO: %v", env)
	}
	plain := gitEnv(nil)
	if !slices.Contains(plain, "GIT_SSL_CAINFO=/etc/ssl/corp-ca.pem") {
		t.Fatalf("host GIT_SSL_CAINFO stripped: %v", plain)
	}
	for _, entry := range append(env, plain...) {
		if strings.Contains(entry, "provider-secret") || strings.Contains(entry, "service-secret") {
			t.Fatalf("git environment leaked %q", entry)
		}
	}
}
