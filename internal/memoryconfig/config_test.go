package memoryconfig

import "testing"

func TestOpenRouterCredentialFallbackIsEndpointRestricted(t *testing.T) {
	for _, test := range []struct{ url, explicit, want string }{
		{"https://openrouter.ai/api/v1", "", "router-key"},
		{"https://openrouter.ai/api/v1/", "", "router-key"},
		{"https://openrouter.ai/api/v1", "explicit-key", "explicit-key"},
		{"http://localhost:1234/v1", "", ""},
		{"https://openrouter.ai.attacker.test/api/v1", "", ""},
		{"https://openrouter.ai/api/v1?redirect=elsewhere", "", ""},
	} {
		values := map[string]string{"AGENT_RUNTIME_MEMORY_MODEL_URL": test.url, "AGENT_RUNTIME_MEMORY_MODEL_API_KEY": test.explicit, "OPENROUTER_API_KEY": "router-key"}
		if got := modelAPIKey(func(key string) string { return values[key] }); got != test.want {
			t.Errorf("endpoint %q: wrong credential selection", test.url)
		}
	}
}
