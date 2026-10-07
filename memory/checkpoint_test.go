package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestEncryptedCheckpointRoundTripAndForget(t *testing.T) {
	directory := t.TempDir()
	s := openTest(t, testConfig(filepath.Join(directory, "live.db")))
	retainTest(t, s, testScope, "private", "Alice prefers Go")
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	encrypted := filepath.Join(directory, "checkpoint.age")
	if err = s.EncryptedSnapshot(context.Background(), encrypted, identity.Recipient()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("Alice")) {
		t.Fatal("plaintext memory present in ciphertext")
	}
	restored := filepath.Join(directory, "restored.db")
	if err = RestoreEncryptedCheckpoint(context.Background(), bytes.NewReader(data), restored, 0, identity); err != nil {
		t.Fatal(err)
	}
	opened := openTest(t, testConfig(restored))
	if result := recallTest(t, opened, testScope, "Go"); len(result.Results) != 1 {
		t.Fatal("restored checkpoint lost facts")
	}
	if err = s.Forget(context.Background(), testScope, "private"); err != nil {
		t.Fatal(err)
	}
	after := filepath.Join(directory, "forgotten.age")
	if err = s.EncryptedSnapshot(context.Background(), after, identity.Recipient()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(after)
	forgotten := filepath.Join(directory, "forgotten.db")
	if err = RestoreEncryptedCheckpoint(context.Background(), bytes.NewReader(data), forgotten, 0, identity); err != nil {
		t.Fatal(err)
	}
	if result := recallTest(t, openTest(t, testConfig(forgotten)), testScope, "Go"); len(result.Results) != 0 {
		t.Fatal("forgotten facts restored from current snapshot")
	}
	if err = RestoreEncryptedCheckpoint(context.Background(), bytes.NewReader(data), restored, 0, identity); err == nil {
		t.Fatal("overwrote existing database")
	}
}

func TestCheckpointCancellationAndEncryptedNonDatabaseLeaveNoDestination(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext bytes.Buffer
	writer, err := age.Encrypt(&ciphertext, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte("authenticated ciphertext containing ordinary text, not SQLite")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "invalid.db")
	if err = RestoreEncryptedCheckpoint(context.Background(), bytes.NewReader(ciphertext.Bytes()), dest, 0, identity); err == nil {
		t.Fatal("accepted non-database plaintext")
	}
	if _, err = os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("invalid database exposed destination")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = RestoreEncryptedCheckpoint(ctx, bytes.NewReader(ciphertext.Bytes()), dest, 0, identity); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled restore: %v", err)
	}
	if files, err := os.ReadDir(dir); err != nil || len(files) != 0 {
		t.Fatalf("restore left temporary plaintext: %v %v", files, err)
	}
}

func TestCheckpointRejectsWrongKeyTamperingTruncationAndSize(t *testing.T) {
	directory := t.TempDir()
	s := openTest(t, testConfig(filepath.Join(directory, "live.db")))
	retainTest(t, s, testScope, "doc", "Alice prefers Go")
	identity, _ := age.GenerateX25519Identity()
	other, _ := age.GenerateX25519Identity()
	path := filepath.Join(directory, "checkpoint.age")
	if err := s.EncryptedSnapshot(context.Background(), path, identity.Recipient()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	corrupted := append([]byte(nil), data...)
	corrupted[len(corrupted)-1] ^= 1
	for name, input := range map[string][]byte{"wrong-key": data, "tampered": corrupted, "truncated": data[:len(data)-8], "oversized": data} {
		t.Run(name, func(t *testing.T) {
			key := identity
			limit := int64(0)
			if name == "wrong-key" {
				key = other
			}
			if name == "oversized" {
				limit = 1
			}
			dest := filepath.Join(directory, name+".db")
			if err := RestoreEncryptedCheckpoint(context.Background(), bytes.NewReader(input), dest, limit, key); err == nil {
				t.Fatal("invalid checkpoint accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("failed restore exposed destination")
			}
		})
	}
}
