package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type WorkspaceLookup interface {
	GetByID(ctx context.Context, appID, id string) (*WorkspaceSkill, error)
	GetActiveByKey(ctx context.Context, appID, key string) (*WorkspaceSkill, error)
}

type ContextualWorkspaceLookup interface {
	GetByIDForContext(ctx context.Context, req LookupRequest) (*WorkspaceSkill, error)
	GetActiveByKeyForContext(ctx context.Context, req LookupRequest) (*WorkspaceSkill, error)
}

type LookupContext struct {
	AppID    string                 `json:"app_id"`
	AgentID  string                 `json:"agent_id,omitempty"`
	RunID    string                 `json:"run_id,omitempty"`
	Target   agentcore.TargetRef    `json:"target,omitempty"`
	Trigger  map[string]interface{} `json:"trigger,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type LookupRequest struct {
	LookupContext
	SkillID string `json:"skill_id,omitempty"`
	Key     string `json:"key,omitempty"`
}

type WorkspaceSkill struct {
	ID                string    `json:"id"`
	Key               string    `json:"key"`
	VersionKey        string    `json:"version_key"`
	Title             string    `json:"title"`
	Description       string    `json:"description"`
	SourceKind        string    `json:"source_kind"`
	Instructions      string    `json:"instructions"`
	RequiredTools     []string  `json:"required_tools,omitempty"`
	SupportedRuntimes []string  `json:"supported_runtimes,omitempty"`
	Policy            Policy    `json:"policy,omitempty"`
	Interface         Interface `json:"interface,omitempty"`
	PackageObjectKey  string    `json:"package_object_key,omitempty"`
	PackageFileName   string    `json:"package_file_name,omitempty"`
	PackageChecksum   string    `json:"package_checksum,omitempty"`
	PackageSize       int64     `json:"package_size,omitempty"`
	Archived          bool      `json:"is_archived,omitempty"`
}

type Registry struct {
	mu         sync.RWMutex
	builtIns   map[string]Definition
	lookup     WorkspaceLookup
	appLookups map[string]WorkspaceLookup
}

func NewRegistry(definitions ...Definition) *Registry {
	r := &Registry{builtIns: map[string]Definition{}, appLookups: map[string]WorkspaceLookup{}}
	for _, definition := range definitions {
		r.RegisterBuiltIn(definition)
	}
	return r
}

func (r *Registry) SetWorkspaceLookup(lookup WorkspaceLookup) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookup = lookup
}

func (r *Registry) SetWorkspaceLookupForApp(appID string, lookup WorkspaceLookup) {
	if r == nil {
		return
	}
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.appLookups == nil {
		r.appLookups = map[string]WorkspaceLookup{}
	}
	if lookup == nil {
		delete(r.appLookups, appID)
		return
	}
	r.appLookups[appID] = lookup
}

func (r *Registry) WorkspaceLookupForApp(appID string) WorkspaceLookup {
	if r == nil {
		return nil
	}
	return r.workspaceLookup(appID)
}

func (r *Registry) RegisterBuiltIn(definition Definition) {
	if r == nil {
		return
	}
	definition = normalizeDefinition(definition)
	if definition.Key == "" {
		return
	}
	if definition.SourceKind == "" {
		definition.SourceKind = SourceBuiltIn
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.builtIns[definition.Key] = definition
}

func (r *Registry) BuiltIn(key string) (Definition, bool) {
	if r == nil {
		return Definition{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	definition, ok := r.builtIns[strings.TrimSpace(key)]
	return definition, ok
}

func (r *Registry) ListBuiltIns() []Definition {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.builtIns))
	for key := range r.builtIns {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Definition, 0, len(keys))
	for _, key := range keys {
		out = append(out, r.builtIns[key])
	}
	return out
}

type Resolution struct {
	Refs                   []ResolvedRef
	CoreRefs               []agentcore.SkillRef
	Definitions            []Definition
	InstructionRefs        []agentcore.SkillRef
	InstructionDefinitions []Definition
	AvailableRefs          []agentcore.SkillRef
	AvailableDefinitions   []Definition
	UsesExplicitRoles      bool
	Instructions           string
	Policy                 Policy
}

func (r *Registry) Resolve(ctx context.Context, appID string, refs []agentcore.SkillRef) (Resolution, error) {
	return r.ResolveForContext(ctx, LookupContext{AppID: appID}, refs)
}

func (r *Registry) ResolveForContext(ctx context.Context, lookupCtx LookupContext, refs []agentcore.SkillRef) (Resolution, error) {
	normalized := NormalizeRefs(refs)
	if len(normalized) == 0 {
		return Resolution{}, nil
	}
	if r == nil {
		return Resolution{}, fmt.Errorf("skill registry is not configured")
	}

	canonical := make([]ResolvedRef, 0, len(normalized))
	coreRefs := make([]agentcore.SkillRef, 0, len(normalized))
	definitions := make([]Definition, 0, len(normalized))
	seen := make(map[string]struct{}, len(normalized))
	for _, ref := range normalized {
		resolvedRef, definition, identity, err := r.resolveOne(ctx, lookupCtx, ref)
		if err != nil {
			return Resolution{}, err
		}
		if _, exists := seen[identity]; exists {
			return Resolution{}, fmt.Errorf("duplicate skill reference %q", identity)
		}
		seen[identity] = struct{}{}
		canonical = append(canonical, resolvedRef)
		coreRefs = append(coreRefs, RefToCore(resolvedRef))
		definitions = append(definitions, definition)
	}

	resolution := Resolution{
		Refs:         canonical,
		CoreRefs:     coreRefs,
		Definitions:  definitions,
		Instructions: CompileInstructions(definitions),
		Policy:       AggregatePolicy(definitions),
	}
	resolution.partitionByRuntimeRole()
	return resolution, nil
}

func (r *Resolution) partitionByRuntimeRole() {
	if r == nil || len(r.CoreRefs) != len(r.Definitions) {
		return
	}
	for index, ref := range r.CoreRefs {
		role := runtimeSkillRole(ref.Config)
		if role != "" {
			r.UsesExplicitRoles = true
		}
		switch role {
		case RuntimeSkillRoleAvailable:
			r.AvailableRefs = append(r.AvailableRefs, ref)
			r.AvailableDefinitions = append(r.AvailableDefinitions, r.Definitions[index])
		default:
			r.InstructionRefs = append(r.InstructionRefs, ref)
			r.InstructionDefinitions = append(r.InstructionDefinitions, r.Definitions[index])
		}
	}
	if !r.UsesExplicitRoles {
		return
	}
	// Explicit-role callers have already compiled instruction modules into the
	// agent system prompt. The runtime still resolves them to enforce policy,
	// but does not inject or stage their text a second time.
	r.Instructions = ""
	r.Policy = AggregatePolicy(r.InstructionDefinitions)
}

func runtimeSkillRole(config json.RawMessage) string {
	if len(config) == 0 {
		return ""
	}
	var values map[string]any
	if err := json.Unmarshal(config, &values); err != nil {
		return ""
	}
	role, _ := values[RuntimeSkillRoleConfigKey].(string)
	switch strings.TrimSpace(role) {
	case RuntimeSkillRoleInstruction:
		return RuntimeSkillRoleInstruction
	case RuntimeSkillRoleAvailable:
		return RuntimeSkillRoleAvailable
	default:
		return ""
	}
}

func (r *Registry) resolveOne(ctx context.Context, lookupCtx LookupContext, ref ResolvedRef) (ResolvedRef, Definition, string, error) {
	ref.Key = strings.TrimSpace(ref.Key)
	ref.SkillID = strings.TrimSpace(ref.SkillID)
	ref.VersionKey = strings.TrimSpace(ref.VersionKey)
	ref.Version = strings.TrimSpace(ref.Version)

	if ref.SkillID != "" {
		lookup := r.workspaceLookup(lookupCtx.AppID)
		if lookup == nil {
			return ResolvedRef{}, Definition{}, "", fmt.Errorf("workspace skill lookup is not configured")
		}
		skill, err := getWorkspaceSkillByID(ctx, lookup, lookupCtx, ref.SkillID)
		if err != nil {
			return ResolvedRef{}, Definition{}, "", err
		}
		if skill == nil || skill.Archived {
			return ResolvedRef{}, Definition{}, "", fmt.Errorf("workspace skill not found")
		}
		if !versionMatches(ref, skill.VersionKey) {
			return ResolvedRef{}, Definition{}, "", fmt.Errorf("workspace skill %q version mismatch", skill.Key)
		}
		definition := definitionFromWorkspaceSkill(skill)
		ref.SkillID = strings.TrimSpace(skill.ID)
		ref.Key = definition.Key
		ref.VersionKey = strings.TrimSpace(skill.VersionKey)
		ref.Version = firstNonEmpty(ref.Version, ref.VersionKey)
		return ref, definition, "workspace:" + ref.SkillID, nil
	}

	if ref.Key == "" {
		return ResolvedRef{}, Definition{}, "", fmt.Errorf("skill key is required")
	}
	if definition, ok := r.BuiltIn(ref.Key); ok {
		ref.Key = definition.Key
		return ref, definition, "builtin:" + definition.Key, nil
	}

	lookup := r.workspaceLookup(lookupCtx.AppID)
	if lookup == nil {
		return ResolvedRef{}, Definition{}, "", fmt.Errorf("unknown skill %q", ref.Key)
	}
	skill, err := getActiveWorkspaceSkillByKey(ctx, lookup, lookupCtx, ref.Key)
	if err != nil {
		return ResolvedRef{}, Definition{}, "", err
	}
	if skill == nil || skill.Archived {
		return ResolvedRef{}, Definition{}, "", fmt.Errorf("unknown skill %q", ref.Key)
	}
	if !versionMatches(ref, skill.VersionKey) {
		return ResolvedRef{}, Definition{}, "", fmt.Errorf("workspace skill %q version mismatch", skill.Key)
	}
	definition := definitionFromWorkspaceSkill(skill)
	ref.SkillID = strings.TrimSpace(skill.ID)
	ref.Key = definition.Key
	ref.VersionKey = strings.TrimSpace(skill.VersionKey)
	ref.Version = firstNonEmpty(ref.Version, ref.VersionKey)
	return ref, definition, "workspace:" + ref.SkillID, nil
}

func getWorkspaceSkillByID(ctx context.Context, lookup WorkspaceLookup, lookupCtx LookupContext, id string) (*WorkspaceSkill, error) {
	if contextual, ok := lookup.(ContextualWorkspaceLookup); ok {
		return contextual.GetByIDForContext(ctx, LookupRequest{LookupContext: lookupCtx, SkillID: strings.TrimSpace(id)})
	}
	return lookup.GetByID(ctx, strings.TrimSpace(lookupCtx.AppID), strings.TrimSpace(id))
}

func getActiveWorkspaceSkillByKey(ctx context.Context, lookup WorkspaceLookup, lookupCtx LookupContext, key string) (*WorkspaceSkill, error) {
	if contextual, ok := lookup.(ContextualWorkspaceLookup); ok {
		return contextual.GetActiveByKeyForContext(ctx, LookupRequest{LookupContext: lookupCtx, Key: strings.TrimSpace(key)})
	}
	return lookup.GetActiveByKey(ctx, strings.TrimSpace(lookupCtx.AppID), strings.TrimSpace(key))
}

func (r *Registry) workspaceLookup(appID string) WorkspaceLookup {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if lookup := r.appLookups[strings.TrimSpace(appID)]; lookup != nil {
		return lookup
	}
	return r.lookup
}

func versionMatches(ref ResolvedRef, skillVersion string) bool {
	expected := firstNonEmpty(ref.VersionKey, ref.Version)
	return expected == "" || expected == strings.TrimSpace(skillVersion)
}

func definitionFromWorkspaceSkill(skill *WorkspaceSkill) Definition {
	if skill == nil {
		return Definition{}
	}
	definition := Definition{
		Key:               strings.TrimSpace(skill.Key),
		Title:             strings.TrimSpace(skill.Title),
		Description:       strings.TrimSpace(skill.Description),
		SourceKind:        strings.TrimSpace(skill.SourceKind),
		Instructions:      strings.TrimSpace(skill.Instructions),
		RequiredTools:     append([]string(nil), skill.RequiredTools...),
		SupportedRuntimes: append([]string(nil), skill.SupportedRuntimes...),
		Policy:            skill.Policy,
		Interface:         skill.Interface,
	}
	if definition.SourceKind == "" {
		definition.SourceKind = SourceWorkspace
	}
	return normalizeDefinition(definition)
}

func normalizeDefinition(definition Definition) Definition {
	definition.Key = strings.TrimSpace(definition.Key)
	definition.Title = strings.TrimSpace(definition.Title)
	definition.Description = strings.TrimSpace(definition.Description)
	definition.SourceKind = strings.TrimSpace(definition.SourceKind)
	definition.PackagePath = strings.TrimSpace(definition.PackagePath)
	definition.Instructions = strings.TrimSpace(definition.Instructions)
	definition.RequiredTools = normalizeStringList(definition.RequiredTools)
	definition.SupportedRuntimes = normalizeStringList(definition.SupportedRuntimes)
	definition.Policy.InteractionContracts = NormalizeInteractionContracts(definition.Policy.InteractionContracts)
	definition.Policy.CompletionRequiresInteractionKinds = SortedUniqueStrings(definition.Policy.CompletionRequiresInteractionKinds)
	return definition
}

func ValidateRuntimeAndTools(runtimeKind string, allowedTools []string, definitions []Definition) error {
	runtimeKind = strings.TrimSpace(runtimeKind)
	allowedSet := make(map[string]struct{}, len(allowedTools))
	for _, toolName := range NormalizeToolNames(allowedTools) {
		allowedSet[toolName] = struct{}{}
	}
	for _, definition := range definitions {
		if len(definition.SupportedRuntimes) > 0 && runtimeKind != "" && !runtimeSupportedBySkill(definition.SupportedRuntimes, runtimeKind) {
			return fmt.Errorf("skill %q does not support runtime %q", definition.Key, runtimeKind)
		}
		for _, toolName := range NormalizeToolNames(definition.RequiredTools) {
			if _, ok := allowedSet[toolName]; !ok {
				return fmt.Errorf("skill %q requires tool %q", definition.Key, toolName)
			}
		}
	}
	return nil
}

func runtimeSupportedBySkill(supported []string, runtimeKind string) bool {
	for _, value := range supported {
		if strings.TrimSpace(value) == runtimeKind {
			return true
		}
	}
	return runtimeKind == "codex" && containsString(supported, agentcore.RuntimeNativeSDK)
}

func NormalizeToolNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	seen := map[string]struct{}{}
	for _, name := range names {
		name = tools.CanonicalName(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}
