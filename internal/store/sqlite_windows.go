package store

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Windows sidecars build without a C toolchain, like the memory SQLite backend.
func sqliteDialector(dsn string) gorm.Dialector { return sqlite.Open(dsn) }
