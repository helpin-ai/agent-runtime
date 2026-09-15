package agentcore

import "context"

// RunModelCredential is never part of the public run representation.
type RunModelCredential struct {
	AppID               string `gorm:"primaryKey"`
	RunID               string `gorm:"primaryKey"`
	Provider            string
	Kind                string
	ConnectionID        string
	AccountID           string
	EncryptedCredential []byte
	Version             int64
	Revoked             bool
}

func (RunModelCredential) TableName() string { return "agent_run_model_credentials" }

type ModelCredentialStore interface {
	CreateRunWithModelCredential(context.Context, *AgentRun, []RunMCPServer, *RunModelCredential) error
	GetRunModelCredential(context.Context, string, string) (*RunModelCredential, error)
	ReplaceRunModelCredential(context.Context, *RunModelCredential, int64) (bool, error)
	ClearRunModelCredential(context.Context, string, string) error
}
