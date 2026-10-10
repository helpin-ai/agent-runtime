package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// The host owns profiles and extension installation; the runtime owns tool
// validation and translates the driver's output through the existing tools.
func (m *BrowserManager) hostBrowserCall(ctx context.Context, session *browserRunSession, action string, command []string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.CommandTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{"app_id": session.appID, "run_id": session.runID, "session": session.sessionName, "action": action, "command": command})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.HostURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("invalid browser host configuration")
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.HostToken)
	req.Header.Set("Content-Type", "application/json")
	// A redirected callback must never receive the host credential.
	client := *m.cfg.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("browser host is unavailable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read browser host response")
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &failure)
		return fmt.Errorf("browser host: %s", firstNonEmptyString(failure.Error, "request failed"))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid browser host response")
	}
	return nil
}

func (m *BrowserManager) extensionCommand(ctx context.Context, session *browserRunSession, maxOutput int, global, command []string) ([]byte, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("missing extension command")
	}
	var result struct {
		Success bool           `json:"success"`
		Error   string         `json:"error"`
		Data    map[string]any `json:"data"`
	}
	wire := append([]string(nil), command...)
	if command[0] == "screenshot" {
		if len(command) != 2 {
			return nil, fmt.Errorf("extension screenshots support the visible tab only; selector and full_page are unavailable")
		}
		if len(global) != 2 || global[0] != "--screenshot-format" {
			return nil, fmt.Errorf("annotated extension screenshots are unavailable")
		}
		wire = []string{"screenshot", global[1]}
	}
	if err := m.hostBrowserCall(ctx, session, "command", wire, &result); err != nil {
		return nil, err
	}
	if !result.Success {
		return nil, fmt.Errorf("extension browser: %s", firstNonEmptyString(result.Error, "command failed"))
	}
	if command[0] == "screenshot" {
		encoded, _ := result.Data["image"].(string)
		prefix := "data:image/" + global[1] + ";base64,"
		if !strings.HasPrefix(encoded, prefix) {
			return nil, fmt.Errorf("invalid extension screenshot")
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(encoded, prefix))
		if err != nil || len(raw) == 0 || len(raw) > maxBrowserScreenshotBytes {
			return nil, fmt.Errorf("invalid extension screenshot size")
		}
		if err := os.WriteFile(command[1], raw, 0600); err != nil {
			return nil, err
		}
		result.Data = map[string]any{"path": command[1]}
	}
	for _, key := range []string{"text", "snapshot"} {
		if value, ok := result.Data[key].(string); ok && maxOutput > 0 {
			chars := []rune(value)
			if len(chars) > maxOutput {
				result.Data[key] = string(chars[:maxOutput]) + "\n[truncated]"
			}
		}
	}
	return json.Marshal(map[string]any{"success": true, "data": result.Data, "_boundary": map[string]any{"type": "untrusted_browser_content", "origin": session.currentURL, "nonce": rand.Text()}})
}
