package store

import (
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Windows sidecars build without a C toolchain, like the memory SQLite backend.
// That driver has no default busy timeout, unlike the cgo driver's 5 seconds.
func sqliteDialector(dsn string) gorm.Dialector {
	if !strings.Contains(dsn, "busy_timeout") {
		dsn += "&_pragma=busy_timeout(5000)"
	}
	return sqlite.Open(dsn)
}
