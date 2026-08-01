package tools

// Preview publishing tools ported from the Helpin worker (tools_preview.go).
// They publish markdown or JSON preview panels as run-scoped artifacts using
// the same artifact envelope Helpin persists (artifact_type "run_preview",
// format "json", inline storage), so host projections mirror them directly.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const (
	toolNamePublishPreview  = "publish_preview"
	toolNamePreviewMarkdown = "preview_md"
	toolNamePreviewJSON     = "preview_json"

	// PreviewFormatMarkdown is the markdown preview panel format.
	PreviewFormatMarkdown = "markdown"
	// PreviewFormatJSON is the JSON preview panel format.
	PreviewFormatJSON = "json"

	// RunPreviewArtifactType matches Helpin's run preview artifact type.
	RunPreviewArtifactType = "run_preview"
)

// PublishedPreviewRequest is the raw input for preview publishing tools.
type PublishedPreviewRequest struct {
	Slot     string          `json:"slot,omitempty"`
	PanelKey string          `json:"panel_key"`
	Title    string          `json:"title"`
	Format   string          `json:"format"`
	Content  json.RawMessage `json:"content"`
	Replace  *bool           `json:"replace,omitempty"`
}

// PublishedPreview is the normalized preview payload persisted as an artifact.
type PublishedPreview struct {
	PanelKey string          `json:"panel_key"`
	Title    string          `json:"title"`
	Format   string          `json:"format"`
	Content  json.RawMessage `json:"content"`
	Replace  bool            `json:"replace"`
}

// RegisterArtifactPreviewTools registers the run-scoped preview publishing
// tools. They require an ArtifactWriter on the CallContext.
func RegisterArtifactPreviewTools(r *Registry) {
	if r == nil {
		return
	}
	r.Register(Definition{
		Name:        toolNamePublishPreview,
		Description: "Publish a structured preview panel in the interactive run drawer right pane. Use this for markdown drafts and JSON plans that should be reviewed separately from the main chat.",
		Category:    "Preview",
		Mutating:    false,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"panel_key": map[string]interface{}{"type": "string"},
				"title":     map[string]interface{}{"type": "string"},
				"format":    map[string]interface{}{"type": "string", "enum": []string{PreviewFormatMarkdown, PreviewFormatJSON}},
				"content": map[string]interface{}{
					"description": "Panel content. Use a string for markdown previews or any JSON value for json previews.",
					"oneOf": []map[string]interface{}{
						{"type": "string"},
						{"type": "object"},
						{"type": "array"},
						{"type": "number"},
						{"type": "boolean"},
						{"type": "null"},
					},
				},
				"replace": map[string]interface{}{"type": "boolean"},
			},
			"required":             []string{"panel_key", "title", "format", "content"},
			"additionalProperties": false,
		},
	}, toolPublishPreview)
	r.Register(Definition{
		Name:        toolNamePreviewMarkdown,
		Description: "Publish a markdown preview into a named review slot in the interactive run drawer right pane.",
		Category:    "Preview",
		Mutating:    false,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"slot":    map[string]interface{}{"type": "string"},
				"title":   map[string]interface{}{"type": "string"},
				"content": map[string]interface{}{"type": "string"},
				"replace": map[string]interface{}{"type": "boolean"},
			},
			"required":             []string{"slot", "content"},
			"additionalProperties": false,
		},
	}, toolPreviewMarkdown)
	r.Register(Definition{
		Name:        toolNamePreviewJSON,
		Description: "Publish a JSON preview into a named review slot in the interactive run drawer right pane.",
		Category:    "Preview",
		Mutating:    false,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"slot":  map[string]interface{}{"type": "string"},
				"title": map[string]interface{}{"type": "string"},
				"content": map[string]interface{}{
					"description": "Structured JSON content for the preview slot.",
					"oneOf": []map[string]interface{}{
						{"type": "object"},
						{"type": "array"},
						{"type": "number"},
						{"type": "boolean"},
						{"type": "null"},
						{"type": "string"},
					},
				},
				"replace": map[string]interface{}{"type": "boolean"},
			},
			"required":             []string{"slot", "content"},
			"additionalProperties": false,
		},
	}, toolPreviewJSON)
}

func toolPublishPreview(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	req, err := decodePublishedPreviewRequest(input)
	return executePreviewToolRequest(ctx, callCtx, toolNamePublishPreview, req, err)
}

func toolPreviewMarkdown(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	req, err := buildSlotPreviewRequest(input, PreviewFormatMarkdown)
	return executePreviewToolRequest(ctx, callCtx, toolNamePreviewMarkdown, req, err)
}

