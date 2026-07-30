package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// envFlagEnabled reports whether an environment variable is set to a truthy
// value, matching how CODEX_APP_SERVER has always been parsed.
func envFlagEnabled(key string) bool {
	value := strings.TrimSpace(os.Getenv(key))
	return strings.EqualFold(value, "true") || value == "1"
}

// codexHomeConfig is the subset of Codex's config.toml the runtime owns. Codex
// reads it from CODEX_HOME, which is per run, so every field here is scoped to
// a single run.
type codexHomeConfig struct {
	Model             string
	ModelProvider     string
	ForcedLoginMethod string

	// UseLegacyLandlock selects Codex's Landlock+seccomp sandbox instead of
	// bubblewrap. bubblewrap needs an unprivileged user namespace, which a
	// Kubernetes pod running under the RuntimeDefault seccomp profile cannot
	// create, so read-only and workspace-write runs abort there. Landlock is a
	// kernel LSM applied to the calling thread and needs no namespace, which
	// keeps a real filesystem boundary in a locked-down pod.
	//
	// Upstream marks this feature Stage::Deprecated: it still works, but Codex
	// records a deprecation notice and will remove it. Track the removal.
	UseLegacyLandlock bool
}

func (c codexHomeConfig) empty() bool {
	return strings.TrimSpace(c.Model) == "" &&
		strings.TrimSpace(c.ModelProvider) == "" &&
		strings.TrimSpace(c.ForcedLoginMethod) == "" &&
		!c.UseLegacyLandlock
}

// writeCodexHomeConfig renders config.toml into codexHome. It always writes the
// whole file: Codex has no merge semantics here, and every caller that touches
// the file must therefore go through this one function or it will silently drop
// another caller's settings.
func writeCodexHomeConfig(codexHome string, cfg codexHomeConfig) error {
	codexHome = strings.TrimSpace(codexHome)
	if codexHome == "" || cfg.empty() {
		return nil
	}

	var lines []string
	if model := strings.TrimSpace(cfg.Model); model != "" {
		lines = append(lines, fmt.Sprintf("model = %q", model))
	}
	if provider := strings.TrimSpace(cfg.ModelProvider); provider != "" {
		lines = append(lines, fmt.Sprintf("model_provider = %q", provider))
	}
	if method := strings.TrimSpace(cfg.ForcedLoginMethod); method != "" {
		lines = append(lines, fmt.Sprintf("forced_login_method = %q", method))
	}
	if cfg.UseLegacyLandlock {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "[features]", "use_legacy_landlock = true")
	}

	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}
