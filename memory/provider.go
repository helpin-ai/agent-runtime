package memory

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed upstream/retain-concise.txt
var extractionPrompt string

//go:embed upstream/retain-schema.json
var extractionSchemaJSON []byte

type ProviderConfig struct {
	BaseURL             string
	APIKey              string
	ExtractionModel     string
	EmbeddingModel      string
	EmbeddingModelID    string
	EmbeddingBaseURL    string
	EmbeddingAPIKey     string
	EmbeddingDimensions int
	RerankModel         string
	Client              *http.Client
	// ExtractionOptions supplies provider-specific sampling/reasoning controls.
	// Core model, messages and response schema cannot be overridden.
	ExtractionOptions map[string]any
}

// Provider implements extraction/embeddings using OpenAI-compatible endpoints,
// and optional cross-encoder reranking using the /rerank endpoint used by vLLM
// and other local model servers. No inference process is started automatically.
type Provider struct {
	cfg    ProviderConfig
	client *http.Client
}

func NewProvider(cfg ProviderConfig) (*Provider, error) {
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	if cfg.ExtractionModel == "" || cfg.EmbeddingModel == "" {
		return nil, fmt.Errorf("extraction and embedding models are required")
	}
	if cfg.EmbeddingDimensions < 0 || cfg.EmbeddingDimensions > 65536 {
		return nil, fmt.Errorf("embedding dimensions must be 0 (provider default) or 1–65536")
	}
	for key := range cfg.ExtractionOptions {
		switch key {
		case "model", "messages", "response_format", "stream", "tools", "tool_choice":
			return nil, fmt.Errorf("extraction option %q cannot override the extraction protocol", key)
		}
	}
	if _, err := json.Marshal(cfg.ExtractionOptions); err != nil {
		return nil, fmt.Errorf("invalid extraction options: %w", err)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.EmbeddingBaseURL == "" {
		cfg.EmbeddingBaseURL = cfg.BaseURL
		if cfg.EmbeddingAPIKey == "" {
			cfg.EmbeddingAPIKey = cfg.APIKey
		}
	}
	if err := validateBaseURL(cfg.EmbeddingBaseURL); err != nil {
		return nil, fmt.Errorf("embedding endpoint: %w", err)
	}
	cfg.EmbeddingBaseURL = strings.TrimRight(cfg.EmbeddingBaseURL, "/")
	if cfg.EmbeddingModelID == "" {
		cfg.EmbeddingModelID = cfg.EmbeddingBaseURL + "/" + cfg.EmbeddingModel + "/hindsight-date-entities-v1"
		if cfg.EmbeddingDimensions > 0 {
			cfg.EmbeddingModelID += fmt.Sprintf("/dimensions/%d", cfg.EmbeddingDimensions)
		}
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Provider{cfg, client}, nil
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("memory endpoint must be an HTTP(S) URL without credentials, query, or fragment")
	}
	return nil
}

// EmbedFacts mirrors upstream augment_texts_with_dates. Queries use Embed directly.
func (p *Provider) EmbedFacts(ctx context.Context, facts []ExtractedFact, mentionedAt time.Time) ([][]float32, error) {
	texts := make([]string, len(facts))
	for i, fact := range facts {
		texts[i] = FactEmbeddingText(fact, mentionedAt)
	}
	return p.Embed(ctx, texts)
}

// FactEmbeddingText renders the pinned upstream fact embedding input.
func FactEmbeddingText(fact ExtractedFact, mentionedAt time.Time) string {
	text := fact.Text
	date := mentionedAt
	if fact.OccurredStart != nil {
		date = *fact.OccurredStart
	}
	if !date.IsZero() {
		if fact.OccurredEnd != nil && (fact.OccurredStart == nil || !fact.OccurredEnd.Equal(*fact.OccurredStart)) {
			text += " (happened from " + date.Format("January 2006") + " to " + fact.OccurredEnd.Format("January 2006") + ")"
		} else {
			text += " (happened in " + date.Format("January 2006") + ")"
		}
	}
	if len(fact.Entities) > 0 {
		text += " [" + strings.Join(fact.Entities, ", ") + "]"
	}
	return text
}

func (p *Provider) ModelID() string { return p.cfg.EmbeddingModelID }

func jsonHTTP(ctx context.Context, client *http.Client, method, endpoint, key string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("memory endpoint request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("memory endpoint returned HTTP %d", response.StatusCode)
	}
	if output == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return fmt.Errorf("memory endpoint response exceeds 16 MiB")
	}
	if err = json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("invalid memory endpoint JSON: %w", err)
	}
	return nil
}