func toolPreviewJSON(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	req, err := buildSlotPreviewRequest(input, PreviewFormatJSON)
	return executePreviewToolRequest(ctx, callCtx, toolNamePreviewJSON, req, err)
}

func executePreviewToolRequest(ctx context.Context, callCtx CallContext, toolName string, req PublishedPreviewRequest, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, wrapPreviewToolError(toolName, err)
	}
	preview, err := normalizePublishedPreviewRequest(&req)
	if err != nil {
		return nil, wrapPreviewToolError(toolName, err)
	}
	if err := persistPublishedPreviewArtifact(ctx, callCtx, toolName, preview); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"status":    "published",
		"panel_key": preview.PanelKey,
		"title":     preview.Title,
		"format":    preview.Format,
		"replace":   preview.Replace,
	})
}

func persistPublishedPreviewArtifact(ctx context.Context, callCtx CallContext, toolName string, preview *PublishedPreview) error {
	if callCtx.ArtifactWriter == nil {
		return fmt.Errorf("%s requires an artifact writer for this run", toolName)
	}
	inline, err := json.Marshal(preview)
	if err != nil {
		return fmt.Errorf("marshal preview artifact: %w", err)
	}
	metadata, _ := json.Marshal(map[string]any{
		"internal":  false,
		"source":    toolName,
		"panel_key": preview.PanelKey,
	})
	if err := callCtx.ArtifactWriter.WriteArtifact(ctx, agentcore.AgentRunArtifact{
		ArtifactType:  RunPreviewArtifactType,
		Format:        "json",
		StorageMode:   "inline",
		InlineContent: string(inline),
		Metadata:      metadata,
	}); err != nil {
		return fmt.Errorf("persist preview artifact: %w", err)
	}
	return nil
}

func wrapPreviewToolError(toolName string, err error) error {
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(err.Error())
	switch {
	case strings.HasPrefix(message, "parse input:"):
		return fmt.Errorf("%s input must be valid JSON: %s", toolName, strings.TrimSpace(strings.TrimPrefix(message, "parse input:")))
	case message == "preview payload is required":
		return fmt.Errorf("%s input must be a JSON object", toolName)
	case message == "panel_key is required":
		switch toolName {
		case toolNamePreviewMarkdown, toolNamePreviewJSON:
			return fmt.Errorf("%s is missing slot; include \"slot\" to choose the preview panel", toolName)
		case toolNamePublishPreview:
			return fmt.Errorf("%s is missing panel_key; include \"panel_key\" or use preview_md/preview_json with \"slot\"", toolName)
		default:
			return fmt.Errorf("%s could not determine which preview panel to publish; include the expected content and title", toolName)
		}
	case message == "title is required":
		return fmt.Errorf("%s is missing title; include \"title\"", toolName)
	case message == "raw tool arguments wrapper is not supported":
		return fmt.Errorf("%s input must be a JSON object with structured fields; do not send a raw string wrapper", toolName)
	case message == "content is required", message == "markdown content is required":
		return fmt.Errorf("%s is missing content; %s", toolName, previewToolContentHint(toolName))
	case message == "markdown content must be a string":
		return fmt.Errorf("%s content must be a markdown string in \"content\"", toolName)
	case message == "json content must be valid JSON":
		return fmt.Errorf("%s content must be valid JSON in \"content\"", toolName)
	case strings.HasPrefix(message, "format must be"):
		return fmt.Errorf("%s format must be %q or %q", toolName, PreviewFormatMarkdown, PreviewFormatJSON)
	default:
		return err
	}
}

func previewToolContentHint(toolName string) string {
	switch toolName {
	case toolNamePreviewMarkdown:
		return "include markdown in \"content\""
	case toolNamePreviewJSON:
		return "include a JSON value in \"content\""
	case toolNamePublishPreview:
		return "include markdown or JSON in \"content\""
	default:
		return "include the preview body in \"content\""
	}
}

