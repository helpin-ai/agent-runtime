package mcp

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const (
	CredentialBearerToken = "bearer_token"
	CredentialHeaders     = "headers"
)

const credentialEnvelopeVersion byte = 1

const (
	maxRunMCPServers     = 16
	maxRunMCPTools       = 128
	maxCredentialHeaders = 16
	maxCredentialBytes   = 64 << 10
	maxMCPResponseBytes  = 16 << 20
)

var (
	safeToolComponent = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
	validServerID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	validServerName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	validRemoteTool   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	validHeaderName   = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

type RunServerRequest struct {
	ServerID   string         `json:"server_id"`
	ServerName string         `json:"server_name"`
	Transport  string         `json:"transport"`
	URL        string         `json:"url"`
	Tools      []RunTool      `json:"tools"`
	Credential *RunCredential `json:"credential,omitempty"`
}

type RunTool struct {
	Name   string `json:"name"`
	Access string `json:"access"`
}

type RunCredential struct {
	Type        string            `json:"type"`
	AccessToken string            `json:"access_token,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
}

type RunConfig struct {
	CredentialKey       []byte
	AllowHTTP           bool
	AllowPrivateNetwork bool
	AllowedHosts        []string
	ConnectTimeout      time.Duration
}

func RunConfigFromEnv(getenv func(string) string) (RunConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	key, err := ParseCredentialEncryptionKey(getenv("AGENT_RUNTIME_MCP_CREDENTIAL_ENCRYPTION_KEY"))
	if err != nil {
		return RunConfig{}, err
	}
	cfg := RunConfig{
		CredentialKey:       key,
		AllowHTTP:           truthy(getenv("AGENT_RUNTIME_MCP_ALLOW_HTTP")),
		AllowPrivateNetwork: truthy(getenv("AGENT_RUNTIME_MCP_ALLOW_PRIVATE_NETWORKS")),
		ConnectTimeout:      30 * time.Second,
	}
	for _, host := range strings.Split(getenv("AGENT_RUNTIME_MCP_ALLOWED_HOSTS"), ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			cfg.AllowedHosts = append(cfg.AllowedHosts, host)
		}
	}
	return cfg, nil
}

func ParseCredentialEncryptionKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	if len(value) == 32 {
		return []byte(value), nil
	}
	return nil, fmt.Errorf("AGENT_RUNTIME_MCP_CREDENTIAL_ENCRYPTION_KEY must be 32 raw bytes or base64-encoded 32 bytes")
}

func PrepareStoredServers(appID, runID string, requests []RunServerRequest, cfg RunConfig) ([]agentcore.RunMCPServer, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	if len(requests) > maxRunMCPServers {
		return nil, fmt.Errorf("mcp_servers cannot contain more than %d servers", maxRunMCPServers)
	}
	seenServers := map[string]bool{}
	seenAliases := map[string]bool{}
	out := make([]agentcore.RunMCPServer, 0, len(requests))
	for i := range requests {
		req := requests[i]
		req.ServerID = strings.TrimSpace(req.ServerID)
		req.ServerName = strings.TrimSpace(req.ServerName)
		req.Transport = strings.TrimSpace(req.Transport)
		if req.ServerID == "" || req.ServerName == "" {
			return nil, fmt.Errorf("mcp_servers[%d].server_id and server_name are required", i)
		}
		if !validServerID.MatchString(req.ServerID) || !validServerName.MatchString(req.ServerName) {
			return nil, fmt.Errorf("mcp_servers[%d].server_id or server_name contains unsupported characters", i)
		}
		if seenServers[req.ServerID] {
			return nil, fmt.Errorf("mcp_servers[%d].server_id %q is duplicated", i, req.ServerID)
		}
		seenServers[req.ServerID] = true
		if req.Transport != agentcore.MCPTransportStreamableHTTP {
			return nil, fmt.Errorf("mcp_servers[%d].transport must be %q", i, agentcore.MCPTransportStreamableHTTP)
		}
		parsed, err := validateRunServerURL(req.URL, cfg)
		if err != nil {
			return nil, fmt.Errorf("mcp_servers[%d].url: %w", i, err)
		}
		if len(req.Tools) == 0 {
			return nil, fmt.Errorf("mcp_servers[%d].tools must contain at least one explicitly allowed tool", i)
		}
		if len(req.Tools) > maxRunMCPTools {
			return nil, fmt.Errorf("mcp_servers[%d].tools cannot contain more than %d tools", i, maxRunMCPTools)
		}
		storedTools := make([]agentcore.RunMCPTool, 0, len(req.Tools))
		seenTools := map[string]bool{}
		for j := range req.Tools {
			name := strings.TrimSpace(req.Tools[j].Name)
			access := strings.TrimSpace(req.Tools[j].Access)
			if name == "" {
				return nil, fmt.Errorf("mcp_servers[%d].tools[%d].name is required", i, j)
			}
			if !validRemoteTool.MatchString(name) {
				return nil, fmt.Errorf("mcp_servers[%d].tools[%d].name is not a valid MCP tool name", i, j)
			}
			if seenTools[name] {
				return nil, fmt.Errorf("mcp_servers[%d].tools[%d].name %q is duplicated", i, j, name)
			}
			seenTools[name] = true
			if access != agentcore.MCPToolAccessRead && access != agentcore.MCPToolAccessWrite {
				return nil, fmt.Errorf("mcp_servers[%d].tools[%d].access must be read or write", i, j)
			}
			alias := RunToolAlias(req.ServerName, name)
			if alias == "" || seenAliases[alias] {
				return nil, fmt.Errorf("mcp_servers[%d].tools[%d] produces duplicate or invalid tool alias %q", i, j, alias)
			}
			seenAliases[alias] = true
			storedTools = append(storedTools, agentcore.RunMCPTool{Name: name, Access: access})
		}
		encrypted, err := encryptCredential(cfg.CredentialKey, appID, runID, req.ServerID, req.Credential)
		if err != nil {
			return nil, fmt.Errorf("mcp_servers[%d].credential: %w", i, err)
		}
		out = append(out, agentcore.RunMCPServer{
			AppID: appID, RunID: runID, ServerID: req.ServerID, ServerName: req.ServerName,
			Transport: req.Transport, URL: parsed.String(), Tools: storedTools, EncryptedCredential: encrypted,
		})
	}
	return out, nil
}

func RunToolAlias(serverName, toolName string) string {
	server := strings.Trim(safeToolComponent.ReplaceAllString(strings.TrimSpace(serverName), "_"), "_")
	tool := strings.Trim(safeToolComponent.ReplaceAllString(strings.TrimSpace(toolName), "_"), "_")
	if server == "" || tool == "" {
		return ""
	}
	return "mcp__" + server + "__" + tool
}

func encryptCredential(key []byte, appID, runID, serverID string, credential *RunCredential) ([]byte, error) {
	if credential == nil {
		return nil, nil
	}
	credential.Type = strings.TrimSpace(credential.Type)
	switch credential.Type {
	case CredentialBearerToken:
		if strings.TrimSpace(credential.AccessToken) == "" || len(credential.Headers) > 0 {
			return nil, fmt.Errorf("bearer_token requires access_token and does not accept headers")
		}
		if len(credential.AccessToken) > maxCredentialBytes {
			return nil, fmt.Errorf("access_token cannot exceed %d bytes", maxCredentialBytes)
		}
	case CredentialHeaders:
		if credential.AccessToken != "" || len(credential.Headers) == 0 {
			return nil, fmt.Errorf("headers requires at least one header and does not accept access_token")
		}
		if len(credential.Headers) > maxCredentialHeaders {
			return nil, fmt.Errorf("headers cannot contain more than %d entries", maxCredentialHeaders)
		}
		headerBytes := 0
		for name, value := range credential.Headers {
			if err := validateCredentialHeader(name, value); err != nil {
				return nil, err
			}
			headerBytes += len(name) + len(value)
		}
		if headerBytes > maxCredentialBytes {
			return nil, fmt.Errorf("credential headers cannot exceed %d bytes", maxCredentialBytes)
		}
	default:
		return nil, fmt.Errorf("type must be bearer_token or headers")
	}
	if credential.ExpiresAt != nil && !credential.ExpiresAt.After(time.Now().UTC()) {
		return nil, fmt.Errorf("expires_at must be in the future")
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("credential encryption is not configured")
	}
	plaintext, err := json.Marshal(credential)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	aad := []byte(appID + "\x00" + runID + "\x00" + serverID)
	sealed := gcm.Seal(nil, nonce, plaintext, aad)
	return append(append([]byte{credentialEnvelopeVersion}, nonce...), sealed...), nil
}

func decryptCredential(key []byte, server agentcore.RunMCPServer) (*RunCredential, error) {
	if len(server.EncryptedCredential) == 0 {
		return nil, nil
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("credential encryption is not configured")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	payload := server.EncryptedCredential
	if payload[0] != credentialEnvelopeVersion || len(payload) < 1+gcm.NonceSize()+gcm.Overhead() {
		return nil, fmt.Errorf("unsupported or truncated credential envelope")
	}
	aad := []byte(server.AppID + "\x00" + server.RunID + "\x00" + server.ServerID)
	plaintext, err := gcm.Open(nil, payload[1:1+gcm.NonceSize()], payload[1+gcm.NonceSize():], aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential: %w", err)
	}
	var credential RunCredential
	if err := json.Unmarshal(plaintext, &credential); err != nil {
		return nil, err
	}
	if credential.ExpiresAt != nil && !credential.ExpiresAt.After(time.Now().UTC()) {
		return nil, fmt.Errorf("credential expired at %s", credential.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return &credential, nil
}

func validateCredentialHeader(name, value string) error {
	name = strings.TrimSpace(name)
	if !validHeaderName.MatchString(name) || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("credential headers require non-empty names and single-line values")
	}
	name = http.CanonicalHeaderKey(name)
	switch name {
	case "Host", "Content-Length", "Connection", "Transfer-Encoding", "Mcp-Session-Id", "Mcp-Protocol-Version", "Origin":
		return fmt.Errorf("credential header %q is reserved", name)
	}
	return nil
}

func validateRunServerURL(raw string, cfg RunConfig) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("must be an absolute URL")
	}
	if len(raw) > 2048 {
		return nil, fmt.Errorf("cannot exceed 2048 characters")
	}
	if parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return nil, fmt.Errorf("userinfo, query parameters, and fragments are not allowed")
	}
	if parsed.Scheme != "https" && !(cfg.AllowHTTP && parsed.Scheme == "http") {
		return nil, fmt.Errorf("must use https (http requires AGENT_RUNTIME_MCP_ALLOW_HTTP=true)")
	}
	if !hostAllowed(parsed.Hostname(), cfg.AllowedHosts) {
		return nil, fmt.Errorf("host %q is not in AGENT_RUNTIME_MCP_ALLOWED_HOSTS", parsed.Hostname())
	}
	return parsed, nil
}

func hostAllowed(host string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pattern := range allowed {
		pattern = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(pattern), "."))
		if host == pattern || (strings.HasPrefix(pattern, "*.") && strings.HasSuffix(host, pattern[1:]) && host != pattern[2:]) {
			return true
		}
	}
	return false
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

type connectedRunProvider struct {
	session *protocol.ClientSession
}

func connectRunProvider(ctx context.Context, server agentcore.RunMCPServer, credential *RunCredential, cfg RunConfig) (*connectedRunProvider, error) {
	if server.Transport != agentcore.MCPTransportStreamableHTTP {
		return nil, fmt.Errorf("unsupported stored MCP transport %q", server.Transport)
	}
	parsed, err := validateRunServerURL(server.URL, cfg)
	if err != nil {
		return nil, fmt.Errorf("stored MCP URL is no longer allowed: %w", err)
	}
	headers := http.Header{}
	if credential != nil {
		switch credential.Type {
		case CredentialBearerToken:
			headers.Set("Authorization", "Bearer "+credential.AccessToken)
		case CredentialHeaders:
			for name, value := range credential.Headers {
				headers.Set(name, value)
			}
		}
	}
	httpClient := safeRunHTTPClient(parsed.String(), headers, cfg)
	client := protocol.NewClient(&protocol.Implementation{Name: "agent-runtime", Version: "0.3.0"}, nil)
	session, err := client.Connect(ctx, &protocol.StreamableClientTransport{
		Endpoint: parsed.String(), HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return nil, err
	}
	return &connectedRunProvider{session: session}, nil
}

func (p *connectedRunProvider) Close() error {
	if p == nil || p.session == nil {
		return nil
	}
	return p.session.Close()
}

func safeRunHTTPClient(endpoint string, headers http.Header, cfg RunConfig) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A process-level HTTP proxy could resolve a denied private destination on
	// the runtime's behalf and bypass the direct-dial SSRF checks below.
	transport.Proxy = nil
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 30 * time.Second
	}
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			if !cfg.AllowPrivateNetwork && isPrivateAddress(address.IP) {
				continue
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
		}
		return nil, fmt.Errorf("MCP host %q resolves only to private or local addresses", host)
	}
	baseHost := ""
	if parsed, err := url.Parse(endpoint); err == nil {
		baseHost = parsed.Host
	}
	return &http.Client{
		Transport: boundedResponseRoundTripper{base: headerRoundTripper{base: transport, headers: headers}, maxBytes: maxMCPResponseBytes},
		Timeout:   5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !strings.EqualFold(req.URL.Host, baseHost) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers http.Header
}

func (t headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for name, values := range t.headers {
		if len(values) > 0 {
			clone.Header.Set(name, values[len(values)-1])
		}
	}
	return t.base.RoundTrip(clone)
}

type boundedResponseRoundTripper struct {
	base     http.RoundTripper
	maxBytes int64
}

func (t boundedResponseRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.LimitReader(resp.Body, t.maxBytes+1), Closer: resp.Body}
	return resp, nil
}

func isPrivateAddress(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// PrepareRunTools connects the run's MCP servers, verifies the app-authorized
// tools exist, and registers an isolated tool overlay. The returned close
// function must remain deferred through adapter execution.
func PrepareRunTools(ctx context.Context, store agentcore.Store, base *tools.Registry, appID, runID string, cfg RunConfig) (*tools.Registry, map[string]bool, func(), error) {
	if store == nil {
		return nil, nil, func() {}, fmt.Errorf("run store is not configured")
	}
	if base == nil {
		return nil, nil, func() {}, fmt.Errorf("tool registry is not configured")
	}
	servers, err := store.ListRunMCPServers(ctx, appID, runID)
	if err != nil {
		return nil, nil, func() {}, err
	}
	overlay := base.CloneForApp(appID)
	allowed := map[string]bool{}
	providers := make([]*connectedRunProvider, 0, len(servers))
	closeAll := func() {
		for _, provider := range providers {
			_ = provider.Close()
		}
	}
	for _, server := range servers {
		credential, err := decryptCredential(cfg.CredentialKey, server)
		if err != nil {
			closeAll()
			return nil, nil, func() {}, fmt.Errorf("MCP server %q: %w", server.ServerName, err)
		}
		provider, err := connectRunProvider(ctx, server, credential, cfg)
		if err != nil {
			closeAll()
			return nil, nil, func() {}, fmt.Errorf("connect MCP server %q: %w", server.ServerName, err)
		}
		providers = append(providers, provider)
		available := map[string]*protocol.Tool{}
		for remoteTool, listErr := range provider.session.Tools(ctx, nil) {
			if listErr != nil {
				closeAll()
				return nil, nil, func() {}, fmt.Errorf("list MCP tools from %q: %w", server.ServerName, listErr)
			}
			available[remoteTool.Name] = remoteTool
		}
		for _, policy := range server.Tools {
			remoteTool := available[policy.Name]
			if remoteTool == nil {
				closeAll()
				return nil, nil, func() {}, fmt.Errorf("MCP server %q does not expose authorized tool %q", server.ServerName, policy.Name)
			}
			alias := RunToolAlias(server.ServerName, policy.Name)
			if _, exists := overlay.Definition(alias); exists {
				closeAll()
				return nil, nil, func() {}, fmt.Errorf("run MCP tool alias %q conflicts with an existing tool", alias)
			}
			schema, err := normalizeRemoteInputSchema(remoteTool.InputSchema)
			if err != nil {
				closeAll()
				return nil, nil, func() {}, fmt.Errorf("MCP server %q tool %q: %w", server.ServerName, policy.Name, err)
			}
			remoteName := policy.Name
			boundProvider := provider
			overlay.Register(tools.Definition{
				Name: alias, Description: remoteTool.Description, Category: "MCP / " + server.ServerName,
				InputSchema: schema, Mutating: policy.Access == agentcore.MCPToolAccessWrite,
			}, func(callCtx context.Context, _ tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
				var arguments map[string]any
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				if err := json.Unmarshal(input, &arguments); err != nil {
					return nil, fmt.Errorf("invalid MCP tool input: %w", err)
				}
				if arguments == nil {
					arguments = map[string]any{}
				}
				result, err := boundProvider.session.CallTool(callCtx, &protocol.CallToolParams{Name: remoteName, Arguments: arguments})
				if err != nil {
					return nil, err
				}
				parts := make([]string, 0, len(result.Content))
				for _, content := range result.Content {
					if text, ok := content.(*protocol.TextContent); ok {
						parts = append(parts, text.Text)
					} else if data, marshalErr := json.Marshal(content); marshalErr == nil {
						parts = append(parts, string(data))
					}
				}
				if len(parts) == 0 && result.StructuredContent != nil {
					data, _ := json.Marshal(result.StructuredContent)
					parts = append(parts, string(data))
				}
				text := strings.TrimSpace(strings.Join(parts, "\n"))
				if result.IsError {
					if text == "" {
						text = "MCP tool returned an error"
					}
					return nil, fmt.Errorf("%s", text)
				}
				if text == "" {
					return json.RawMessage(`{}`), nil
				}
				return json.Marshal(map[string]string{"text": text})
			})
			allowed[alias] = true
		}
	}
	aliases := make([]string, 0, len(allowed))
	for alias := range allowed {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return overlay, allowed, closeAll, nil
}

func normalizeRemoteInputSchema(input any) (map[string]any, error) {
	if input == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}, nil
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("input schema is not valid JSON: %w", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(payload, &schema); err != nil {
		return nil, fmt.Errorf("input schema is not an object: %w", err)
	}
	if schema["type"] != "object" {
		return nil, fmt.Errorf("input schema type must be object")
	}
	return schema, nil
}
