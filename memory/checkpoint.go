package memory

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"filippo.io/age"
)

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

// EncryptedSnapshot creates a consistent age-encrypted checkpoint for upload.
// Recipients are selected by the host; private identities never leave the device.
// The destination must not exist. Plaintext temporary files are removed on return.
func (s *SQLite) EncryptedSnapshot(ctx context.Context, destination string, recipients ...age.Recipient) (err error) {
	if len(recipients) == 0 {
		return fmt.Errorf("checkpoint requires at least one age recipient")
	}
	directory, err := os.MkdirTemp("", "agent-memory-checkpoint-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	snapshot := filepath.Join(directory, "memory.db")
	if err = s.Snapshot(ctx, snapshot); err != nil {
		return err
	}
	input, err := os.Open(snapshot)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		output.Close()
		if err != nil {
			os.Remove(destination)
		}
	}()
	writer, err := age.Encrypt(output, recipients...)
	if err != nil {
		return err
	}
	if _, err = io.Copy(writer, contextReader{ctx, input}); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	if err = output.Sync(); err != nil {
		return err
	}
	return output.Close()
}

// RestoreEncryptedCheckpoint authenticates and validates a checkpoint before
// creating a standalone SQLite file. It never overwrites an existing database.
// maxBytes bounds plaintext output; zero defaults to 1 GiB.
func RestoreEncryptedCheckpoint(ctx context.Context, input io.Reader, destination string, maxBytes int64, identities ...age.Identity) (err error) {
	if maxBytes == 0 {
		maxBytes = 1 << 30
	}
	if maxBytes < 1 || maxBytes >= 1<<62 {
		return fmt.Errorf("invalid checkpoint restore size limit")
	}
	if len(identities) == 0 {
		return fmt.Errorf("checkpoint restore requires an age identity")
	}
	reader, err := age.Decrypt(contextReader{ctx, input}, identities...)
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp(filepath.Dir(destination), ".agent-memory-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	temporary := filepath.Join(directory, "memory.db")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	written, err := io.Copy(file, io.LimitReader(contextReader{ctx, reader}, maxBytes+1))
	if err != nil {
		return err
	}
	if written > maxBytes {
		return fmt.Errorf("checkpoint exceeds restore size limit")
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	path, err := filepath.Abs(temporary)
	if err != nil {
		return err
	}
	uri := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", uri.String()+"?mode=ro")
	if err != nil {
		return err
	}
	var check string
	err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check)
	if err == nil && check != "ok" {
		err = fmt.Errorf("checkpoint SQLite integrity check failed")
	}
	var version int
	if err == nil {
		err = db.QueryRowContext(ctx, "SELECT version FROM memory_schema").Scan(&version)
	}
	if err == nil && (version < 1 || version > 6) {
		err = fmt.Errorf("unsupported checkpoint memory schema")
	}
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Linking a fully validated file atomically creates destination without clobber.
	// The temporary file is in the same directory/filesystem as the destination.
	return os.Link(temporary, destination)
}
