package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in live model, fixed local evidence; never performs external CRM writes.
// Run the identical test on both revisions with the same model and credential.
func TestWorkerLiveComparison(t *testing.T) {
	if os.Getenv("WORKER_LIVE_COMPARISON") != "1" {
		t.Skip("opt-in live model comparison")
	}
	var credential sdk.ModelCredential
	if json.Unmarshal([]byte(os.Getenv("WORKER_LIVE_CREDENTIAL")), &credential) != nil {
		t.Fatal("missing live credential")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	x := contextTestExec(t)
	x.Context = ctx
	db := store.NewMemory()
	x.Store = db
	x.Run.Input.Model = &sdk.RunModel{Provider: "openai_chatgpt", Model: "gpt-5.6-sol"}
	x.Run.Input.CredentialSource = "app"
	x.ModelCredentials = &modelauth.Manager{Store: db, Key: []byte(strings.Repeat("k", 32)), ChatGPTEnabled: true, Callbacks: map[string]modelauth.Callback{x.AppID: {URL: os.Getenv("WORKER_LIVE_CALLBACK"), Token: os.Getenv("WORKER_LIVE_CALLBACK_TOKEN")}}}
	record, err := x.ModelCredentials.Prepare(x.AppID, x.Run.ID, "openai_chatgpt", credential)
	if err != nil {
		t.Fatal("credential preparation failed")
	}
	if err := db.CreateRunWithModelCredential(ctx, x.Run, nil, record); err != nil {
		t.Fatal(err)
	}
	x.Agent.ApprovalMode = agentcore.ApprovalModeNever
	x.Agent.ExecutionConfig = json.RawMessage(`{"native_context":{"enabled":true,"context_window":64000,"max_output_tokens":1500,"trigger_tokens":12000,"keep_recent_tokens":1500,"summary_tokens":1000,"max_total_tokens":180000}}`)
	x.Agent.SystemPrompt = "You verify research using supplied evidence. Preserve verified values and source IDs. Do not invent facts."
	x.Run.Input.Instructions = "Use read_probe to read six evidence pages, with page numbers 1 through 6, strictly one page per turn in ascending order. Pages 1 and 2 concern Alpha, pages 3 and 4 Beta, pages 5 and 6 Gamma. After all six pages, return each company's verified seat price and contact name with its source page numbers. Do not reread pages. Do not summarize irrelevant catalog entries. Finish when the table is complete."
	seen := map[int]int{}
	x.Tools = tools.NewRegistry()
	x.AllowedTools = map[string]bool{"read_probe": true}
	x.Tools.Register(tools.Definition{Name: "read_probe", Description: "Read one evidence page by number, 1 to 6.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"page": map[string]any{"type": "integer", "minimum": 1, "maximum": 6}}, "required": []string{"page"}, "additionalProperties": false}}, func(_ context.Context, _ tools.CallContext, raw json.RawMessage) (json.RawMessage, error) {
		var req struct {
			Page int `json:"page"`
		}
		if json.Unmarshal(raw, &req) != nil || req.Page < 1 || req.Page > 6 {
			return nil, fmt.Errorf("page must be 1 to 6")
		}
		seen[req.Page]++
		facts := []string{"Alpha seat price USD 17", "Alpha verified contact Ada Reed", "Beta seat price USD 29", "Beta verified contact Ben Stone", "Gamma seat price USD 43", "Gamma verified contact Cora Lake"}
		fact := facts[req.Page-1]
		return json.Marshal(map[string]string{"evidence": fmt.Sprintf("Source page %d: %s.\n%s\nVerified: %s.", req.Page, fact, strings.Repeat("Archived catalog navigation: products, support, release history, legal, careers.\n", 210), fact)})
	})
	start := time.Now()
	result, runErr := executeNativeModel(ctx, x, NativeConfig{ModelFactory: EinoProviderFactory{}, MaxToolSteps: 12})
	valid := result != nil
	if valid {
		for _, want := range []string{"17", "29", "43", "Ada Reed", "Ben Stone", "Cora Lake"} {
			valid = valid && strings.Contains(result.AssistantText, want)
		}
	}
	for i := 1; i <= 6; i++ {
		valid = valid && seen[i] == 1
	}
	report := map[string]any{"elapsed_seconds": time.Since(start).Seconds(), "evidence_pages": len(seen), "answer_valid": valid, "execution_error": fmt.Sprint(runErr)}
	if result != nil {
		report["provider_usage"] = result.Usage
	}
	recorder, loadErr := openNativeRecorder(ctx, x, true)
	if loadErr == nil {
		report["compactions"] = recorder.state.Generation
	}
	encoded, _ := json.Marshal(report)
	t.Log(string(encoded))
	if runErr != nil || !valid {
		t.Fatalf("live comparison failed correctness check: %v", runErr)
	}
}
