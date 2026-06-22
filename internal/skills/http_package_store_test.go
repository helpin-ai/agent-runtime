package skills

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPPackageStoreFetchesEscapedObjectKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/objects/workspaces%2Fws-1%2Fskills%2Fskill-1%2Fskill.zip" {
			t.Fatalf("unexpected path %s", r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "Bearer pkg-token" {
			t.Fatalf("unexpected auth header %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte("zip-bytes"))
	}))
	defer server.Close()

	store := HTTPPackageStore{BaseURL: server.URL, Token: "pkg-token"}
	payload, err := store.GetObject(context.Background(), "workspaces/ws-1/skills/skill-1/skill.zip")
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if string(payload) != "zip-bytes" {
		t.Fatalf("unexpected payload %q", string(payload))
	}
}
