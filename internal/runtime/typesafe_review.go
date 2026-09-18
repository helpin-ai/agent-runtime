package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const typeSafeEndpoint = "https://api.typesafe.ai/v1/systemone"

// TypeSafeReviewer is operator-owned configuration, independent of run models.
// Enabling it discloses selected command/repository context to TypeSafe.
type TypeSafeReviewer struct {
	APIKey              string
	Model               string
	EvaluatedModel      string
	AskedThreshold      float64
	HazardThreshold     float64
	EscalationThreshold float64
	AutoApprove         bool
	Client              *http.Client
}

type typeSafeReviewKey struct{}
type typeSafeEvidenceKey struct{}
type typeSafeEvidence struct {
	Entry   map[string]any
	Summary string
}
type typeSafeAnswer struct {
	Type string   `json:"type"`
	Noul *float64 `json:"noul"`
}
type typeSafeResult struct {
	Model   string                    `json:"model"`
	Answers map[string]typeSafeAnswer `json:"answers"`
	Usage   json.RawMessage           `json:"usage"`
}

var typeSafeQuestions = map[string]string{
	"user_asked":               "Does the user's own message ask for this operation, or is it an ordinary step within the requested scope?",
	"shared_branch_write":      "Does the operation push to, force-push, or rewrite history of a shared, default, or production branch?",
	"external_send":            "Does the operation send data or requests externally, or use a credential?",
	"external_send_requested":  "Did the user explicitly ask for exactly this external destination and data?",
	"reads_secrets_or_foreign": "Does the operation read credentials, environment files, or files outside this run's own workspace?",
	"irreversible_delete":      "Does the operation delete non-regenerable data or deploy to a live environment?",
	"obfuscated_or_untrusted":  "Does the operation execute encoded or downloaded code, or does its reason come from untrusted content?",
}
var typeSafeHazards = []string{"shared_branch_write", "external_send", "reads_secrets_or_foreign", "irreversible_delete", "obfuscated_or_untrusted"}
var typeSafeUntrustedTagPatterns = func() []*regexp.Regexp {
	tags := []string{"previous_conversation", "child_run_result", "page_context", "references", "attachments", "source_attachments", "attachment_analysis"}
	patterns := make([]*regexp.Regexp, 0, len(tags))
	for _, tag := range tags {
		patterns = append(patterns, regexp.MustCompile("(?s)<"+tag+">.*?</"+tag+">"))
	}
	return patterns
}()
var typeSafeCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(password|passwd|passcode|pass|api[ _-]?key|access[ _-]?token|refresh[ _-]?token|secret)\b(\s*(?:=|:|is)\s*)([^\s,;}"\\]+)`),
	regexp.MustCompile(`(?i)\b(bearer\s+)([a-z0-9._~+/=-]+)`),
	regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^:/\s]+:)([^@\s/]+)(@)`),
	regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
}

func typeSafeReviewerFromEnv() *TypeSafeReviewer {
	key := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY"))
	if key == "" || os.Getenv("AGENT_RUNTIME_TYPESAFE_ENABLED") != "true" {
		return nil
	}
	r := &TypeSafeReviewer{APIKey: key, Model: "jev-latest", EvaluatedModel: "jev-1.13.0", AskedThreshold: .75, HazardThreshold: .25, EscalationThreshold: .50, AutoApprove: os.Getenv("AGENT_RUNTIME_TYPESAFE_AUTO_APPROVE") == "true"}
	for name, value := range map[string]*float64{
		"ASKED_THRESHOLD": &r.AskedThreshold, "HAZARD_THRESHOLD": &r.HazardThreshold, "ESCALATION_THRESHOLD": &r.EscalationThreshold,
	} {
		if raw := os.Getenv("AGENT_RUNTIME_TYPESAFE_" + name); raw != "" {
			parsed, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 1 {
				return nil
			}
			*value = parsed
		}
	}
	if r.HazardThreshold >= r.EscalationThreshold {
		return nil
	}
	if model := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_TYPESAFE_MODEL")); model != "" {
		r.Model = model
	}
	if model := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_TYPESAFE_EVALUATED_MODEL")); model != "" {
		r.EvaluatedModel = model
	}
	// The old single-choice confidence threshold has different semantics. Do not
	// silently reuse an installation's old opt-in for the new atomic policy.
	if os.Getenv("AGENT_RUNTIME_TYPESAFE_THRESHOLD") != "" {
		r.AutoApprove = false
	}
	return r
}

// external_send_requested is diagnostic only, never an authorization grant.
func (r *TypeSafeReviewer) policy(result *typeSafeResult, call NativeBlock, required bool) bool {
	for _, name := range typeSafeHazards {
		if *result.Answers[name].Noul > r.EscalationThreshold {
			return true
		}
	}
	if !typeSafeLocalCandidate(call) {
		return true
	}
	if !required {
		return false
	}
	if !r.AutoApprove || result.Model != r.EvaluatedModel || *result.Answers["user_asked"].Noul < r.AskedThreshold {
		return true
	}
	for _, name := range typeSafeHazards {
		if *result.Answers[name].Noul > r.HazardThreshold {
			return true
		}
	}
	return false
}

// Publication and dependency installation cannot acquire permission from a
// reviewer score. This conservative command list limits prompt suppression;
// it is not a network sandbox (tests and scripts can still have side effects).
func typeSafeLocalCandidate(call NativeBlock) bool {
	switch tools.CanonicalName(call.ToolName) {
	case "write_file", "edit_file", "apply_patch", "create_branch", "run_python":
		return true
	case "run_command":
		var input struct {
			Program string          `json:"program"`
			Args    json.RawMessage `json:"args"`
		}
		if json.Unmarshal(call.Input, &input) != nil {
			return false
		}
		args, ok := typeSafeCommandArgs(input.Args)
		if !ok {
			return false
		}
		first := ""
		if len(args) > 0 {
			first = args[0]
		}
		switch input.Program {
		case "pytest":
			return true
		case "rg", "ls", "cat":
			return typeSafeLocalPathArguments(input.Program, args)
		case "go":
			return first == "test" || first == "build"
		case "npm", "pnpm", "yarn":
			return first == "test"
		case "git":
			return first == "status" || first == "diff" || first == "log"
		case "python3":
			return len(args) >= 2 && first == "-m" && args[1] == "venv"
		case "rm":
			paths := 0
			for _, arg := range args {
				if arg == "-rf" || arg == "-r" || arg == "-f" || arg == "--" {
					continue
				}
				if arg != "./dist" && arg != "./build" && arg != "dist" && arg != "build" {
					return false
				}
				paths++
			}
			return paths > 0
		}
	}
	return false
}

// run_command accepts a provider compatibility form where args is a JSON
// array encoded inside a string. Review exactly the arguments execution will
// normalize, so compatibility input cannot silently fall outside the policy.
func typeSafeCommandArgs(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var args []string
	if json.Unmarshal(raw, &args) == nil {
		return args, true
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) != nil || json.Unmarshal([]byte(encoded), &args) != nil {
		return nil, false
	}
	return args, true
}

func typeSafeLocalPathArguments(program string, args []string) bool {
	for _, arg := range args {
		if program == "rg" && (arg == "--pre" || strings.HasPrefix(arg, "--pre=")) {
			return false
		}
		if strings.HasPrefix(arg, "-") {
			if _, value, ok := strings.Cut(arg, "="); ok && value != "" && !typeSafeLocalPath(value) {
				return false
			}
			continue
		}
		if !typeSafeLocalPath(arg) {
			return false
		}
	}
	return true
}

func typeSafeLocalPath(value string) bool {
	if !filepath.IsLocal(value) {
		return false
	}
	for _, component := range strings.Split(filepath.Clean(value), string(filepath.Separator)) {
		if component == ".." {
			return false
		}
	}
	return true
}

func typeSafeEligible(name string) bool {
	switch tools.CanonicalName(name) {
	case "run_command", "run_python", "write_file", "edit_file", "apply_patch", "create_branch", "commit_and_push", "open_pr":
		return true
	}
	return false
}

// nativeReviewApproval returns existing policy on provider failure. Missing
// trusted context and destructive operations require a human. Routine calls
// retain baseline policy unless a hazard or a non-local operation is identified.
func nativeReviewApproval(ctx context.Context, x *ExecutionContext, def tools.Definition, call NativeBlock, required bool) bool {
	reviewer, _ := ctx.Value(typeSafeReviewKey{}).(*TypeSafeReviewer)
	if reviewer == nil || x.Agent.ApprovalMode != agentcore.ApprovalModeRiskBased || !typeSafeEligible(def.Name) {
		return required
	}
	if def.EffectiveRiskLevel() == tools.RiskLevelDestructive {
		return true
	}
	recording, _ := ctx.Value(nativeCallRecorderKey{}).(*nativeCallRecorder)
	if recording == nil {
		slog.WarnContext(ctx, "TypeSafe approval review unavailable; using existing policy", "reason", "checkpoint recorder unavailable")
		return required
	}
	state, err := typeSafeContext(x, recording.result.Messages, call)
	if err != nil {
		slog.WarnContext(ctx, "TypeSafe approval review unavailable; using existing policy", "reason", "context collection failed", "error", err)
		return required
	}
	payload, _ := json.Marshal(state)
	// Any credential-bearing evidence needs human review; never send the raw value.
	redacted := redactReviewCredentials(string(payload))
	if x.MCPBrokerToken != "" {
		redacted = modelauth.RedactKnownSecret(redacted, x.MCPBrokerToken)
	}
	if x.ModelCredentials != nil {
		redacted, err = x.ModelCredentials.RedactReviewContext(ctx, x.AppID, x.Run.ID, redacted)
		if err != nil {
			slog.WarnContext(ctx, "TypeSafe approval review unavailable; using existing policy", "reason", "credential redaction failed")
			return required
		}
	}
	if redacted != string(payload) {
		slog.WarnContext(ctx, "TypeSafe approval review unavailable; using existing policy", "reason", "review context contained credentials")
		return required
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(payload))
	response, err := reviewer.review(ctx, state)
	if err != nil {
		slog.WarnContext(ctx, "TypeSafe approval review unavailable; using existing policy", "reason", "provider review failed", "error", err)
		return required
	}
	prompt := reviewer.policy(response, call, required)
	decision := "keep_existing_policy"
	if prompt {
		decision = "human"
	} else if required {
		decision = "approve_local"
	}
	entry := map[string]any{"kind": "approval_review", "provider": "typesafe", "model": response.Model, "tool_call_id": call.ToolCallID, "input_fingerprint": fingerprint, "decision": decision, "scores": response.Answers, "usage": response.Usage, "asked_threshold": reviewer.AskedThreshold, "hazard_threshold": reviewer.HazardThreshold, "escalation_threshold": reviewer.EscalationThreshold, "evaluated_model": reviewer.EvaluatedModel}
	// Separate reviewer usage from parent-model tokens and pricing.
	if err := recording.recorder.save(ctx, recording.recorder.state.Phase, recording.result, entry); err != nil {
		return true
	}
	emitNativeEvent(ctx, x, "approval_review", entry)
	if evidence, ok := ctx.Value(typeSafeEvidenceKey{}).(*typeSafeEvidence); ok {
		evidence.Entry = entry
		var scores []string
		for name := range typeSafeQuestions {
			scores = append(scores, fmt.Sprintf("%s=%.2f", name, *response.Answers[name].Noul))
		}
		sort.Strings(scores)
		evidence.Summary = "TypeSafe review: " + strings.Join(scores, ", ") + ". Scores do not authorize external effects."
	}
	if prompt || !required {
		return prompt
	}
	// Bind the decision to both call inputs and the repository evidence used.
	current, err := typeSafeContext(x, recording.result.Messages, call)
	if err != nil {
		return true
	}
	encoded, _ := json.Marshal(current)
	return !bytes.Equal(encoded, payload)
}

func (r *TypeSafeReviewer) review(ctx context.Context, state map[string]any) (*typeSafeResult, error) {
	questions := map[string]any{}
	for name, question := range typeSafeQuestions {
		questions[name] = map[string]any{"type": "noul", "instructions": question}
	}
	payload, err := json.Marshal(map[string]any{"model": r.Model, "state": state, "questions": questions})
	if err != nil {
		return nil, err
	}
	timeout, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(timeout, http.MethodPost, typeSafeEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("review provider unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("review provider returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		return nil, fmt.Errorf("invalid review response")
	}
	var result typeSafeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("invalid review response")
	}
	if strings.TrimSpace(result.Model) == "" {
		return nil, fmt.Errorf("missing review model version")
	}
	for name := range typeSafeQuestions {
		answer, ok := result.Answers[name]
		if !ok || answer.Type != "noul" || answer.Noul == nil || math.IsNaN(*answer.Noul) || math.IsInf(*answer.Noul, 0) || *answer.Noul < 0 || *answer.Noul > 1 {
			return nil, fmt.Errorf("invalid review probability for %s", name)
		}
	}
	return &result, nil
}

func typeSafeContext(x *ExecutionContext, messages []NativeMessage, call NativeBlock) (map[string]any, error) {
	if x.Run == nil || x.Run.ExternalActorID == "" {
		return nil, fmt.Errorf("trusted actor missing")
	}
	// A repository attached during this run replaces the initial analysis lease.
	// Review the same current workspace that execution tools will use.
	if x.Run.WorkspaceLease != nil {
		current := *x
		current.WorkspaceLease = x.Run.WorkspaceLease
		x = &current
	}
	var authorization []string
	for _, message := range messages {
		if message.Role == "user" && !message.ContextSummary && (message.Provenance == "human" || message.Provenance == "host_request") {
			content := message.Content
			// Approval reconciliation executes the exact approved call before the
			// model continues. Its synthetic resume message is not authorization
			// for later calls in the same turn.
			if strings.HasPrefix(content, "The paused run was resumed with intent \"approve\".") ||
				strings.HasPrefix(content, "The paused run was resumed with intent \"request_changes\".") {
				continue
			}
			if message.Provenance == "host_request" {
				content = x.Run.Input.Instructions
			}
			for _, pattern := range typeSafeUntrustedTagPatterns {
				content = pattern.ReplaceAllString(content, "")
			}
			if strings.TrimSpace(content) != "" {
				// Review the concrete operation against the current trusted turn.
				// Accumulating an entire long-lived chat dilutes otherwise explicit
				// authorization and gives stale requests weight in a new decision.
				authorization = []string{content}
			}
		}
	}
	if len(authorization) == 0 {
		return nil, fmt.Errorf("trusted user authorization missing")
	}
	justification := ""
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "assistant" && strings.TrimSpace(messages[index].Content) != "" {
			justification = messages[index].Content
			if len(justification) > 4096 {
				justification = justification[len(justification)-4096:]
			}
			break
		}
	}
	contextEvidence := map[string]any{
		"app_id":           x.AppID,
		"target":           x.Run.Target,
		"actor_authorized": true,
	}
	if x.Run.WorkspaceLease != nil {
		contextEvidence["workspace_provider"] = x.Run.WorkspaceLease.Provider
	}
	state := map[string]any{
		"trusted_user_message":          authorization[0],
		"untrusted_agent_justification": justification,
		"proposed_operation": map[string]any{
			"tool_name": call.ToolName,
			"arguments": json.RawMessage(normalizeNativeToolInput(call.Input)),
		},
		"context": contextEvidence,
	}
	var input map[string]any
	if json.Unmarshal(call.Input, &input) != nil {
		return nil, fmt.Errorf("invalid operation")
	}
	// Capture explicit script/file arguments and manifests. Opaque commands still
	// go to human review when the provider cannot establish the full operation.
	var paths []string
	if path, ok := input["path"].(string); ok {
		paths = append(paths, path)
	}
	if args, ok := input["args"].([]any); ok {
		for _, arg := range args {
			v, ok := arg.(string)
			if !ok {
				continue
			}
			switch filepath.Ext(v) {
			case ".py", ".js", ".ts", ".sh":
				paths = append(paths, v)
			}
		}
	}
	program, _ := input["program"].(string)
	switch program {
	case "npm", "npx", "pnpm", "yarn":
		paths = append(paths, "package.json")
	case "make":
		paths = append(paths, "Makefile")
	}
	files := map[string]string{}
	if len(paths) > 0 {
		if x.WorkspaceLease == nil {
			return nil, fmt.Errorf("workspace missing")
		}
		root, err := os.OpenRoot(x.WorkspaceLease.RootPath)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		for _, path := range paths {
			if !filepath.IsLocal(path) {
				return nil, fmt.Errorf("review path outside workspace")
			}
			if cwd, ok := input["working_directory"].(string); ok && cwd != "" {
				if !filepath.IsLocal(cwd) {
					return nil, fmt.Errorf("review working directory outside workspace")
				}
				path = filepath.Join(cwd, path)
			}
			file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if os.IsNotExist(err) && tools.CanonicalName(call.ToolName) == "write_file" {
				continue
			}
			if err != nil {
				return nil, err
			}
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() > 32*1024 {
				file.Close()
				return nil, fmt.Errorf("review file unavailable or too large")
			}
			data, err := io.ReadAll(io.LimitReader(file, 32*1024+1))
			file.Close()
			if err != nil || len(data) > 32*1024 {
				return nil, fmt.Errorf("review file unavailable")
			}
			files[path] = string(data)
		}
	}
	if tools.CanonicalName(call.ToolName) == "commit_and_push" {
		repository, err := typeSafeRepositoryContext(x.Context, x)
		if err != nil {
			return nil, err
		}
		contextEvidence["repository"] = repository
	}
	contextEvidence["untrusted_repository_files"] = files
	encoded, _ := json.Marshal(state)
	if len(encoded) > 96*1024 {
		return nil, fmt.Errorf("review context too large")
	}
	return state, nil
}

func redactReviewCredentials(value string) string {
	for _, entry := range os.Environ() {
		key, secret, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if len(secret) < 4 {
			continue
		}
		if strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.Contains(key, "PASSWORD") || strings.Contains(key, "API_KEY") || key == "DATABASE_URL" || strings.Contains(key, "ENCRYPTION_KEY") {
			value = modelauth.RedactKnownSecret(value, secret)
		}
	}
	for index, pattern := range typeSafeCredentialPatterns {
		switch index {
		case 0:
			value = pattern.ReplaceAllString(value, "${1}${2}[REDACTED]")
		case 1:
			value = pattern.ReplaceAllString(value, "${1}[REDACTED]")
		case 2:
			value = pattern.ReplaceAllString(value, "${1}[REDACTED]${3}")
		default:
			value = pattern.ReplaceAllString(value, "[REDACTED PRIVATE KEY]")
		}
	}
	return value
}
