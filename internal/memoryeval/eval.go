// Package memoryeval compares document-level evidence retrieval on a shared
// corpus. It does not score generated answers or claim benchmark parity.
package memoryeval

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/helpin-ai/agent-runtime/memory"
)

type Query struct {
	Name              string               `json:"name"`
	Request           memory.RecallRequest `json:"request"`
	ExpectedDocuments []string             `json:"expected_documents"`
}

type Fixture struct {
	EmbeddingPreprocessing string                            `json:"embedding_preprocessing,omitempty"`
	Documents              []memory.RetainRequest            `json:"documents"`
	Queries                []Query                           `json:"queries"`
	RecordedFacts          map[string][]memory.ExtractedFact `json:"recorded_facts,omitempty"`
	RecordedEmbeddings     map[string][]float32              `json:"recorded_embeddings,omitempty"`
}

func (f Fixture) Validate() error {
	if f.EmbeddingPreprocessing != "" && f.EmbeddingPreprocessing != "hindsight-date-entities-v1" {
		return fmt.Errorf("unknown recorded embedding preprocessing")
	}
	if len(f.Documents) == 0 || len(f.Queries) == 0 {
		return fmt.Errorf("fixture needs documents and queries")
	}
	ids := map[string]bool{}
	for _, doc := range f.Documents {
		if doc.DocumentID == "" || ids[doc.DocumentID] {
			return fmt.Errorf("fixture document IDs must be unique and nonempty")
		}
		ids[doc.DocumentID] = true
	}
	for _, q := range f.Queries {
		if q.Name == "" || q.Request.Query == "" {
			return fmt.Errorf("fixture query name and text are required")
		}
		for _, id := range q.ExpectedDocuments {
			if !ids[id] {
				return fmt.Errorf("unknown expected document %q", id)
			}
		}
	}
	return nil
}

type QueryResult struct {
	Name               string              `json:"name"`
	ExpectedDocuments  []string            `json:"expected_documents"`
	RetrievedDocuments []string            `json:"retrieved_documents"`
	EvidenceRecall     float64             `json:"evidence_recall"`
	ReciprocalRank     float64             `json:"reciprocal_rank"`
	LatencyMS          int64               `json:"latency_ms"`
	Evidence           memory.RecallResult `json:"evidence"`
}

type Report struct {
	Backend                     string                `json:"backend"`
	Scope                       memory.Scope          `json:"scope"`
	PinnedUpstreamCommit        string                `json:"pinned_upstream_commit"`
	ReferenceDeploymentVerified bool                  `json:"reference_deployment_verified"`
	RecordedModels              bool                  `json:"recorded_models"`
	RetainWorkers               int                   `json:"retain_workers"`
	RetainResults               []memory.RetainResult `json:"retain_results"`
	RetainMS                    int64                 `json:"retain_ms"`
	MeanEvidenceRecall          float64               `json:"mean_evidence_recall"`
	MeanReciprocalRank          float64               `json:"mean_reciprocal_rank"`
	Queries                     []QueryResult         `json:"queries"`
}

func Evaluate(ctx context.Context, backend memory.Backend, scope memory.Scope, fixture Fixture, name string, recorded bool) (Report, error) {
	return EvaluateWithWorkers(ctx, backend, scope, fixture, name, recorded, 1)
}

