package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/helpin-ai/agent-runtime-go"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/appconfig"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workjobs"
)

// newWorkJobTestServer serves a SQLite-backed runtime where only "jobs-app"
// opted into work jobs. Workers use the deterministic fallback model, which
// never submits, so jobs settle in no_progress: this test covers the HTTP
// surface; the job lifecycle is covered in package workjobs.
func newWorkJobTestServer(t *testing.T, s agentcore.Store) (*httptest.Server, *workjobs.Service) {
	t.Helper()
	t.Setenv("AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK", "true")
	cfg := &appconfig.Config{Apps: []appconfig.App{{AppID: "jobs-app", WorkJobs: true}, {AppID: "legacy-app"}}}
	targets := host.NewStaticContextProvider()
	targets.Register("jobs-app", agentcore.TargetRef{Type: "batch", ID: "B-1"}, host.TargetContext{Summary: "batch"})
	registry := tools.NewRegistry()
	eng := engine.New(engine.Config{
		DefaultExecutionMode: engine.ExecutionModeLightweight, Store: s, Tools: registry, Targets: targets,
		Runtimes: runtime.NewRegistry(runtime.NewNativeAdapter()),
	})
	jobs := workjobs.New(workjobs.Config{Engine: eng, Store: s, Tools: registry, Interval: 20 * time.Millisecond,
		Enabled: func(appID string) bool { return appconfig.WorkJobsEnabled(cfg, appID) }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); jobs.Run(ctx) }()
	server := httptest.NewServer(NewServer(Config{Engine: eng, Store: s, Tools: registry, AllowAnonymous: true, AppConfig: cfg, WorkJobs: jobs}))
	t.Cleanup(func() { server.Close(); cancel(); <-done })
	return server, jobs
}

func sqliteTestStore(t *testing.T) *store.SQL {
	t.Helper()
	s, err := store.OpenSQL(store.SQLConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "runtime.db")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	return s
}

func sdkClient(t *testing.T, server *httptest.Server, appID string) *sdk.Client {
	t.Helper()
	client, err := sdk.NewClient(server.URL, sdk.WithAppID(appID))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func statusOf(err error) int {
	var apiErr *sdk.HTTPStatusError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// TestJobsDisabledPreservesLegacyRunContract: an app without the work_jobs
// capability gets 404 from every job endpoint, its capability says why, and
// its runs, listings and search behave exactly as before.
func TestJobsDisabledPreservesLegacyRunContract(t *testing.T) {
	server, _ := newWorkJobTestServer(t, sqliteTestStore(t))
	client := sdkClient(t, server, "legacy-app")
	ctx := context.Background()
	caps, err := client.GetCapabilities(ctx)
	if err != nil || caps.WorkJobs == nil || caps.WorkJobs.Supported || caps.WorkJobs.Reason != workjobs.ReasonAppDisabled {
		t.Fatalf("capability = %+v, %v", caps.WorkJobs, err)
	}
	for _, call := range []func() error{
		func() error { _, err := client.ListWorkJobs(ctx); return err },
		func() error { _, err := client.GetWorkJob(ctx, "workjob_x"); return err },
		func() error {
			_, err := client.CreateWorkJob(ctx, sdk.CreateWorkJobRequest{Spec: sdk.WorkJobSpec{Objective: "x"}, Items: []sdk.WorkItemInput{{ID: "a"}}})
			return err
		},
		func() error { _, err := client.CancelWorkJob(ctx, "workjob_x"); return err },
		func() error { _, err := client.GetWorkJobDataset(ctx, "workjob_x"); return err },
	} {
		if err := call(); statusOf(err) != http.StatusNotFound {
			t.Fatalf("disabled app job endpoint = %v", err)
		}
	}
	if _, err := client.SearchRuns(ctx, sdk.RunSearchRequest{IncludeChildren: true}); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("include_children without capability = %v", err)
	}
	if _, err := client.SearchRuns(ctx, sdk.RunSearchRequest{}); err != nil {
		t.Fatalf("legacy search = %v", err)
	}
	if runs, err := client.ListRuns(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("legacy list = %v, %v", runs, err)
	}
}

func TestWorkJobsOnMemoryStoreAreExplicitlyUnsupported(t *testing.T) {
	server, _ := newWorkJobTestServer(t, store.NewMemory())
	client := sdkClient(t, server, "jobs-app")
	caps, err := client.GetCapabilities(context.Background())
	if err != nil || caps.WorkJobs == nil || caps.WorkJobs.Supported || caps.WorkJobs.Reason != workjobs.ReasonUnsupportedStore {
		t.Fatalf("capability = %+v, %v", caps.WorkJobs, err)
	}
	_, err = client.CreateWorkJob(context.Background(), sdk.CreateWorkJobRequest{Spec: sdk.WorkJobSpec{Objective: "x"}, Items: []sdk.WorkItemInput{{ID: "a"}}})
	if statusOf(err) != http.StatusNotImplemented || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("opted-in app on memory store = %v", err)
	}
}

