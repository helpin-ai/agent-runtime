//go:build !windows

package store

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func sqliteDialector(dsn string) gorm.Dialector { return sqlite.Open(dsn) }
