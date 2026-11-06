package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Workers that start together on a freshly launched browser each keep their
// own tab: every read sees the worker's own page, even after the others
// navigate.
func TestExtensionWorkersStartingTogetherKeepTheirTabs(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_EXTENSION_E2E") != "1" {
		t.Skip("set AGENT_RUNTIME_EXTENSION_E2E=1 to run Chrome for Testing without DevTools")
	}
	executable := firstNonEmptyString(os.Getenv("AGENT_RUNTIME_BROWSER_EXTENSION_EXECUTABLE"), BundledExtensionBrowser())
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = fmt.Fprintf(w, `<title>%s</title><h1 id="name">%s</h1><p>%s</p>`, r.URL.Path, r.URL.Path, strings.Repeat("text ", 2000))
	}))
	defer fixture.Close()
	m := newBrowserManager(BrowserToolsConfig{ProfileRoot: t.TempDir(), ExtensionExecutable: executable})
	r := registryForBrowser(m)
	t.Cleanup(func() { _ = r.CloseBrowsers(context.Background()) })
	errs := make(chan error, 3)
	for i := range 3 {
		session := &browserRunSession{appID: "test", agentID: "first", runID: fmt.Sprintf("ar-run-%d", i), sessionName: fmt.Sprintf("ar-%d", i), policy: BrowserPolicy{Backend: "extension"}}
		go func() {
			do := func(args ...string) (map[string]any, error) {
				raw, err := m.nativeExtensionCommand(t.Context(), session, args)
				var out struct {
					Success bool           `json:"success"`
					Data    map[string]any `json:"data"`
				}
				if err != nil || json.Unmarshal(raw, &out) != nil || !out.Success {
					return nil, fmt.Errorf("%s %v: %s %v", session.runID, args, raw, err)
				}
				return out.Data, nil
			}
			errs <- func() error {
				for round := range 4 {
					path := fmt.Sprintf("/w%d-r%d", i, round)
					if _, err := do("open", fixture.URL+path); err != nil {
						return err
					}
					if _, err := do("wait", "300"); err != nil {
						return err
					}
					if _, err := do("snapshot", "-i"); err != nil {
						return err
					}
					time.Sleep(time.Duration(200*(3-i)) * time.Millisecond)
					for _, read := range [][]string{{"get", "text", "#name"}, {"get", "url"}} {
						data, err := do(read...)
						if err != nil {
							return err
						}
						got := fmt.Sprint(data["text"], data["url"])
						if !strings.Contains(got, path) {
							return fmt.Errorf("%s round %d read %v: got %q, want %s", session.runID, round, read, got, path)
						}
					}
				}
				return nil
			}()
		}()
	}
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
