package memory

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

type checkpointStore struct {
	metadata map[string]Checkpoint
	blobs    map[string][]byte
}

func (s *checkpointStore) Put(_ context.Context, c Checkpoint, input io.Reader) error {
	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	s.metadata[c.ID] = c
	s.blobs[c.ID] = data
	return nil
}
func (s *checkpointStore) Open(_ context.Context, id string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.blobs[id])), nil
}
func (s *checkpointStore) List(context.Context) ([]Checkpoint, error) {
	var result []Checkpoint
	for _, c := range s.metadata {
		result = append(result, c)
	}
	return result, nil
}
func (s *checkpointStore) Delete(_ context.Context, id string) error {
	delete(s.metadata, id)
	delete(s.blobs, id)
	return nil
}

func TestCheckpointStoreReceivesCiphertextAndPrunesForgottenHistory(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	s := openTest(t, testConfig(filepath.Join(directory, "memory.db")))
	retainTest(t, s, testScope, "doc", "Alice prefers Go")
	identity, _ := age.GenerateX25519Identity()
	store := &checkpointStore{metadata: map[string]Checkpoint{}, blobs: map[string][]byte{}}
	old, err := s.SaveCheckpoint(ctx, store, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(store.blobs[old.ID], []byte("Alice")) || old.CiphertextBytes == 0 || len(old.SHA256) != 64 {
		t.Fatal("invalid sealed upload")
	}
	if err = s.Forget(ctx, testScope, "doc"); err != nil {
		t.Fatal(err)
	}
	latest, err := s.SaveCheckpoint(ctx, store, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if err = PruneCheckpoints(ctx, store, 1); err != nil {
		t.Fatal(err)
	}
	if len(store.blobs) != 1 || store.blobs[old.ID] != nil || store.blobs[latest.ID] == nil {
		t.Fatal("retained forgotten checkpoint history")
	}
	restored := filepath.Join(directory, "restored.db")
	if err = RestoreCheckpoint(ctx, store, latest.ID, restored, 0, identity); err != nil {
		t.Fatal(err)
	}
	if result := recallTest(t, openTest(t, testConfig(restored)), testScope, "Go"); len(result.Results) != 0 {
		t.Fatal("cloud restore resurrected forgotten memory")
	}
}
