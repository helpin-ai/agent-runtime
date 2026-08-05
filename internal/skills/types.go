package skills

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const (
	SourceBuiltIn    = "built_in"
	SourceWorkspace  = "workspace"
	SourceImported   = "imported"
	SourceConfigured = "configured"

	InteractionKindRequestUserInput = "request_user_input"
	InteractionKindApprovalRequest  = "approval_request"
	InteractionKindReviewCheckpoint = "review_checkpoint"

	TransportTypeToolCall      = "tool_call"
	TransportTypeFencedJSON    = "fenced_json"
	TransportTypeRuntimeBridge = "runtime_bridge"

	RuntimeSkillRoleConfigKey   = "runtime_skill_role"
	RuntimeSkillRoleInstruction = "instruction"
	RuntimeSkillRoleAvailable   = "available"
)

type Definition struct {
	Key               string
	Title             string
	Description       string
	SourceKind        string
	PackagePath       string
	Instructions      string
	RequiredTools     []string
	SupportedRuntimes []string
	Policy            Policy
	Interface         Interface
}

type Interface struct {
	DisplayName      string `json:"display_name,omitempty" yaml:"display_name"`
	ShortDescription string `json:"short_description,omitempty" yaml:"short_description"`
	IconSmall        string `json:"icon_small,omitempty" yaml:"icon_small"`
	IconLarge        string `json:"icon_large,omitempty" yaml:"icon_large"`
	BrandColor       string `json:"brand_color,omitempty" yaml:"brand_color"`
	DefaultPrompt    string `json:"default_prompt,omitempty" yaml:"default_prompt"`
}

type Policy struct {
	AllowImplicitInvocation            *bool                 `json:"allow_implicit_invocation,omitempty" yaml:"allow_implicit_invocation,omitempty"`
	CompletionRequiresInteractionKinds []string              `json:"completion_requires_interaction_kinds,omitempty" yaml:"completion_requires_interaction_kinds,omitempty"`
	InteractionContracts               []InteractionContract `json:"interaction_contracts,omitempty" yaml:"interaction_contracts,omitempty"`
}

type InteractionContract struct {
	Kind       string                          `json:"kind,omitempty" yaml:"kind,omitempty"`
	Schema     string                          `json:"schema,omitempty" yaml:"schema,omitempty"`
	Transports map[string]InteractionTransport `json:"transports,omitempty" yaml:"transports,omitempty"`
}

type InteractionTransport struct {
	Type       string `json:"type,omitempty" yaml:"type,omitempty"`
	ToolName   string `json:"tool_name,omitempty" yaml:"tool_name,omitempty"`
	BlockLabel string `json:"block_label,omitempty" yaml:"block_label,omitempty"`
}

type ResolvedRef struct {
	SkillID    string          `json:"skill_id,omitempty"`
	Key        string          `json:"key,omitempty"`
	VersionKey string          `json:"version_key,omitempty"`
	Version    string          `json:"version,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
}

func RefFromCore(ref agentcore.SkillRef) ResolvedRef {
	out := ResolvedRef{
		SkillID:    strings.TrimSpace(ref.SkillID),
		Key:        strings.TrimSpace(ref.Key),
		VersionKey: strings.TrimSpace(ref.VersionKey),
		Version:    strings.TrimSpace(ref.Version),
	}
	if len(bytes.TrimSpace(ref.Config)) > 0 && !bytes.Equal(bytes.TrimSpace(ref.Config), []byte("null")) {
		out.Config = append(json.RawMessage(nil), ref.Config...)
	}
	return out
}

func RefToCore(ref ResolvedRef) agentcore.SkillRef {
	return agentcore.SkillRef{
		SkillID:    strings.TrimSpace(ref.SkillID),
		Key:        strings.TrimSpace(ref.Key),
		VersionKey: strings.TrimSpace(ref.VersionKey),
		Version:    strings.TrimSpace(firstNonEmpty(ref.Version, ref.VersionKey)),
		Config:     append(json.RawMessage(nil), ref.Config...),
	}
}

func NormalizeRefs(refs []agentcore.SkillRef) []ResolvedRef {
	out := make([]ResolvedRef, 0, len(refs))
	for _, ref := range refs {
		normalized := RefFromCore(ref)
		if normalized.SkillID == "" && normalized.Key == "" {
			continue
		}
		out = append(out, normalized)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
