package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/openai/openai-go/v3/responses"
	"net/http"
	"os"
	"strings"

	agenticopenai "github.com/cloudwego/eino-ext/components/model/agenticopenai"
	einoclaude "github.com/cloudwego/eino-ext/components/model/claude"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const (
	defaultNativeAnthropicModel   = "claude-opus-4-8"
	defaultNativeOpenAIModel      = "gpt-5.6-terra"
	defaultNativeOpenRouterModel  = "openai/gpt-5.6-terra"
	defaultOpenAIResponsesBaseURL = "https://api.openai.com/v1"
	defaultOpenRouterBaseURL      = "https://openrouter.ai/api/v1"
	defaultNativeMaxTokens        = 16384
)

// ProviderCapability reports whether a native-SDK model provider is configured
// (i.e. its API key is present) along with the default model it would use.
type ProviderCapability struct {
	AuthModes                []string `json:"auth_modes"`
	RunCredentialsConfigured bool     `json:"run_credentials_configured"`
	Name                     string   `json:"name"`
	Configured               bool     `json:"configured"`
	DefaultModel             string   `json:"default_model"`
	BaseURLOverridden        bool     `json:"base_url_overridden"`
}

// NativeProviderCapabilities returns the configuration state of each supported
// native-SDK provider, derived from the environment. It is the single source of
// truth for which providers/models the runtime would use.
func NativeProviderCapabilities() []ProviderCapability {
	env := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	return []ProviderCapability{
		{
			AuthModes: []string{"api_key"}, RunCredentialsConfigured: env("AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY") != "",
			Name:              "anthropic",
			Configured:        env("ANTHROPIC_API_KEY") != "",
			DefaultModel:      defaultNativeAnthropicModel,
			BaseURLOverridden: env("ANTHROPIC_BASE_URL") != "",
		},
		{
			AuthModes: []string{"api_key"}, RunCredentialsConfigured: env("AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY") != "",
			Name:              "openai",
			Configured:        env("OPENAI_API_KEY") != "",
			DefaultModel:      defaultNativeOpenAIModel,
			BaseURLOverridden: env("OPENAI_BASE_URL") != "",
		},
		{
			AuthModes: []string{"api_key"}, RunCredentialsConfigured: env("AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY") != "",
			Name:              "openrouter",
			Configured:        env("OPENROUTER_API_KEY") != "",
			DefaultModel:      defaultNativeOpenRouterModel,
			BaseURLOverridden: env("OPENROUTER_BASE_URL") != "",
		},
		{Name: "openai_chatgpt", AuthModes: []string{"oauth"}, RunCredentialsConfigured: env("AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY") != "" && strings.EqualFold(env("AGENT_RUNTIME_CHATGPT_ENABLED"), "true")},
	}
}

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
	if apiKey == "" && !truthyEnv("AGENT_RUNTIME_NATIVE_EINO") && os.Getenv("AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY") == "" {
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
	policy, err := nativeContextPolicy(execCtx)
	if err != nil {
		return nil, err
	}
	if policy.Enabled {
		f.MaxTokens = policy.MaxOutputTokens
	}
	provider, modelName := f.resolveProviderAndModel(execCtx)
	var modelClient *http.Client
	var runRetries *int
	if execCtx != nil && execCtx.Run != nil && execCtx.Run.Input.CredentialSource == "app" {
		if execCtx.ModelCredentials == nil {
			return nil, fmt.Errorf("run model credentials are not configured")
		}
		retries := 0
		runRetries = &retries
		modelClient = &http.Client{Transport: execCtx.ModelCredentials.Transport(nil, execCtx.Run, provider), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		f.AnthropicAPIKey = "run-scoped"
		f.OpenAIAPIKey = "run-scoped"
		f.OpenRouterAPIKey = "run-scoped"
	}
	if provider == "openai_chatgpt" {
		if modelClient == nil || !execCtx.ModelCredentials.ChatGPTEnabled {
			return nil, fmt.Errorf("ChatGPT requires an enabled run credential")
		}
		modelClient.Transport = chatGPTTransport{base: modelClient.Transport}
	}
	reasoning, serviceTier, err := nativeModelControls(execCtx, provider)
	if err != nil {
		return nil, err
	}
	switch provider {
	case "anthropic", "":
		if strings.TrimSpace(f.AnthropicAPIKey) == "" {
			return nil, fmt.Errorf("anthropic API key is not configured")
		}
		cfg := &einoclaude.Config{
			HTTPClient: modelClient,
			APIKey:     strings.TrimSpace(f.AnthropicAPIKey),
			Model:      modelName,
			MaxTokens:  f.maxTokens(),
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
	case "openai", "openai_chatgpt":
		if strings.TrimSpace(f.OpenAIAPIKey) == "" {
			return nil, fmt.Errorf("openai API key is not configured")
		}
		maxTokens := f.maxTokens()
		modelConfig := &agenticopenai.ResponsesConfig{
			HTTPClient:  modelClient,
			MaxRetries:  runRetries,
			Reasoning:   reasoning,
			ServiceTier: serviceTier,
			APIKey:      strings.TrimSpace(f.OpenAIAPIKey),
			BaseURL:     resolveOpenAIResponsesBaseURL(f.OpenAIBaseURL),
			Model:       modelName,
			MaxTokens:   &maxTokens,
		}
		if provider == "openai_chatgpt" {
			store := false
			retries := 0
			modelConfig.BaseURL = "https://chatgpt.com/backend-api/codex"
			modelConfig.Store = &store
			modelConfig.MaxRetries = &retries
			modelConfig.MaxTokens = nil
			modelConfig.Include = []responses.ResponseIncludable{"reasoning.encrypted_content"}
		}
		model, err := agenticopenai.NewResponsesModel(ctx, modelConfig)
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
			HTTPClient:  modelClient,
			MaxRetries:  runRetries,
			Reasoning:   reasoning,
			APIKey:      strings.TrimSpace(f.OpenRouterAPIKey),
			BaseURL:     resolveOpenRouterBaseURL(f.OpenRouterBaseURL),
			Model:       modelName,
			MaxTokens:   &maxTokens,
			ExtraFields: openRouterExtraFields(execCtx),
		})
		if err != nil {
			return nil, err
		}
		return EinoAgenticModelFactory{Model: model, Provider: provider}.ResolveNativeModel(ctx, execCtx, definitions)
	default:
		return nil, fmt.Errorf("unsupported native Eino provider %q", provider)
	}
}

func openRouterExtraFields(execCtx *ExecutionContext) map[string]any {
	if execCtx == nil || execCtx.Agent == nil || len(execCtx.Agent.ExecutionConfig) == 0 {
		return nil
	}
	var config struct {
		OpenRouter struct {
			Provider struct {
				Quantizations []string `json:"quantizations"`
			} `json:"provider"`
		} `json:"openrouter"`
	}
	if err := json.Unmarshal(execCtx.Agent.ExecutionConfig, &config); err != nil {
		return nil
	}
	quantizations := normalizedOpenRouterQuantizations(config.OpenRouter.Provider.Quantizations)
	if len(quantizations) == 0 {
		return nil
	}
	return map[string]any{
		"provider": map[string]any{"quantizations": quantizations},
	}
}

func normalizedOpenRouterQuantizations(values []string) []string {
	quantizations := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		quantizations = append(quantizations, value)
	}
	return quantizations
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
	if execCtx != nil && execCtx.Run != nil && execCtx.Run.Input.Model != nil {
		provider = execCtx.Run.Input.Model.Provider
		modelName = execCtx.Run.Input.Model.Model
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
	case "openai", "openai_chatgpt":
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
