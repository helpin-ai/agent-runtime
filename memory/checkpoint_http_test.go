package memory

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

type testCheckpointAuthority struct {
	url        string
	checkpoint Checkpoint
	deleted    []string
}

func (a *testCheckpointAuthority) UploadURL(_ context.Context, c Checkpoint) (CheckpointURL, error) {
	a.checkpoint = c
	return CheckpointURL{URL: a.url + "?signature=private-test", Headers: http.Header{"X-Checkpoint": []string{c.ID}}}, nil
}
func (a *testCheckpointAuthority) DownloadURL(context.Context, string) (CheckpointURL, error) {
	return CheckpointURL{URL: a.url + "?signature=private-test"}, nil
}
func (a *testCheckpointAuthority) List(context.Context) ([]Checkpoint, error) {
	return []Checkpoint{a.checkpoint}, nil
}
func (a *testCheckpointAuthority) Delete(_ context.Context, id string) error {
	a.deleted = append(a.deleted, id)
	return nil
}

func TestPresignedCheckpointTransferOnlyUploadsCiphertext(t *testing.T) {
	var stored []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("signature") != "private-test" {
			t.Error("signed query missing")
		}
		switch r.Method {
		case http.MethodPut:
			var err error
			stored, err = io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if r.ContentLength != int64(len(stored)) {
				t.Error("wrong upload length")
			}
			if r.Header.Get("X-Checkpoint") == "" {
				t.Error("signed headers missing")
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Write(stored)
		default:
			t.Error("unexpected transfer method")
		}
	}))
	defer server.Close()
	authority := &testCheckpointAuthority{url: server.URL}
	store, err := NewPresignedCheckpointStore(authority, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "live.db")))
	retainTest(t, s, testScope, "source", "Alice prefers jasmine tea")
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := s.SaveCheckpoint(ctx, store, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("Alice")) || !bytes.HasPrefix(stored, []byte("age-encryption.org/v1")) {
		t.Fatal("cloud received plaintext or invalid age ciphertext")
	}
	dest := filepath.Join(t.TempDir(), "restored.db")
	if err = RestoreCheckpoint(ctx, store, checkpoint.ID, dest, 0, identity); err != nil {
		t.Fatal(err)
	}
	if r := recallTest(t, openTest(t, testConfig(dest)), testScope, "tea"); len(r.Results) != 1 {
		t.Fatal("signed download failed to restore source")
	}
	if err = store.Delete(ctx, checkpoint.ID); err != nil {
		t.Fatal(err)
	}
	if len(authority.deleted) != 1 {
		t.Fatal("deletion not delegated to host authority")
	}
}

func TestPresignedCheckpointRejectsRedirectsWithoutLeakingURL(t *testing.T) {
	requests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"?secret=private-test", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	store, err := NewPresignedCheckpointStore(&testCheckpointAuthority{url: redirect.URL}, redirect.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Open(context.Background(), "00112233445566778899aabbccddeeff")
	if err == nil || strings.Contains(err.Error(), "private-test") || strings.Contains(err.Error(), "http://") || requests != 0 {
		t.Fatalf("redirect/credential handling: %v requests=%d", err, requests)
	}
}
