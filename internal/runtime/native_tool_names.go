package runtime

import (
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type nativeToolNameMapper struct {
	runtimeToModel map[string]string
	modelToRuntime map[string]string
}

func newNativeToolNameMapper(defs []tools.Definition) nativeToolNameMapper {
	mapper := nativeToolNameMapper{
		runtimeToModel: map[string]string{},
		modelToRuntime: map[string]string{},
	}
	used := map[string]bool{}
	for _, def := range defs {
		runtimeName := tools.CanonicalName(def.Name)
		if runtimeName == "" {
			continue
		}
		modelName := sanitizeNativeModelToolName(runtimeName)
		base := modelName
		for suffix := 2; used[modelName]; suffix++ {
			modelName = base + "_" + strconvItoa(suffix)
		}
		used[modelName] = true
		mapper.runtimeToModel[runtimeName] = modelName
		mapper.modelToRuntime[modelName] = runtimeName
	}
	return mapper
}

func (m nativeToolNameMapper) ModelName(runtimeName string) string {
	runtimeName = tools.CanonicalName(runtimeName)
	if runtimeName == "" {
		return ""
	}
	if modelName := m.runtimeToModel[runtimeName]; modelName != "" {
		return modelName
	}
	return sanitizeNativeModelToolName(runtimeName)
}

func (m nativeToolNameMapper) RuntimeName(modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return ""
	}
	if runtimeName := m.modelToRuntime[modelName]; runtimeName != "" {
		return runtimeName
	}
	return tools.CanonicalName(modelName)
}

func sanitizeNativeModelToolName(name string) string {
	name = strings.TrimSpace(name)
	var out strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	result := strings.Trim(out.String(), "_")
	if result == "" {
		return "tool"
	}
	return result
}

func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}
