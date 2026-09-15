package appconfig

import "testing"

func TestCredentialPolicyIsAppScopedAndOptional(t *testing.T) {
	cfg := &Config{Apps: []App{{AppID: "helpin", RequireRunModelCredentials: true}, {AppID: "usermaven"}}}
	for _, tc := range []struct {
		app  string
		want bool
	}{{"helpin", true}, {"usermaven", false}, {"unknown", false}} {
		if got := RequiresRunModelCredentials(cfg, tc.app); got != tc.want {
			t.Fatalf("%s policy=%v", tc.app, got)
		}
	}
	if RequiresRunModelCredentials(nil, "helpin") {
		t.Fatal("nil configuration requires credentials")
	}
	if !SummaryForApp(cfg, "helpin").RequireRunModelCredentials || SummaryForApp(cfg, "usermaven").RequireRunModelCredentials {
		t.Fatal("capability policy differs")
	}
}
