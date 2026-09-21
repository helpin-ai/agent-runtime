package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestLocalChromiumBrowserIntegration(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_CHROMIUM_INTEGRATION") != "1" {
		t.Skip("set AGENT_RUNTIME_CHROMIUM_INTEGRATION=1 to use a real local Chromium browser")
	}
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "chromium-integration", AllowedDomains: []string{"example.com"},
	})
	callCtx := browserTestCallContextForApp("chromium-integration", "local-browser")
	t.Cleanup(func() {
		if err := registry.CloseRun(context.Background(), callCtx.AppID, callCtx.RunID); err != nil {
			t.Errorf("close local Chromium integration browser: %v", err)
		}
	})

	output, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`))
	if err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if !strings.Contains(string(output), `"url":"https://example.com/"`) || !strings.Contains(string(output), "Example Domain") {
		t.Fatalf("unexpected local Chromium output: %s", output)
	}
}