func (p *Provider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	all := make([][]float32, 0, len(texts))
	for begin := 0; begin < len(texts); begin += 64 {
		end := min(begin+64, len(texts))
		var response struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		body := map[string]any{"model": p.cfg.EmbeddingModel, "input": texts[begin:end]}
		if p.cfg.EmbeddingDimensions > 0 {
			body["dimensions"] = p.cfg.EmbeddingDimensions
		}
		if err := jsonHTTP(ctx, p.client, http.MethodPost, p.cfg.EmbeddingBaseURL+"/embeddings", p.cfg.EmbeddingAPIKey, body, &response); err != nil {
			return nil, err
		}
		batch := make([][]float32, end-begin)
		for _, item := range response.Data {
			if p.cfg.EmbeddingDimensions > 0 && len(item.Embedding) != p.cfg.EmbeddingDimensions {
				return nil, fmt.Errorf("embedding endpoint returned unexpected dimensions")
			}
			if item.Index < 0 || item.Index >= len(batch) || batch[item.Index] != nil {
				return nil, fmt.Errorf("invalid/duplicate embedding index")
			}
			batch[item.Index] = item.Embedding
		}
		if err := validateVectors(batch, len(batch)); err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}
	if len(all) > 0 {
		if err := validateVectors(all, len(texts)); err != nil {
			return nil, err
		}
	}
	return all, nil
}

type wireFact struct {
	What            string       `json:"what"`
	When            string       `json:"when"`
	Where           string       `json:"where"`
	Who             string       `json:"who"`
	Why             string       `json:"why"`
	FactKind        string       `json:"fact_kind"`
	FactType        string       `json:"fact_type"`
	OccurredStart   *string      `json:"occurred_start"`
	OccurredEnd     *string      `json:"occurred_end"`
	Entities        []string     `json:"entities"`
	CausalRelations []CausalLink `json:"causal_relations"`
	FromAttachments []int        `json:"from_attachments"`
}

// This schema is generated from the pinned upstream FactExtractionResponse
// through OpenAI's strict-schema conversion. Descriptions affect extraction.
func extractionSchema() map[string]any {
	var schema map[string]any
	if err := json.Unmarshal(extractionSchemaJSON, &schema); err != nil {
		panic(err)
	}
	return schema
}

func pythonTimestamp(date time.Time) string {
	value := date.Format("2006-01-02T15:04:05")
	if micros := date.Nanosecond() / 1000; micros != 0 {
		value += fmt.Sprintf(".%06d", micros)
	}
	return value + date.Format("-07:00")
}

func extractionUserMessage(req RetainRequest, chunk string, index, total int) string {
	date := "Unknown"
	if !req.Timestamp.IsZero() {
		date = req.Timestamp.Format("Monday, January 02, 2006") + " (" + pythonTimestamp(req.Timestamp) + ")"
	}
	context := req.Context
	if context == "" {
		context = "none"
	}
	metadata := ""
	keys := make([]string, 0, len(req.Metadata))
	for key := range req.Metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		metadata = "\nMetadata:"
		for _, key := range keys {
			metadata += "\n  " + key + ": " + req.Metadata[key]
		}
	}
	return fmt.Sprintf("Extract facts from the following chunk.\n\nChunk: %d/%d\nEvent Date: %s\nContext: %s%s\n\nContent:\n%s", index+1, total, date, context, metadata, chunk)
}

