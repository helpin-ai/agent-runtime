package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func codexHomeTestExecutionContext() *ExecutionContext {
	return &ExecutionContext{
		AppID: "app-a",
		Agent: &agentcore.Agent{Name: "Codex"},
		Run: &agentcore.AgentRun{
			ID:          "run-1",
			AppID:       "app-a",
			RuntimeKind: agentcore.RuntimeCodex,
		},
	}
}

func readCodexConfig(t *testing.T, codexHome string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	return string(content)
}

func TestWriteCodexHomeConfigEmitsLandlockFeature(t *testing.T) {
	codexHome := t.TempDir()

	if err := writeCodexHomeConfig(codexHome, codexHomeConfig{UseLegacyLandlock: true}); err != nil {
		t.Fatalf("writeCodexHomeConfig: %v", err)
	}

	got := readCodexConfig(t, codexHome)
	if !strings.Contains(got, "[features]") || !strings.Contains(got, "use_legacy_landlock = true") {
		t.Fatalf("config.toml = %q, want [features].use_legacy_landlock", got)
	}
}

// The auth flow and the execution path both write config.toml. Whichever runs
// last must not drop the other's settings.
func TestWriteCodexHomeConfigKeepsAuthAndLandlockTogether(t *testing.T) {
	codexHome := t.TempDir()
	adapter := NewCodexAdapterWithConfig(CodexConfig{UseLegacyLandlock: true})

	if err := writeCodexHomeConfig(codexHome, adapter.codexHomeConfig("gpt-5-codex", "openai", codexOpenAIAuthModeDevice)); err != nil {
		t.Fatalf("writeCodexHomeConfig: %v", err)
	}

	got := readCodexConfig(t, codexHome)
	for _, want := range []string{
		`model = "gpt-5-codex"`,
		`model_provider = "openai"`,
		`forced_login_method = "chatgpt"`,
		"use_legacy_landlock = true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config.toml = %q, want to contain %q", got, want)
		}
	}
}

// prepareCodexHome rewrites config.toml on every execute, including resumes.
// It must not drop what the device-code auth flow already wrote there.
func TestPrepareCodexHomePreservesDeviceAuthSettings(t *testing.T) {
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		RuntimeRoot:       t.TempDir(),
		UseLegacyLandlock: true,
	})
	execCtx := codexHomeTestExecutionContext()
	state := &codexSessionState{
		Model:    "gpt-5-codex",
		Provider: "openai",
		AuthMode: codexOpenAIAuthModeDevice,
	}

	if err := adapter.prepareCodexHome(t.Context(), execCtx, state); err != nil {
		t.Fatalf("prepareCodexHome: %v", err)
	}

	got := readCodexConfig(t, state.CodexHome)
	for _, want := range []string{
		`model = "gpt-5-codex"`,
		`model_provider = "openai"`,
		`forced_login_method = "chatgpt"`,
		"use_legacy_landlock = true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config.toml = %q, want to contain %q", got, want)
		}
	}
}

// api_key runs must not be pushed onto the ChatGPT login path.
func TestCodexHomeConfigOmitsForcedLoginForAPIKeyAuth(t *testing.T) {
	adapter := NewCodexAdapterWithConfig(CodexConfig{})

	cfg := adapter.codexHomeConfig("gpt-5-codex", "openai", codexOpenAIAuthModeAPIKey)

	if cfg.ForcedLoginMethod != "" {
		t.Fatalf("ForcedLoginMethod = %q for api_key auth, want empty", cfg.ForcedLoginMethod)
	}
}

func TestWriteCodexHomeConfigSkipsFileWhenNothingToWrite(t *testing.T) {
	codexHome := t.TempDir()

	if err := writeCodexHomeConfig(codexHome, codexHomeConfig{}); err != nil {
		t.Fatalf("writeCodexHomeConfig: %v", err)
	}

	if _, err := os.Stat(filepath.Join(codexHome, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("config.toml exists with no settings to write (err = %v)", err)
	}
}

func TestDefaultCodexConfigFromEnvReadsLandlockFlag(t *testing.T) {
	t.Setenv("CODEX_USE_LEGACY_LANDLOCK", "true")
	if cfg := DefaultCodexConfigFromEnv(); !cfg.UseLegacyLandlock {
		t.Fatalf("UseLegacyLandlock = false, want true")
	}

	t.Setenv("CODEX_USE_LEGACY_LANDLOCK", "1")
	if cfg := DefaultCodexConfigFromEnv(); !cfg.UseLegacyLandlock {
		t.Fatalf("UseLegacyLandlock = false for \"1\", want true")
	}

	t.Setenv("CODEX_USE_LEGACY_LANDLOCK", "")
	if cfg := DefaultCodexConfigFromEnv(); cfg.UseLegacyLandlock {
		t.Fatalf("UseLegacyLandlock = true when unset, want false")
	}
}

// prepareCodexHome runs on every app-server run, so the sandbox setting must
// land in CODEX_HOME without the auth flow having to run first.
func TestPrepareCodexHomeWritesLandlockConfig(t *testing.T) {
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		RuntimeRoot:       t.TempDir(),
		UseLegacyLandlock: true,
	})
	execCtx := codexHomeTestExecutionContext()
	state := &codexSessionState{}

	if err := adapter.prepareCodexHome(t.Context(), execCtx, state); err != nil {
		t.Fatalf("prepareCodexHome: %v", err)
	}

	got := readCodexConfig(t, state.CodexHome)
	if !strings.Contains(got, "use_legacy_landlock = true") {
		t.Fatalf("config.toml = %q, want use_legacy_landlock", got)
	}
}

func TestPrepareCodexHomeSkipsConfigWhenLandlockDisabled(t *testing.T) {
	adapter := NewCodexAdapterWithConfig(CodexConfig{RuntimeRoot: t.TempDir()})
	execCtx := codexHomeTestExecutionContext()
	state := &codexSessionState{}

	if err := adapter.prepareCodexHome(t.Context(), execCtx, state); err != nil {
		t.Fatalf("prepareCodexHome: %v", err)
	}

	if _, err := os.Stat(filepath.Join(state.CodexHome, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("config.toml written with landlock disabled (err = %v)", err)
	}
}
