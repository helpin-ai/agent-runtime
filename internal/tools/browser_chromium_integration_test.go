package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLocalChromiumBrowserIntegration(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_CHROMIUM_INTEGRATION") != "1" {
		t.Skip("set AGENT_RUNTIME_CHROMIUM_INTEGRATION=1 to use a real local Chromium browser")
	}
	var uploadedRecording []byte
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxBrowserRecordingBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		uploadedRecording, _ = io.ReadAll(file)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"local-recording","artifact_ref":"integration://artifacts/local-recording","visibility":"private","file_name":"local-chromium.mp4","content_type":"video/mp4"}`))
	}))
	defer uploader.Close()
	registry := NewRegistry()
	RegisterBrowserTools(registry, BrowserToolsConfig{
		Enabled: true, AppID: "chromium-integration", AllowedDomains: []string{"example.com"},
		ArtifactUploadURL: uploader.URL,
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
	if _, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"start","name":"local-chromium","max_duration_seconds":10}`)); err != nil {
		t.Fatalf("start local Chromium recording: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_act", json.RawMessage(`{"action":"scroll","value":"down","amount":100}`)); err != nil {
		t.Fatalf("browser_act during local Chromium recording: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	recording, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"stop"}`))
	if err != nil {
		t.Fatalf("stop local Chromium recording: %v", err)
	}
	if !strings.Contains(string(recording), `"format":"mp4"`) || len(uploadedRecording) < 12 || string(uploadedRecording[4:8]) != "ftyp" {
		t.Fatalf("unexpected local Chromium recording output=%s bytes=%d", recording, len(uploadedRecording))
	}
}
