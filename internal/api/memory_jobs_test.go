package api

import (
	"context"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/memoryjobs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestMemoryLearningSDKRoundTripAndAuthentication(t *testing.T) {
	ctx := context.Background()
	jobs, err := memoryjobs.Open(ctx, filepath.Join(t.TempDir(), "jobs.db"), map[string]memoryjobs.Callback{"app": {URL: "http://127.0.0.1:1", Token: "host-token"}})
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	server := httptest.NewServer(NewServer(Config{MemoryJobs: jobs, ServiceToken: "runtime-token"}))
	defer server.Close()
	client, err := sdk.NewClient(server.URL, sdk.WithAppID("app"), sdk.WithServiceToken("runtime-token"))
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.SubmitMemoryLearning(ctx, sdk.MemoryLearningJob{BankID: "bank", Kind: "extraction", SourceID: "episode/one", Revision: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if job.AppID != "app" || job.State != "queued" || job.ID == "" {
		t.Fatalf("job=%+v", job)
	}
	if err = client.SetMemoryLearningPolicy(ctx, sdk.MemoryLearningPolicy{}); err != nil {
		t.Fatal(err)
	}
	listed, err := client.MemoryLearningJobs(ctx, "bank")
	if err != nil || len(listed) != 1 || listed[0].ID != job.ID {
		t.Fatalf("jobs=%+v err=%v", listed, err)
	}
	response, err := http.Get(server.URL + "/v1/memory/jobs?app_id=app&bank_id=bank")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("anonymous access=%d", response.StatusCode)
	}
}