func normalizePublishedPreviewRequest(req *PublishedPreviewRequest) (*PublishedPreview, error) {
	if req == nil {
		return nil, fmt.Errorf("preview payload is required")
	}

	panelKey := normalizePreviewPanelKey(req.PanelKey)
	if panelKey == "" {
		panelKey = normalizePreviewPanelKey(req.Slot)
	}
	if panelKey == "" {
		return nil, fmt.Errorf("panel_key is required")
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = defaultPreviewTitle(panelKey)
	}
	if title == "" {
		return nil, fmt.Errorf("title is required")
	}

	format := normalizePreviewFormat(req.Format)
	if format == "" {
		format = inferPreviewFormat(req)
	}
	if format == "" {
		return nil, fmt.Errorf("format must be %q or %q", PreviewFormatMarkdown, PreviewFormatJSON)
	}

	if len(req.Content) == 0 || string(req.Content) == "null" {
		return nil, fmt.Errorf("content is required")
	}

	content, err := normalizePreviewContent(format, req.Content)
	if err != nil {
		return nil, err
	}

	replace := true
	if req.Replace != nil {
		replace = *req.Replace
	}

	return &PublishedPreview{
		PanelKey: panelKey,
		Title:    title,
		Format:   format,
		Content:  content,
		Replace:  replace,
	}, nil
}

type slotPreviewRequest struct {
	Slot    string          `json:"slot"`
	Title   string          `json:"title"`
	Content json.RawMessage `json:"content"`
	Replace *bool           `json:"replace,omitempty"`
}

func buildSlotPreviewRequest(input json.RawMessage, format string) (PublishedPreviewRequest, error) {
	input, err := unwrapRawToolArguments(input)
	if err != nil {
		return PublishedPreviewRequest{}, err
	}
	var req slotPreviewRequest
	if err := json.Unmarshal(input, &req); err != nil {
		return PublishedPreviewRequest{}, fmt.Errorf("parse input: %w", err)
	}
	raw := previewObjectMap(input)
	if strings.TrimSpace(req.Slot) == "" {
		var legacy PublishedPreviewRequest
		if err := json.Unmarshal(input, &legacy); err == nil && strings.TrimSpace(legacy.PanelKey) != "" {
			req.Slot = legacy.PanelKey
		}
	}
	if strings.TrimSpace(req.Slot) == "" {
		for _, key := range []string{"panel_key", "panelKey"} {
			if value, ok := raw[key]; ok {
				_ = json.Unmarshal(value, &req.Slot)
				if strings.TrimSpace(req.Slot) != "" {
					break
				}
			}
		}
	}
	if strings.TrimSpace(req.Title) == "" {
		if value, ok := raw["panelTitle"]; ok {
			_ = json.Unmarshal(value, &req.Title)
		}
	}
	if len(req.Content) == 0 {
		req.Content = extractPreviewContent(raw, format)
	}
	return PublishedPreviewRequest{
		Slot:    req.Slot,
		Title:   req.Title,
		Format:  format,
		Content: req.Content,
		Replace: req.Replace,
	}, nil
}

func previewObjectMap(input json.RawMessage) map[string]json.RawMessage {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input, &raw); err != nil {
		return nil
	}
	return raw
}

func extractPreviewContent(raw map[string]json.RawMessage, format string) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}

	for _, key := range []string{"content", "body", "markdown", "text"} {
		if value, ok := raw[key]; ok && len(value) != 0 && string(value) != "null" {
			return append(json.RawMessage(nil), value...)
		}
	}

	for _, nestedKey := range []string{"preview", "payload"} {
		nested, ok := raw[nestedKey]
		if !ok || len(nested) == 0 || string(nested) == "null" {
			continue
		}
		nestedRaw := previewObjectMap(nested)
		if content := extractPreviewContent(nestedRaw, format); len(content) != 0 {
			return content
		}
		if format == PreviewFormatJSON {
			if stripped := stripPreviewMetaFields(nestedRaw); len(stripped) != 0 {
				return stripped
			}
		}
	}

	if format == PreviewFormatJSON {
		if stripped := stripPreviewMetaFields(raw); len(stripped) != 0 {
			return stripped
		}
	}

	return nil
}

func stripPreviewMetaFields(raw map[string]json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	metaKeys := map[string]bool{
		"slot":           true,
		"panel_key":      true,
		"panelKey":       true,
		"title":          true,
		"panelTitle":     true,
		"format":         true,
		"preview_format": true,
		"replace":        true,
		"content":        true,
		"body":           true,
		"markdown":       true,
		"text":           true,
		"raw":            true,
		"preview":        true,
		"payload":        true,
	}
	filtered := make(map[string]json.RawMessage)
	for key, value := range raw {
		if metaKeys[key] {
			continue
		}
		filtered[key] = value
	}
	if len(filtered) == 0 {
		return nil
	}
	normalized, _ := json.Marshal(filtered)
	return normalized
}

