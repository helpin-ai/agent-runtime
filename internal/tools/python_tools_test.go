package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestPythonRetainsFilesAndPrivateEnvironment(t *testing.T) {
	registry, call := workspaceToolTestRegistry(t)
	for _, source := range []string{`import pathlib,sys,os; assert sys.prefix != sys.base_prefix; pathlib.Path('data.txt').write_text('retained'); import site; (pathlib.Path(site.getsitepackages()[0])/'run_dependency.py').write_text('value = 42'); print(os.environ['PIP_CACHE_DIR'])`, `import pathlib,run_dependency; assert run_dependency.value == 42; print(pathlib.Path('data.txt').read_text())`} {
		input, _ := json.Marshal(map[string]any{"source": source})
		output, err := registry.Execute(context.Background(), call, "run_python", input)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(output), "run") && !strings.Contains(string(output), "retained") {
			t.Fatalf("unexpected output %s", output)
		}
	}
	for _, input := range []string{`{"source":"pass","timeout_seconds":301}`, `{"source":"pass","unknown":true}`, `{"source":""}`} {
		if _, err := registry.Execute(context.Background(), call, "run_python", json.RawMessage(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestAnalysisPrivateOutputSelection(t *testing.T) {
	uploads := 0
	var stored []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer host-secret" {
			t.Error("missing host auth")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("artifact_type") != "analysis_output" {
			t.Error("wrong artifact type")
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		stored, err = io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Error(err)
		}
		uploads++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"artifact_id":"private-1","artifact_ref":"host-artifact://private-1","visibility":"private"}`))
	}))
	defer server.Close()
	registry, call := workspaceToolTestRegistry(t)
	RegisterAnalysisTools(registry, BrowserToolsConfig{ArtifactUploadURL: server.URL, ArtifactUploadToken: "host-secret"})
	root := call.Run.WorkspaceLease.RootPath
	if err := os.WriteFile(filepath.Join(root, "result.csv"), []byte("a,b\n1,2"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Execute(context.Background(), call, "publish_outputs", json.RawMessage(`{"paths":["result.csv"]}`)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("private"), 0600)
	os.Symlink(outside, filepath.Join(root, "escape.txt"))
	syscall.Mkfifo(filepath.Join(root, "pipe.txt"), 0600)
	for _, path := range []string{"../secret.txt", outside, "escape.txt", "pipe.txt", "."} {
		input, _ := json.Marshal(map[string]any{"paths": []string{path}})
		if _, err := registry.Execute(context.Background(), call, "publish_outputs", input); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	if uploads != 1 {
		t.Fatalf("unexpected uploads: %d", uploads)
	}
	file, err := os.Create(filepath.Join(root, "large.txt"))
	if err != nil {
		t.Fatal(err)
	}
	file.Truncate(workspace.ScratchLimitBytes + 1)
	file.Close()
	if err := workspace.CheckScratchSize(root); err == nil {
		t.Fatal("oversized scratch accepted")
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if string(stored) != "a,b\n1,2" {
		t.Fatal("published artifact depended on scratch path")
	}
}
