// Package memoryconfig wires optional memory into runtime entrypoints.
package memoryconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/memory"
)

// Open returns nil when disabled. SQLite requires an explicit path; inference
// configuration is independent of the agent's chat model and credentials.
func Open(ctx context.Context, getenv func(string) string) (memory.Backend, func() error, error) {
	close := func() error { return nil }
	driver := strings.TrimSpace(getenv("AGENT_RUNTIME_MEMORY_BACKEND"))
	var client *http.Client
	if driver != "" && driver != "disabled" {
		if raw := getenv("AGENT_RUNTIME_MEMORY_MODEL_TIMEOUT"); raw != "" {
			duration, err := time.ParseDuration(raw)
			if err != nil || duration <= 0 || duration > time.Hour {
				return nil, close, fmt.Errorf("memory model timeout must be a positive duration of at most 1h")
			}
			client = &http.Client{Timeout: duration}
		}
	}
	switch driver {
	case "", "disabled":
		return nil, close, nil
	case "hindsight":
		b, err := memory.NewHindsight(getenv("AGENT_RUNTIME_MEMORY_HINDSIGHT_URL"), getenv("AGENT_RUNTIME_MEMORY_HINDSIGHT_API_KEY"), client)
		return b, close, err
	case "sqlite":
		dimensions := 0
		if raw := getenv("AGENT_RUNTIME_MEMORY_EMBEDDING_DIMENSIONS"); raw != "" {
			var err error
			dimensions, err = strconv.Atoi(raw)
			if err != nil || dimensions < 1 || dimensions > 65536 {
				return nil, close, fmt.Errorf("memory embedding dimensions must be 1–65536")
			}
		}
		var options map[string]any
		if raw := getenv("AGENT_RUNTIME_MEMORY_EXTRACTION_OPTIONS"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &options); err != nil || options == nil {
				return nil, close, fmt.Errorf("memory extraction options must be a JSON object")
			}
		}
		p, err := memory.NewProvider(memory.ProviderConfig{BaseURL: getenv("AGENT_RUNTIME_MEMORY_MODEL_URL"), APIKey: modelAPIKey(getenv), ExtractionModel: getenv("AGENT_RUNTIME_MEMORY_EXTRACTION_MODEL"), EmbeddingModel: getenv("AGENT_RUNTIME_MEMORY_EMBEDDING_MODEL"), EmbeddingModelID: getenv("AGENT_RUNTIME_MEMORY_EMBEDDING_MODEL_ID"), EmbeddingBaseURL: getenv("AGENT_RUNTIME_MEMORY_EMBEDDING_URL"), EmbeddingAPIKey: getenv("AGENT_RUNTIME_MEMORY_EMBEDDING_API_KEY"), EmbeddingDimensions: dimensions, RerankModel: getenv("AGENT_RUNTIME_MEMORY_RERANK_MODEL"), ExtractionOptions: options, Client: client})
		if err != nil {
			return nil, close, err
		}
		candidateLimit := 0
		if raw := getenv("AGENT_RUNTIME_MEMORY_CANDIDATE_LIMIT"); raw != "" {
			candidateLimit, err = strconv.Atoi(raw)
			if err != nil || candidateLimit < 1 || candidateLimit > 1000 {
				return nil, close, fmt.Errorf("memory candidate limit must be 1–1000")
			}
		}
		rerankerLimit := 0
		if raw := getenv("AGENT_RUNTIME_MEMORY_RERANKER_MAX_CANDIDATES"); raw != "" {
			rerankerLimit, err = strconv.Atoi(raw)
			if err != nil || rerankerLimit < 1 || rerankerLimit > 10000 {
				return nil, close, fmt.Errorf("memory reranker candidate limit must be 1–10000")
			}
		}
		cfg := memory.SQLiteConfig{CandidateLimit: candidateLimit, RerankerMaxCandidates: rerankerLimit, Path: getenv("AGENT_RUNTIME_MEMORY_SQLITE_PATH"), Extractor: p, Embedder: p}
		if getenv("AGENT_RUNTIME_MEMORY_RERANK_MODEL") != "" {
			cfg.Reranker = p
		}
		b, err := memory.OpenSQLite(ctx, cfg)
		if err != nil {
			return nil, close, err
		}
		return b, b.Close, nil
	default:
		return nil, close, fmt.Errorf("unsupported memory backend %q", driver)
	}
}

// Only the exact OpenRouter endpoint may use its named credential as a fallback.
// A custom inference URL must never receive that credential implicitly.
func modelAPIKey(getenv func(string) string) string {
	if key := getenv("AGENT_RUNTIME_MEMORY_MODEL_API_KEY"); key != "" {
		return key
	}
	if strings.TrimRight(getenv("AGENT_RUNTIME_MEMORY_MODEL_URL"), "/") == "https://openrouter.ai/api/v1" {
		return getenv("OPENROUTER_API_KEY")
	}
	return ""
}
