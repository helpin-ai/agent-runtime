package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"filippo.io/age"
)

type Checkpoint struct {
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	CiphertextBytes int64     `json:"ciphertext_bytes"`
	SHA256          string    `json:"sha256"`
}

// CheckpointStore is selected and authorized by the desktop host for one profile.
// Implementations upload opaque encrypted bytes. Delete must remove the named
// checkpoint's historical object versions too when used for forgetting history.
type CheckpointStore interface {
	Put(context.Context, Checkpoint, io.Reader) error
	Open(context.Context, string) (io.ReadCloser, error)
	List(context.Context) ([]Checkpoint, error)
	Delete(context.Context, string) error
}

// SaveCheckpoint encrypts locally before the store receives any bytes.
// It returns the checkpoint identity even when upload fails, for reconciliation.
func (s *SQLite) SaveCheckpoint(ctx context.Context, store CheckpointStore, recipients ...age.Recipient) (checkpoint Checkpoint, err error) {
	if store == nil {
		return checkpoint, fmt.Errorf("checkpoint store is required")
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return checkpoint, err
	}
	checkpoint.ID = hex.EncodeToString(random)
	checkpoint.CreatedAt = time.Now().UTC()
	directory, err := os.MkdirTemp("", "agent-memory-upload-")
	if err != nil {
		return checkpoint, err
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "memory.age")
	if err = s.EncryptedSnapshot(ctx, path, recipients...); err != nil {
		return checkpoint, err
	}
	file, err := os.Open(path)
	if err != nil {
		return checkpoint, err
	}
	defer file.Close()
	digest := sha256.New()
	checkpoint.CiphertextBytes, err = io.Copy(digest, contextReader{ctx, file})
	if err != nil {
		return checkpoint, err
	}
	checkpoint.SHA256 = hex.EncodeToString(digest.Sum(nil))
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return checkpoint, err
	}
	err = store.Put(ctx, checkpoint, contextReader{ctx, file})
	return checkpoint, err
}

func RestoreCheckpoint(ctx context.Context, store CheckpointStore, checkpointID, destination string, maxBytes int64, identities ...age.Identity) error {
	if store == nil || !validCheckpointID(checkpointID) {
		return fmt.Errorf("checkpoint store and valid ID are required")
	}
	reader, err := store.Open(ctx, checkpointID)
	if err != nil {
		return err
	}
	defer reader.Close()
	return RestoreEncryptedCheckpoint(ctx, reader, destination, maxBytes, identities...)
}

func validCheckpointID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// PruneCheckpoints retains the newest keep checkpoints. Call with keep=1 after
// a successful post-forget upload to remove older snapshots containing forgotten
// facts. Provider implementations must actually purge object versions.
func PruneCheckpoints(ctx context.Context, store CheckpointStore, keep int) error {
	if store == nil || keep < 1 {
		return fmt.Errorf("checkpoint store and positive retention count are required")
	}
	checkpoints, err := store.List(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, checkpoint := range checkpoints {
		if !validCheckpointID(checkpoint.ID) || checkpoint.CreatedAt.IsZero() || seen[checkpoint.ID] {
			return fmt.Errorf("invalid checkpoint listing")
		}
		seen[checkpoint.ID] = true
	}
	sort.Slice(checkpoints, func(i, j int) bool {
		if checkpoints[i].CreatedAt.Equal(checkpoints[j].CreatedAt) {
			return checkpoints[i].ID > checkpoints[j].ID
		}
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})
	var failures []error
	for _, checkpoint := range checkpoints[min(keep, len(checkpoints)):] {
		if err = ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err = store.Delete(ctx, checkpoint.ID); err != nil {
			failures = append(failures, fmt.Errorf("delete checkpoint %s: %w", checkpoint.ID, err))
		}
	}
	return errors.Join(failures...)
}