// EvaluateWithWorkers keeps queries behind a successful ingestion barrier.
// Concurrency is explicit in the report; both backends receive the same limit.
func EvaluateWithWorkers(ctx context.Context, backend memory.Backend, scope memory.Scope, fixture Fixture, name string, recorded bool, workers int) (Report, error) {
	report := Report{Backend: name, Scope: scope, PinnedUpstreamCommit: memory.UpstreamCommit, RecordedModels: recorded, RetainWorkers: workers, Queries: []QueryResult{}}
	if err := fixture.Validate(); err != nil {
		return report, err
	}
	if workers < 1 || workers > 16 {
		return report, fmt.Errorf("retain workers must be 1–16")
	}
	start := time.Now()
	group, retainCtx := errgroup.WithContext(ctx)
	group.SetLimit(workers)
	retained := make([]memory.RetainResult, len(fixture.Documents))
	for i, doc := range fixture.Documents {
		group.Go(func() error {
			// The reference adapter preserves the original ID in metadata. Give
			// both extractors that same field, so their user prompts align too.
			metadata := make(map[string]string, len(doc.Metadata)+1)
			for key, value := range doc.Metadata {
				metadata[key] = value
			}
			metadata["agent_runtime_document_id"] = doc.DocumentID
			doc.Metadata = metadata
			result, err := backend.Retain(retainCtx, scope, doc)
			if err != nil {
				return fmt.Errorf("retain %s: %w", doc.DocumentID, err)
			}
			retained[i] = result
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return report, err
	}
	report.RetainResults = retained
	report.RetainMS = time.Since(start).Milliseconds()
	for _, query := range fixture.Queries {
		start = time.Now()
		result, err := backend.Recall(ctx, scope, query.Request)
		if err != nil {
			return report, fmt.Errorf("recall %s: %w", query.Name, err)
		}
		q := QueryResult{Name: query.Name, ExpectedDocuments: query.ExpectedDocuments, RetrievedDocuments: []string{}, LatencyMS: time.Since(start).Milliseconds(), Evidence: result}
		expected, seen := map[string]bool{}, map[string]bool{}
		for _, id := range query.ExpectedDocuments {
			expected[id] = true
		}
		hits := 0
		for _, fact := range result.Results {
			id := fact.DocumentID
			if seen[id] {
				continue
			}
			seen[id] = true
			q.RetrievedDocuments = append(q.RetrievedDocuments, id)
			if expected[id] {
				hits++
				if q.ReciprocalRank == 0 {
					q.ReciprocalRank = 1 / float64(len(q.RetrievedDocuments))
				}
			}
		}
		if len(expected) > 0 {
			q.EvidenceRecall = float64(hits) / float64(len(expected))
		} else if len(q.RetrievedDocuments) == 0 {
			q.EvidenceRecall = 1
		}
		report.MeanEvidenceRecall += q.EvidenceRecall
		report.MeanReciprocalRank += q.ReciprocalRank
		report.Queries = append(report.Queries, q)
	}
	report.MeanEvidenceRecall /= float64(len(report.Queries))
	report.MeanReciprocalRank /= float64(len(report.Queries))
	return report, nil
}

// RecordedModels fails for unknown inputs instead of fabricating embeddings or
// facts. It isolates storage/retrieval tests from live model nondeterminism.
type RecordedModels struct{ Fixture Fixture }

func (r RecordedModels) ModelID() string {
	return "recorded-fixture-v1/" + r.Fixture.EmbeddingPreprocessing
}
func (r RecordedModels) Extract(_ context.Context, req memory.RetainRequest) ([]memory.ExtractedFact, error) {
	for _, doc := range r.Fixture.Documents {
		if doc.DocumentID == req.DocumentID && doc.Content == req.Content {
			facts, ok := r.Fixture.RecordedFacts[req.DocumentID]
			if ok {
				return facts, nil
			}
		}
	}
	return nil, fmt.Errorf("no recorded extraction for %s", req.DocumentID)
}
func (r RecordedModels) Embed(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i, text := range texts {
		v, ok := r.Fixture.RecordedEmbeddings[text]
		if !ok {
			return nil, fmt.Errorf("no recorded embedding for input")
		}
		result[i] = v
	}
	return result, nil
}

func (r RecordedModels) EmbedFacts(ctx context.Context, facts []memory.ExtractedFact, mentionedAt time.Time) ([][]float32, error) {
	texts := make([]string, len(facts))
	for i, fact := range facts {
		texts[i] = fact.Text
		if r.Fixture.EmbeddingPreprocessing == "hindsight-date-entities-v1" {
			texts[i] = memory.FactEmbeddingText(fact, mentionedAt)
		}
	}
	return r.Embed(ctx, texts)
}
