package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const eurostatInspectionSource = `import json, re
d = json.load(open('eurostat_crim_se_nuts3.json'))
print('id:', d['id'])
print('size:', d['size'])
dims = d['dimension']
for k in d['id']:
    lab = dims[k]['label']
    cat = dims[k]['category']
    idxs = cat['index']
    rev = {v:k for k,v in idxs.items()}
    print(f'--- {k} ({lab}) n={len(idxs)}')
    for i in sorted(rev)[:25]:
        print('    ', i, rev[i])
`

func TestTypeSafeEurostatLocalRiskPolicy(t *testing.T) {
	reviewer := &TypeSafeReviewer{EvaluatedModel: "jev-1.13.0", AskedThreshold: .75, HazardThreshold: .25, EscalationThreshold: .50, AutoApprove: true}
	// The seven original scores are from the reported production review. The
	// new constraints answer is synthetic: offline tests cannot establish Jev's
	// judgment on the new question. The live regression suite covers that.
	result := testTypeSafeResult(.53, .08)
	for name, score := range map[string]float64{"external_send": .10, "irreversible_delete": .02, "shared_branch_write": .01, "external_send_requested": .15, "violates_user_constraints": .01} {
		result.Answers[name] = typeSafeAnswer{Type: "noul", Noul: &score}
	}
	python, _ := json.Marshal(map[string]any{"source": eurostatInspectionSource})
	command, _ := json.Marshal(map[string]any{"program": "python3", "args": []string{"-c", eurostatInspectionSource}})
	for _, call := range []NativeBlock{{ToolName: "run_python", Input: python}, {ToolName: "run_command", Input: command}} {
		if reason := reviewer.policyReason(result, call, true); reason != "" {
			t.Fatalf("%s local inspection prompted: %s", call.ToolName, reason)
		}
		for _, hazard := range typeSafeHazards {
			original := result.Answers[hazard]
			score := .26
			result.Answers[hazard] = typeSafeAnswer{Type: "noul", Noul: &score}
			if reason := reviewer.policyReason(result, call, true); reason != "hazard:"+hazard {
				t.Fatalf("%s bypassed %s: %s", call.ToolName, hazard, reason)
			}
			result.Answers[hazard] = original
		}
	}
}

func TestTypeSafePythonCommandForms(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{[]string{"-c", "print(1)"}, true},
		{[]string{"-I", "-u", "-c", "print(1)", "data.py"}, true},
		{[]string{"analyze.py", "data.json"}, true},
		{[]string{"-I", "scripts/analyze.py"}, true},
		{[]string{"-m", "venv", ".venv"}, true},
		{[]string{"-m", "pip", "install", "reqeusts"}, false},
		{[]string{"-m", "unknown_module"}, false},
		{[]string{"-"}, false},
		{[]string{"-c"}, false},
		{[]string{"-c", ""}, false},
		{[]string{"../foreign/script.py"}, false},
		{[]string{"/tmp/script.py"}, false},
		{[]string{"-X", "unknown", "script.py"}, false},
		{nil, false},
	} {
		for _, encoded := range []bool{false, true} {
			var args any = tt.args
			if encoded {
				data, _ := json.Marshal(tt.args)
				args = string(data)
			}
			input, _ := json.Marshal(map[string]any{"program": "python3", "args": args})
			if got := typeSafeLocalCandidate(NativeBlock{ToolName: "run_command", Input: input}); got != tt.want {
				t.Fatalf("args=%v encoded=%v candidate=%v want=%v", tt.args, encoded, got, tt.want)
			}
		}
	}
}

func TestTypeSafePythonReviewCapturesExecutedScript(t *testing.T) {
	x := contextTestExec(t)
	x.Run.ExternalActorID = "owner"
	root := t.TempDir()
	x.Run.WorkspaceLease = &agentcore.WorkspaceLease{RootPath: root, Provider: "analysis"}
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "analyze.py"), []byte("print('reviewed source')"), 0600); err != nil {
		t.Fatal(err)
	}
	messages := []NativeMessage{{Role: "user", Provenance: "human", Content: "Analyze the saved data."}}
	for _, tt := range []struct {
		name, input, wantFile string
		wantError             bool
	}{
		{"inline argv is data", `{"program":"python3","args":["-c","print(1)","missing.py"]}`, "", false},
		{"script", `{"program":"python3","args":["scripts/analyze.py"]}`, "scripts/analyze.py", false},
		{"encoded script with cwd", `{"program":"python3","working_directory":"scripts","args":"[\"-I\",\"analyze.py\"]"}`, "scripts/analyze.py", false},
		{"missing script", `{"program":"python3","args":["missing.py"]}`, "", true},
		{"bad cwd", `{"program":"python3","working_directory":"..","args":["analyze.py"]}`, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state, err := typeSafeContext(x, messages, NativeBlock{ToolName: "run_command", Input: json.RawMessage(tt.input)})
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v wantError=%v", err, tt.wantError)
			}
			if err != nil {
				return
			}
			files := state["context"].(map[string]any)["untrusted_repository_files"].(map[string]string)
			if tt.wantFile == "" && len(files) != 0 || tt.wantFile != "" && files[tt.wantFile] != "print('reviewed source')" {
				t.Fatalf("wrong script evidence: %v", files)
			}
		})
	}
}
