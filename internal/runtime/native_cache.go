package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const (
	nativeCacheModeDisabled   = "disabled"
	nativeCacheModeOpenAI     = "openai_responses"
	nativeCacheModeOpenRouter = "openrouter_responses"
	nativeCacheModeAnthropic  = "anthropic_prompt_cache"

	nativeCacheRetention24h = "24h"
	nativeCacheTTL1h        = "1h"
)

type NativeCacheMetadata struct {
	Enabled          bool   `json:"enabled"`
	Provider         string `json:"provider,omitempty"`
	Mode             string `json:"mode,omitempty"`
	Scope            string `json:"scope,omitempty"`
	CacheKeyHash     string `json:"cache_key_hash,omitempty"`
	Retention        string `json:"retention,omitempty"`
	AnthropicTTL     string `json:"anthropic_ttl,omitempty"`
	SessionIDEnabled bool   `json:"session_id_enabled,omitempty"`
	Downgraded       bool   `json:"downgraded,omitempty"`
	DowngradeReason  string `json:"downgrade_reason,omitempty"`
}

type nativeCacheProfile struct {
	metadata NativeCacheMetadata
	cacheKey string
}

func nativeCacheProfileForExecution(execCtx *ExecutionContext, provider, modelName string, definitions []tools.Definition) nativeCacheProfile {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "anthropic"
	}
	provider = strings.TrimSpace(strings.ToLower(provider))
	modelName = strings.TrimSpace(modelName)
	mode := nativeCacheModeForProvider(provider)
	if mode == nativeCacheModeDisabled {
		return nativeCacheProfile{metadata: NativeCacheMetadata{Enabled: false, Provider: provider, Mode: mode}}
	}

	keyHash := nativeCacheKeyHash(execCtx, provider, modelName, definitions)
	profile := nativeCacheProfile{
		cacheKey: "ar:" + keyHash,
		metadata: NativeCacheMetadata{
			Enabled:      true,
			Provider:     provider,
			Mode:         mode,
			Scope:        "workspace_agent",
			CacheKeyHash: keyHash,
		},
	}
	switch mode {
	case nativeCacheModeOpenAI:
		profile.metadata.Retention = nativeCacheRetention24h
	case nativeCacheModeOpenRouter:
		profile.metadata.Retention = nativeCacheRetention24h
		profile.metadata.SessionIDEnabled = true
	case nativeCacheModeAnthropic:
		profile.metadata.AnthropicTTL = nativeCacheTTL1h
	}
	return profile
}

func nativeCacheModeForProvider(provider string) string {
	switch strings.TrimSpace(strings.ToLower(provider)) {
	case "openai":
		return nativeCacheModeOpenAI
	case "openrouter", "openrouter_responses":
		return nativeCacheModeOpenRouter
	case "anthropic", "":
		return nativeCacheModeAnthropic
	default:
		return nativeCacheModeDisabled
	}
}

func (p nativeCacheProfile) enabled() bool {
	return p.metadata.Enabled && strings.TrimSpace(p.cacheKey) != ""
}

func (p nativeCacheProfile) summary() NativeCacheMetadata {
	return p.metadata
}

func (p nativeCacheProfile) downgraded(reason string) nativeCacheProfile {
	p.metadata.Downgraded = true
	p.metadata.DowngradeReason = truncateNativeText(reason, 240)
	return p
}

func nativeCacheKeyHash(execCtx *ExecutionContext, provider, modelName string, definitions []tools.Definition) string {
	identity := map[string]any{
		"scope":              "workspace_agent",
		"workspace":          nativeCacheWorkspaceKey(execCtx),
		"agent_id":           nativeCacheAgentID(execCtx),
		"provider":           strings.TrimSpace(strings.ToLower(provider)),
		"model":              strings.TrimSpace(modelName),
		"runtime":            "native_sdk",
		"static_prompt_hash": nativeCacheStaticPromptHash(execCtx),
		"tool_schema_hash":   nativeCacheToolSchemaHash(definitions),
	}
	body, _ := json.Marshal(identity)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:32]
}

func nativeCacheWorkspaceKey(execCtx *ExecutionContext) string {
	if execCtx == nil {
		return ""
	}
	if execCtx.Run != nil && execCtx.Run.Input.Metadata != nil {
		for _, key := range []string{"workspace_id", "workspace_identifier", "workspace"} {
			if value := strings.TrimSpace(fmt.Sprint(execCtx.Run.Input.Metadata[key])); value != "" && value != "<nil>" {
				return value
			}
		}
	}
	if strings.TrimSpace(execCtx.AppID) != "" {
		return strings.TrimSpace(execCtx.AppID)
	}
	if execCtx.Run != nil {
		return strings.TrimSpace(execCtx.Run.AppID)
	}
	return ""
}

func nativeCacheAgentID(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.Agent == nil {
		return ""
	}
	if strings.TrimSpace(execCtx.Agent.ID) != "" {
		return strings.TrimSpace(execCtx.Agent.ID)
	}
	return strings.TrimSpace(execCtx.Agent.Name)
}

func nativeCacheStaticPromptHash(execCtx *ExecutionContext) string {
	parts := []string{}
	if execCtx != nil {
		if execCtx.Agent != nil && strings.TrimSpace(execCtx.Agent.SystemPrompt) != "" {
			parts = append(parts, strings.TrimSpace(execCtx.Agent.SystemPrompt))
		}
		if strings.TrimSpace(execCtx.SkillInstructions) != "" {
			parts = append(parts, strings.TrimSpace(execCtx.SkillInstructions))
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n\n")))
	return hex.EncodeToString(sum[:])[:16]
}

func nativeCacheToolSchemaHash(definitions []tools.Definition) string {
	defs := append([]tools.Definition(nil), definitions...)
	sort.SliceStable(defs, func(i, j int) bool {
		return tools.CanonicalName(defs[i].Name) < tools.CanonicalName(defs[j].Name)
	})
	body, _ := json.Marshal(defs)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:16]
}

func nativeCacheSpecificError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"prompt_cache",
		"prompt cache",
		"session_id",
		"previous_response_id",
		"unknown parameter",
		"unsupported parameter",
		"unrecognized request argument",
		"invalid request",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
