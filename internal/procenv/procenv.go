// Package procenv builds environments for subprocesses that agents can
// influence.
//
// The runtime process holds credentials that no agent-controlled subprocess
// needs: the datastore URL, the Temporal client credentials, the internal
// service token, and encrypted credential keys. Child processes receive only
// the environment required for their specific operation.
//
// Callers therefore build child environments from an explicit allowlist and
// add whatever else that specific child legitimately needs.
package procenv

import (
	"os"
	"sort"
	"strings"
)

// AllowlistEnvVar names a comma-separated list of additional keys to pass
// through, as an escape hatch for deployments that need a variable we have not
// anticipated. It is read from the parent environment.
const AllowlistEnvVar = "AGENT_RUNTIME_SUBPROCESS_ENV_ALLOWLIST"

// allowedKeys are safe for any agent-controlled child: locale, terminal, and
// the paths and CA settings a toolchain needs to run and to make TLS calls.
var allowedKeys = map[string]bool{
	"HOME":     true,
	"LANG":     true,
	"LANGUAGE": true,
	"LOGNAME":  true,
	"PATH":     true,
	"SHELL":    true,
	"TERM":     true,
	"TMPDIR":   true,
	"TZ":       true,
	"USER":     true,

	// TLS trust stores. Without these, outbound HTTPS from the child fails on
	// images that keep the CA bundle outside the default location.
	"CURL_CA_BUNDLE":      true,
	"NODE_EXTRA_CA_CERTS": true,
	"REQUESTS_CA_BUNDLE":  true,
	"SSL_CERT_DIR":        true,
	"SSL_CERT_FILE":       true,

	"HTTP_PROXY":  true,
	"HTTPS_PROXY": true,
	"NO_PROXY":    true,
	"http_proxy":  true,
	"https_proxy": true,
	"no_proxy":    true,

	"XDG_CACHE_HOME":  true,
	"XDG_CONFIG_HOME": true,
	"XDG_DATA_HOME":   true,
}

// allowedPrefixes covers key families that are safe as a group.
var allowedPrefixes = []string{"LC_"}

// Sanitized returns the allowlisted subset of the current process environment,
// with overrides applied last. Overrides are "KEY=VALUE" entries and replace
// any allowlisted value for the same key, so a caller can pass a credential a
// specific child needs without widening the allowlist for every other child.
func Sanitized(overrides ...string) []string {
	return SanitizedFrom(os.Environ(), overrides...)
}

// Command excludes provider credentials and the deployment allowlist escape
// hatch from agent-selected commands. Specific service clients use Sanitized.
func Command() []string {
	var base []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case AllowlistEnvVar, "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "CODEX_HOME":
			continue
		}
		base = append(base, entry)
	}
	return SanitizedFrom(base)
}

// SanitizedFrom is Sanitized against an explicit base environment.
func SanitizedFrom(base []string, overrides ...string) []string {
	extra := extraAllowedKeys(base)
	kept := make(map[string]string, len(allowedKeys))
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if allowed(key) || extra[key] {
			kept[key] = value
		}
	}
	for _, entry := range overrides {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		kept[key] = value
	}

	keys := make([]string, 0, len(kept))
	for key := range kept {
		keys = append(keys, key)
	}
	// Sorted so child environments are deterministic and diffable in tests.
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+kept[key])
	}
	return env
}

// Allowed reports whether a key passes the built-in allowlist. It ignores the
// deployment escape hatch so callers can test the static policy directly.
func Allowed(key string) bool {
	return allowed(key)
}

func allowed(key string) bool {
	if allowedKeys[key] {
		return true
	}
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func extraAllowedKeys(base []string) map[string]bool {
	raw := ""
	for _, entry := range base {
		if key, value, ok := strings.Cut(entry, "="); ok && key == AllowlistEnvVar {
			raw = value
		}
	}
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	extra := make(map[string]bool)
	for _, key := range strings.Split(raw, ",") {
		if key = strings.TrimSpace(key); key != "" {
			extra[key] = true
		}
	}
	return extra
}
