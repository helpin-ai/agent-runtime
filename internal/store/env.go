package store

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

type ResolvedConfig struct {
	Driver   string
	DSN      string
	InMemory bool
}

func ResolveConfigFromEnv(getenv func(string) string) (ResolvedConfig, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	driver := strings.TrimSpace(getenv("AGENT_RUNTIME_STORE_DRIVER"))
	dsn := strings.TrimSpace(getenv("DATABASE_URL"))
	if driver == "" && dsn != "" {
		driver = "postgres"
	}
	if driver == "" {
		driver = strings.TrimSpace(getenv("AGENT_RUNTIME_STORE"))
	}
	if driver == "" || driver == "memory" {
		return ResolvedConfig{Driver: "memory", InMemory: true}, nil
	}
	if driver == "sqlite" || driver == "sqlite3" {
		if dsn == "" {
			dsn = strings.TrimSpace(getenv("AGENT_RUNTIME_SQLITE_DSN"))
		}
		return ResolvedConfig{Driver: driver, DSN: dsn}, nil
	}
	if driver == "postgres" || driver == "postgresql" {
		if dsn == "" {
			var err error
			dsn, err = postgresDSNFromEnv(getenv)
			if err != nil {
				return ResolvedConfig{}, err
			}
		}
		return ResolvedConfig{Driver: driver, DSN: dsn}, nil
	}
	if dsn == "" {
		return ResolvedConfig{}, fmt.Errorf("DATABASE_URL is required for %s store", driver)
	}
	return ResolvedConfig{Driver: driver, DSN: dsn}, nil
}

func postgresDSNFromEnv(getenv func(string) string) (string, error) {
	host := strings.TrimSpace(getenv("PGHOST"))
	database := strings.TrimSpace(getenv("PGDATABASE"))
	user := strings.TrimSpace(getenv("PGUSER"))
	password := strings.TrimSpace(getenv("PGPASSWORD"))
	missing := make([]string, 0, 4)
	for key, value := range map[string]string{
		"PGHOST":     host,
		"PGDATABASE": database,
		"PGUSER":     user,
		"PGPASSWORD": password,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("postgres store requires DATABASE_URL or %s", strings.Join(missing, ", "))
	}

	port := strings.TrimSpace(getenv("PGPORT"))
	if port == "" {
		port = "5432"
	}
	sslMode := strings.TrimSpace(getenv("PGSSLMODE"))
	if sslMode == "" {
		sslMode = "disable"
	}

	dsn := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + database,
	}
	query := dsn.Query()
	query.Set("sslmode", sslMode)
	dsn.RawQuery = query.Encode()
	return dsn.String(), nil
}