func isRawToolArgumentsWrapper(raw map[string]json.RawMessage) bool {
	if len(raw) != 1 {
		return false
	}
	value, ok := raw["raw"]
	if !ok || len(value) == 0 || string(value) == "null" {
		return false
	}
	var text string
	return json.Unmarshal(value, &text) == nil && strings.TrimSpace(text) != ""
}

func unwrapRawToolArguments(input json.RawMessage) (json.RawMessage, error) {
	raw := previewObjectMap(input)
	if !isRawToolArgumentsWrapper(raw) {
		return input, nil
	}
	var wrapped string
	if err := json.Unmarshal(raw["raw"], &wrapped); err != nil {
		return nil, fmt.Errorf("raw tool arguments wrapper is not supported")
	}
	wrapped = strings.TrimSpace(wrapped)
	if wrapped == "" || !json.Valid([]byte(wrapped)) {
		return nil, fmt.Errorf("raw tool arguments wrapper is not supported")
	}
	if wrapped[0] != '{' {
		return nil, fmt.Errorf("raw tool arguments wrapper is not supported")
	}
	return json.RawMessage(wrapped), nil
}

func normalizePreviewPanelKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizePreviewFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case PreviewFormatMarkdown:
		return PreviewFormatMarkdown
	case PreviewFormatJSON:
		return PreviewFormatJSON
	default:
		return ""
	}
}

func decodePublishedPreviewRequest(input json.RawMessage) (PublishedPreviewRequest, error) {
	var req PublishedPreviewRequest
	if err := json.Unmarshal(input, &req); err != nil {
		return PublishedPreviewRequest{}, fmt.Errorf("parse input: %w", err)
	}
	if strings.TrimSpace(req.PanelKey) != "" {
		return req, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input, &raw); err != nil {
		return req, nil
	}

	if alias, ok := raw["panelKey"]; ok && strings.TrimSpace(req.PanelKey) == "" {
		_ = json.Unmarshal(alias, &req.PanelKey)
	}
	if strings.TrimSpace(req.Title) == "" {
		if alias, ok := raw["panelTitle"]; ok {
			_ = json.Unmarshal(alias, &req.Title)
		}
	}
	if strings.TrimSpace(req.Format) == "" {
		if alias, ok := raw["preview_format"]; ok {
			_ = json.Unmarshal(alias, &req.Format)
		}
	}
	if len(req.Content) == 0 {
		if alias, ok := raw["body"]; ok {
			req.Content = append(json.RawMessage(nil), alias...)
		}
	}

	if strings.TrimSpace(req.PanelKey) != "" {
		return req, nil
	}

	for _, nestedKey := range []string{"preview", "payload"} {
		nested, ok := raw[nestedKey]
		if !ok || len(nested) == 0 || string(nested) == "null" {
			continue
		}
		nestedReq, err := decodePublishedPreviewRequest(nested)
		if err == nil && strings.TrimSpace(nestedReq.PanelKey) != "" {
			return nestedReq, nil
		}
	}

	return req, nil
}

func normalizePreviewContent(format string, raw json.RawMessage) (json.RawMessage, error) {
	switch format {
	case PreviewFormatMarkdown:
		var content string
		if err := json.Unmarshal(raw, &content); err != nil {
			return nil, fmt.Errorf("markdown content must be a string")
		}
		if strings.TrimSpace(content) == "" {
			return nil, fmt.Errorf("markdown content is required")
		}
		normalized, _ := json.Marshal(content)
		return normalized, nil
	case PreviewFormatJSON:
		var content any
		if err := json.Unmarshal(raw, &content); err != nil {
			return nil, fmt.Errorf("json content must be valid JSON")
		}
		normalized, _ := json.Marshal(content)
		return normalized, nil
	default:
		return nil, fmt.Errorf("unsupported preview format %q", format)
	}
}

func inferPreviewFormat(req *PublishedPreviewRequest) string {
	if req == nil || len(req.Content) == 0 || string(req.Content) == "null" {
		return ""
	}

	var text string
	if err := json.Unmarshal(req.Content, &text); err == nil {
		if strings.TrimSpace(text) != "" {
			return PreviewFormatMarkdown
		}
	}

	var content any
	if err := json.Unmarshal(req.Content, &content); err == nil {
		return PreviewFormatJSON
	}

	return ""
}

func defaultPreviewTitle(panelKey string) string {
	switch normalizePreviewPanelKey(panelKey) {
	case "prd_draft":
		return "PRD Draft"
	case "task_plan":
		return "Task Plan"
	case "task_plan_doc":
		return "Task Planning Document"
	default:
		return ""
	}
}
