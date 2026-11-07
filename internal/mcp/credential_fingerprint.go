package mcp

import (
	"crypto/sha256"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// CredentialFingerprint identifies a rejected bearer token without disclosing it.
// Expired or unreadable credentials have no fingerprint and still require recovery.
func CredentialFingerprint(key []byte, server agentcore.RunMCPServer) string {
	value, err := decryptCredential(key, server)
	if err != nil || value == nil || value.Type != CredentialBearerToken {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value.AccessToken)))
}
