// Package memory provides optional, bank-scoped long-term agent memory.
// Run checkpoints remain the runtime store's responsibility.
package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const UpstreamCommit = "017b3f5d888d67341e70f43c102cb8155bb9f51e"

var (
	ErrConflict  = errors.New("memory document changed during retain")
	ErrForgotten = errors.New("memory document was forgotten; use a new document ID to reimport")
)

// Scope is selected by the host, never by model-supplied tool arguments.
type Scope struct {
	AppID  string `json:"app_id"`
	BankID string `json:"bank_id"`
}

func (s Scope) Validate() error {
	if strings.TrimSpace(s.AppID) == "" || strings.TrimSpace(s.BankID) == "" || len(s.AppID) > 512 || len(s.BankID) > 512 {
		return fmt.Errorf("memory requires an app ID and bank ID of at most 512 bytes")
	}
	return nil
}

// RetainRequest replaces one source document atomically. DocumentID is a stable
// host/source identifier, not a random ID generated for each retry.
type RetainRequest struct {
	DocumentID string            `json:"document_id"`
	Content    string            `json:"content"`
	Context    string            `json:"context,omitempty"`
	Timestamp  time.Time         `json:"timestamp,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

func (r RetainRequest) validate() error {
	if strings.TrimSpace(r.DocumentID) == "" || len(r.DocumentID) > 512 || strings.TrimSpace(r.Content) == "" || len(r.Content) > 256<<10 || len(r.Context) > 8192 {
		return fmt.Errorf("retain requires a document ID, nonempty content (maximum 256 KiB), and context of at most 8 KiB")
	}
	if len(r.Metadata) > 64 {
		return fmt.Errorf("too many metadata fields")
	}
	for k, v := range r.Metadata {
		if len(k) > 512 || len(v) > 8192 {
			return fmt.Errorf("metadata field exceeds limit")
		}
	}
	return nil
}

type RetainResult struct {
	DocumentID string `json:"document_id"`
	FactCount  int    `json:"fact_count"`
	Unchanged  bool   `json:"unchanged"`
}

// TemporalWindow boosts the temporal retrieval arm. It is not a hard filter on
// semantic, keyword, or graph results, matching Hindsight's API semantics.
type TemporalWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type RecallRequest struct {
	Query          string          `json:"query"`
	IncludeChunks  bool            `json:"include_chunks,omitempty"`
	MaxChunkTokens int             `json:"max_chunk_tokens,omitempty"`
	Limit          int             `json:"limit,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Types          []string        `json:"types,omitempty"`
	QueryTimestamp time.Time       `json:"query_timestamp,omitempty"`
	TemporalWindow *TemporalWindow `json:"temporal_window,omitempty"`
}

func (r RecallRequest) normalized() (RecallRequest, error) {
	if strings.TrimSpace(r.Query) == "" || len(r.Query) > 8192 {
		return r, fmt.Errorf("recall query must be nonempty and at most 8 KiB")
	}
	if r.Limit == 0 {
		r.Limit = 20
	}
	if r.MaxTokens == 0 {
		r.MaxTokens = 4096
	}
	if r.IncludeChunks && r.MaxChunkTokens == 0 {
		r.MaxChunkTokens = 16384
	}
	if r.MaxChunkTokens < 0 || r.MaxChunkTokens > 32768 {
		return r, fmt.Errorf("max_chunk_tokens must be 1–32768 when including chunks")
	}
	if r.Limit < 1 || r.Limit > 100 || r.MaxTokens < 1 || r.MaxTokens > 32768 {
		return r, fmt.Errorf("recall limit must be 1–100 and max_tokens 1–32768")
	}
	if len(r.Types) == 0 {
		r.Types = []string{"world", "experience"}
	}
	for _, t := range r.Types {
		if t != "world" && t != "experience" {
			return r, fmt.Errorf("this milestone supports world and experience facts only")
		}
	}
	if w := r.TemporalWindow; w != nil && (w.Start.IsZero() || w.End.IsZero() || w.End.Before(w.Start)) {
		return r, fmt.Errorf("invalid temporal window")
	}
	return r, nil
}

type Fact struct {
	ID            string            `json:"id"`
	ChunkID       string            `json:"chunk_id,omitempty"`
	DocumentID    string            `json:"document_id"`
	Text          string            `json:"text"`
	Type          string            `json:"type"`
	Entities      []string          `json:"entities"`
	Context       string            `json:"context,omitempty"`
	MentionedAt   time.Time         `json:"mentioned_at"`
	Where         string            `json:"where,omitempty"`
	OccurredStart *time.Time        `json:"occurred_start,omitempty"`
	OccurredEnd   *time.Time        `json:"occurred_end,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

type Result struct {
	Fact
	Score       float64        `json:"score"`
	SourceRanks map[string]int `json:"source_ranks,omitempty"`
}

type RecallResult struct {
	Results              []Result               `json:"results"`
	Chunks               map[string]SourceChunk `json:"chunks,omitempty"`
	EstimatedChunkTokens int                    `json:"estimated_chunk_tokens,omitempty"`
	// EstimatedTokens includes JSON result framing. A caller may supply an exact
	// tokenizer; the default uses Hindsight's o200k_base encoding.
	EstimatedTokens int `json:"estimated_tokens"`
	// Nil means the reference server did not expose its reranking configuration.
	Reranked *bool `json:"reranked,omitempty"`
}

type Backend interface {
	Retain(context.Context, Scope, RetainRequest) (RetainResult, error)
	Recall(context.Context, Scope, RecallRequest) (RecallResult, error)
	Forget(context.Context, Scope, string) error
}

type CausalLink struct {
	TargetIndex  int    `json:"target_index"`
	RelationType string `json:"relation_type"`
}

type ExtractedChunk struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

type SourceChunk struct {
	ID         string `json:"id"`
	DocumentID string `json:"document_id"`
	Index      int    `json:"chunk_index"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
}

type ExtractedFact struct {
	SourceChunk     *ExtractedChunk `json:"source_chunk,omitempty"`
	Text            string          `json:"text"`
	Where           string          `json:"where,omitempty"`
	Type            string          `json:"type"`
	Entities        []string        `json:"entities"`
	OccurredStart   *time.Time      `json:"occurred_start,omitempty"`
	OccurredEnd     *time.Time      `json:"occurred_end,omitempty"`
	CausalRelations []CausalLink    `json:"causal_relations,omitempty"`
}

type Extractor interface {
	Extract(context.Context, RetainRequest) ([]ExtractedFact, error)
}
type Embedder interface {
	// ModelID must change when the model, dimensions, or preprocessing changes.
	ModelID() string
	Embed(context.Context, []string) ([][]float32, error)
}

// FactEmbedder optionally supplies upstream fact-specific date/entity augmentation.
type FactEmbedder interface {
	EmbedFacts(context.Context, []ExtractedFact, time.Time) ([][]float32, error)
}
type Reranker interface {
	Rerank(context.Context, string, []string) ([]float64, error)
}
type TokenCounter func(string) int
