package appconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelCredentialCallbackCapabilityIsAppScopedAndSecretFree(t *testing.T) {
	t.Setenv("TEST_MODEL_CALLBACK_TOKEN", "callback-secret")
	cfg := &Config{Apps: []App{{AppID: "helpin", ModelCredentialCallback: &ModelCredentialCallback{URL: "https://helpin.example/private-refresh", TokenEnv: "TEST_MODEL_CALLBACK_TOKEN"}}, {AppID: "usermaven"}}}
	for _, app := range Summaries(cfg) {
		if app.AppID == "helpin" {
			if len(app.Components) != 1 || app.Components[0].Kind != "model_credentials" || !app.Components[0].Configured || !app.Components[0].AuthConfigured {
				t.Fatalf("callback capability: %+v", app)
			}
		} else if len(app.Components) != 0 {
			t.Fatal("another app inherited callback")
		}
		raw, err := json.Marshal(app)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "callback-secret") || strings.Contains(string(raw), "private-refresh") {
			t.Fatal("disclosed callback details")
		}
	}
	t.Setenv("TEST_MODEL_CALLBACK_TOKEN", "")
	if SummaryForApp(cfg, "helpin").Components[0].AuthConfigured {
		t.Fatal("unset callback token reported ready")
	}
}
