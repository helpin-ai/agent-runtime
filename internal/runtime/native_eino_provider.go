package runtime

import (
	"context"
	"fmt"
	"os"
	"strings"

	agenticopenai "github.com/cloudwego/eino-ext/components/model/agenticopenai"
	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const (
	defaultNativeAnthropicModel   = "claude-sonnet-4-6"
	defaultNativeOpenAIModel      = "gpt-4.1"
	defaultNativeOpenRouterModel  = "openai/gpt-4.1"
	defaultOpenAIResponsesBaseURL = "https://api.openai.com/v1"
	defaultOpenRouterBaseURL      = "https://openrouter.ai/api/v1"
	defaultNativeMaxTokens        = 16384
)

type EinoProviderFactory struct {
	AnthropicAPIKey   string
	AnthropicBaseURL  string
	OpenAIAPIKey      string
	OpenAIBaseURL     string
	OpenRouterAPIKey  string
	OpenRouterBaseURL string
	DefaultProvider   string
	DefaultModel      string
	MaxTokens         int
}

func DefaultNativeConfigFromEnv() NativeConfig {
	apiKey := firstNonEmpty(
		os.Getenv("ANTHROPIC_API_KEY"),
		os.Getenv("OPENAI_API_KEY"),
		os.Getenv("OPENROUTER_API_KEY"),
	)
	if apiKey == "" && !truthyEnv("AGENT_RUNTIME_NATIVE_EINO") {
		return NativeConfig{}
	}
	return NativeConfig{
		ModelFactory: EinoProviderFactory{
			AnthropicAPIKey:   strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
			AnthropicBaseURL:  strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
			OpenAIAPIKey:      strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
			OpenAIBaseURL:     strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
			OpenRouterAPIKey:  strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")),
			OpenRouterBaseURL: strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL")),
			DefaultProvider:   strings.TrimSpace(os.Getenv("AGENT_RUNTIME_NATIVE_PROVIDER")),
			DefaultModel:      strings.TrimSpace(os.Getenv("AGENT_RUNTIME_NATIVE_MODEL")),
			MaxTokens:         defaultNativeMaxTokens,
		},
		MaxToolSteps: defaultNativeMaxToolSteps,
	}
}

func (f EinoProviderFactory) ResolveNativeModel(ctx context.Context, execCtx *ExecutionContext, definitions []tools.Definition) (NativeModel, error) {
	provider, modelName := f.resolveProviderAndModel(execCtx)
	switch provider {
	case "anthropic", "":
		if strings.TrimSpace(f.AnthropicAPIKey) == "" {
			return nil, fmt.Errorf("anthropic API key is not configured")
		}
		cfg := &einoclaude.Config{
			APIKey:    strings.TrimSpace(f.AnthropicAPIKey),
			Model:     modelName,
			MaxTokens: f.maxTokens(),
		}
		if strings.TrimSpace(f.AnthropicBaseURL) != "" {
			baseURL := strings.TrimSpace(f.AnthropicBaseURL)
			cfg.BaseURL = &baseURL
		}
		model, err := einoclaude.NewChatModel(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return EinoChatModelFactory{Model: model}.ResolveNativeModel(ctx, execCtx, definitions)
	case "openai":
		if strings.TrimSpace(f.OpenAIAPIKey) == "" {
			return nil, fmt.Errorf("openai API key is not configured")
		}
		maxTokens := f.maxTokens()
		model, err := agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{
			APIKey:    strings.TrimSpace(f.OpenAIAPIKey),
			BaseURL:   resolveOpenAIResponsesBaseURL(f.OpenAIBaseURL),
			Model:     modelName,
			MaxTokens: &maxTokens,
		})
		if err != nil {
			return nil, err
		}
		return EinoAgenticModelFactory{Model: model, Provider: provider}.ResolveNativeModel(ctx, execCtx, definitions)
	case "openrouter", "openrouter_responses":
		if strings.TrimSpace(f.OpenRouterAPIKey) == "" {
			return nil, fmt.Errorf("openrouter API key is not configured")
		}
		maxTokens := f.maxTokens()
		model, err := agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{
			APIKey:    strings.TrimSpace(f.OpenRouterAPIKey),
			BaseURL:   resolveOpenRouterBaseURL(f.OpenRouterBaseURL),
			Model:     modelName,
			MaxTokens: &maxTokens,
		})
		if err != nil {
			return nil, err
		}
		return EinoAgenticModelFactory{Model: model, Provider: provider}.ResolveNativeModel(ctx, execCtx, definitions)
	default:
		return nil, fmt.Errorf("unsupported native Eino provider %q", provider)
	}
}

func (f EinoProviderFactory) resolveProviderAndModel(execCtx *ExecutionContext) (string, string) {
	provider := strings.TrimSpace(f.DefaultProvider)
	modelName := strings.TrimSpace(f.DefaultModel)
	if execCtx != nil && execCtx.Agent != nil {
		if strings.TrimSpace(execCtx.Agent.Provider) != "" {
			provider = strings.TrimSpace(execCtx.Agent.Provider)
		}
		if strings.TrimSpace(execCtx.Agent.Model) != "" {
			modelName = strings.TrimSpace(execCtx.Agent.Model)
		}
	}
	if provider == "" {
		provider = "anthropic"
	}
	if modelName == "" {
		modelName = defaultNativeModelForProvider(provider)
	}
	return provider, modelName
}

func defaultNativeModelForProvider(provider string) string {
	switch strings.TrimSpace(provider) {
	case "openai":
		return defaultNativeOpenAIModel
	case "openrouter", "openrouter_responses":
		return defaultNativeOpenRouterModel
	default:
		return defaultNativeAnthropicModel
	}
}

func resolveOpenAIResponsesBaseURL(baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		return defaultOpenAIResponsesBaseURL
	}
	return strings.TrimSpace(baseURL)
}

func resolveOpenRouterBaseURL(baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		return defaultOpenRouterBaseURL
	}
	return strings.TrimSpace(baseURL)
}

func (f EinoProviderFactory) maxTokens() int {
	if f.MaxTokens > 0 {
		return f.MaxTokens
	}
	return defaultNativeMaxTokens
}

func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
