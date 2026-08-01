package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type stageTestStore struct {
	objects map[string][]byte
}

func (s *stageTestStore) GetObject(_ context.Context, key string) ([]byte, error) {
	return s.objects[key], nil
}

func TestStageResolvedIntoStagesBuiltInSkillPackageReferences(t *testing.T) {
	destRoot := filepath.Join(t.TempDir(), "skills")
	definition, ok := NewDefaultRegistry().BuiltIn("dependency_auditor")
	if !ok {
		t.Fatal("expected dependency_auditor built-in")
	}
	resolution := Resolution{
		CoreRefs:    []agentcore.SkillRef{{Key: "dependency_auditor"}},
		Definitions: []Definition{definition},
	}

	if err := StageResolvedInto(context.Background(), resolution, StageOptions{DestRoot: destRoot}); err != nil {
		t.Fatalf("stage dependency auditor skill: %v", err)
	}
	for _, rel := range []string{
		filepath.Join("01-dependency_auditor", "SKILL.md"),
		filepath.Join("01-dependency_auditor", "ecosystems", "go.md"),
		filepath.Join("01-dependency_auditor", "ecosystems", "rust.md"),
		filepath.Join("01-dependency_auditor", "ecosystems", "python.md"),
		filepath.Join("01-dependency_auditor", "ecosystems", "node.md"),
		filepath.Join("01-dependency_auditor", "ecosystems", "java.md"),
		filepath.Join("01-dependency_auditor", "verification.md"),
	} {
		if _, err := os.Stat(filepath.Join(destRoot, rel)); err != nil {
			t.Fatalf("expected staged dependency auditor file %s: %v", rel, err)
		}
	}
}

func TestStageResolvedIntoStagesWorkspaceSkillArchive(t *testing.T) {
	definition := Definition{
		Key:          "workspace_review",
		Title:        "Workspace Review",
		Description:  "Review changes for the workspace.",
		Instructions: "Inspect the repo, call `update_plan`, request review with `request_approval`, and produce a review summary.",
		SourceKind:   SourceWorkspace,
	}
	archive, checksum, filename, err := BuildSkillArchive(definition)
	if err != nil {
		t.Fatalf("build archive: %v", err)
	}
	skillID := "skill-123"
	versionKey := SkillVersionForBytes(archive)
	objectKey := "workspaces/ws_123/skills/skill-123/" + filename
	lookup := &recordingContextualLookup{skill: &WorkspaceSkill{
		ID:               skillID,
		SourceKind:       SourceWorkspace,
		Key:              definition.Key,
		VersionKey:       versionKey,
		Title:            definition.Title,
		Description:      definition.Description,
		Instructions:     definition.Instructions,
		PackageObjectKey: objectKey,
		PackageChecksum:  checksum,
	}}
	store := &stageTestStore{objects: map[string][]byte{objectKey: archive}}
	destRoot := filepath.Join(t.TempDir(), "skills")
	resolution := Resolution{
		CoreRefs:    []agentcore.SkillRef{{SkillID: skillID, Key: definition.Key, VersionKey: versionKey}},
		Definitions: []Definition{definition},
	}

	if err := StageResolvedInto(context.Background(), resolution, StageOptions{
		Lookup:       lookup,
		PackageStore: store,
		LookupContext: LookupContext{
			AppID: "app-a",
			RunID: "run-1",
		},
		DestRoot: destRoot,
	}); err != nil {
		t.Fatalf("stage skills: %v", err)
	}
	if lookup.byIDReq.RunID != "run-1" || lookup.byIDReq.SkillID != skillID {
		t.Fatalf("expected contextual lookup by id, got %#v", lookup.byIDReq)
	}
	payload, err := os.ReadFile(filepath.Join(destRoot, "01-workspace_review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read staged workspace SKILL.md: %v", err)
	}
	if !strings.Contains(string(payload), definition.Description) {
		t.Fatalf("expected staged workspace skill markdown to contain description, got %q", string(payload))
	}
	if !strings.Contains(string(payload), "`mcp__agent_runtime__update_plan`") {
		t.Fatalf("expected staged workspace skill markdown to use runtime update_plan tool name, got %q", string(payload))
	}
	if strings.Contains(string(payload), "`update_plan`") {
		t.Fatalf("expected staged workspace skill markdown not to expose bare update_plan tool name, got %q", string(payload))
	}

	codexDestRoot := filepath.Join(t.TempDir(), "codex-skills")
	if err := StageResolvedInto(context.Background(), resolution, StageOptions{
		Lookup:       lookup,
		PackageStore: store,
		LookupContext: LookupContext{
			AppID: "app-a",
			RunID: "run-codex",
		},
		DestRoot:    codexDestRoot,
		RuntimeKind: agentcore.RuntimeCodex,
	}); err != nil {
		t.Fatalf("stage Codex skills: %v", err)
	}
	codexPayload, err := os.ReadFile(filepath.Join(codexDestRoot, "01-workspace_review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read Codex staged workspace SKILL.md: %v", err)
	}
	if !strings.Contains(string(codexPayload), "`update_plan`") || strings.Contains(string(codexPayload), "`mcp__agent_runtime__update_plan`") {
		t.Fatalf("expected Codex staged skill markdown to preserve native update_plan, got %q", string(codexPayload))
	}
	if !strings.Contains(string(codexPayload), "`request_approval`") || strings.Contains(string(codexPayload), "`mcp__agent_runtime__request_approval`") {
		t.Fatalf("expected Codex staged skill markdown to use the logical dynamic-tool approval name, got %q", string(codexPayload))
	}
}

