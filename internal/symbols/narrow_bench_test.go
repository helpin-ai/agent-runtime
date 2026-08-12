package symbols

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Measures the ripgrep-narrowed alternative to a whole-repo index: let rg find
// candidate files containing the identifier, then parse only those.
func TestNarrowedLookup(t *testing.T) {
	if os.Getenv("SYMBOL_WALK_BENCH") == "" {
		t.Skip("set SYMBOL_WALK_BENCH=1 to run the narrowed-lookup measurement")
	}
	root := os.Getenv("SYMBOL_NARROW_ROOT")
	if root == "" {
		t.Skip("set SYMBOL_NARROW_ROOT")
	}
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep not in PATH")
	}

	for _, symbol := range strings.Split(os.Getenv("SYMBOL_NARROW_NAMES"), ",") {
		symbol = strings.TrimSpace(symbol)
		if symbol == "" {
			continue
		}
		// Stage 1: a plain literal search, which ripgrep can optimise hard.
		start := time.Now()
		cmd := exec.Command(rg, "-l", "-w", "-F",
			"--type-add", "src:*.{go,ts,tsx,js,jsx,mjs,cjs,py,pyi,rs,java}", "-t", "src",
			"--", symbol, root)
		out, _ := cmd.Output()
		rgTime := time.Since(start)

		var mentioned []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" && Supported(line) {
				mentioned = append(mentioned, line)
			}
		}

		// Stage 2: apply the declaration pattern in process. IO is ~1% of the
		// cost of parsing, so reading a file to reject it is far cheaper than
		// parsing it, and far cheaper than making ripgrep run a complex regex
		// over the whole repository.
		filterStart := time.Now()
		var candidates []string
		if len(mentioned) > 0 {
			args := append([]string{"-l", "--", DeclarationPattern(symbol)}, mentioned...)
			declOut, _ := exec.Command(rg, args...).Output()
			for _, line := range strings.Split(strings.TrimSpace(string(declOut)), "\n") {
				if line != "" {
					candidates = append(candidates, line)
				}
			}
		}
		filterTime := time.Since(filterStart)

		parseStart := time.Now()
		hits, parsed := 0, 0
		for _, path := range candidates {
			content, err := os.ReadFile(path)
			if err != nil || len(content) > MaxParseBytes {
				continue
			}
			found, err := Extract(path, content)
			if err != nil {
				continue
			}
			parsed++
			hits += len(Find(found, symbol))
		}
		parseTime := time.Since(parseStart)

		t.Logf("%-12s mentioned=%-4d candidates=%-3d parsed=%-3d decls=%-3d rg=%-7v filter=%-7v parse=%-8v TOTAL=%v",
			symbol, len(mentioned), len(candidates), parsed, hits,
			rgTime.Round(time.Millisecond), filterTime.Round(time.Millisecond),
			parseTime.Round(time.Millisecond),
			(rgTime + filterTime + parseTime).Round(time.Millisecond))
	}
}
