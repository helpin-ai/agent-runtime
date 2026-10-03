package memory

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// CheckpointURL carries an opaque host-issued signed PUT/GET URL. Never persist
// or log these URLs: they may contain temporary cloud access credentials.
type CheckpointURL struct {
	URL     string
	Headers http.Header
}

// CheckpointAuthority authorizes one profile's storage operations. Implement it
// using the desktop host's authenticated backup API or cloud SDK. Signed URLs
// work with S3-compatible storage without embedding cloud credentials here.
// List/Delete include the host's checkpoint index and object-version policy.
type CheckpointAuthority interface {
	UploadURL(context.Context, Checkpoint) (CheckpointURL, error)
	DownloadURL(context.Context, string) (CheckpointURL, error)
	List(context.Context) ([]Checkpoint, error)
	Delete(context.Context, string) error
}

type PresignedCheckpointStore struct {
	authority CheckpointAuthority
	client    *http.Client
}

func NewPresignedCheckpointStore(authority CheckpointAuthority, client *http.Client) (*PresignedCheckpointStore, error) {
	if authority == nil {
		return nil, fmt.Errorf("checkpoint authority required")
	}
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &PresignedCheckpointStore{authority, &copyClient}, nil
}

func checkpointRequest(ctx context.Context, method string, target CheckpointURL, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(target.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("invalid checkpoint URL")
	}
	req, err := http.NewRequestWithContext(ctx, method, target.URL, body)
	if err != nil {
		return nil, fmt.Errorf("cannot create checkpoint request")
	}
	req.Header = target.Headers.Clone()
	return req, nil
}

func (s *PresignedCheckpointStore) Put(ctx context.Context, checkpoint Checkpoint, body io.Reader) error {
	if body == nil || !validCheckpointID(checkpoint.ID) || checkpoint.CiphertextBytes <= 0 || checkpoint.CreatedAt.IsZero() {
		return fmt.Errorf("invalid checkpoint metadata")
	}
	target, err := s.authority.UploadURL(ctx, checkpoint)
	if err != nil {
		return err
	}
	req, err := checkpointRequest(ctx, http.MethodPut, target, contextReader{ctx, body})
	if err != nil {
		return err
	}
	req.ContentLength = checkpoint.CiphertextBytes
	response, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("checkpoint upload transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("checkpoint upload returned HTTP %d", response.StatusCode)
	}
	return nil
}
func (s *PresignedCheckpointStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if !validCheckpointID(id) {
		return nil, fmt.Errorf("invalid checkpoint ID")
	}
	target, err := s.authority.DownloadURL(ctx, id)
	if err != nil {
		return nil, err
	}
	req, err := checkpointRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("checkpoint download transport failed")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("checkpoint download returned HTTP %d", response.StatusCode)
	}
	return response.Body, nil
}
func (s *PresignedCheckpointStore) List(ctx context.Context) ([]Checkpoint, error) {
	return s.authority.List(ctx)
}
func (s *PresignedCheckpointStore) Delete(ctx context.Context, id string) error {
	if !validCheckpointID(id) {
		return fmt.Errorf("invalid checkpoint ID")
	}
	return s.authority.Delete(ctx, id)
}
