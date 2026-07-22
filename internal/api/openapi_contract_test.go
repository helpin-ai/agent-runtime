package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

type openAPIContract struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Version string `yaml:"version"`
	} `yaml:"info"`
	Components struct {
		Schemas map[string]struct {
			Properties map[string]interface{} `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
	Paths map[string]map[string]interface{} `yaml:"paths"`
}

func TestOpenAPIContractCoversPublicRuntimeSurface(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	payload, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	var contract openAPIContract
	if err := yaml.Unmarshal(payload, &contract); err != nil {
		t.Fatalf("decode OpenAPI contract: %v", err)
	}
	if contract.OpenAPI != "3.1.0" || contract.Info.Version != "0.2.0" {
		t.Fatalf("unexpected OpenAPI metadata: version=%q api=%q", contract.Info.Version, contract.OpenAPI)
	}

	routes := map[string][]string{
		"/capabilities":                                {"get"},
		"/app-health":                                  {"get"},
		"/agents":                                      {"get", "post"},
		"/agents/{agent_id}":                           {"get", "put"},
		"/runs":                                        {"get", "post"},
		"/runs/search":                                 {"get"},
		"/runs/{run_id}":                               {"get"},
		"/runs/{run_id}/events":                        {"get"},
		"/runs/{run_id}/events/history":                {"get"},
		"/runs/{run_id}/execution":                     {"get"},
		"/runs/{run_id}/messages":                      {"get", "post"},
		"/runs/{run_id}/artifacts":                     {"get", "post"},
		"/runs/{run_id}/interactions":                  {"get"},
		"/runs/{run_id}/tool-calls":                    {"get"},
		"/runs/{run_id}/tools":                         {"get", "post"},
		"/runs/{run_id}/resume":                        {"post"},
		"/runs/{run_id}/approve":                       {"post"},
		"/runs/{run_id}/request-changes":               {"post"},
		"/runs/{run_id}/cancel":                        {"post"},
		"/runs/{run_id}/codex-auth/device-code/start":  {"post"},
		"/runs/{run_id}/codex-auth/device-code/cancel": {"post"},
	}
	for path, methods := range routes {
		operations, found := contract.Paths[path]
		if !found {
			t.Errorf("OpenAPI path %q is missing", path)
			continue
		}
		for _, method := range methods {
			if _, found := operations[method]; !found {
				t.Errorf("OpenAPI operation %s %s is missing", method, path)
			}
		}
	}

	fields := map[string][]string{
		"Agent":            {"created_at", "updated_at"},
		"AgentRun":         {"started_at", "completed_at", "created_at", "updated_at"},
		"Message":          {"runtime_message_id", "content_blocks", "tool_invocations", "created_at"},
		"Artifact":         {"created_at"},
		"Interaction":      {"resolved_by_external_id", "resolved_at", "created_at", "updated_at"},
		"ResumeRunRequest": {"resume_id", "interaction_id"},
		"RunEvent":         {"event_id", "sequence_no", "sent_at"},
		"RunExecutionInfo": {"execution_mode", "state", "workflow_id"},
		"RunPage":          {"items", "total", "limit", "offset"},
		"Capabilities":     {"runtime_kinds", "providers", "apps", "tools"},
	}
	for schemaName, expectedFields := range fields {
		schema, found := contract.Components.Schemas[schemaName]
		if !found {
			t.Errorf("OpenAPI schema %q is missing", schemaName)
			continue
		}
		for _, field := range expectedFields {
			if _, found := schema.Properties[field]; !found {
				t.Errorf("OpenAPI schema %s.%s is missing", schemaName, field)
			}
		}
	}
}
