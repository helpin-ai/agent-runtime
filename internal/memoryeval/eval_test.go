package memoryeval

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/memory"
)

func TestEvaluateUsesDocumentEvidenceAndFailsOnMissingRecordings(t *testing.T) {
	f := Fixture{Documents: []memory.RetainRequest{{DocumentID: "doc", Content: "source"}}, Queries: []Query{{Name: "q", Request: memory.RecallRequest{Query: "question"}, ExpectedDocuments: []string{"doc"}}}, RecordedFacts: map[string][]memory.ExtractedFact{"doc": {{Text: "first fact", Type: "world"}, {Text: "second fact", Type: "world"}}}, RecordedEmbeddings: map[string][]float32{"first fact": {1, 0}, "second fact": {1, 0}, "question": {1, 0}}}
	models := RecordedModels{Fixture: f}
	backend, err := memory.OpenSQLite(context.Background(), memory.SQLiteConfig{Path: filepath.Join(t.TempDir(), "memory.db"), Extractor: models, Embedder: models})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	r, err := Evaluate(context.Background(), backend, memory.Scope{AppID: "test", BankID: "bank"}, f, "local", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.MeanEvidenceRecall != 1 || r.MeanReciprocalRank != 1 || len(r.Queries[0].RetrievedDocuments) != 1 || r.ReferenceDeploymentVerified {
		t.Fatalf("invalid evidence report: %+v", r)
	}
	if len(r.RetainResults) != 1 || r.RetainResults[0].DocumentID != "doc" || r.RetainResults[0].FactCount != 2 {
		t.Fatalf("missing ingestion diagnostics: %+v", r.RetainResults)
	}
	if _, err = models.Embed(context.Background(), []string{"unrecorded query"}); err == nil {
		t.Fatal("fabricated recorded model response")
	}
	f.Queries[0].ExpectedDocuments = []string{"unknown"}
	if err = f.Validate(); err == nil {
		t.Fatal("invalid expectations accepted")
	}
}

type blockingBackend struct {
	started  chan string
	release  chan struct{}
	fail     bool
	recalled bool
}

func (b *blockingBackend) Retain(ctx context.Context, _ memory.Scope, req memory.RetainRequest) (memory.RetainResult, error) {
	b.started <- req.DocumentID
	select {
	case <-ctx.Done():
		return memory.RetainResult{}, ctx.Err()
	case <-b.release:
	}
	if b.fail {
		return memory.RetainResult{}, fmt.Errorf("ingestion failed")
	}
	return memory.RetainResult{}, nil
}
func (b *blockingBackend) Recall(context.Context, memory.Scope, memory.RecallRequest) (memory.RecallResult, error) {
	b.recalled = true
	return memory.RecallResult{Results: []memory.Result{}}, nil
}
func (*blockingBackend) Forget(context.Context, memory.Scope, string) error { return nil }

func TestConcurrentEvaluateHasIngestionBarrierAndHonorsLimit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		b := &blockingBackend{started: make(chan string, 3), release: make(chan struct{}), fail: fail}
		fixture := Fixture{Documents: []memory.RetainRequest{{DocumentID: "one", Content: "one"}, {DocumentID: "two", Content: "two"}, {DocumentID: "three", Content: "three"}}, Queries: []Query{{Name: "q", Request: memory.RecallRequest{Query: "q"}}}}
		done := make(chan error, 1)
		go func() {
			_, err := EvaluateWithWorkers(context.Background(), b, memory.Scope{AppID: "test", BankID: "bank"}, fixture, "local", false, 2)
			done <- err
		}()
		for i := 0; i < 2; i++ {
			select {
			case <-b.started:
			case <-time.After(5 * time.Second):
				t.Fatal("workers did not run concurrently")
			}
		}
		select {
		case <-b.started:
			t.Fatal("worker limit exceeded")
		default:
		}
		close(b.release)
		select {
		case err := <-done:
			if (err != nil) != fail || b.recalled == fail {
				t.Fatalf("barrier: fail=%v recalled=%v err=%v", fail, b.recalled, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("evaluation did not finish")
		}
	}
}