func (p *Provider) Extract(ctx context.Context, req RetainRequest) ([]ExtractedFact, error) {
	chunks := splitChunks(req.Content, 3000)
	facts := []ExtractedFact{}
	for i, chunk := range chunks {
		chunkFacts, err := p.extractAdaptive(ctx, req, chunk, i, len(chunks), 0)
		if err != nil {
			return nil, err
		}
		offset := len(facts)
		for _, f := range chunkFacts {
			f.SourceChunk = &ExtractedChunk{Index: i, Text: chunk}
			for k := range f.CausalRelations {
				f.CausalRelations[k].TargetIndex += offset
			}
			facts = append(facts, f)
		}
	}
	return facts, nil
}

type extractionOutputError struct{ err error }

func (e *extractionOutputError) Error() string { return e.err.Error() }
func (e *extractionOutputError) Unwrap() error { return e.err }

// Preserve the original source chunk and deterministic fact order while
// recovering from malformed or truncated output. Unlike upstream's last-resort
// drop, an exhausted split returns an error so old document facts remain intact.
func (p *Provider) extractAdaptive(ctx context.Context, req RetainRequest, chunk string, index, total, depth int) ([]ExtractedFact, error) {
	user := extractionUserMessage(req, chunk, index, total)
	// Preserve upstream property order: constrained generation can depend on
	// field order even though JSON Schema itself treats objects as unordered.
	body := map[string]any{"model": p.cfg.ExtractionModel, "messages": []map[string]string{{"role": "system", "content": extractionPrompt}, {"role": "user", "content": user}}, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "FactExtractionResponse", "strict": true, "schema": json.RawMessage(extractionSchemaJSON)}}}
	for key, value := range p.cfg.ExtractionOptions {
		body[key] = value
	}
	facts, err := p.extractChunk(ctx, body, req.Timestamp)
	if err == nil {
		return facts, nil
	}
	var outputErr *extractionOutputError
	if !errors.As(err, &outputErr) || depth >= 3 {
		return nil, err
	}
	first, second, ok := splitOutputRetry(chunk)
	if !ok {
		return nil, err
	}
	left, err := p.extractAdaptive(ctx, req, first, index, total, depth+1)
	if err != nil {
		return nil, err
	}
	right, err := p.extractAdaptive(ctx, req, second, index, total, depth+1)
	if err != nil {
		return nil, err
	}
	for i := range right {
		for j := range right[i].CausalRelations {
			right[i].CausalRelations[j].TargetIndex += len(left)
		}
	}
	return append(left, right...), nil
}

// Retry invalid structured output, without accepting partial JSON or partially
// appending a chunk. Transport failures and refusals are returned immediately.
func (p *Provider) extractChunk(ctx context.Context, body map[string]any, timestamp time.Time) ([]ExtractedFact, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var response struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content string  `json:"content"`
					Refusal *string `json:"refusal"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := jsonHTTP(ctx, p.client, http.MethodPost, p.cfg.BaseURL+"/chat/completions", p.cfg.APIKey, body, &response); err != nil {
			return nil, err
		}
		if len(response.Choices) != 1 || response.Choices[0].Message.Refusal != nil {
			return nil, fmt.Errorf("extraction did not complete normally")
		}
		if response.Choices[0].FinishReason == "length" {
			return nil, &extractionOutputError{fmt.Errorf("extraction output truncated")}
		}
		if response.Choices[0].FinishReason != "stop" {
			return nil, fmt.Errorf("extraction did not complete normally")
		}
		facts, err := parseExtractedFacts(response.Choices[0].Message.Content, timestamp)
		if err == nil {
			return facts, nil
		}
		lastErr = err
	}
	return nil, &extractionOutputError{fmt.Errorf("extraction invalid after 3 attempts: %w", lastErr)}
}

func parseExtractedFacts(content string, timestamp time.Time) ([]ExtractedFact, error) {
	var parsed struct {
		Facts []wireFact `json:"facts"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("invalid extracted facts: %w", err)
	}
	if parsed.Facts == nil {
		return nil, fmt.Errorf("extraction response is missing facts")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("trailing extraction output")
	}
	facts := make([]ExtractedFact, 0, len(parsed.Facts))
	for index, wire := range parsed.Facts {
		fact, err := convertFact(wire, timestamp)
		if err != nil {
			return nil, err
		}
		if err := validateExtracted(fact, index); err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return facts, nil
}

