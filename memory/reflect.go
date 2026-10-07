package memory

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

//go:embed upstream/reflect-system.txt
var reflectPrompt string

//go:embed upstream/reflect-tools.json
var reflectToolsJSON []byte

//go:embed upstream/reflect-mental-models-system.txt
var reflectMentalModelsPrompt string

//go:embed upstream/reflect-mental-models-tools.json
var reflectMentalModelsToolsJSON []byte

type ReflectionToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type ReflectionMessage struct {
	Role       string               `json:"role"`
	Content    string               `json:"content,omitempty"`
	ToolCalls  []ReflectionToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
}
type ReflectionModel interface {
	ReflectionStep(context.Context, []ReflectionMessage, string) (ReflectionMessage, error)
}

type ReflectBackend interface {
	ReflectMemory(context.Context, Scope, ReflectRequest) (ReflectResult, error)
}

func (s *SQLite) ReflectMemory(ctx context.Context, scope Scope, req ReflectRequest) (ReflectResult, error) {
	model, ok := s.cfg.Extractor.(ReflectionModel)
	if !ok {
		return ReflectResult{}, fmt.Errorf("configured extractor does not support reflection")
	}
	return s.Reflect(ctx, scope, req, model)
}

type ReflectRequest struct {
	Query            string   `json:"query"`
	Directives       []string `json:"directives,omitempty"`
	MaxSteps         int      `json:"max_steps,omitempty"`
	MaxContextTokens int      `json:"max_context_tokens,omitempty"`
}
type ReflectResult struct {
	Answer         string   `json:"answer"`
	MemoryIDs      []string `json:"memory_ids"`
	ObservationIDs []string `json:"observation_ids"`
	MentalModelIDs []string `json:"mental_model_ids"`
	SourceFactIDs  []string `json:"source_fact_ids"`
	Steps          int      `json:"steps"`
}

