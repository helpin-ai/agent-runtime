// memory-eval evaluates both backends on the same corpus using isolated banks.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"
	"github.com/helpin-ai/agent-runtime/internal/memoryconfig"
	"github.com/helpin-ai/agent-runtime/internal/memoryeval"
	"github.com/helpin-ai/agent-runtime/memory"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	fixturePath := flag.String("fixture", "memory/testdata/retain-recall.json", "shared JSON corpus and evidence expectations")
	mode := flag.String("mode", "local", "local, hindsight, or both")
	recorded := flag.Bool("recorded", false, "use recorded local extraction/embeddings; unavailable with a live reference")
	bankID := flag.String("bank-id", "", "reuse an isolated evaluation bank for resuming retained documents")
	sqlitePath := flag.String("sqlite-path", "", "preserve the local evaluation database at this path for diagnostics")
	workers := flag.Int("retain-workers", 1, "parallel source documents per backend (1–16)")
	flag.Parse()
	if *workers < 1 || *workers > 16 {
		return fmt.Errorf("retain workers must be 1–16")
	}
	if *mode != "local" && *mode != "hindsight" && *mode != "both" {
		return fmt.Errorf("invalid mode")
	}
	if *recorded && *mode != "local" {
		return fmt.Errorf("recorded models are for local deterministic verification; compare both backends using the same live models")
	}
	file, err := os.Open(*fixturePath)
	if err != nil {
		return err
	}
	defer file.Close()
	var fixture memoryeval.Fixture
	d := json.NewDecoder(file)
	d.DisallowUnknownFields()
	if err = d.Decode(&fixture); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("trailing fixture content")
	}
	if err = fixture.Validate(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "agent-runtime-memory-eval-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	databasePath := filepath.Join(dir, "memory.db")
	if *sqlitePath != "" {
		databasePath = *sqlitePath
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	scope := memory.Scope{AppID: "memory-eval", BankID: uuid.NewString()}
	if *bankID != "" {
		scope.BankID = *bankID
		if err := scope.Validate(); err != nil {
			return err
		}
	}
	reports := []memoryeval.Report{}
	names := []string{*mode}
	if *mode == "both" {
		names = []string{"local", "hindsight"}
	}
	for _, name := range names {
		var backend memory.Backend
		closeBackend := func() error { return nil }
		if *recorded {
			models := memoryeval.RecordedModels{Fixture: fixture}
			s, err := memory.OpenSQLite(ctx, memory.SQLiteConfig{Path: databasePath, Extractor: models, Embedder: models})
			if err != nil {
				return err
			}
			backend = s
			closeBackend = s.Close
		} else {
			getenv := func(key string) string {
				switch key {
				case "AGENT_RUNTIME_MEMORY_BACKEND":
					if name == "local" {
						return "sqlite"
					}
					return "hindsight"
				case "AGENT_RUNTIME_MEMORY_SQLITE_PATH":
					return databasePath
				default:
					return os.Getenv(key)
				}
			}
			backend, closeBackend, err = memoryconfig.Open(ctx, getenv)
			if err != nil {
				return err
			}
		}
		report, e := memoryeval.EvaluateWithWorkers(ctx, backend, scope, fixture, name, *recorded, *workers)
		closeErr := closeBackend()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		reports = append(reports, report)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(reports)
}
