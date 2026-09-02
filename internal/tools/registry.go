package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type Definition struct {
	Name                 string      `json:"name"`
	Description          string      `json:"description"`
	Category             string      `json:"category"`
	InputSchema          interface{} `json:"input_schema"`
	Mutating             bool        `json:"mutating"`
	RiskLevel            string      `json:"risk_level,omitempty"`
	SupportedTargetTypes []string    `json:"supported_target_types,omitempty"`
}

const (
	RiskLevelRead        = "read"
	RiskLevelRoutine     = "routine_mutation"
	RiskLevelSensitive   = "sensitive_mutation"
	RiskLevelDestructive = "destructive_mutation"
)

// EffectiveRiskLevel returns a safe normalized classification. Existing
// mutating tools without metadata remain approval-gated as sensitive.
func (d Definition) EffectiveRiskLevel() string {
	if !d.Mutating {
		return RiskLevelRead
	}
	switch strings.TrimSpace(d.RiskLevel) {
	case RiskLevelRoutine, RiskLevelSensitive, RiskLevelDestructive:
		return strings.TrimSpace(d.RiskLevel)
	default:
		return RiskLevelSensitive
	}
}

type CallContext struct {
	AppID            string
	RunID            string
	Agent            *agentcore.Agent
	Run              *agentcore.AgentRun
	Target           agentcore.TargetRef
	StagedSkillRoot  string
	ArtifactWriter   ArtifactWriter
	WorkspaceManager WorkspaceManager
}

// ArtifactWriter persists run-scoped artifacts produced by tool handlers.
// It mirrors the runtime package's ArtifactWriter so callers can pass the
// same implementation through CallContext without an import cycle.
type ArtifactWriter interface {
	WriteArtifact(ctx context.Context, artifact agentcore.AgentRunArtifact) error
}

// WorkspaceManager performs runtime-owned workspace state changes requested by
// tools. Implementations must keep credentials out of model-visible outputs.
type WorkspaceManager interface {
	CheckoutRepository(ctx context.Context, req CheckoutRepositoryRequest) (*CheckoutRepositoryResult, error)
}

type CheckoutRepositoryRequest struct {
	RepositoryID string
	RepoFullName string
	BaseBranch   string
	WorkBranch   string
	Alias        string
	Primary      bool
}

type CheckoutRepositoryResult struct {
	Alias        string                    `json:"alias,omitempty"`
	Primary      bool                      `json:"primary"`
	Lease        *agentcore.WorkspaceLease `json:"lease,omitempty"`
	RepositoryID string                    `json:"repository_id,omitempty"`
	RepoFullName string                    `json:"repo_full_name,omitempty"`
	BaseBranch   string                    `json:"base_branch,omitempty"`
	WorkBranch   string                    `json:"work_branch,omitempty"`
}

type Handler func(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error)

type Registry struct {
	state *registryState
	appID string
}

type registryState struct {
	mu             sync.RWMutex
	snapshot       atomic.Pointer[registrySnapshot]
	defs           map[string]Definition
	handlers       map[string]Handler
	aliases        map[string]string
	appDefs        map[string]map[string]Definition
	appHandlers    map[string]map[string]Handler
	providers      map[string]map[string]providerLayer
	providerHealth map[string]ProviderHealth
	refreshers     map[string]*providerRefresher
	runClosers     []RunCloser
}

type registrySnapshot struct {
	global toolSnapshot
	apps   map[string]toolSnapshot
}

// toolSnapshot is immutable after publication. Readers load one pointer and
// perform direct lookups without rebuilding or locking the catalog.
type toolSnapshot struct {
	defs     map[string]Definition
	handlers map[string]Handler
	aliases  map[string]string
}

type providerLayer struct {
	order    int
	defs     map[string]Definition
	handlers map[string]Handler
	aliases  map[string]string
}