func convertFact(w wireFact, reference time.Time) (ExtractedFact, error) {
	f := ExtractedFact{Text: w.What, Type: w.FactType, Entities: w.Entities, CausalRelations: w.CausalRelations}
	if w.FactType == "assistant" {
		f.Type = "experience"
	}
	// Preserve upstream's stored fact rendering, including its dimension labels.
	if w.When != "" && w.When != "N/A" {
		f.Text += " | When: " + w.When
	}
	if w.Who != "" && w.Who != "N/A" {
		f.Text += " | Involving: " + w.Who
	}
	if w.Why != "" && w.Why != "N/A" {
		f.Text += " | " + w.Why
	}
	if w.Where != "N/A" {
		f.Where = w.Where
	}
	if w.FactKind == "event" {
		f.OccurredStart = parseExtractionDate(w.OccurredStart)
		f.OccurredEnd = parseExtractionDate(w.OccurredEnd)
		if w.OccurredStart == nil || *w.OccurredStart == "" {
			f.OccurredStart = inferEventDate(f.Text, reference)
		}
		if (w.OccurredEnd == nil || *w.OccurredEnd == "") && f.OccurredStart != nil {
			end := *f.OccurredStart
			f.OccurredEnd = &end
		}
	}
	if strings.TrimSpace(w.What) == "" {
		return f, fmt.Errorf("fact is missing what")
	}
	return f, nil
}

// Ordered fallbacks from upstream _infer_temporal_date. Unknown dates stay
// unknown; a reference timestamp alone is not evidence of an event's date.
var eventDateFallbacks = []struct {
	pattern *regexp.Regexp
	days    int
}{
	{regexp.MustCompile(`\blast night\b`), -1},
	{regexp.MustCompile(`\byesterday\b`), -1},
	{regexp.MustCompile(`\btoday\b`), 0},
	{regexp.MustCompile(`\bthis morning\b`), 0},
	{regexp.MustCompile(`\bthis afternoon\b`), 0},
	{regexp.MustCompile(`\bthis evening\b`), 0},
	{regexp.MustCompile(`\btonigh?t\b`), 0},
	{regexp.MustCompile(`\btomorrow\b`), 1},
	{regexp.MustCompile(`\blast week\b`), -7},
	{regexp.MustCompile(`\bthis week\b`), 0},
	{regexp.MustCompile(`\bnext week\b`), 7},
	{regexp.MustCompile(`\blast month\b`), -30},
	{regexp.MustCompile(`\bthis month\b`), 0},
	{regexp.MustCompile(`\bnext month\b`), 30},
}

func inferEventDate(text string, reference time.Time) *time.Time {
	if reference.IsZero() {
		return nil
	}
	for _, fallback := range eventDateFallbacks {
		if fallback.pattern.MatchString(strings.ToLower(text)) {
			target := reference.AddDate(0, 0, fallback.days)
			date := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, target.Location())
			return &date
		}
	}
	return nil
}

func parseDate(value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	for _, format := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(format, *value); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("invalid extracted date")
}

func (p *Provider) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if p.cfg.RerankModel == "" {
		return nil, fmt.Errorf("reranking model is not configured")
	}
	var response struct {
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := jsonHTTP(ctx, p.client, http.MethodPost, p.cfg.BaseURL+"/rerank", p.cfg.APIKey, map[string]any{"model": p.cfg.RerankModel, "query": query, "documents": documents, "top_n": len(documents)}, &response); err != nil {
		return nil, err
	}
	if len(response.Results) != len(documents) {
		return nil, fmt.Errorf("reranker omitted results")
	}
	scores := make([]float64, len(documents))
	seen := map[int]bool{}
	for _, item := range response.Results {
		if item.Index < 0 || item.Index >= len(documents) || seen[item.Index] {
			return nil, fmt.Errorf("invalid/duplicate reranker index")
		}
		seen[item.Index] = true
		scores[item.Index] = item.Score
	}
	return scores, nil
}
