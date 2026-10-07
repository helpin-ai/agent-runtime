package memory

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

//go:embed upstream/consolidation-system.txt
var consolidationPrompt string

//go:embed upstream/consolidation-input.txt
var consolidationInput string

//go:embed upstream/consolidation-schema.json
var consolidationSchema []byte

var errConsolidationOutput = errors.New("invalid consolidation output")

// Consolidate uses the pinned default mission/language and the configured local
// extraction model. Host-controlled nondefault missions are not supported yet.
func (p *Provider) Consolidate(ctx context.Context, facts []Fact, observations []Observation) (ConsolidationPlan, error) {
	return p.consolidateWithCapacity(ctx, facts, observations, "")
}

func (p *Provider) consolidateWithCapacity(ctx context.Context, facts []Fact, observations []Observation, capacityNote string) (ConsolidationPlan, error) {
	lines := make([]string, len(facts))
	for i, f := range facts {
		temporal := []string{}
		if f.OccurredStart != nil {
			temporal = append(temporal, "occurred_start="+pythonTimestamp(*f.OccurredStart))
		}
		if f.OccurredEnd != nil {
			temporal = append(temporal, "occurred_end="+pythonTimestamp(*f.OccurredEnd))
		}
		if !f.MentionedAt.IsZero() {
			temporal = append(temporal, "mentioned_at="+pythonTimestamp(f.MentionedAt))
		}
		lines[i] = "[" + f.ID + "] " + f.Text
		if len(temporal) > 0 {
			lines[i] += " (" + strings.Join(temporal, ", ") + ")"
		}
	}
	encoded, err := consolidationObservationJSON(observations)
	if err != nil {
		return ConsolidationPlan{}, err
	}
	// Replace once, so placeholders inside source text cannot rewrite the prompt.
	user := strings.NewReplacer("{facts_text}", strings.Join(lines, "\n"), "{observations_text}", string(encoded)).Replace(consolidationInput)
	if capacityNote != "" {
		user = capacityNote + "\n\n" + user
	}
	body := map[string]any{"model": p.cfg.ExtractionModel, "messages": []map[string]string{{"role": "system", "content": consolidationPrompt}, {"role": "user", "content": user}}, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "_ConsolidationBatchResponse", "strict": true, "schema": json.RawMessage(consolidationSchema)}}}
	for k, v := range p.cfg.ExtractionOptions {
		body[k] = v
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
	if err = jsonHTTP(ctx, p.client, http.MethodPost, p.cfg.BaseURL+"/chat/completions", p.cfg.APIKey, body, &response); err != nil {
		return ConsolidationPlan{}, err
	}
	if len(response.Choices) != 1 || response.Choices[0].Message.Refusal != nil {
		return ConsolidationPlan{}, fmt.Errorf("consolidation refused or returned no unique choice")
	}
	if response.Choices[0].FinishReason != "stop" {
		return ConsolidationPlan{}, fmt.Errorf("%w: did not complete normally", errConsolidationOutput)
	}
	var plan ConsolidationPlan
	decoder := json.NewDecoder(strings.NewReader(response.Choices[0].Message.Content))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("%w: %v", errConsolidationOutput, err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || plan.Creates == nil || plan.Updates == nil || plan.Deletes == nil {
		return ConsolidationPlan{}, fmt.Errorf("%w: incomplete or trailing output", errConsolidationOutput)
	}
	return plan, nil
}

// This prompt projection mirrors _build_observations_for_llm. Internal IDs,
// staleness and update bookkeeping are deliberately absent from model input.
func consolidationObservationJSON(observations []Observation) ([]byte, error) {
	type proof struct {
		Text          string  `json:"text"`
		Context       string  `json:"context,omitempty"`
		OccurredStart *string `json:"occurred_start,omitempty"`
		OccurredEnd   *string `json:"occurred_end,omitempty"`
		MentionedAt   *string `json:"mentioned_at,omitempty"`
	}
	type observation struct {
		ID             string  `json:"id"`
		Text           string  `json:"text"`
		ProofCount     int     `json:"proof_count"`
		OccurredStart  *string `json:"occurred_start,omitempty"`
		OccurredEnd    *string `json:"occurred_end,omitempty"`
		MentionedAt    *string `json:"mentioned_at,omitempty"`
		SourceMemories []proof `json:"source_memories,omitempty"`
	}
	date := func(t *time.Time) *string {
		if t == nil {
			return nil
		}
		value := pythonTimestamp(*t)
		return &value
	}
	mentioned := func(t time.Time) *string {
		if t.IsZero() {
			return nil
		}
		return date(&t)
	}
	projected := make([]observation, 0, len(observations))
	for _, o := range observations {
		ids := map[string]bool{}
		for _, id := range o.SourceFactIDs {
			ids[id] = true
		}
		p := observation{ID: o.ID, Text: o.Text, ProofCount: max(1, len(ids)), OccurredStart: date(o.OccurredStart), OccurredEnd: date(o.OccurredEnd), MentionedAt: mentioned(o.MentionedAt)}
		for _, f := range o.SourceMemories {
			context := []rune(f.Context)
			if len(context) > 200 {
				context = append(context[:200], []rune("...")...)
			}
			p.SourceMemories = append(p.SourceMemories, proof{Text: f.Text, Context: string(context), OccurredStart: date(f.OccurredStart), OccurredEnd: date(f.OccurredEnd), MentionedAt: mentioned(f.MentionedAt)})
		}
		projected = append(projected, p)
	}
	return json.MarshalIndent(projected, "", "  ")
}
