package agentcore

import "time"

const (
	MCPTransportStreamableHTTP = "streamable_http"
	MCPToolAccessRead          = "read"
	MCPToolAccessWrite         = "write"
)

// RunMCPServer is the encrypted, run-scoped MCP attachment persisted beside a
// run. EncryptedCredential is opaque outside the MCP credential codec.
type RunMCPServer struct {
	AppID               string
	RunID               string
	ServerID            string
	ServerName          string
	Transport           string
	URL                 string
	Tools               []RunMCPTool
	Skills              []SkillRef
	EncryptedCredential []byte
	CreatedAt           time.Time
}

type RunMCPTool struct {
	Name   string `json:"name"`
	Access string `json:"access"`
}
