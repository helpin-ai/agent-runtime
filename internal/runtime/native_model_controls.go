package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/openai/openai-go/v3/responses"
)

// effectiveModelControls lets an explicit run snapshot replace legacy model
// controls without touching native context, tools, or execution limits.
func effectiveModelControls(execCtx *ExecutionContext) (sdk.ModelControls, error) {
	if execCtx != nil && execCtx.Run != nil && execCtx.Run.Input.Model != nil && execCtx.Run.Input.Model.Controls != nil {
		return *execCtx.Run.Input.Model.Controls, nil
	}
	var controls sdk.ModelControls
	if execCtx != nil && execCtx.Agent != nil && len(execCtx.Agent.ExecutionConfig) > 0 {
		if err := json.Unmarshal(execCtx.Agent.ExecutionConfig, &controls); err != nil {
			return controls, fmt.Errorf("invalid model controls: %w", err)
		}
	}
	return controls, nil
}

func nativeModelControls(execCtx *ExecutionContext, provider string) (*responses.ReasoningParam, *responses.ResponseNewParamsServiceTier, error) {
	config, err := effectiveModelControls(execCtx)
	if err != nil {
		return nil, nil, err
	}
	if err := sdk.ValidateModelControls(provider, config); err != nil {
		return nil, nil, err
	}
	var reasoning *responses.ReasoningParam
	var serviceTier *responses.ResponseNewParamsServiceTier
	if config.ReasoningEffort != nil {
		effort := strings.ToLower(strings.TrimSpace(*config.ReasoningEffort))
		if effort != "" {
			reasoning = &responses.ReasoningParam{Effort: responses.ReasoningEffort(effort)}
		}
	}
	if config.ServiceTier != nil {
		tier := strings.ToLower(strings.TrimSpace(*config.ServiceTier))
		switch tier {
		case "fast":
			tier = "priority"
		case "standard":
			tier = "default"
		}
		if tier != "" {
			value := responses.ResponseNewParamsServiceTier(tier)
			serviceTier = &value
		}
	}
	return reasoning, serviceTier, nil
}
