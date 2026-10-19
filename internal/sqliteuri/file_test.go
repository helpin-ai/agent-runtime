package sqliteuri

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/glebarez/go-sqlite"
)

func TestNativeDatabasePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory spaces # percent %.db")
	uri := File(path)
	db, err := sql.Open("sqlite", uri.String()+"?_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE evidence (value TEXT)"); err != nil {
		t.Fatal(err)
	}
	plain, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.Exec("INSERT INTO evidence VALUES ('same file')"); err != nil {
		t.Fatal(err)
	}
}
