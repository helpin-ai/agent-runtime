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

func TestKernelBrowserRecordingSmartTrimIntegration(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_KERNEL_INTEGRATION") != "1" {
		t.Skip("set AGENT_RUNTIME_KERNEL_INTEGRATION=1 to use a real Kernel browser and replay")
	}
	var uploaded []byte
	uploader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxBrowserRecordingBytes); err != nil {
			t.Errorf("parse upload: %v", err)
			http.Error(w, "invalid upload", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("read uploaded recording: %v", err)
			http.Error(w, "missing recording", http.StatusBadRequest)
			return
		}
		defer file.Close()
		uploaded, _ = io.ReadAll(file)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifact_id":"kernel-integration-recording","artifact_ref":"integration://artifacts/kernel-integration-recording","visibility":"private","file_name":"kernel-smart-trim.mp4","content_type":"video/mp4"}`))
	}))
	defer uploader.Close()

	cfg := BrowserToolsConfigFromEnv()
	if !cfg.Enabled || cfg.Kernel == nil {
		t.Fatal("Kernel browser integration requires AGENT_RUNTIME_BROWSER_ENABLED=true and KERNEL_API_KEY")
	}
	cfg.AppID = "kernel-integration"
	cfg.AllowedDomains = []string{"example.com"}
	cfg.ArtifactUploadURL = uploader.URL
	registry := NewRegistry()
	RegisterBrowserTools(registry, cfg)
	callCtx := browserTestCallContextForApp("kernel-integration", "smart-trim")
	defer func() {
		if err := registry.CloseRun(context.Background(), callCtx.AppID, callCtx.RunID); err != nil {
			t.Errorf("close Kernel integration browser: %v", err)
		}
	}()

	if _, err := registry.Execute(context.Background(), callCtx, "browser_open", json.RawMessage(`{"url":"https://example.com","wait_ms":0}`)); err != nil {
		t.Fatalf("browser_open: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"start","name":"kernel-smart-trim","max_duration_seconds":30}`)); err != nil {
		t.Fatalf("start recording: %v", err)
	}
	time.Sleep(3 * time.Second)
	if _, err := registry.Execute(context.Background(), callCtx, "browser_act", json.RawMessage(`{"action":"scroll","value":"down","amount":200}`)); err != nil {
		t.Fatalf("browser_act: %v", err)
	}
	time.Sleep(3 * time.Second)
	output, err := registry.Execute(context.Background(), callCtx, "browser_record", json.RawMessage(`{"action":"stop"}`))
	if err != nil {
		t.Fatalf("stop recording: %v", err)
	}
	if !strings.Contains(string(output), `"smart_trimmed":true`) || !strings.Contains(string(output), `"trim_status":"trimmed"`) {
		t.Fatalf("Kernel recording was not smart trimmed: %s", output)
	}
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode browser_record output: %v", err)
	}
	rawDuration, _ := result["raw_duration_ms"].(float64)
	outputDuration, _ := result["output_duration_ms"].(float64)
	if rawDuration <= 0 || outputDuration <= 0 || outputDuration >= rawDuration {
		t.Fatalf("expected smart trim to reduce playback duration: raw=%v output=%v", rawDuration, outputDuration)
	}
	if len(uploaded) < 12 || string(uploaded[4:8]) != "ftyp" {
		t.Fatalf("uploaded smart-trimmed recording is not an MP4: %d bytes", len(uploaded))
	}
}
