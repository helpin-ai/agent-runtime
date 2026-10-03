package memory

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Hindsight is the reference backend, using the upstream HTTP API without
// requiring the Python service in the local implementation's dependency graph.
type Hindsight struct {
	baseURL, apiKey string
	client          *http.Client
	count           TokenCounter
}

func NewHindsight(baseURL, apiKey string, client *http.Client) (*Hindsight, error) {
	if err := validateBaseURL(baseURL); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Hindsight{strings.TrimRight(baseURL, "/"), apiKey, client, countTokens}, nil
}

// BankName maps the complete host scope to an upstream bank, preventing two
// apps with the same human-readable bank ID sharing memory accidentally.
func BankName(scope Scope) string { return "agent-runtime-" + stableID(scope.AppID, scope.BankID) }

func (h *Hindsight) endpoint(scope Scope, path string) string {
	return h.baseURL + "/v1/default/banks/" + BankName(scope) + path
}

func (h *Hindsight) Retain(ctx context.Context, scope Scope, req RetainRequest) (RetainResult, error) {
	result := RetainResult{DocumentID: req.DocumentID, FactCount: -1}
	if err := scope.Validate(); err != nil {
		return result, err
	}
	if err := req.validate(); err != nil {
		return result, err
	}
	metadata := map[string]string{}
	for k, v := range req.Metadata {
		metadata[k] = v
	}
	metadata["agent_runtime_document_id"] = req.DocumentID
	item := map[string]any{"content": req.Content, "document_id": stableID(req.DocumentID), "context": req.Context, "metadata": metadata, "update_mode": "replace"}
	if !req.Timestamp.IsZero() {
		item["timestamp"] = req.Timestamp.Format(time.RFC3339Nano)
	}
	var response struct {
		Success bool `json:"success"`
		Async   bool `json:"async"`
	}
	err := jsonHTTP(ctx, h.client, http.MethodPost, h.endpoint(scope, "/memories"), h.apiKey, map[string]any{"items": []any{item}, "async": false}, &response)
	if err != nil {
		return result, err
	}
	if !response.Success || response.Async {
		return result, fmt.Errorf("Hindsight retain was not completed synchronously")
	}
	// Upstream retain reports item count, not extracted fact count.
	return result, nil
}

func (h *Hindsight) Recall(ctx context.Context, scope Scope, req RecallRequest) (RecallResult, error) {
	if err := scope.Validate(); err != nil {
		return RecallResult{}, err
	}
	req, err := req.normalized()
	if err != nil {
		return RecallResult{}, err
	}
	body := map[string]any{"query": req.Query, "types": req.Types, "max_tokens": req.MaxTokens, "budget": "high", "trace": true}
	include := map[string]any{"entities": nil}
	if req.IncludeChunks {
		include["chunks"] = map[string]any{"max_tokens": req.MaxChunkTokens}
	}
	body["include"] = include
	if req.TemporalWindow != nil {
		body["temporal_window"] = req.TemporalWindow
	}
	if !req.QueryTimestamp.IsZero() {
		body["query_timestamp"] = req.QueryTimestamp.Format(time.RFC3339Nano)
	}
	var response struct {
		Chunks  map[string]SourceChunk `json:"chunks"`
		Results []struct {
			ID            string            `json:"id"`
			ChunkID       string            `json:"chunk_id"`
			Text          string            `json:"text"`
			Type          string            `json:"type"`
			Entities      []string          `json:"entities"`
			Context       string            `json:"context"`
			DocumentID    string            `json:"document_id"`
			Metadata      map[string]string `json:"metadata"`
			MentionedAt   *string           `json:"mentioned_at"`
			OccurredStart *string           `json:"occurred_start"`
			OccurredEnd   *string           `json:"occurred_end"`
		} `json:"results"`
	}
	if err = jsonHTTP(ctx, h.client, http.MethodPost, h.endpoint(scope, "/memories/recall"), h.apiKey, body, &response); err != nil {
		return RecallResult{}, err
	}
	if response.Results == nil {
		return RecallResult{}, fmt.Errorf("Hindsight recall response is missing results")
	}
	results := make([]Result, 0, len(response.Results))
	for _, wire := range response.Results {
		start, e := parseDate(wire.OccurredStart)
		if e != nil {
			return RecallResult{}, e
		}
		end, e := parseDate(wire.OccurredEnd)
		if e != nil {
			return RecallResult{}, e
		}
		mentioned, e := parseDate(wire.MentionedAt)
		if e != nil {
			return RecallResult{}, e
		}
		f := Fact{ID: wire.ID, ChunkID: wire.ChunkID, Text: wire.Text, Type: wire.Type, Entities: wire.Entities, DocumentID: wire.DocumentID, Context: wire.Context, Metadata: wire.Metadata, OccurredStart: start, OccurredEnd: end}
		if original := wire.Metadata["agent_runtime_document_id"]; original != "" {
			f.DocumentID = original
		}
		if mentioned != nil {
			f.MentionedAt = *mentioned
		}
		results = append(results, Result{Fact: f})
	}
	// Upstream does not expose a stable unified score. Preserve its result order;
	// do not manufacture scores or claim a particular server reranker was used.
	result, err := budgetResults(results, req, h.count, false)
	if err == nil && req.IncludeChunks {
		for _, fact := range results {
			if chunk, ok := response.Chunks[fact.ChunkID]; ok {
				chunk.ID = fact.ChunkID
				chunk.DocumentID = fact.DocumentID
				response.Chunks[fact.ChunkID] = chunk
			}
		}
		err = includeChunks(&result, response.Chunks, req.MaxChunkTokens, h.count)
	}
	result.Reranked = nil
	return result, err
}

func (h *Hindsight) Forget(ctx context.Context, scope Scope, documentID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(documentID) == "" || len(documentID) > 512 {
		return fmt.Errorf("document ID is required")
	}
	return jsonHTTP(ctx, h.client, http.MethodDelete, h.endpoint(scope, "/documents/"+stableID(documentID)), h.apiKey, nil, nil)
}
