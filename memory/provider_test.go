package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderUsesPinnedExtractionAndMapsFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing provider credentials")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			var messages []map[string]string
			json.Unmarshal(body["messages"], &messages)
			if len(messages) != 2 || messages[0]["content"] != extractionPrompt || !strings.Contains(messages[1]["content"], "2026-10-01") {
				t.Error("extraction prompt drift")
			}
			if !strings.Contains(string(body["response_format"]), `"strict":true`) {
				t.Error("missing structured schema")
			}
			previous := -1
			for _, field := range []string{"what", "when", "where", "who", "why", "fact_kind", "occurred_start", "occurred_end", "fact_type", "entities", "causal_relations", "from_attachments"} {
				index := strings.Index(string(body["response_format"]), `"`+field+`":`)
				if index <= previous {
					t.Errorf("upstream property order lost at %s", field)
				}
				previous = index
			}
			content := `{"facts":[{"what":"Alice moved to Paris","when":"October 1, 2026","where":"Paris","who":"Alice","why":"N/A","fact_kind":"event","fact_type":"world","occurred_start":"2026-10-01","occurred_end":"2026-10-01","entities":["Alice","Paris"],"causal_relations":[]},{"what":"I booked the trip","when":"N/A","where":"N/A","who":"N/A","why":"Requested by Alice","fact_kind":"conversation","fact_type":"assistant","occurred_start":null,"occurred_end":null,"entities":["Alice"],"causal_relations":[{"target_index":0,"relation_type":"caused_by"}]}]}`
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}}})
		case "/v1/embeddings":
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 1, "embedding": []float32{0, 1}}, map[string]any{"index": 0, "embedding": []float32{1, 0}}}})
		case "/v1/rerank":
			json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"index": 1, "relevance_score": 0.9}, map[string]any{"index": 0, "relevance_score": 0.1}}})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p, err := NewProvider(ProviderConfig{BaseURL: server.URL + "/v1", APIKey: "test-key", ExtractionModel: "extract", EmbeddingModel: "embed", RerankModel: "rerank"})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := p.Extract(context.Background(), RetainRequest{Content: "Alice moved yesterday", Timestamp: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || facts[0].Text != "Alice moved to Paris | When: October 1, 2026 | Involving: Alice" || facts[0].Where != "Paris" || facts[1].Type != "experience" || facts[1].CausalRelations[0].TargetIndex != 0 {
		t.Fatalf("fact rendering: %+v", facts)
	}
	vectors, err := p.Embed(context.Background(), []string{"a", "b"})
	if err != nil || vectors[0][0] != 1 || vectors[1][1] != 1 {
		t.Fatalf("embedding order: %v %v", vectors, err)
	}
	scores, err := p.Rerank(context.Background(), "q", []string{"a", "b"})
	if err != nil || scores[0] != 0.1 || scores[1] != 0.9 {
		t.Fatalf("rerank order: %v %v", scores, err)
	}
}

func TestProviderPreservesSchemaSemanticsAndProtectsProtocol(t *testing.T) {
	schema := extractionSchema()
	defs := schema["$defs"].(map[string]any)
	properties := defs["ExtractedFact"].(map[string]any)["properties"].(map[string]any)
	description := properties["fact_type"].(map[string]any)["description"].(string)
	if !strings.Contains(description, "user preferences") || !strings.Contains(description, "actually performed") {
		t.Fatal("upstream fact classification guidance was lost")
	}
	for _, key := range []string{"model", "messages", "response_format", "stream", "tools", "tool_choice"} {
		if _, err := NewProvider(ProviderConfig{BaseURL: "http://localhost:1234/v1", ExtractionModel: "x", EmbeddingModel: "e", ExtractionOptions: map[string]any{key: "override"}}); err == nil {
			t.Fatalf("allowed protocol override %s", key)
		}
	}
	message := extractionUserMessage(RetainRequest{Content: "Alice prefers Go", Metadata: map[string]string{"source": "chat"}}, "Alice prefers Go", 0, 1)
	if !strings.Contains(message, "Event Date: Unknown\nContext: none\nMetadata:\n  source: chat\n\nContent:") {
		t.Fatalf("upstream user message semantics drifted: %s", message)
	}
}

func TestProviderEventDateFallbackMatchesUpstream(t *testing.T) {
	reference := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	unknown, err := convertFact(wireFact{What: "Assistant explained the process", FactType: "assistant", FactKind: "event"}, reference)
	if err != nil || unknown.OccurredStart != nil || unknown.OccurredEnd != nil {
		t.Fatalf("unknown event date was rejected or invented: %+v %v", unknown, err)
	}
	fact, err := convertFact(wireFact{What: "Alice visited last night", FactType: "world", FactKind: "event"}, reference)
	expected := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err != nil || fact.OccurredStart == nil || !fact.OccurredStart.Equal(expected) || fact.OccurredEnd == nil || !fact.OccurredEnd.Equal(expected) {
		t.Fatalf("relative date fallback: %+v %v", fact, err)
	}
	start := "2026-09-01"
	fact, err = convertFact(wireFact{What: "Alice visited", FactType: "world", FactKind: "event", OccurredStart: &start}, reference)
	if err != nil || fact.OccurredEnd == nil || !fact.OccurredEnd.Equal(*fact.OccurredStart) {
		t.Fatalf("point event end fallback: %+v %v", fact, err)
	}
}

func TestProviderSeparateEmbeddingEndpointDoesNotReceiveExtractionCredential(t *testing.T) {
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("hosted extraction credential leaked to local embedding endpoint")
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float32{1, 0}}}})
	}))
	defer embeddings.Close()
	p, err := NewProvider(ProviderConfig{BaseURL: "https://example.test/v1", APIKey: "hosted-secret", ExtractionModel: "x", EmbeddingModel: "e", EmbeddingBaseURL: embeddings.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Embed(context.Background(), []string{"Alice"}); err != nil {
		t.Fatal(err)
	}
	if p.ModelID() != embeddings.URL+"/e/hindsight-date-entities-v1" {
		t.Fatal("embedding identity used extraction endpoint")
	}
}

func TestProviderEnforcesRequestedEmbeddingDimensions(t *testing.T) {
	for _, returnedDimensions := range []int{2, 3} {
		t.Run(fmt.Sprint(returnedDimensions), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Dimensions int `json:"dimensions"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if body.Dimensions != 2 {
					t.Error("embedding dimensions not forwarded")
				}
				vector := make([]float32, returnedDimensions)
				vector[0] = 1
				json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vector}}})
			}))
			defer server.Close()
			provider, err := NewProvider(ProviderConfig{BaseURL: server.URL, ExtractionModel: "x", EmbeddingModel: "e", EmbeddingDimensions: 2})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Embed(context.Background(), []string{"Alice"})
			if (err != nil) != (returnedDimensions != 2) {
				t.Fatalf("dimension validation: %v", err)
			}
			if !strings.HasSuffix(provider.ModelID(), "/dimensions/2") {
				t.Fatal("model identity omits dimensions")
			}
		})
	}
}

func TestProviderRejectsTruncatedAndMalformedResults(t *testing.T) {
	for _, output := range []string{
		`{"choices":[{"finish_reason":"length","message":{"content":"{}"}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"{\"facts\":[]} trailing"}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"{\"facts\":[],\"unknown\":1}"}}]}`,
	} {
		t.Run(output, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(output)) }))
			defer server.Close()
			p, _ := NewProvider(ProviderConfig{BaseURL: server.URL, ExtractionModel: "x", EmbeddingModel: "e"})
			if _, err := p.Extract(context.Background(), RetainRequest{Content: "text"}); err == nil {
				t.Fatal("accepted invalid extraction")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte("secret-provider-key"))
	}))
	defer server.Close()
	p, _ := NewProvider(ProviderConfig{BaseURL: server.URL, ExtractionModel: "x", EmbeddingModel: "e"})
	_, err := p.Embed(context.Background(), []string{"text"})
	if err == nil || strings.Contains(err.Error(), "secret-provider-key") {
		t.Fatalf("provider error leaked response: %v", err)
	}
}

func TestHindsightReferenceWireContractAndScope(t *testing.T) {
	seen := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/v1/default/banks/" + BankName(testScope)
		if !strings.HasPrefix(r.URL.Path, prefix) {
			t.Errorf("unscoped path: %s", r.URL.Path)
		}
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Error("missing credentials")
		}
		switch {
		case r.Method == http.MethodDelete:
			w.Write([]byte(`{"success":true}`))
		case strings.HasSuffix(r.URL.Path, "/recall"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["max_tokens"] != float64(32768) {
				t.Error("budget not forwarded")
			}
			w.Write([]byte(`{"results":[{"id":"fact","text":"Alice prefers Go","type":"world","entities":["Alice"],"document_id":"remote","metadata":{"agent_runtime_document_id":"../source"},"occurred_start":null,"occurred_end":null}]}`))
		default:
			var body struct {
				Async bool             `json:"async"`
				Items []map[string]any `json:"items"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Async || len(body.Items) != 1 || body.Items[0]["document_id"] != stableID("../source") || body.Items[0]["update_mode"] != "replace" {
				t.Errorf("bad retain body: %+v", body)
			}
			w.Write([]byte(`{"success":true,"async":false,"items_count":1}`))
		}
	}))
	defer server.Close()
	h, err := NewHindsight(server.URL, "key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Retain(context.Background(), testScope, RetainRequest{DocumentID: "../source", Content: "Alice prefers Go"}); err != nil {
		t.Fatal(err)
	}
	r, err := h.Recall(context.Background(), testScope, RecallRequest{Query: "Alice", MaxTokens: 32768})
	if err != nil || len(r.Results) != 1 || r.Results[0].DocumentID != "../source" {
		t.Fatalf("reference recall: %+v %v", r, err)
	}
	if err = h.Forget(context.Background(), testScope, "../source"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || !strings.HasSuffix(seen[2], "/documents/"+stableID("../source")) {
		t.Fatalf("forget path: %v", seen)
	}
	if BankName(Scope{"other-app", "user"}) == BankName(testScope) {
		t.Fatal("bank collision across apps")
	}
}