type providerRefresher struct {
	mu          sync.Mutex
	cooldown    time.Duration
	lastAttempt time.Time
	inFlight    chan struct{}
	lastError   error
	refresh     func(context.Context) error
}

// ProviderHealth describes the active catalog source and its refresh state.
type ProviderHealth struct {
	AppID       string    `json:"app_id"`
	Provider    string    `json:"provider"`
	Ready       bool      `json:"ready"`
	Degraded    bool      `json:"degraded"`
	Source      string    `json:"source"`
	ToolCount   int       `json:"tool_count"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// ProviderRegistration is one atomically replaceable provider tool.
type ProviderRegistration struct {
	Definition Definition
	Handler    Handler
	Aliases    []string
}

// RunCloser releases run-scoped resources owned by a tool family.
type RunCloser interface {
	CloseRun(ctx context.Context, appID, runID string) error
}

func (r *Registry) RegisterRunCloser(closer RunCloser) {
	if r == nil || r.state == nil || closer == nil {
		return
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	r.state.runClosers = append(r.state.runClosers, closer)
}

// CloseRun releases resources retained by runtime-owned tool families.
func (r *Registry) CloseRun(ctx context.Context, appID, runID string) error {
	if r == nil || r.state == nil {
		return nil
	}
	r.state.mu.RLock()
	closers := append([]RunCloser(nil), r.state.runClosers...)
	r.state.mu.RUnlock()
	var errs []error
	for _, closer := range closers {
		if err := closer.CloseRun(ctx, strings.TrimSpace(appID), strings.TrimSpace(runID)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

const (
	agentRuntimeMCPPrefix = "mcp__agent_runtime__"
	legacyMCPPrefix       = "mcp__" + "hel" + "pin" + "__"
)

func NewRegistry() *Registry {
	r := &Registry{
		state: &registryState{
			defs:           map[string]Definition{},
			handlers:       map[string]Handler{},
			aliases:        map[string]string{},
			appDefs:        map[string]map[string]Definition{},
			appHandlers:    map[string]map[string]Handler{},
			providers:      map[string]map[string]providerLayer{},
			providerHealth: map[string]ProviderHealth{},
			refreshers:     map[string]*providerRefresher{},
		},
	}
	r.Register(Definition{
		Name:        "get_context",
		Description: "Return the pre-resolved target context summary for the current run.",
		Category:    "Context",
		InputSchema: map[string]interface{}{
			"type":                 "object",
			"properties":           map[string]interface{}{},
			"additionalProperties": false,
		},
	}, func(_ context.Context, callCtx CallContext, _ json.RawMessage) (json.RawMessage, error) {
		if callCtx.Run == nil {
			return json.RawMessage(`{}`), nil
		}
		return json.Marshal(map[string]interface{}{
			"target":          callCtx.Run.Target,
			"context_summary": callCtx.Run.Input.ContextSummary,
		})
	})
	RegisterWorkspaceTools(r)
	RegisterRepositoryCheckoutTools(r)
	RegisterWorkspaceScanTools(r)
	RegisterArtifactPreviewTools(r)
	RegisterRepositoryProviderTools(r)
	RegisterSkillTools(r)
	RegisterWebToolsFromEnv(r)
	return r
}

// ForApp returns a registration view whose definitions and handlers are
// visible only to the specified host app. Runtime-owned tools remain global.
func (r *Registry) ForApp(appID string) *Registry {
	if r == nil {
		return nil
	}
	return &Registry{state: r.state, appID: strings.TrimSpace(appID)}
}

// CloneForApp returns an isolated snapshot containing the runtime-owned tools
// and the selected app's tools. Run-scoped tools can be registered on the
// clone without mutating the process-wide registry or leaking into other runs.
func (r *Registry) CloneForApp(appID string) *Registry {
	clone := &Registry{state: &registryState{
		defs: map[string]Definition{}, handlers: map[string]Handler{}, aliases: map[string]string{},
		appDefs: map[string]map[string]Definition{}, appHandlers: map[string]map[string]Handler{},
		providers:      map[string]map[string]providerLayer{},
		providerHealth: map[string]ProviderHealth{},
		refreshers:     map[string]*providerRefresher{},
	}}
	if r == nil || r.state == nil {
		return clone
	}
	appID = strings.TrimSpace(appID)
	snapshot := r.state.snapshot.Load()
	if snapshot == nil {
		return clone
	}
	r.state.mu.RLock()
	clone.state.runClosers = append([]RunCloser(nil), r.state.runClosers...)
	r.state.mu.RUnlock()
	view := snapshot.forApp(appID)
	for name, def := range view.defs {
		clone.state.defs[name] = def
		if handler := view.handlers[name]; handler != nil {
			clone.state.handlers[name] = handler
		}
	}
	for alias, canonical := range view.aliases {
		clone.state.aliases[alias] = canonical
	}
	clone.publishSnapshotLocked()
	return clone
}

// SetProviderHealth atomically records provider readiness and degradation.
func (r *Registry) SetProviderHealth(health ProviderHealth) {
	if r == nil || r.state == nil {
		return
	}
	health.AppID = strings.TrimSpace(health.AppID)
	health.Provider = strings.TrimSpace(health.Provider)
	if health.AppID == "" || health.Provider == "" {
		return
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	r.state.providerHealth[health.AppID+"\x00"+health.Provider] = health
}

// ProviderHealth returns a stable snapshot of all configured provider states.
func (r *Registry) ProviderHealth() []ProviderHealth {
	if r == nil || r.state == nil {
		return nil
	}
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	out := make([]ProviderHealth, 0, len(r.state.providerHealth))
	for _, health := range r.state.providerHealth {
		out = append(out, health)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AppID == out[j].AppID {
			return out[i].Provider < out[j].Provider
		}
		return out[i].AppID < out[j].AppID
	})
	return out
}

// Ready reports whether every configured provider has a usable catalog or fallback.
func (r *Registry) Ready() bool {
	for _, health := range r.ProviderHealth() {
		if !health.Ready {
			return false
		}
	}
	return true
}

// RegisterProviderRefresher installs the provider's unknown-alias refresh hook.
func (r *Registry) RegisterProviderRefresher(appID, provider string, cooldown time.Duration, refresh func(context.Context) error) {
	if r == nil || r.state == nil || refresh == nil {
		return
	}
	appID = strings.TrimSpace(appID)
	provider = strings.TrimSpace(provider)
	if appID == "" || provider == "" {
		return
	}
	key := appID + "\x00" + provider
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	r.state.refreshers[key] = &providerRefresher{cooldown: cooldown, refresh: refresh}
}

// RefreshProvidersForApp refreshes each provider at most once per cooldown window.
func (r *Registry) RefreshProvidersForApp(ctx context.Context, appID string) {
	if r == nil || r.state == nil {
		return
	}
	prefix := strings.TrimSpace(appID) + "\x00"
	r.state.mu.RLock()
	refreshers := make([]*providerRefresher, 0)
	for key, refresher := range r.state.refreshers {
		if strings.HasPrefix(key, prefix) {
			refreshers = append(refreshers, refresher)
		}
	}
	r.state.mu.RUnlock()
	for _, refresher := range refreshers {
		_ = refresher.run(ctx, true)
	}
}

// RefreshProvider refreshes one provider through the same single-flight path
// used by unknown-alias recovery. Scheduled refreshes bypass only the cooldown;
// they still join an in-flight request.
func (r *Registry) RefreshProvider(ctx context.Context, appID, provider string) error {
	if r == nil || r.state == nil {
		return fmt.Errorf("tool registry is not configured")
	}
	key := strings.TrimSpace(appID) + "\x00" + strings.TrimSpace(provider)
	r.state.mu.RLock()
	refresher := r.state.refreshers[key]
	r.state.mu.RUnlock()
	if refresher == nil {
		return fmt.Errorf("provider %q is not configured for app %q", provider, appID)
	}
	return refresher.run(ctx, false)
}

func (r *providerRefresher) run(ctx context.Context, enforceCooldown bool) error {
	r.mu.Lock()
	if r.inFlight != nil {
		waiting := r.inFlight
		r.mu.Unlock()
		select {
		case <-waiting:
			r.mu.Lock()
			err := r.lastError
			r.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if enforceCooldown && r.cooldown > 0 && time.Since(r.lastAttempt) < r.cooldown {
		r.mu.Unlock()
		return nil
	}
	r.lastAttempt = time.Now()
	r.inFlight = make(chan struct{})
	done := r.inFlight
	r.mu.Unlock()
	err := r.refresh(ctx)
	r.mu.Lock()
	r.lastError = err
	close(done)
	r.inFlight = nil
	r.mu.Unlock()
	return err
}

func (r *Registry) Register(def Definition, handler Handler) {
	if r == nil || r.state == nil {
		return
	}
	def.Name = CanonicalName(def.Name)
	if def.Name == "" {
		return
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	if r.appID == "" {
		r.state.defs[def.Name] = def
		if handler != nil {
			r.state.handlers[def.Name] = handler
		}
		r.publishSnapshotLocked()
		return
	}
	if r.state.appDefs[r.appID] == nil {
		r.state.appDefs[r.appID] = map[string]Definition{}
		r.state.appHandlers[r.appID] = map[string]Handler{}
	}
	r.state.appDefs[r.appID][def.Name] = def
	if handler != nil {
		r.state.appHandlers[r.appID][def.Name] = handler
	}
	r.publishSnapshotLocked()
}

// ReplaceAppProvider atomically replaces one app provider's complete snapshot.
func (r *Registry) ReplaceAppProvider(appID, provider string, order int, registrations []ProviderRegistration, rejectGlobalCollision bool) error {
	if r == nil || r.state == nil {
		return fmt.Errorf("tool registry is not configured")
	}
	appID = strings.TrimSpace(appID)
	provider = strings.TrimSpace(provider)
	if appID == "" || provider == "" {
		return fmt.Errorf("app_id and provider are required")
	}
	layer := providerLayer{order: order, defs: map[string]Definition{}, handlers: map[string]Handler{}, aliases: map[string]string{}}
	for _, registration := range registrations {
		def := registration.Definition
		def.Name = CanonicalName(def.Name)
		if def.Name == "" {
			return fmt.Errorf("provider %q contains an empty tool name", provider)
		}
		if _, exists := layer.defs[def.Name]; exists {
			return fmt.Errorf("provider %q contains duplicate tool %q", provider, def.Name)
		}
		if registration.Handler == nil {
			return fmt.Errorf("provider %q tool %q has no handler", provider, def.Name)
		}
		layer.defs[def.Name] = def
		layer.handlers[def.Name] = registration.Handler
		for _, alias := range registration.Aliases {
			alias = CanonicalName(alias)
			if alias == "" || alias == def.Name {
				continue
			}
			if existing := layer.aliases[alias]; existing != "" && existing != def.Name {
				return fmt.Errorf("provider %q alias %q maps to multiple tools", provider, alias)
			}
			layer.aliases[alias] = def.Name
		}
	}
	if len(layer.defs) == 0 {
		return fmt.Errorf("provider %q returned an empty catalog", provider)
	}
	for alias, canonical := range layer.aliases {
		if _, exists := layer.defs[alias]; exists && alias != canonical {
			return fmt.Errorf("provider %q alias %q collides with a canonical tool", provider, alias)
		}
	}
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	if rejectGlobalCollision {
		for name := range layer.defs {
			if _, exists := r.state.defs[name]; exists {
				return fmt.Errorf("provider %q tool %q collides with a runtime-global tool", provider, name)
			}
		}
		for alias := range layer.aliases {
			if _, exists := r.state.defs[alias]; exists {
				return fmt.Errorf("provider %q alias %q collides with a runtime-global tool", provider, alias)
			}
		}
	}
	if r.state.providers[appID] == nil {
		r.state.providers[appID] = map[string]providerLayer{}
	}
	r.state.providers[appID][provider] = layer
	r.publishSnapshotLocked()
	return nil
}

func (r *Registry) Definitions() []Definition {
	if r == nil || r.state == nil {
		return nil
	}
	return r.DefinitionsForApp(r.appID)
}

func (r *Registry) DefinitionsForApp(appID string) []Definition {
	if r == nil || r.state == nil {
		return nil
	}
	appID = strings.TrimSpace(appID)
	snapshot := r.state.snapshot.Load()
	if snapshot == nil {
		return nil
	}
	view := snapshot.forApp(appID)
	out := make([]Definition, 0, len(view.defs))
	for _, def := range view.defs {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *registrySnapshot) forApp(appID string) toolSnapshot {
	if view, ok := s.apps[strings.TrimSpace(appID)]; ok {
		return view
	}
	return s.global
}

func (r *Registry) Definition(name string) (Definition, bool) {
	return r.DefinitionForApp(r.appID, name)
}

func (r *Registry) DefinitionForApp(appID, name string) (Definition, bool) {
	if r == nil || r.state == nil {
		return Definition{}, false
	}
	appID = strings.TrimSpace(appID)
	snapshot := r.state.snapshot.Load()
	if snapshot == nil {
		return Definition{}, false
	}
	view := snapshot.forApp(appID)
	name = resolveAlias(view.aliases, name)
	def, ok := view.defs[name]
	return def, ok
}

func (r *Registry) Execute(ctx context.Context, callCtx CallContext, name string, input json.RawMessage) (json.RawMessage, error) {
	if r == nil || r.state == nil {
		return nil, fmt.Errorf("tool registry is not configured")
	}
	appID := strings.TrimSpace(callCtx.AppID)
	if appID == "" {
		appID = r.appID
	}
	snapshot := r.state.snapshot.Load()
	if snapshot == nil {
		return nil, fmt.Errorf("tool registry is not configured")
	}
	view := snapshot.forApp(appID)
	name = resolveAlias(view.aliases, name)
	handler := view.handlers[name]
	if handler == nil {
		return nil, fmt.Errorf("tool %q is not registered", name)
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return handler(ctx, callCtx, input)
}

// ResolveNameForApp returns the canonical provider-aware name for an app tool.
func (r *Registry) ResolveNameForApp(appID, name string) string {
	if r == nil || r.state == nil {
		return CanonicalName(name)
	}
	snapshot := r.state.snapshot.Load()
	if snapshot == nil {
		return CanonicalName(name)
	}
	return resolveAlias(snapshot.forApp(appID).aliases, name)
}

func (r *Registry) publishSnapshotLocked() {
	global := toolSnapshot{
		defs: cloneDefinitions(r.state.defs), handlers: cloneHandlers(r.state.handlers), aliases: cloneAliases(r.state.aliases),
	}
	snapshot := &registrySnapshot{global: global, apps: map[string]toolSnapshot{}}
	appIDs := map[string]struct{}{}
	for appID := range r.state.appDefs {
		appIDs[appID] = struct{}{}
	}
	for appID := range r.state.providers {
		appIDs[appID] = struct{}{}
	}
	for appID := range appIDs {
		view := toolSnapshot{
			defs: cloneDefinitions(global.defs), handlers: cloneHandlers(global.handlers), aliases: cloneAliases(global.aliases),
		}
		for name, def := range r.state.appDefs[appID] {
			view.defs[name] = def
			view.handlers[name] = r.state.appHandlers[appID][name]
		}
		layers := make([]providerLayer, 0, len(r.state.providers[appID]))
		for _, layer := range r.state.providers[appID] {
			layers = append(layers, layer)
		}
		sort.SliceStable(layers, func(i, j int) bool { return layers[i].order < layers[j].order })
		for _, layer := range layers {
			for name, def := range layer.defs {
				view.defs[name] = def
				view.handlers[name] = layer.handlers[name]
			}
			for alias, canonical := range layer.aliases {
				view.aliases[alias] = canonical
			}
		}
		snapshot.apps[appID] = view
	}
	r.state.snapshot.Store(snapshot)
}

func cloneDefinitions(source map[string]Definition) map[string]Definition {
	out := make(map[string]Definition, len(source))
	for name, definition := range source {
		out[name] = definition
	}
	return out
}

func cloneHandlers(source map[string]Handler) map[string]Handler {
	out := make(map[string]Handler, len(source))
	for name, handler := range source {
		out[name] = handler
	}
	return out
}

func cloneAliases(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for alias, canonical := range source {
		out[alias] = canonical
	}
	return out
}

func resolveAlias(aliases map[string]string, name string) string {
	name = CanonicalName(name)
	seen := map[string]bool{}
	for aliases[name] != "" && !seen[name] {
		seen[name] = true
		name = aliases[name]
	}
	return name
}

func CanonicalName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, agentRuntimeMCPPrefix)
	name = strings.TrimPrefix(name, legacyMCPPrefix)
	switch name {
	case "request_human_input":
		return "request_user_input"
	case "request_human_approval":
		return "request_approval"
	}
	return name
}

// ToolResultText returns the model-visible text represented by a tool result.
// Runtime-local text tools encode their output as a top-level JSON string;
// structured object and array results remain compact JSON.
func ToolResultText(output json.RawMessage) string {
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err == nil {
			return text
		}
	}
	return string(output)
}

func AllowedSet(agent *agentcore.Agent, requested []string) map[string]bool {
	return allowedSetWithResolver(agent, requested, CanonicalName)
}

// AllowedSetForApp resolves provider aliases and returns the effective app allowlist.
func (r *Registry) AllowedSetForApp(appID string, agent *agentcore.Agent, requested []string) map[string]bool {
	return allowedSetWithResolver(agent, requested, func(name string) string {
		return r.ResolveNameForApp(appID, name)
	})
}

func allowedSetWithResolver(agent *agentcore.Agent, requested []string, resolve func(string) string) map[string]bool {
	set := map[string]bool{}
	if agent != nil {
		for _, tool := range agent.AllowedTools {
			tool = resolve(tool)
			if tool != "" {
				set[tool] = true
			}
		}
	}
	if len(requested) == 0 {
		return set
	}
	subset := map[string]bool{}
	for _, tool := range requested {
		tool = resolve(tool)
		if tool != "" && set[tool] {
			subset[tool] = true
		}
	}
	return subset
}

func ValidateAllowedSubset(agent *agentcore.Agent, requested []string) error {
	return validateAllowedSubsetWithResolver(agent, requested, CanonicalName)
}

// ValidateAllowedSubsetForApp validates a requested subset using provider aliases.
func (r *Registry) ValidateAllowedSubsetForApp(appID string, agent *agentcore.Agent, requested []string) error {
	return validateAllowedSubsetWithResolver(agent, requested, func(name string) string {
		return r.ResolveNameForApp(appID, name)
	})
}

func validateAllowedSubsetWithResolver(agent *agentcore.Agent, requested []string, resolve func(string) string) error {
	if len(requested) == 0 {
		return nil
	}
	allowed := allowedSetWithResolver(agent, nil, resolve)
	for _, tool := range requested {
		tool = resolve(tool)
		if !allowed[tool] {
			return fmt.Errorf("tool %q is not allowed for agent", tool)
		}
	}
	return nil
}