func (p *Provider) ReflectionStep(ctx context.Context, messages []ReflectionMessage, forcedTool string) (ReflectionMessage, error) {
	var tools []json.RawMessage
	definition := reflectToolsJSON
	if len(messages) > 0 && strings.Contains(messages[0].Content, "### 1. MENTAL MODELS") {
		definition = reflectMentalModelsToolsJSON
	}
	if err := json.Unmarshal(definition, &tools); err != nil {
		return ReflectionMessage{}, err
	}
	// Every turn completes through a tool, including done(), so citation checks
	// never get bypassed by an unstructured assistant answer.
	body := map[string]any{"model": p.cfg.ExtractionModel, "messages": messages, "tools": tools, "parallel_tool_calls": false, "tool_choice": "required"}
	if forcedTool != "" {
		// LM Studio supports required/auto, but rejects object tool_choice.
		// Expose only the required function to preserve forced-call semantics.
		selected := []json.RawMessage{}
		for _, tool := range tools {
			var definition struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(tool, &definition); err != nil {
				return ReflectionMessage{}, err
			}
			if definition.Function.Name == forcedTool {
				selected = append(selected, tool)
			}
		}
		if len(selected) != 1 {
			return ReflectionMessage{}, fmt.Errorf("unknown required reflection tool")
		}
		body["tools"] = selected
		body["tool_choice"] = "required"
	}
	for k, v := range p.cfg.ExtractionOptions {
		body[k] = v
	}
	var response struct {
		Choices []struct {
			FinishReason string            `json:"finish_reason"`
			Message      ReflectionMessage `json:"message"`
		} `json:"choices"`
	}
	if err := jsonHTTP(ctx, p.client, http.MethodPost, p.cfg.BaseURL+"/chat/completions", p.cfg.APIKey, body, &response); err != nil {
		return ReflectionMessage{}, err
	}
	if len(response.Choices) != 1 {
		return ReflectionMessage{}, fmt.Errorf("reflection requires exactly one response choice")
	}
	if forcedTool != "" && response.Choices[0].FinishReason == "stop" && len(response.Choices[0].Message.ToolCalls) == 0 {
		// Some local servers also ignore required with one tool. Ask for that
		// tool's arguments through constrained JSON, then use the same tool
		// execution and citation validation path. Plain prose is never accepted.
		selected := body["tools"].([]json.RawMessage)
		var definition struct {
			Function struct {
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(selected[0], &definition); err != nil {
			return ReflectionMessage{}, err
		}
		delete(body, "tools")
		delete(body, "tool_choice")
		delete(body, "parallel_tool_calls")
		body["messages"] = append(append([]ReflectionMessage{}, messages...), ReflectionMessage{Role: "user", Content: "Return only the JSON arguments for the required function " + forcedTool + ". Use the supplied evidence and schema. Do not invent references."})
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": forcedTool, "strict": true, "schema": definition.Function.Parameters}}
		response.Choices = nil
		if err := jsonHTTP(ctx, p.client, http.MethodPost, p.cfg.BaseURL+"/chat/completions", p.cfg.APIKey, body, &response); err != nil {
			return ReflectionMessage{}, err
		}
		if len(response.Choices) != 1 || response.Choices[0].FinishReason != "stop" || !json.Valid([]byte(response.Choices[0].Message.Content)) {
			return ReflectionMessage{}, fmt.Errorf("required reflection arguments did not complete as JSON")
		}
		arguments := response.Choices[0].Message.Content
		wire, _ := json.Marshal(messages)
		call := ReflectionToolCall{ID: "structured-" + stableID(string(wire), forcedTool, arguments)[:24], Type: "function"}
		call.Function.Name = forcedTool
		call.Function.Arguments = arguments
		return ReflectionMessage{Role: "assistant", ToolCalls: []ReflectionToolCall{call}}, nil
	}
	if forcedTool == "" && response.Choices[0].FinishReason == "stop" && len(response.Choices[0].Message.ToolCalls) == 0 && response.Choices[0].Message.Content != "" {
		// Some local servers ignore required when several tools are offered.
		// Do not accept that prose as a grounded result. Ask the model to finish
		// through the sole done tool, then validate its evidence references.
		finalMessages := append(append([]ReflectionMessage{}, messages...), response.Choices[0].Message)
		finalMessages = append(finalMessages, ReflectionMessage{Role: "user", Content: "Finish by calling done. Put supporting IDs from the retrieved evidence in its reference arrays. Do not invent IDs."})
		return p.ReflectionStep(ctx, finalMessages, "done")
	}
	if response.Choices[0].FinishReason != "tool_calls" || len(response.Choices[0].Message.ToolCalls) != 1 {
		return ReflectionMessage{}, fmt.Errorf("reflection requires one completed tool call (finish=%q, calls=%d)", response.Choices[0].FinishReason, len(response.Choices[0].Message.ToolCalls))
	}
	return response.Choices[0].Message, nil
}

// Reflect runs a bounded native tool-calling loop over derived observations and
// raw facts. It performs no writes. Directives are supplied by the host.
func (s *SQLite) Reflect(ctx context.Context, scope Scope, req ReflectRequest, model ReflectionModel) (ReflectResult, error) {
	return s.reflect(ctx, scope, req, model, true)
}
func (s *SQLite) reflect(ctx context.Context, scope Scope, req ReflectRequest, model ReflectionModel, includeMentalModels bool) (ReflectResult, error) {
	if err := scope.Validate(); err != nil {
		return ReflectResult{}, err
	}
	if _, err := (RecallRequest{Query: req.Query}).normalized(); err != nil {
		return ReflectResult{}, err
	}
	if req.MaxSteps == 0 {
		req.MaxSteps = 10
	}
	if req.MaxContextTokens == 0 {
		req.MaxContextTokens = 100000
	}
	if model == nil || req.MaxSteps < 2 || req.MaxSteps > 20 || req.MaxContextTokens < 8192 || req.MaxContextTokens > 131072 {
		return ReflectResult{}, fmt.Errorf("reflection requires model, 2–20 steps and context budget 8192–131072")
	}
	before, err := s.reflectionState(ctx, scope)
	if err != nil {
		return ReflectResult{}, err
	}
	prompt := reflectPrompt
	forced := "search_observations"
	if includeMentalModels {
		var n int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM memory_mental_models WHERE app_id=? AND bank_id=? AND json_extract(payload,'$.is_stale')=0`, scope.AppID, scope.BankID).Scan(&n); err != nil {
			return ReflectResult{}, err
		}
		if n > 0 {
			prompt = reflectMentalModelsPrompt
			forced = "search_mental_models"
		}
	}
	if len(req.Directives) > 20 {
		return ReflectResult{}, fmt.Errorf("too many reflection directives")
	}
	directives := ""
	for _, directive := range req.Directives {
		if len(directive) > 2048 {
			return ReflectResult{}, fmt.Errorf("reflection directive too long")
		}
		directives += "\n- " + directive
	}
	messages := []ReflectionMessage{{Role: "system", Content: strings.ReplaceAll(prompt, "{current_datetime}", time.Now().UTC().Format("2006-01-02 15:04 UTC"))}, {Role: "user", Content: req.Query}}
	if directives != "" {
		messages[0].Content += "\n\nHost directives:" + directives
	}
	memories := map[string]bool{}
	observations := map[string]Observation{}
	mentalModels := map[string]MentalModel{}
	readableModels := map[string]MentalModel{}
	derivedProofs := map[string]bool{}
	callIDs := map[string]bool{}
	for step := 1; step <= req.MaxSteps; step++ {
		// Like upstream _finish, reserve the last iteration for done rather
		// than spending the entire budget retrieving and returning no answer.
		if step == req.MaxSteps && (len(memories) > 0 || len(observations) > 0 || len(mentalModels) > 0) {
			forced = "done"
		}
		wire, _ := json.Marshal(messages)
		if s.cfg.CountTokens(string(wire)) > req.MaxContextTokens {
			// Keep recent evidence with complete tool-call pairs and synthesize
			// before another retrieval can exhaust the context again.
			for len(messages) > 4 && s.cfg.CountTokens(string(wire)) > req.MaxContextTokens {
				messages = append(messages[:2:2], messages[4:]...)
				wire, _ = json.Marshal(messages)
			}
			if s.cfg.CountTokens(string(wire)) > req.MaxContextTokens {
				return ReflectResult{}, fmt.Errorf("reflection evidence exceeds context budget")
			}
			forced = "done"
		}
		message, err := model.ReflectionStep(ctx, messages, forced)
		if err != nil {
			return ReflectResult{}, err
		}
		if message.Role != "assistant" || len(message.ToolCalls) != 1 {
			return ReflectResult{}, fmt.Errorf("invalid reflection assistant turn")
		}
		call := message.ToolCalls[0]
		if call.Type != "function" || call.ID == "" || len(call.ID) > 512 || callIDs[call.ID] || len(call.Function.Arguments) > 32768 {
			return ReflectResult{}, fmt.Errorf("invalid reflection tool call")
		}
		callIDs[call.ID] = true
		if forced != "" && call.Function.Name != forced {
			return ReflectResult{}, fmt.Errorf("reflection skipped required retrieval")
		}
		forced = ""
		messages = append(messages, message)
		var result any
		switch call.Function.Name {
		case "search_mental_models":
			if prompt != reflectMentalModelsPrompt {
				return ReflectResult{}, fmt.Errorf("mental model retrieval is disabled")
			}
			var args struct {
				Reason     string `json:"reason"`
				Query      string `json:"query"`
				MaxResults int    `json:"max_results"`
			}
			if err = decodeReflectionArgs(call.Function.Arguments, &args); err != nil {
				return ReflectResult{}, err
			}
			if args.MaxResults == 0 {
				args.MaxResults = 5
			}
			found, err := s.SearchMentalModels(ctx, scope, args.Query, args.MaxResults)
			if err != nil {
				return ReflectResult{}, err
			}
			kept := []MentalModel{}
			for index, m := range found {
				full := m
				if index > 0 {
					content := []rune(m.Content)
					if len(content) > 300 {
						m.Content = string(content[:300]) + "..."
					}
					m.SourceFactIDs = nil
				}
				trial := append(append([]MentalModel{}, kept...), m)
				encoded, _ := json.Marshal(map[string]any{"mental_models": trial, "count": len(trial)})
				if s.cfg.CountTokens(string(encoded)) > min(8192, req.MaxContextTokens/3) {
					continue
				}
				kept = trial
				readableModels[m.ID] = full
				if index == 0 {
					mentalModels[m.ID] = full
				}
				for _, id := range m.SourceFactIDs {
					derivedProofs[id] = true
				}
			}
			result = map[string]any{"mental_models": kept, "count": len(kept)}
			if len(kept) == 0 {
				forced = "search_observations"
			}
		case "read_mental_models":
			var args struct {
				Reason string   `json:"reason"`
				IDs    []string `json:"mental_model_ids"`
			}
			if err = decodeReflectionArgs(call.Function.Arguments, &args); err != nil {
				return ReflectResult{}, err
			}
			if len(args.IDs) == 0 || len(args.IDs) > 20 {
				return ReflectResult{}, fmt.Errorf("invalid mental model read")
			}
			models := []MentalModel{}
			for _, id := range args.IDs {
				m, ok := readableModels[id]
				if !ok {
					return ReflectResult{}, fmt.Errorf("read requires a previously retrieved mental model")
				}
				models = append(models, m)
				mentalModels[id] = m
				for _, source := range m.SourceFactIDs {
					derivedProofs[source] = true
				}
			}
			result = map[string]any{"mental_models": models, "count": len(models)}
		case "search_observations", "recall":
			var args struct {
				Reason         string `json:"reason"`
				Query          string `json:"query"`
				MaxTokens      int    `json:"max_tokens"`
				MaxChunkTokens int    `json:"max_chunk_tokens"`
			}
			if err = decodeReflectionArgs(call.Function.Arguments, &args); err != nil {
				return ReflectResult{}, err
			}
			if args.MaxTokens == 0 {
				args.MaxTokens = 4096
			}
			if args.MaxTokens < 1 || args.MaxTokens > 32768 {
				return ReflectResult{}, fmt.Errorf("invalid reflection retrieval budget")
			}
			args.MaxTokens = min(args.MaxTokens, req.MaxContextTokens/3)
			if call.Function.Name == "recall" {
				if args.MaxChunkTokens == 0 {
					args.MaxChunkTokens = 1000
				}
				args.MaxChunkTokens = min(args.MaxChunkTokens, req.MaxContextTokens/3)
				r, err := s.Recall(ctx, scope, RecallRequest{Query: args.Query, Limit: 100, MaxTokens: args.MaxTokens, IncludeChunks: true, MaxChunkTokens: args.MaxChunkTokens})
				if err != nil {
					return ReflectResult{}, err
				}
				for _, f := range r.Results {
					memories[f.ID] = true
				}
				result = map[string]any{"memories": r.Results, "chunks": r.Chunks, "count": len(r.Results)}
			} else {
				found, err := s.SearchObservations(ctx, scope, args.Query, 20)
				if err != nil {
					return ReflectResult{}, err
				}
				kept := []Observation{}
				for _, o := range found {
					trial := append(append([]Observation{}, kept...), o)
					encoded, _ := json.Marshal(map[string]any{"observations": trial, "count": len(trial)})
					if s.cfg.CountTokens(string(encoded)) > args.MaxTokens {
						continue
					}
					kept = trial
					observations[o.ID] = o
					for _, id := range o.SourceFactIDs {
						derivedProofs[id] = true
					}
				}
				result = map[string]any{"observations": kept, "count": len(kept)}
				// Upstream's forced hierarchy checks raw memories after
				// observations, including apparently fresh observations.
				if len(memories) == 0 {
					forced = "recall"
				}
			}
		case "done":
			var args struct {
				Answer         string   `json:"answer"`
				MemoryIDs      []string `json:"memory_ids"`
				ObservationIDs []string `json:"observation_ids"`
				MentalModelIDs []string `json:"mental_model_ids"`
			}
			if err = decodeReflectionArgs(call.Function.Arguments, &args); err != nil {
				return ReflectResult{}, err
			}
			if strings.TrimSpace(args.Answer) == "" || len(args.Answer) > 32768 {
				return ReflectResult{}, fmt.Errorf("invalid reflection answer")
			}
			proofs := map[string]bool{}
			// Local models sometimes put a known observation ID in memory_ids.
			// Resolve only against evidence already returned in this scope, then
			// normalize the reference arrays. Unknown IDs remain hard errors.
			allIDs := append(append(append([]string{}, args.MemoryIDs...), args.ObservationIDs...), args.MentalModelIDs...)
			args.MemoryIDs = nil
			args.ObservationIDs = nil
			args.MentalModelIDs = nil
			seenReferences := map[string]bool{}
			for _, id := range allIDs {
				if seenReferences[id] {
					continue
				}
				seenReferences[id] = true
				if memories[id] || derivedProofs[id] {
					args.MemoryIDs = append(args.MemoryIDs, id)
				} else if _, ok := observations[id]; ok {
					args.ObservationIDs = append(args.ObservationIDs, id)
				} else if _, ok := mentalModels[id]; ok {
					args.MentalModelIDs = append(args.MentalModelIDs, id)
				} else {
					return ReflectResult{}, fmt.Errorf("answer cites unretrieved evidence")
				}
			}
			for _, id := range args.MemoryIDs {
				if !memories[id] && !derivedProofs[id] {
					return ReflectResult{}, fmt.Errorf("answer cites an unretrieved memory")
				}
				proofs[id] = true
			}
			for _, id := range args.ObservationIDs {
				o, ok := observations[id]
				if !ok {
					return ReflectResult{}, fmt.Errorf("answer cites an unretrieved observation")
				}
				for _, f := range o.SourceFactIDs {
					proofs[f] = true
				}
			}
			for _, id := range args.MentalModelIDs {
				m, ok := mentalModels[id]
				if !ok {
					return ReflectResult{}, fmt.Errorf("answer cites an unretrieved mental model")
				}
				for _, f := range m.SourceFactIDs {
					proofs[f] = true
				}
			}
			if len(memories)+len(observations)+len(mentalModels) > 0 && len(proofs) == 0 {
				return ReflectResult{}, fmt.Errorf("answer omits retrieved evidence references")
			}
			after, err := s.reflectionState(ctx, scope)
			if err != nil {
				return ReflectResult{}, err
			}
			if after != before {
				return ReflectResult{}, ErrConflict
			}
			ids := []string{}
			for id := range proofs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			return ReflectResult{Answer: args.Answer, MemoryIDs: args.MemoryIDs, ObservationIDs: args.ObservationIDs, MentalModelIDs: args.MentalModelIDs, SourceFactIDs: ids, Steps: step}, nil
		default:
			return ReflectResult{}, fmt.Errorf("unknown reflection tool %q", call.Function.Name)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return ReflectResult{}, err
		}
		messages = append(messages, ReflectionMessage{Role: "tool", ToolCallID: call.ID, Content: string(encoded)})
	}
	return ReflectResult{}, fmt.Errorf("reflection step budget exhausted")
}
func decodeReflectionArgs(content string, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid reflection arguments: %w", err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fmt.Errorf("trailing reflection arguments")
	}
	return nil
}
func (s *SQLite) reflectionState(ctx context.Context, scope Scope) (string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	state, err := bankState(ctx, tx, scope)
	if err != nil {
		return "", err
	}
	return state, tx.Commit()
}
