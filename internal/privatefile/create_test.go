package privatefile

import (
	"path/filepath"
	"testing"
)

func TestCreateDoesNotReplaceExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	file, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if second, err := Create(path); err == nil {
		second.Close()
		t.Fatal("replaced existing private file")
	}
}
