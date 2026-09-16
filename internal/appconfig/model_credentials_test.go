package appconfig

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestModelCallbackHTTPRequiresExactOperatorAllowance(t *testing.T) {
	for _, tc := range []struct {
		url     string
		enabled bool
		hosts   string
		want    bool
	}{
		{"https://helpin.example/refresh", false, "", true},
		{"http://localhost:8080/refresh", false, "", true},
		{"http://helpin-api:8080/refresh", false, "helpin-api:8080", false},
		{"http://helpin-api:8080/refresh", true, "", false},
		{"http://helpin-api:8080/refresh", true, "helpin-api:8080", true},
		{"http://helpin-api:9090/refresh", true, "helpin-api:8080", false},
		{"http://helpin-api.evil:8080/refresh", true, "helpin-api:8080", false},
		{"http://evil:8080/refresh", true, "*:8080", false},
		{"http://token@helpin-api:8080/refresh", true, "helpin-api:8080", false},
		{"http://helpin-api:8080/refresh?token=secret", true, "helpin-api:8080", false},
		{"http://helpin-api:8080/refresh?", true, "helpin-api:8080", false},
		{"http://helpin-api:8080/refresh#", true, "helpin-api:8080", false},
		{"http://169.254.169.254/refresh", true, "helpin-api:8080", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			if got := validModelCallbackURL(tc.url, tc.enabled, tc.hosts); got != tc.want {
				t.Fatalf("accepted=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestComposeCallbackStillRequiresToken(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	t.Setenv("AGENT_RUNTIME_MODEL_CALLBACK_ALLOW_HTTP", "true")
	t.Setenv("AGENT_RUNTIME_MODEL_CALLBACK_HTTP_HOSTS", "helpin-api:8080")
	t.Setenv("COMPOSE_TEST_TOKEN", "")
	cfg := &Config{Apps: []App{{AppID: "helpin", ModelCredentialCallback: &ModelCredentialCallback{URL: "http://helpin-api:8080/api/internal/agent-runtime/model-credentials/refresh", TokenEnv: "COMPOSE_TEST_TOKEN"}}}}
	if _, err := ModelCredentialManager(cfg, store.NewMemory()); err == nil {
		t.Fatal("missing token accepted")
	}
	t.Setenv("COMPOSE_TEST_TOKEN", "test-only-token")
	if _, err := ModelCredentialManager(cfg, store.NewMemory()); err != nil {
		t.Fatal(err)
	}
}
