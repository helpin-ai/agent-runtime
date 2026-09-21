package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type analysisUploaderKey struct{}

// RegisterAnalysisTools shares the app's private binary artifact callback with
// browser tools. No browser or browser credentials are needed for analysis.
func RegisterAnalysisTools(r *Registry, cfg BrowserToolsConfig) {
	registerAnalysisTools(r, cfg)
}

// registerAnalysisTools returns the app-scoped pack so tests can observe that
// its per-run state is released through the registry's run closers.
func registerAnalysisTools(r *Registry, cfg BrowserToolsConfig) *workspaceToolPack {
	if r == nil {
		return nil
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	manager := &BrowserManager{cfg: cfg}
	pack := newWorkspaceToolPack()
	// The app-scoped pack keys state by app/run like the global one; without a
	// closer it would retain every run's state for the worker's lifetime.
	r.RegisterRunCloser(pack)
	r.Register(runPythonDefinition(), func(ctx context.Context, call CallContext, input json.RawMessage) (json.RawMessage, error) {
		return pack.runPython(context.WithValue(ctx, analysisUploaderKey{}, manager), call, input)
	})
	r.Register(workspaceToolDefinition("publish_outputs", "Publish explicitly selected workspace CSV, PNG, JSON or text files as durable private downloads. Maximum 10 files, 10 MiB per file and 25 MiB total; artifacts survive run workspace deletion.", true, map[string]any{"type": "object", "properties": map[string]any{"paths": map[string]any{"type": "array", "minItems": 1, "maxItems": 10, "items": map[string]any{"type": "string"}}}, "required": []string{"paths"}, "additionalProperties": false}), func(ctx context.Context, call CallContext, input json.RawMessage) (json.RawMessage, error) {
		var args struct {
			Paths []string `json:"paths"`
		}
		if err := decodeStrictWorkspaceInput(input, &args); err != nil {
			return nil, err
		}
		if len(args.Paths) == 0 {
			return nil, fmt.Errorf("paths is required")
		}
		root, err := requireWorkspaceRoot(call, "publish_outputs")
		if err != nil {
			return nil, err
		}
		assets, err := publishAnalysisOutputs(context.WithValue(ctx, analysisUploaderKey{}, manager), call, root, args.Paths)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"artifacts": assets})
	})
	return pack
}

// analysisUploader returns the app's private artifact uploader, or a clear
// error when the app has no artifact provider configured.
func analysisUploader(ctx context.Context) (*BrowserManager, error) {
	manager, ok := ctx.Value(analysisUploaderKey{}).(*BrowserManager)
	if !ok || manager == nil {
		return nil, fmt.Errorf("private artifact storage is not configured for this app; output_paths and publish_outputs are unavailable")
	}
	return manager, nil
}

func publishAnalysisOutputs(ctx context.Context, call CallContext, root string, paths []string) ([]*browserAsset, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > 10 {
		return nil, fmt.Errorf("at most 10 output files may be published")
	}
	manager, err := analysisUploader(ctx)
	if err != nil {
		return nil, err
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	uploads := make([]browserArtifactUpload, 0, len(paths))
	var total int64
	for _, path := range paths {
		if !filepath.IsLocal(path) {
			return nil, fmt.Errorf("output path must be relative and beneath the workspace")
		}
		for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
			if segment == ".." {
				return nil, fmt.Errorf("output path traversal is not allowed")
			}
		}
		contentType := map[string]string{".csv": "text/csv", ".png": "image/png", ".json": "application/json", ".txt": "text/plain"}[strings.ToLower(filepath.Ext(path))]
		if contentType == "" {
			return nil, fmt.Errorf("output must be CSV, PNG, JSON or text")
		}
		// OpenRoot prevents escaping symlinks even if a background process races
		// the open. NONBLOCK prevents a FIFO from blocking before the regular-file check.
		file, err := directory.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("open selected output: %w", err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 10<<20 {
			return nil, fmt.Errorf("output must be a regular file between 1 byte and 10 MiB")
		}
		total += info.Size()
		if total > 25<<20 {
			return nil, fmt.Errorf("outputs exceed 25 MiB total")
		}
		uploads = append(uploads, browserArtifactUpload{File: file, FileName: filepath.Base(path), ArtifactType: "analysis_output", ContentType: contentType, Size: info.Size(), MaxBytes: 10 << 20})
	}
	assets := make([]*browserAsset, 0, len(uploads))
	for _, upload := range uploads {
		asset, err := manager.uploadAsset(ctx, &browserRunSession{appID: call.AppID, runID: call.RunID}, upload)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	return assets, nil
}
