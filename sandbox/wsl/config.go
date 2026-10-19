package wsl

import "strings"

type Config struct{ Distribution, Helper, ProbeRoot string }

func InteropDisabled(raw []byte) bool {
	section := ""
	disabled, noPath := false, false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line[1 : len(line)-1])
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section != "interop" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "enabled":
			disabled = strings.EqualFold(strings.TrimSpace(value), "false")
		case "appendwindowspath":
			noPath = strings.EqualFold(strings.TrimSpace(value), "false")
		}
	}
	return disabled && noPath
}
