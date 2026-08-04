package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type capturingArtifactWriter struct {
	artifacts []agentcore.AgentRunArtifact
}

func (w *capturingArtifactWriter) WriteArtifact(_ context.Context, artifact agentcore.AgentRunArtifact) error {
	w.artifacts = append(w.artifacts, artifact)
	return nil
}

func previewToolTestRegistry(t *testing.T) (*Registry, CallContext, *capturingArtifactWriter) {
	t.Helper()
	registry, callCtx := workspaceToolTestRegistry(t)
	writer := &capturingArtifactWriter{}
	callCtx.ArtifactWriter = writer
	return registry, callCtx, writer
}

func TestPreviewToolsRegisteredAsNonMutating(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"publish_preview", "preview_md", "preview_json", "publish_task_plan"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s to be registered", name)
		}
		if def.Mutating {
			t.Fatalf("expected %s to be non-mutating", name)
		}
		if def.Category != "Preview" {
			t.Fatalf("expected %s category Preview, got %q", name, def.Category)
		}
	}
}

func TestPublishTaskPlanUsesStrictStructuredContract(t *testing.T) {
	registry, callCtx, writer := previewToolTestRegistry(t)
	input := `{
		"content": {
			"summary": "Ship the feature safely",
			"proposed_tasks": [
				{
					"ref": "task_1",
					"name": "Add the feature",
					"description": "Implement the bounded feature slice.",
					"task_type": "feature",
					"acceptance_criteria": ["GIVEN valid input WHEN submitted THEN it succeeds"],
					"dependency_refs": []
				}
			]
		}
	}`
	if _, err := registry.Execute(context.Background(), callCtx, "publish_task_plan", json.RawMessage(input)); err != nil {
		t.Fatalf("publish_task_plan returned error: %v", err)
	}
	if len(writer.artifacts) != 1 {
		t.Fatalf("expected one preview artifact, got %d", len(writer.artifacts))
	}
	var preview PublishedPreview
	if err := json.Unmarshal([]byte(writer.artifacts[0].InlineContent), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.PanelKey != "task_plan" || preview.Title != "Task Plan" || preview.Format != PreviewFormatJSON {
		t.Fatalf("unexpected task plan preview: %+v", preview)
	}
	if !strings.Contains(string(preview.Content), `"proposed_tasks"`) {
		t.Fatalf("structured content was not preserved: %s", preview.Content)
	}
}

func TestPublishTaskPlanRejectsMalformedPlansBeforePersistence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "encoded string", content: `"{\"summary\":\"x\"}"`, wantErr: "JSON object"},
		{name: "missing tasks", content: `{"summary":"x","proposed_tasks":[]}`, wantErr: "at least one"},
		{name: "wrong title field", content: `{"summary":"x","proposed_tasks":[{"title":"Wrong","description":"d","task_type":"feature","acceptance_criteria":["a"],"dependency_refs":[]}]}`, wantErr: ".name is required"},
		{name: "unknown dependency", content: `{"summary":"x","proposed_tasks":[{"ref":"task_1","name":"One","description":"d","task_type":"feature","acceptance_criteria":["a"],"dependency_refs":["task_2"]}]}`, wantErr: "unknown dependency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, callCtx, writer := previewToolTestRegistry(t)
			input := json.RawMessage(`{"content":` + tc.content + `}`)
			_, err := registry.Execute(context.Background(), callCtx, "publish_task_plan", input)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
			if len(writer.artifacts) != 0 {
				t.Fatalf("invalid plan persisted %d artifacts", len(writer.artifacts))
			}
		})
	}
}

func TestPreviewToolsPublishRunPreviewArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tool         string
		input        string
		wantPanelKey string
		wantTitle    string
		wantFormat   string
		wantContent  string
		wantReplace  bool
	}{
		{
			name:         "publish_preview markdown",
			tool:         "publish_preview",
			input:        `{"panel_key":"draft","title":"Draft","format":"markdown","content":"# Hello"}`,
			wantPanelKey: "draft",
			wantTitle:    "Draft",
			wantFormat:   "markdown",
			wantContent:  `"# Hello"`,
			wantReplace:  true,
		},
		{
			name:         "publish_preview infers json format",
			tool:         "publish_preview",
			input:        `{"panel_key":"plan","title":"Plan","content":{"steps":[1,2]}}`,
			wantPanelKey: "plan",
			wantTitle:    "Plan",
			wantFormat:   "json",
			wantContent:  `{"steps":[1,2]}`,
			wantReplace:  true,
		},
		{
			name:         "preview_md slot",
			tool:         "preview_md",
			input:        `{"slot":"Notes","title":"Notes","content":"body text"}`,
			wantPanelKey: "notes",
			wantTitle:    "Notes",
			wantFormat:   "markdown",
			wantContent:  `"body text"`,
			wantReplace:  true,
		},
		{
			name:         "preview_md accepts panel_key alias and body content",
			tool:         "preview_md",
			input:        `{"panel_key":"alias_slot","title":"Alias","body":"aliased content"}`,
			wantPanelKey: "alias_slot",
			wantTitle:    "Alias",
			wantFormat:   "markdown",
			wantContent:  `"aliased content"`,
			wantReplace:  true,
		},
		{
			name:         "preview_json slot with replace false",
			tool:         "preview_json",
			input:        `{"slot":"data","title":"Data","content":{"a":1},"replace":false}`,
			wantPanelKey: "data",
			wantTitle:    "Data",
			wantFormat:   "json",
			wantContent:  `{"a":1}`,
			wantReplace:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, callCtx, writer := previewToolTestRegistry(t)
			output, err := registry.Execute(context.Background(), callCtx, tc.tool, json.RawMessage(tc.input))
			if err != nil {
				t.Fatalf("%s returned error: %v", tc.tool, err)
			}
			var status struct {
				Status   string `json:"status"`
				PanelKey string `json:"panel_key"`
				Format   string `json:"format"`
			}
			if err := json.Unmarshal(output, &status); err != nil {
				t.Fatalf("decode output: %v; raw=%s", err, string(output))
			}
			if status.Status != "published" || status.PanelKey != tc.wantPanelKey || status.Format != tc.wantFormat {
				t.Fatalf("unexpected status %+v", status)
			}
			if len(writer.artifacts) != 1 {
				t.Fatalf("expected 1 artifact, got %d", len(writer.artifacts))
			}
			artifact := writer.artifacts[0]
			if artifact.ArtifactType != RunPreviewArtifactType || artifact.Format != "json" || artifact.StorageMode != "inline" {
				t.Fatalf("unexpected artifact envelope %+v", artifact)
			}
			var preview PublishedPreview
			if err := json.Unmarshal([]byte(artifact.InlineContent), &preview); err != nil {
				t.Fatalf("decode artifact content: %v", err)
			}
			if preview.PanelKey != tc.wantPanelKey || preview.Title != tc.wantTitle || preview.Format != tc.wantFormat || preview.Replace != tc.wantReplace {
				t.Fatalf("unexpected preview payload %+v", preview)
			}
			if string(preview.Content) != tc.wantContent {
				t.Fatalf("unexpected preview content %s, want %s", string(preview.Content), tc.wantContent)
			}
		})
	}
}

func TestPreviewToolsInputErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tool    string
		input   string
		wantErr string
	}{
		{
			name:    "publish_preview missing panel_key",
			tool:    "publish_preview",
			input:   `{"title":"T","format":"markdown","content":"x"}`,
			wantErr: "missing panel_key",
		},
		{
			name:    "preview_md missing slot",
			tool:    "preview_md",
			input:   `{"title":"T","content":"x"}`,
			wantErr: "missing slot",
		},
		{
			name:    "preview_md missing content",
			tool:    "preview_md",
			input:   `{"slot":"s","title":"T"}`,
			wantErr: "missing content",
		},
		{
			name:    "preview_md non-string content",
			tool:    "preview_md",
			input:   `{"slot":"s","title":"T","content":{"a":1}}`,
			wantErr: "content must be a markdown string",
		},
		{
			name:    "preview_json missing title for unknown slot",
			tool:    "preview_json",
			input:   `{"slot":"custom","content":{"a":1}}`,
			wantErr: "missing title",
		},
		{
			name:    "preview_md raw wrapper rejected when not json object",
			tool:    "preview_md",
			input:   `{"raw":"not-json"}`,
			wantErr: "raw string wrapper",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, callCtx, writer := previewToolTestRegistry(t)
			_, err := registry.Execute(context.Background(), callCtx, tc.tool, json.RawMessage(tc.input))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
			if len(writer.artifacts) != 0 {
				t.Fatalf("expected no artifacts on error, got %d", len(writer.artifacts))
			}
		})
	}
}

func TestPreviewToolsRequireArtifactWriter(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	callCtx.ArtifactWriter = nil
	_, err := registry.Execute(context.Background(), callCtx, "preview_md", json.RawMessage(`{"slot":"s","title":"T","content":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "artifact writer") {
		t.Fatalf("expected artifact writer error, got %v", err)
	}
}

func TestPreviewMarkdownUnwrapsRawArguments(t *testing.T) {
	registry, callCtx, writer := previewToolTestRegistry(t)
	input := `{"raw":"{\"slot\":\"wrapped\",\"title\":\"Wrapped\",\"content\":\"inner\"}"}`
	if _, err := registry.Execute(context.Background(), callCtx, "preview_md", json.RawMessage(input)); err != nil {
		t.Fatalf("preview_md returned error: %v", err)
	}
	if len(writer.artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(writer.artifacts))
	}
	var preview PublishedPreview
	if err := json.Unmarshal([]byte(writer.artifacts[0].InlineContent), &preview); err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	if preview.PanelKey != "wrapped" || string(preview.Content) != `"inner"` {
		t.Fatalf("unexpected preview %+v", preview)
	}
}
