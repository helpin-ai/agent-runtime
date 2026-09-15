// agent-runtime-cutover exports a reviewed disposition list, then explicitly
// terminates and fails only listed retired-engine runs. Run during maintenance
// with old admission and schedules disabled; keep old workers until it finishes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/temporalclient"
	"go.temporal.io/api/serviceerror"
	tclient "go.temporal.io/sdk/client"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type runRow struct {
	ID                string `json:"id"`
	Scope             string `json:"scope"`
	RuntimeKind       string `json:"runtime_kind"`
	Status            string `json:"status"`
	WorkflowID        string `json:"workflow_id"`
	ExternalRuntimeID string `json:"external_runtime_id,omitempty"`
}
type manifest struct {
	Database string   `json:"database"`
	Runs     []runRow `json:"runs"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	database := flag.String("database", "runtime", "runtime or helpin; DATABASE_URL selects that database")
	apply := flag.String("apply", "", "reviewed JSON manifest to dispose; omitted exports a read-only manifest")
	flag.Parse()
	if *database != "runtime" && *database != "helpin" {
		return errors.New("database must be runtime or helpin")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return errors.New("database connection failed; check the selected profile")
	}
	conn, err := db.DB()
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	query := `SELECT id,app_id AS scope,runtime_kind,status,'agent-run-' || id AS workflow_id,'' AS external_runtime_id FROM agent_runs`
	if *database == "helpin" {
		query = `SELECT id,workspace_id AS scope,runtime_kind,status,coalesce(workflow_id,'') AS workflow_id,coalesce(external_runtime_id,'') AS external_runtime_id FROM agent_runs`
	}
	query += ` WHERE runtime_kind IN ('codex','opencode') AND status NOT IN ('completed','failed','cancelled') ORDER BY id`
	var current []runRow
	if err = db.WithContext(ctx).Raw(query).Scan(&current).Error; err != nil {
		return errors.New("inventory query failed; verify database schema")
	}
	if *apply == "" {
		return json.NewEncoder(os.Stdout).Encode(manifest{Database: *database, Runs: current})
	}
	data, err := os.ReadFile(*apply)
	if err != nil {
		return err
	}
	var reviewed manifest
	if err = json.Unmarshal(data, &reviewed); err != nil {
		return err
	}
	if reviewed.Database != *database {
		return errors.New("manifest database does not match")
	}
	if err = validateManifest(reviewed.Runs, current); err != nil {
		return err
	}
	var temporal tclient.Client
	for _, r := range current {
		// Runtime owns mapped workflows. Dispose of runtime rows first, then their
		// Helpin projections. Only pre-runtime Helpin workflows are terminated here.
		if r.WorkflowID != "" && (*database == "runtime" || r.ExternalRuntimeID == "") {
			if temporal == nil {
				address := os.Getenv("TEMPORAL_ADDRESS")
				if address == "" {
					return errors.New("TEMPORAL_ADDRESS is required to terminate workflows")
				}
				temporal, err = tclient.DialContext(ctx, temporalclient.BuildOptionsFromEnv(address))
				if err != nil {
					return errors.New("Temporal connection failed")
				}
				defer temporal.Close()
			}
			err = temporal.TerminateWorkflow(ctx, r.WorkflowID, "", engine.ErrRetiredRuntime.Error())
			var missing *serviceerror.NotFound
			if err != nil && !errors.As(err, &missing) {
				return fmt.Errorf("terminate workflow %s: %w", r.WorkflowID, err)
			}
		}
		scopeColumn := "app_id"
		if *database == "helpin" {
			scopeColumn = "workspace_id"
		}
		// Conditional updates make interrupted disposition rerunnable and retain the
		// original engine identity, transcript, and completed side effects.
		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			result := tx.Exec(`UPDATE agent_runs SET status='failed',pause_reason='none',approval_state='not_required',error_message=?,completed_at=now(),updated_at=now() WHERE id=? AND `+scopeColumn+`=? AND runtime_kind=? AND status NOT IN ('completed','failed','cancelled')`, engine.ErrRetiredRuntime.Error(), r.ID, r.Scope, r.RuntimeKind)
			if result.Error != nil || result.RowsAffected == 0 {
				return result.Error
			}
			if err := tx.Exec(`UPDATE agent_run_interactions SET status='cancelled',updated_at=now() WHERE run_id=? AND `+scopeColumn+`=? AND status='pending'`, r.ID, r.Scope).Error; err != nil {
				return err
			}
			if *database == "runtime" {
				return tx.Exec(`UPDATE agent_run_mcp_servers SET encrypted_credential=NULL WHERE run_id=? AND app_id=?`, r.ID, r.Scope).Error
			}
			// Recompute the host's busy indicator without disturbing another run.
			return tx.Exec(`UPDATE agents SET status='idle',active_task_id=NULL,updated_at=now() WHERE id=(SELECT agent_id FROM agent_runs WHERE id=?) AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.agent_id=agents.id AND r.status NOT IN ('completed','failed','cancelled'))`, r.ID).Error
		})
		if err != nil {
			return fmt.Errorf("record disposition for run %s failed", r.ID)
		}

		fmt.Fprintf(os.Stderr, "disposed %s %s (%s)\n", *database, r.ID, r.WorkflowID)
	}
	return nil
}
func validateManifest(reviewed, current []runRow) error {
	byID := make(map[string]runRow, len(reviewed))
	for _, r := range reviewed {
		if _, ok := byID[r.ID]; ok {
			return errors.New("duplicate manifest run")
		}
		byID[r.ID] = r
	}
	for _, r := range current {
		old, ok := byID[r.ID]
		if !ok || old.Scope != r.Scope || old.RuntimeKind != r.RuntimeKind || old.WorkflowID != r.WorkflowID || old.ExternalRuntimeID != r.ExternalRuntimeID {
			return fmt.Errorf("inventory changed at run %s; export and review a new manifest", r.ID)
		}
	}
	return nil
}
