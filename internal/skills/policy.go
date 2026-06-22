package skills

import (
	"sort"
	"strings"
)

func CompileInstructions(definitions []Definition) string {
	sections := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		instructions := strings.TrimSpace(definition.Instructions)
		if instructions == "" {
			continue
		}
		sections = append(sections, instructions)
	}
	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func AggregatePolicy(definitions []Definition) Policy {
	if len(definitions) == 0 {
		return Policy{}
	}

	var policy Policy
	var allowImplicit *bool
	requiredInteractionKinds := make([]string, 0, len(definitions))
	interactionContracts := make([]InteractionContract, 0, len(definitions))
	for _, definition := range definitions {
		if definition.Policy.AllowImplicitInvocation != nil {
			value := *definition.Policy.AllowImplicitInvocation
			switch {
			case allowImplicit == nil:
				allowImplicit = &value
			case !value:
				allowImplicit = &value
			}
		}
		requiredInteractionKinds = append(requiredInteractionKinds, definition.Policy.CompletionRequiresInteractionKinds...)
		interactionContracts = append(interactionContracts, definition.Policy.InteractionContracts...)
	}

	policy.AllowImplicitInvocation = allowImplicit
	policy.CompletionRequiresInteractionKinds = SortedUniqueStrings(requiredInteractionKinds)
	policy.InteractionContracts = NormalizeInteractionContracts(interactionContracts)
	return policy
}

func NormalizeInteractionContracts(contracts []InteractionContract) []InteractionContract {
	if len(contracts) == 0 {
		return nil
	}
	merged := make(map[string]InteractionContract, len(contracts))
	order := make([]string, 0, len(contracts))
	for _, contract := range contracts {
		kind := strings.TrimSpace(contract.Kind)
		if kind == "" {
			continue
		}
		contract.Kind = kind
		if _, ok := merged[kind]; !ok {
			order = append(order, kind)
			merged[kind] = normalizeInteractionContract(contract)
			continue
		}
		merged[kind] = mergeInteractionContract(merged[kind], contract)
	}
	out := make([]InteractionContract, 0, len(order))
	for _, kind := range order {
		out = append(out, merged[kind])
	}
	return out
}

func normalizeInteractionContract(contract InteractionContract) InteractionContract {
	contract.Kind = strings.TrimSpace(contract.Kind)
	contract.Schema = strings.TrimSpace(contract.Schema)
	if len(contract.Transports) == 0 {
		contract.Transports = nil
		return contract
	}
	normalized := make(map[string]InteractionTransport, len(contract.Transports))
	for runtimeKind, transport := range contract.Transports {
		runtimeKind = strings.TrimSpace(runtimeKind)
		if runtimeKind == "" {
			continue
		}
		transport.Type = strings.TrimSpace(transport.Type)
		transport.ToolName = strings.TrimSpace(transport.ToolName)
		transport.BlockLabel = strings.TrimSpace(transport.BlockLabel)
		normalized[runtimeKind] = transport
	}
	if len(normalized) == 0 {
		contract.Transports = nil
		return contract
	}
	contract.Transports = normalized
	return contract
}

func mergeInteractionContract(base, incoming InteractionContract) InteractionContract {
	base = normalizeInteractionContract(base)
	incoming = normalizeInteractionContract(incoming)
	if base.Kind == "" {
		base.Kind = incoming.Kind
	}
	if base.Schema == "" {
		base.Schema = incoming.Schema
	}
	if len(base.Transports) == 0 && len(incoming.Transports) > 0 {
		base.Transports = make(map[string]InteractionTransport, len(incoming.Transports))
	}
	for runtimeKind, incomingTransport := range incoming.Transports {
		existing := base.Transports[runtimeKind]
		if existing.Type == "" {
			existing.Type = incomingTransport.Type
		}
		if existing.ToolName == "" {
			existing.ToolName = incomingTransport.ToolName
		}
		if existing.BlockLabel == "" {
			existing.BlockLabel = incomingTransport.BlockLabel
		}
		base.Transports[runtimeKind] = existing
	}
	return base
}

func (p Policy) InteractionContract(kind string) (InteractionContract, bool) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return InteractionContract{}, false
	}
	for _, contract := range NormalizeInteractionContracts(p.InteractionContracts) {
		if strings.TrimSpace(contract.Kind) == kind {
			return contract, true
		}
	}
	return InteractionContract{}, false
}

func RequestUserInputUsesRuntimeBridge(policy Policy, runtimeKind string) bool {
	contract, ok := policy.InteractionContract(InteractionKindRequestUserInput)
	if !ok {
		switch strings.TrimSpace(runtimeKind) {
		case "codex", "opencode":
			return true
		default:
			return false
		}
	}
	transport, ok := contract.Transports[strings.TrimSpace(runtimeKind)]
	if !ok || strings.TrimSpace(contract.Kind) == "" {
		return false
	}
	return strings.TrimSpace(transport.Type) == TransportTypeRuntimeBridge
}

func ReviewCheckpointFencedBlockLabel(policy Policy, runtimeKind string) string {
	contract, ok := policy.InteractionContract(InteractionKindReviewCheckpoint)
	if !ok {
		return "helpin-review"
	}
	transport, ok := contract.Transports[strings.TrimSpace(runtimeKind)]
	if !ok || strings.TrimSpace(transport.Type) != TransportTypeFencedJSON {
		return "helpin-review"
	}
	label := strings.TrimSpace(transport.BlockLabel)
	if label == "" {
		return "helpin-review"
	}
	return label
}

func SortedUniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}
