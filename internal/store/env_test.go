package store

import (
	"strings"
	"testing"
)

func TestResolveConfigFromEnvUsesDatabaseURLFirst(t *testing.T) {
	cfg, err := ResolveConfigFromEnv(mapEnv(map[string]string{
		"AGENT_RUNTIME_STORE_DRIVER": "postgres",
		"DATABASE_URL":               "postgres://explicit/value",
		"PGHOST":                     "ignored",
		"PGDATABASE":                 "ignored",
		"PGUSER":                     "ignored",
		"PGPASSWORD":                 "ignored",
	}))
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	if cfg.Driver != "postgres" || cfg.DSN != "postgres://explicit/value" || cfg.InMemory {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestResolveConfigFromEnvBuildsPostgresDSNFromPGEnv(t *testing.T) {
	cfg, err := ResolveConfigFromEnv(mapEnv(map[string]string{
		"AGENT_RUNTIME_STORE_DRIVER": "postgres",
		"PGHOST":                     "agent-runtime-postgres-rw",
		"PGDATABASE":                 "agent_runtime",
		"PGUSER":                     "agent_runtime",
		"PGPASSWORD":                 "secret value",
	}))
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	want := "postgres://agent_runtime:secret%20value@agent-runtime-postgres-rw:5432/agent_runtime?sslmode=disable"
	if cfg.Driver != "postgres" || cfg.DSN != want || cfg.InMemory {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestResolveConfigFromEnvUsesPGPortAndSSLMode(t *testing.T) {
	cfg, err := ResolveConfigFromEnv(mapEnv(map[string]string{
		"AGENT_RUNTIME_STORE_DRIVER": "postgresql",
		"PGHOST":                     "db.internal",
		"PGPORT":                     "6432",
		"PGDATABASE":                 "agent_runtime",
		"PGUSER":                     "agent_runtime",
		"PGPASSWORD":                 "secret",
		"PGSSLMODE":                  "require",
	}))
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	want := "postgres://agent_runtime:secret@db.internal:6432/agent_runtime?sslmode=require"
	if cfg.Driver != "postgresql" || cfg.DSN != want {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestResolveConfigFromEnvFailsWhenPostgresEnvIsMissing(t *testing.T) {
	_, err := ResolveConfigFromEnv(mapEnv(map[string]string{
		"AGENT_RUNTIME_STORE_DRIVER": "postgres",
		"PGHOST":                     "db.internal",
		"PGUSER":                     "agent_runtime",
		"PGPASSWORD":                 "secret",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") || !strings.Contains(err.Error(), "PGDATABASE") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveConfigFromEnvDefaultsToMemory(t *testing.T) {
	cfg, err := ResolveConfigFromEnv(mapEnv(nil))
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	if cfg.Driver != "memory" || !cfg.InMemory {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func mapEnv(values map[string]string) func(string) string {
	return func(key string) string {
		if values == nil {
			return ""
		}
		return values[key]
	}
}
