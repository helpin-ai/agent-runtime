package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/responses"
)

func nativeModelControls(execCtx *ExecutionContext, provider string) (*responses.ReasoningParam, *responses.ResponseNewParamsServiceTier, error) {
	var config struct {
		ReasoningEffort string `json:"reasoning_effort"`
		ServiceTier     string `json:"service_tier"`
	}
	if execCtx != nil && execCtx.Agent != nil && len(execCtx.Agent.ExecutionConfig) > 0 {
		if err := json.Unmarshal(execCtx.Agent.ExecutionConfig, &config); err != nil {
			return nil, nil, fmt.Errorf("invalid model controls: %w", err)
		}
	}
	effort, tier := strings.ToLower(strings.TrimSpace(config.ReasoningEffort)), strings.ToLower(strings.TrimSpace(config.ServiceTier))
	var reasoning *responses.ReasoningParam
	var serviceTier *responses.ResponseNewParamsServiceTier
	if effort != "" {
		if provider != "openai_chatgpt" && provider != "openai" && provider != "openrouter" && provider != "openrouter_responses" {
			return nil, nil, fmt.Errorf("reasoning_effort requires an OpenAI Responses provider")
		}
		switch effort {
		case "none", "minimal", "low", "medium", "high", "xhigh":
		default:
			return nil, nil, fmt.Errorf("unsupported reasoning_effort %q", effort)
		}
		reasoning = &responses.ReasoningParam{Effort: responses.ReasoningEffort(effort)}
	}
	if tier != "" {
		if provider != "openai" && provider != "openai_chatgpt" {
			return nil, nil, fmt.Errorf("service_tier requires provider openai")
		}
		switch tier {
		case "fast":
			tier = "priority"
		case "standard":
			tier = "default"
		case "auto", "default", "flex", "priority":
		default:
			return nil, nil, fmt.Errorf("unsupported service_tier %q", tier)
		}
		value := responses.ResponseNewParamsServiceTier(tier)
		serviceTier = &value
	}
	return reasoning, serviceTier, nil
}
