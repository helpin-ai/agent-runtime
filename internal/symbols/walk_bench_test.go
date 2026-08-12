package symbols

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Measurement harness for the Phase 2 index question: is a whole-repo walk
// parse-bound or IO-bound, and how does it scale with workers?

func collectSourceFiles(root string) []string {
	var out []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			base := fi.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" || base == "dist" || base == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		if !Supported(p) || fi.Size() > MaxParseBytes {
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out
}

func walkRepo(t *testing.T, root string, workers int) (dur time.Duration, files int, syms int, ioTime, parseTime time.Duration) {
	paths := collectSourceFiles(root)
	if len(paths) == 0 {
		t.Skipf("no source files under %s", root)
	}

	var symCount, ioNanos, parseNanos int64
	jobs := make(chan string, len(paths))
	for _, p := range paths {
		jobs <- p
	}
	close(jobs)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				ioStart := time.Now()
				content, err := os.ReadFile(path)
				atomic.AddInt64(&ioNanos, int64(time.Since(ioStart)))
				if err != nil {
					continue
				}
				parseStart := time.Now()
				found, err := Extract(path, content)
				atomic.AddInt64(&parseNanos, int64(time.Since(parseStart)))
				if err == nil {
					atomic.AddInt64(&symCount, int64(len(found)))
				}
			}
		}()
	}
	wg.Wait()
	return time.Since(start), len(paths), int(symCount),
		time.Duration(ioNanos), time.Duration(parseNanos)
}

func TestWalkScaling(t *testing.T) {
	if os.Getenv("SYMBOL_WALK_BENCH") == "" {
		t.Skip("set SYMBOL_WALK_BENCH=1 to run the walk measurement")
	}
	roots := strings.Split(os.Getenv("SYMBOL_WALK_ROOTS"), ",")
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		t.Run(filepath.Base(root), func(t *testing.T) {
			for _, workers := range []int{1, 2, 4, 8, 16} {
				dur, files, syms, ioTime, parseTime := walkRepo(t, root, workers)
				var peak runtime.MemStats
				runtime.ReadMemStats(&peak)
				t.Logf("workers=%-3d wall=%-10v files=%-5d symbols=%-6d cpu_io=%-10v cpu_parse=%-10v heap=%dMB",
					workers, dur.Round(time.Millisecond), files, syms,
					ioTime.Round(time.Millisecond), parseTime.Round(time.Millisecond),
					peak.HeapAlloc/1048576)
			}
		})
	}
}