// TestWorkJobHTTPSurface exercises every job endpoint through the SDK.
func TestWorkJobHTTPSurface(t *testing.T) {
	s := sqliteTestStore(t)
	server, _ := newWorkJobTestServer(t, s)
	client := sdkClient(t, server, "jobs-app")
	ctx := context.Background()
	agent := agentcore.Agent{AppID: "jobs-app", ID: "researcher", Name: "Researcher", RuntimeKind: agentcore.RuntimeNativeSDK}
	if err := s.CreateAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	caps, err := client.GetCapabilities(ctx)
	if err != nil || caps.WorkJobs == nil || !caps.WorkJobs.Supported {
		t.Fatalf("capability = %+v, %v", caps.WorkJobs, err)
	}
	spec := sdk.WorkJobSpec{Objective: "Look up items", ResultSchema: json.RawMessage(`{"type":"object","required":["value"]}`), MaxNoProgressAttempts: 1}
	worker := sdk.WorkJobWorker{AgentID: "researcher", Target: sdk.TargetRef{Type: "batch", ID: "B-1"}}
	if _, err := client.CreateWorkJob(ctx, sdk.CreateWorkJobRequest{Spec: sdk.WorkJobSpec{Objective: "x"}, Worker: worker, Items: []sdk.WorkItemInput{{ID: "a"}}}); statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "result_schema") {
		t.Fatalf("invalid spec = %v", err)
	}
	bad := worker
	bad.AllowedTools = []string{"submit_item_result"}
	if _, err := client.CreateWorkJob(ctx, sdk.CreateWorkJobRequest{Spec: spec, Worker: bad, Items: []sdk.WorkItemInput{{ID: "a"}}}); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("reserved tool in worker template = %v", err)
	}
	req := sdk.CreateWorkJobRequest{IdempotencyKey: "k1", Spec: spec, Worker: worker,
		Items: []sdk.WorkItemInput{{ID: "a", Input: json.RawMessage(`{"name":"A"}`)}, {ID: "b"}}}
	job, err := client.CreateWorkJob(ctx, req)
	if err != nil || job.Counts.Total != 2 || job.Spec.BatchSize != 10 {
		t.Fatalf("create = %+v, %v", job, err)
	}
	again, err := client.CreateWorkJob(ctx, req)
	if err != nil || again.ID != job.ID {
		t.Fatalf("idempotent create = %+v, %v", again, err)
	}
	// The fallback worker never submits; one no-progress attempt makes the
	// job wait for the user.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if job, err = client.GetWorkJob(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
		if job.State == sdk.WorkJobStateWaiting || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != sdk.WorkJobStateWaiting || job.WaitingReason != sdk.WorkJobWaitingNoProgress {
		t.Fatalf("job = %s/%s", job.State, job.WaitingReason)
	}
	items, err := client.ListWorkItems(ctx, job.ID, "", "", 1)
	if err != nil || len(items.Items) != 1 || items.Items[0].ID != "a" || items.NextAfter != "a" || string(items.Items[0].Input) != `{"name":"A"}` {
		t.Fatalf("items = %+v, %v", items, err)
	}
	attempts, err := client.ListWorkAttempts(ctx, job.ID)
	if err != nil || len(attempts) != 1 || attempts[0].State != sdk.WorkAttemptStateFailed {
		t.Fatalf("attempts = %+v, %v", attempts, err)
	}
	events, err := client.ListWorkJobEvents(ctx, job.ID, 0, 0)
	if err != nil || len(events.Items) == 0 || events.Items[0].Type != "work_job.created" {
		t.Fatalf("events = %+v, %v", events, err)
	}
	// Workers are hidden by default and listed on explicit request.
	if page, err := client.SearchRuns(ctx, sdk.RunSearchRequest{}); err != nil || page.Total != 0 {
		t.Fatalf("default search = %+v, %v", page, err)
	}
	if page, err := client.SearchRuns(ctx, sdk.RunSearchRequest{IncludeChildren: true}); err != nil || page.Total != 1 {
		t.Fatalf("include_children search = %+v, %v", page, err)
	}
	if runs, err := client.ListRuns(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("default list = %d, %v", len(runs), err)
	}
	if _, err := client.AddWorkItems(ctx, job.ID, sdk.AddWorkItemsRequest{Items: []sdk.WorkItemInput{{ID: "a"}}}); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("duplicate added item = %v", err)
	}
	if job, err = client.AddWorkItems(ctx, job.ID, sdk.AddWorkItemsRequest{Items: []sdk.WorkItemInput{{ID: "c", DependsOn: []string{"a"}}}}); err != nil || job.Counts.Total != 3 {
		t.Fatalf("add items = %+v, %v", job, err)
	}
	if _, err := client.ReviseWorkJob(ctx, job.ID, sdk.ReviseWorkJobRequest{ExpectedRevision: 7, Spec: spec}); statusOf(err) != http.StatusConflict {
		t.Fatalf("stale revision = %v", err)
	}
	if job, err = client.ReviseWorkJob(ctx, job.ID, sdk.ReviseWorkJobRequest{ExpectedRevision: 1, Spec: spec}); err != nil || job.SpecRevision != 2 {
		t.Fatalf("revise = %+v, %v", job, err)
	}
	if job, err = client.PauseWorkJob(ctx, job.ID); err != nil || job.State != sdk.WorkJobStatePaused {
		t.Fatalf("pause = %+v, %v", job, err)
	}
	if job, err = client.ResumeWorkJob(ctx, job.ID); err != nil || job.State != sdk.WorkJobStateRunning {
		t.Fatalf("resume = %+v, %v", job, err)
	}
	if job, err = client.CancelWorkJob(ctx, job.ID); err != nil || job.State != sdk.WorkJobStateCancelled {
		t.Fatalf("cancel = %+v, %v", job, err)
	}
	dataset, err := client.GetWorkJobDataset(ctx, job.ID)
	if err != nil || dataset.Revision != 1 || len(dataset.Items) != 0 {
		t.Fatalf("dataset = %+v, %v", dataset, err)
	}
	jobs, err := client.ListWorkJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list = %d, %v", len(jobs), err)
	}
	// The job event stream replays from the cursor.
	streamReq, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/work-jobs/"+job.ID+"/events/stream?app_id=jobs-app&after=0", nil)
	streamCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(streamReq.WithContext(streamCtx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(buf[:n]), "event: work_job.created") {
		t.Fatalf("stream = %d %q", resp.StatusCode, buf[:n])
	}
	if _, err := client.GetWorkJob(ctx, "workjob_missing"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("missing job = %v", err)
	}
	other := sdkClient(t, server, "legacy-app")
	if _, err := other.GetWorkJob(ctx, job.ID); statusOf(err) != http.StatusNotFound {
		t.Fatalf("another app read a job = %v", err)
	}
}
