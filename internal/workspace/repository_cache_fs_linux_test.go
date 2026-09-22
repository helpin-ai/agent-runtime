package workspace

import (
	"os"
	"strings"
	"testing"
)

func TestLocalCacheFilesystem(t *testing.T) {
	if err := validateLocalCacheFilesystem(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// Staging provides the actual JuiceFS mount, rather than mocking statfs.
	if shared := os.Getenv("AGENT_RUNTIME_TEST_SHARED_FILESYSTEM"); shared != "" {
		if err := validateLocalCacheFilesystem(shared); err == nil || !strings.Contains(err.Error(), "requires local storage") {
			t.Fatalf("accepted shared filesystem: %v", err)
		}
	}
}
