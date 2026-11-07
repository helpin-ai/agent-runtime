package workjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
)

type credentialStore interface {
	CompareAndSwapWorkJobWorker(context.Context, string, string, []byte, []byte) (bool, error)
}

func credentialVersion(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func credentialState(raw []byte, worker agentcore.WorkJobWorker) *sdk.WorkJobCredentialState {
	out := &sdk.WorkJobCredentialState{Version: credentialVersion(raw), MCPServerIDs: []string{}}
	for _, server := range worker.MCPServers {
		out.MCPServerIDs = append(out.MCPServerIDs, server.ServerID)
	}
	sort.Strings(out.MCPServerIDs)
	if worker.ModelCredential != nil {
		out.ModelConnectionID = worker.ModelCredential.ConnectionID
	}
	return out
}
func (s *Service) credentialSnapshot(ctx context.Context, appID, jobID string) ([]byte, agentcore.WorkJobWorker, error) {
	raw, err := s.store.GetWorkJobWorker(ctx, appID, jobID)
	if err != nil {
		return nil, agentcore.WorkJobWorker{}, err
	}
	var key []byte
	if s.cfg.Credentials != nil {
		key = s.cfg.Credentials.Key
	}
	worker, err := openWorker(key, appID, jobID, raw)
	return raw, worker, err
}
func (s *Service) CredentialState(ctx context.Context, appID, jobID string) (*sdk.WorkJobCredentialState, error) {
	if _, err := s.Get(ctx, appID, jobID); err != nil {
		return nil, err
	}
	raw, worker, err := s.credentialSnapshot(ctx, appID, jobID)
	if err != nil {
		return nil, err
	}
	return credentialState(raw, worker), nil
}
func (s *Service) UpdateCredentials(ctx context.Context, appID, jobID string, req sdk.UpdateWorkJobCredentialsRequest) (*sdk.WorkJobCredentialState, error) {
	job, err := s.Get(ctx, appID, jobID)
	if err != nil {
		return nil, err
	}
	if agentcore.IsTerminalWorkJobState(job.State) {
		return nil, invalid("job is terminal")
	}
	db, ok := s.store.(credentialStore)
	if !ok {
		return nil, ErrUnsupported
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, worker, err := s.credentialSnapshot(ctx, appID, jobID)
	if err != nil {
		return nil, err
	}
	if req.ExpectedVersion == "" || req.ExpectedVersion != credentialVersion(raw) {
		return nil, agentcore.ErrWorkJobRevisionConflict
	}
	for serverID, credential := range req.MCP {
		found := false
		for i := range worker.MCPServers {
			if worker.MCPServers[i].ServerID != serverID {
				continue
			}
			encoded, err := json.Marshal(credential)
			if err != nil {
				return nil, invalid("invalid MCP credential")
			}
			var value mcp.RunCredential
			if err = json.Unmarshal(encoded, &value); err != nil {
				return nil, invalid("invalid MCP credential")
			}
			if _, err = mcp.PrepareRotatedCredential(appID, jobID, serverID, value, s.cfg.RunMCP); err != nil {
				return nil, invalid("invalid MCP credential")
			}
			worker.MCPServers[i].Credential = &credential
			found = true
			break
		}
		if !found {
			return nil, invalid("MCP server is not attached to this job")
		}
	}
	if req.Model != nil {
		if worker.ModelCredential == nil || req.Model.ConnectionID != worker.ModelCredential.ConnectionID || req.Model.AccountID != worker.ModelCredential.AccountID {
			return nil, invalid("model credential identity cannot change")
		}
		provider := ""
		if worker.Model != nil {
			provider = worker.Model.Provider
		}
		if provider == "" {
			agent, err := s.store.GetAgent(ctx, appID, worker.AgentID)
			if err != nil {
				return nil, err
			}
			if agent != nil {
				provider = agent.Provider
			}
		}
		if err := modelauth.Validate(provider, *req.Model); err != nil {
			return nil, invalid("invalid model credential")
		}
		worker.ModelCredential = req.Model
	}
	if len(req.MCP) == 0 && req.Model == nil {
		return nil, invalid("a credential is required")
	}
	if s.cfg.Credentials == nil {
		return nil, errors.New("credential encryption is not configured")
	}
	next, err := sealWorker(s.cfg.Credentials.Key, appID, jobID, worker)
	if err != nil {
		return nil, err
	}
	changed, err := db.CompareAndSwapWorkJobWorker(ctx, appID, jobID, raw, next)
	if err != nil {
		return nil, err
	}
	if !changed {
		return nil, agentcore.ErrWorkJobRevisionConflict
	}
	s.Wake()
	return credentialState(next, worker), nil
}