func TestReconcileResolutionFromStagedPackagesRestoresPackageInteractionContract(t *testing.T) {
	packageDefinition := Definition{
		Key:               "workspace_approval",
		Title:             "Workspace Approval",
		Description:       "Require approval for a workspace result.",
		Instructions:      "Publish the result, then call `request_approval`.",
		SourceKind:        SourceWorkspace,
		RequiredTools:     []string{"request_approval"},
		SupportedRuntimes: []string{agentcore.RuntimeCodex},
		Policy: Policy{
			CompletionRequiresInteractionKinds: []string{InteractionKindApprovalRequest},
		},
	}
	archive, checksum, filename, err := BuildSkillArchive(packageDefinition)
	if err != nil {
		t.Fatalf("build archive: %v", err)
	}
	objectKey := "workspaces/ws-1/skills/workspace_approval/" + filename
	lookup := &recordingContextualLookup{skill: &WorkspaceSkill{
		ID:               "skill-approval",
		Key:              packageDefinition.Key,
		VersionKey:       SkillVersionForBytes(archive),
		Title:            packageDefinition.Title,
		Description:      packageDefinition.Description,
		SourceKind:       SourceWorkspace,
		Instructions:     packageDefinition.Instructions,
		PackageObjectKey: objectKey,
		PackageChecksum:  checksum,
		// Deliberately omit RequiredTools and Policy to reproduce a host
		// metadata projection that lags behind the versioned package.
	}}
	resolution := Resolution{
		CoreRefs: []agentcore.SkillRef{{
			SkillID:    lookup.skill.ID,
			Key:        packageDefinition.Key,
			VersionKey: lookup.skill.VersionKey,
		}},
		Definitions: []Definition{{
			Key:          packageDefinition.Key,
			Title:        packageDefinition.Title,
			Description:  packageDefinition.Description,
			SourceKind:   SourceWorkspace,
			Instructions: packageDefinition.Instructions,
		}},
	}
	destRoot := filepath.Join(t.TempDir(), "skills")
	if err := StageResolvedInto(context.Background(), resolution, StageOptions{
		Lookup:        lookup,
		PackageStore:  &stageTestStore{objects: map[string][]byte{objectKey: archive}},
		LookupContext: LookupContext{AppID: "app-a", RunID: "run-approval"},
		DestRoot:      destRoot,
		RuntimeKind:   agentcore.RuntimeCodex,
	}); err != nil {
		t.Fatalf("stage skill: %v", err)
	}

	reconciled, err := ReconcileResolutionFromStagedPackages(resolution, destRoot)
	if err != nil {
		t.Fatalf("reconcile staged package: %v", err)
	}
	if len(reconciled.Definitions) != 1 || !containsString(reconciled.Definitions[0].RequiredTools, "request_approval") {
		t.Fatalf("expected package required tool, got %#v", reconciled.Definitions)
	}
	if got := CompletionRequiredInteractionKinds(reconciled.Policy, reconciled.Definitions); !containsString(got, InteractionKindApprovalRequest) {
		t.Fatalf("expected package approval policy, got %#v", got)
	}
}

func TestStageResolvedIntoDequalifiesHostMCPToolNamesForCodex(t *testing.T) {
	definition := Definition{
		Key:          "host_qualified_approval",
		Title:        "Host-qualified approval",
		Description:  "Host-authored approval instructions.",
		Instructions: "Call `mcp__helpin__publish_task_plan_doc`, then `mcp__helpin__request_approval`.",
		SourceKind:   SourceWorkspace,
	}
	archive, checksum, filename, err := BuildSkillArchive(definition)
	if err != nil {
		t.Fatalf("build archive: %v", err)
	}
	objectKey := "workspaces/ws-1/skills/host_qualified_approval/" + filename
	lookup := &recordingContextualLookup{skill: &WorkspaceSkill{
		ID:               "skill-qualified",
		Key:              definition.Key,
		VersionKey:       SkillVersionForBytes(archive),
		Title:            definition.Title,
		Description:      definition.Description,
		SourceKind:       SourceWorkspace,
		Instructions:     definition.Instructions,
		PackageObjectKey: objectKey,
		PackageChecksum:  checksum,
	}}
	resolution := Resolution{
		CoreRefs: []agentcore.SkillRef{{
			SkillID:    lookup.skill.ID,
			Key:        definition.Key,
			VersionKey: lookup.skill.VersionKey,
		}},
		Definitions: []Definition{definition},
	}
	destRoot := filepath.Join(t.TempDir(), "skills")
	if err := StageResolvedInto(context.Background(), resolution, StageOptions{
		Lookup:        lookup,
		PackageStore:  &stageTestStore{objects: map[string][]byte{objectKey: archive}},
		LookupContext: LookupContext{AppID: "app-a", RunID: "run-qualified"},
		DestRoot:      destRoot,
		RuntimeKind:   agentcore.RuntimeCodex,
	}); err != nil {
		t.Fatalf("stage skill: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(destRoot, "01-host_qualified_approval", "SKILL.md"))
	if err != nil {
		t.Fatalf("read staged skill: %v", err)
	}
	staged := string(payload)
	for _, logical := range []string{"`publish_task_plan_doc`", "`request_approval`"} {
		if !strings.Contains(staged, logical) {
			t.Fatalf("expected staged instructions to contain %s, got %q", logical, staged)
		}
	}
	if strings.Contains(staged, "mcp__helpin__") {
		t.Fatalf("expected Codex instructions to remove host MCP qualification, got %q", staged)
	}
}
