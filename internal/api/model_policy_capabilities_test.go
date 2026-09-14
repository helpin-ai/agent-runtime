package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/appconfig"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
)

func TestCapabilitiesCombineAppPolicyWithStartupProviderSnapshot(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	startup := runtime.NativeProviderCapabilities()
	cfg := &appconfig.Config{Apps: []appconfig.App{{AppID: "helpin", RequireRunModelCredentials: true}, {AppID: "usermaven"}}}
	s := &Server{cfg: Config{AppConfig: cfg, Capabilities: Capabilities{Providers: startup}}}
	t.Setenv("OPENAI_API_KEY", "rotated-key")
	response := httptest.NewRecorder()
	s.capabilities(response, httptest.NewRequest("GET", "/v1/capabilities?app_id=helpin", nil))
	var got capabilitiesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Apps) != 1 || !got.Apps[0].RequireRunModelCredentials {
		t.Fatalf("app policy=%+v", got.Apps)
	}
	for _, provider := range got.Providers {
		if provider.Name == "openai" && provider.Configured {
			t.Fatal("startup key snapshot unexpectedly refreshed")
		}
	}
	// Rebuilding the startup snapshot sees the rotated environment configuration.
	found := false
	for _, provider := range runtime.NativeProviderCapabilities() {
		if provider.Name == "openai" {
			found = provider.Configured
		}
	}
	if !found {
		t.Fatal("new startup snapshot did not see configured key")
	}
	response = httptest.NewRecorder()
	s.capabilities(response, httptest.NewRequest("GET", "/v1/capabilities?app_id=usermaven", nil))
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Apps) != 1 || got.Apps[0].RequireRunModelCredentials {
		t.Fatalf("default app policy=%+v", got.Apps)
	}
}