func TestProviderRetriesInvalidExtractionWithoutPartialFacts(t *testing.T) {
	valid := `{"facts":[{"what":"Alice likes Go","fact_type":"world","entities":["Alice"]}]}`
	for _, recover := range []bool{true, false} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				content := valid + ` {"facts":[]}`
				if recover && calls == 2 {
					content = valid
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}}})
			}))
			defer server.Close()
			provider, _ := NewProvider(ProviderConfig{BaseURL: server.URL, ExtractionModel: "x", EmbeddingModel: "e"})
			facts, err := provider.Extract(context.Background(), RetainRequest{Content: "Alice likes Go"})
			if recover {
				if err != nil || calls != 2 || len(facts) != 1 {
					t.Fatalf("retry duplicated or lost facts: calls=%d facts=%v err=%v", calls, facts, err)
				}
			} else if err == nil || calls != 3 || facts != nil {
				t.Fatalf("retry failed to stop atomically: calls=%d facts=%v err=%v", calls, facts, err)
			}
		})
	}
}

func TestProviderDoesNotRetryRefusalOrTransportFailure(t *testing.T) {
	for _, status := range []int{200, 402} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"","refusal":"declined"}}]}`))
			}))
			defer server.Close()
			provider, _ := NewProvider(ProviderConfig{BaseURL: server.URL, ExtractionModel: "x", EmbeddingModel: "e"})
			if _, err := provider.Extract(context.Background(), RetainRequest{Content: strings.Repeat("text", 500)}); err == nil || calls != 1 {
				t.Fatalf("unexpected retry: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestHindsightIncludesSourceChunksAndMapsOriginalDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		include, ok := body["include"].(map[string]any)
		if !ok {
			t.Fatal("missing nested HTTP include options")
		}
		chunks, ok := include["chunks"].(map[string]any)
		if !ok || chunks["max_tokens"] != float64(1024) || include["entities"] != nil {
			t.Error("source chunk options not forwarded")
		}
		if _, exists := body["include_chunks"]; exists {
			t.Error("SDK-style field sent to HTTP API")
		}
		w.Write([]byte(`{"results":[{"id":"fact","chunk_id":"chunk","text":"study discussed","type":"experience","document_id":"hashed","metadata":{"agent_runtime_document_id":"original"}}],"chunks":{"chunk":{"id":"chunk","text":"The study included 24 subjects.","chunk_index":2,"truncated":false}}}`))
	}))
	defer server.Close()
	h, err := NewHindsight(server.URL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.Recall(context.Background(), testScope, RecallRequest{Query: "study", IncludeChunks: true, MaxChunkTokens: 1024})
	if err != nil || len(result.Chunks) != 1 || result.Chunks["chunk"].DocumentID != "original" || result.Chunks["chunk"].Index != 2 {
		t.Fatalf("reference chunks: %+v %v", result, err)
	}
}

func TestProviderAdaptiveSplitPreservesSourceAndCausalOffsets(t *testing.T) {
	turns := []map[string]string{}
	for i := 0; i < 4; i++ {
		turns = append(turns, map[string]string{"role": "user", "content": strings.Repeat("Alice discussed Go. ", 15)})
	}
	content, _ := json.Marshal(turns)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		input := strings.SplitN(body.Messages[1].Content, "Content:\n", 2)[1]
		var received []map[string]string
		if err := json.Unmarshal([]byte(input), &received); err != nil {
			t.Fatal(err)
		}
		output := `{"facts":[{"what":"Alice discussed Go","fact_type":"world"},{"what":"Alice selected Go","fact_type":"world","causal_relations":[{"target_index":0,"relation_type":"caused_by"}]}]}`
		if len(received) == 4 {
			output += ` {"facts":[]}`
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": output}}}})
	}))
	defer server.Close()
	provider, _ := NewProvider(ProviderConfig{BaseURL: server.URL, ExtractionModel: "x", EmbeddingModel: "e"})
	facts, err := provider.Extract(context.Background(), RetainRequest{Content: string(content)})
	if err != nil || calls != 5 || len(facts) != 4 {
		t.Fatalf("adaptive extraction: calls=%d facts=%v err=%v", calls, facts, err)
	}
	if facts[1].CausalRelations[0].TargetIndex != 0 || facts[3].CausalRelations[0].TargetIndex != 2 {
		t.Fatal("adaptive causal offsets corrupted")
	}
	for _, fact := range facts {
		if fact.SourceChunk == nil || fact.SourceChunk.Index != 0 || fact.SourceChunk.Text != string(content) {
			t.Fatal("adaptive split lost original source")
		}
	}
}

func TestProviderAdaptiveFailureIsBoundedAndPreservesExistingDocument(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"facts\":[]} {\"facts\":[]}"}}]}`))
	}))
	defer server.Close()
	provider, _ := NewProvider(ProviderConfig{BaseURL: server.URL, ExtractionModel: "x", EmbeddingModel: "e"})
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "doc", "original facts")
	s.cfg.Extractor = provider
	if _, err := s.Retain(context.Background(), testScope, RetainRequest{DocumentID: "doc", Content: strings.Repeat("word ", 480)}); err == nil || calls != 12 {
		t.Fatalf("unbounded or silently dropped extraction: calls=%d err=%v", calls, err)
	}
	result := recallTest(t, s, testScope, "original")
	if len(result.Results) != 1 || result.Results[0].Text != "original facts" {
		t.Fatal("failed adaptive replacement damaged existing memory")
	}
}

