package sandbox

import (
	"strings"
	"testing"
)

func TestParseLandlockExecArgs(t *testing.T) {
	parsed, err := parseLandlockExecArgs([]string{"--root", "/run", "--cwd", "/run/src", "--read-exec", "/toolchain", "--read-write", "/state", "--", "go", "build", "-o", "x"})
	if err != nil || parsed.root != "/run" || parsed.cwd != "/run/src" || strings.Join(parsed.readExec, " ") != "/toolchain" || strings.Join(parsed.readWrite, " ") != "/state" || strings.Join(parsed.argv, " ") != "go build -o x" {
		t.Fatalf("unexpected parse: %+v %v", parsed, err)
	}
	for _, args := range [][]string{{}, {"--root", "/run"}, {"--root", "/run", "--cwd", "/run", "--"}, {"--bogus"}} {
		if _, err := parseLandlockExecArgs(args); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
}
