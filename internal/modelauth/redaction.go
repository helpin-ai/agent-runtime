package modelauth

import (
	"context"
	"encoding/json"
	"strings"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/credentials"
)

// RedactReviewContext masks locally known run credentials without refreshing
// tokens or exposing them to the caller. Failure makes external review unsafe.
func (m *Manager) RedactReviewContext(ctx context.Context, appID, runID, value string) (string, error) {
	if m == nil {
		return value, nil
	}
	for _, callback := range m.Callbacks {
		if callback.Token != "" {
			value = redactKnownSecret(value, callback.Token)
		}
	}
	if m.Store == nil {
		return value, nil
	}
	record, err := m.Store.GetRunModelCredential(ctx, appID, runID)
	if err != nil {
		return "", err
	}
	if record == nil || record.Revoked || len(record.EncryptedCredential) == 0 {
		return value, nil
	}
	raw, err := credentials.Open(m.Key, aad(record), record.EncryptedCredential)
	if err != nil {
		return "", err
	}
	var credential sdk.ModelCredential
	if err := json.Unmarshal(raw, &credential); err != nil {
		return "", err
	}
	for _, secret := range []string{credential.APIKey, credential.AccessToken} {
		if secret != "" {
			value = redactKnownSecret(value, secret)
		}
	}
	return value, nil
}

func redactKnownSecret(value, secret string) string {
	value = strings.ReplaceAll(value, secret, "[REDACTED]")
	encoded, _ := json.Marshal(secret)
	return strings.ReplaceAll(value, string(encoded[1:len(encoded)-1]), "[REDACTED]")
}