func TestReflectionIgnoredRequiredToolUsesConstrainedArguments(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		calls++
		if calls == 1 {
			if _, ok := body["tools"]; !ok {
				t.Error("missing initial native tools")
			}
			w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ungrounded plain prose"}}]}`))
			return
		}
		if _, ok := body["tools"]; ok {
			t.Error("fallback still has native tools")
		}
		var format struct {
			Type   string `json:"type"`
			Schema struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		}
		if err := json.Unmarshal(body["response_format"], &format); err != nil {
			t.Error(err)
		}
		if format.Type != "json_schema" || format.Schema.Name != "done" || len(format.Schema.Schema) == 0 {
			t.Errorf("unconstrained fallback: %s", body["response_format"])
		}
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"answer\":\"Paris\",\"memory_ids\":[\"known-proof\"],\"observation_ids\":[]}"}}]}`))
	}))
	defer server.Close()
	p, err := NewProvider(ProviderConfig{BaseURL: server.URL, ExtractionModel: "x", EmbeddingModel: "e"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := p.ReflectionStep(context.Background(), []ReflectionMessage{{Role: "system", Content: reflectPrompt}, {Role: "user", Content: "Where?"}}, "done")
	if err != nil || calls != 2 || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "done" {
		t.Fatalf("calls=%d message=%+v err=%v", calls, message, err)
	}
	if strings.Contains(message.ToolCalls[0].Function.Arguments, "ungrounded") {
		t.Fatal("plain prose accepted as evidence")
	}
}
