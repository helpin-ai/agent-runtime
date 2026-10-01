package runtime

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
)

const (
	a2aTargetContextKey    = "a2a"
	a2aMaxResponseBytes    = 8 << 20
	a2aDefaultTurnDuration = 110 * time.Minute
)

// a2aConnection is the per-turn connection the host delivers in the target
// context. It is never persisted: the token lives only for this turn.
type a2aConnection struct {
	ExternalAgentID     string          `json:"external_agent_id"`
	Name                string          `json:"name"`
	CardURL             string          `json:"card_url"`
	AgentCard           json.RawMessage `json:"agent_card"`
	Auth                a2aAuth         `json:"auth"`
	ContextID           string          `json:"context_id"`
	MessageAppendix     string          `json:"message_appendix"`
	AllowPrivateNetwork bool            `json:"allow_private_network"`
	MaxTurnSeconds      int             `json:"max_turn_seconds"`
}

type a2aAuth struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

// takeA2AConnection reads data.a2a and removes it from the target context so
// the token cannot reach anything else that sees the context later.
func takeA2AConnection(execCtx *ExecutionContext) (*a2aConnection, error) {
	if execCtx == nil || execCtx.TargetContext == nil || execCtx.TargetContext.Data == nil {
		return nil, errA2AConnectionMissing
	}
	raw, ok := execCtx.TargetContext.Data[a2aTargetContextKey]
	delete(execCtx.TargetContext.Data, a2aTargetContextKey)
	if !ok || raw == nil {
		return nil, errA2AConnectionMissing
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("read external agent connection: %w", err)
	}
	var conn a2aConnection
	if err := json.Unmarshal(encoded, &conn); err != nil {
		return nil, errors.New("read external agent connection: data.a2a is malformed")
	}
	conn.Name = strings.TrimSpace(conn.Name)
	conn.CardURL = strings.TrimSpace(conn.CardURL)
	conn.ContextID = strings.TrimSpace(conn.ContextID)
	if conn.CardURL == "" && len(conn.AgentCard) == 0 {
		return nil, errors.New("external agent connection has neither card_url nor agent_card")
	}
	if typ := strings.TrimSpace(conn.Auth.Type); typ != "" && typ != "bearer" && typ != "none" {
		return nil, fmt.Errorf("external agent auth type %q is not supported", typ)
	}
	return &conn, nil
}

var errA2AConnectionMissing = errors.New("external agent connection details are missing from the target context; check that the host app still has this agent enabled")

func (c *a2aConnection) turnLimit() time.Duration {
	if c == nil || c.MaxTurnSeconds <= 0 {
		return a2aDefaultTurnDuration
	}
	return time.Duration(c.MaxTurnSeconds) * time.Second
}

// redact removes the bearer token from text that may echo request details,
// such as a remote error message.
func (c *a2aConnection) redact(text string) string {
	if c == nil || strings.TrimSpace(c.Auth.Token) == "" {
		return text
	}
	return strings.ReplaceAll(text, c.Auth.Token, "[redacted]")
}

func (c *a2aConnection) redactErr(err error) error {
	if err == nil || c == nil || strings.TrimSpace(c.Auth.Token) == "" || !strings.Contains(err.Error(), c.Auth.Token) {
		return err
	}
	return errors.New(c.redact(err.Error()))
}

// newA2AClient builds a JSON-RPC-first client from the cached card, or from
// the card fetched at card_url when no usable cached card was supplied.
func newA2AClient(ctx context.Context, conn *a2aConnection) (*a2aclient.Client, error) {
	httpClient := newA2AHTTPClient(conn.AllowPrivateNetwork)
	card, err := a2aAgentCard(ctx, conn, httpClient)
	if err != nil {
		return nil, err
	}
	interfaces := make([]*a2a.AgentInterface, 0, len(card.SupportedInterfaces))
	for _, iface := range card.SupportedInterfaces {
		if iface != nil && validateA2AURL(iface.URL, conn.AllowPrivateNetwork) == nil {
			interfaces = append(interfaces, iface)
		}
	}
	if len(interfaces) == 0 {
		return nil, errors.New("external agent card has no interface with an allowed URL")
	}
	cardCopy := *card
	cardCopy.SupportedInterfaces = interfaces
	options := []a2aclient.FactoryOption{
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithConfig(a2aclient.Config{
			PreferredTransports: []a2a.TransportProtocol{a2a.TransportProtocolJSONRPC, a2a.TransportProtocolHTTPJSON},
		}),
		a2aclient.WithJSONRPCTransport(httpClient),
		a2aclient.WithRESTTransport(httpClient),
		// Helpin accepts v0.3 agents too; they speak the older method names.
		a2av0.WithJSONRPCTransport(a2av0.JSONRPCTransportConfig{Client: httpClient}),
		a2av0.WithRESTTransport(a2av0.RESTTransportConfig{Client: httpClient}),
	}
	if token := strings.TrimSpace(conn.Auth.Token); token != "" {
		options = append(options, a2aclient.WithCallInterceptors(a2aBearerInterceptor{token: token}))
	}
	client, err := a2aclient.NewFromCard(ctx, &cardCopy, options...)
	if err != nil {
		return nil, fmt.Errorf("connect to external agent: %w", conn.redactErr(err))
	}
	return client, nil
}

func a2aAgentCard(ctx context.Context, conn *a2aConnection, httpClient *http.Client) (*a2a.AgentCard, error) {
	if len(conn.AgentCard) > 0 && string(conn.AgentCard) != "null" {
		if card, err := parseA2AAgentCard(conn.AgentCard); err == nil && len(card.SupportedInterfaces) > 0 {
			return card, nil
		}
	}
	if err := validateA2AURL(conn.CardURL, conn.AllowPrivateNetwork); err != nil {
		return nil, fmt.Errorf("external agent card URL: %w", err)
	}
	var options []agentcard.ResolveOption
	if token := strings.TrimSpace(conn.Auth.Token); token != "" {
		options = append(options, agentcard.WithRequestHeader("Authorization", "Bearer "+token))
	}
	resolver := &agentcard.Resolver{Client: httpClient, CardParser: parseA2AAgentCard}
	card, err := resolver.Resolve(ctx, conn.CardURL, options...)
	if err != nil {
		return nil, fmt.Errorf("fetch external agent card: %w", conn.redactErr(err))
	}
	return card, nil
}

// parseA2AAgentCard reads v1.0 cards, v0.3 cards, and v1.0 cards that still
// use v0.3 fields such as {"type": "http"} security schemes (as Hermes does).
func parseA2AAgentCard(body []byte) (*a2a.AgentCard, error) {
	card, err := a2av0.NewAgentCardParser()(body)
	if err != nil || len(card.SupportedInterfaces) > 0 {
		return card, err
	}
	// v0.3 makes preferredTransport optional, defaulting to JSON-RPC.
	var legacy struct {
		URL             string              `json:"url"`
		ProtocolVersion a2a.ProtocolVersion `json:"protocolVersion"`
	}
	if json.Unmarshal(body, &legacy) == nil && strings.TrimSpace(legacy.URL) != "" {
		version := legacy.ProtocolVersion
		if version == "" {
			version = a2av0.Version
		}
		card.SupportedInterfaces = []*a2a.AgentInterface{{URL: strings.TrimSpace(legacy.URL), ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: version}}
	}
	return card, nil
}

// validateA2AURL requires HTTPS for public agents. Plain HTTP is accepted only
// when the host explicitly allows private-network agents.
func validateA2AURL(raw string, allowPrivateNetwork bool) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%q is not a valid URL", raw)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		if allowPrivateNetwork {
			return nil
		}
		return errors.New("HTTPS is required unless private-network agents are allowed")
	default:
		return fmt.Errorf("URL scheme %q is not allowed", parsed.Scheme)
	}
}

type a2aBearerInterceptor struct {
	a2aclient.PassthroughInterceptor
	token string
}

func (i a2aBearerInterceptor) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	if req.ServiceParams == nil {
		req.ServiceParams = a2aclient.ServiceParams{}
	}
	req.ServiceParams["Authorization"] = []string{"Bearer " + i.token}
	return ctx, nil, nil
}

// newA2AHTTPClient dials only public addresses unless the host allows the
// private network. The check runs on the resolved address at connect time so
// DNS rebinding cannot route a public name to an internal service.
func newA2AHTTPClient(allowPrivateNetwork bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivateNetwork {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || a2aBlockedIP(ip) {
				return fmt.Errorf("external agent address %s is not allowed: private, loopback and link-local networks are blocked", host)
			}
			return nil
		}
	}
	transport := &http.Transport{
		// A process-level proxy would resolve the destination itself and bypass
		// the dial-time address check.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   2,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		// Calls carry their own deadlines; a blocking SendMessage may
		// legitimately take as long as the turn.
		Transport: a2aBoundedResponses{base: transport, maxBytes: a2aMaxResponseBytes},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

func a2aBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || !ip.IsGlobalUnicast() {
		return true
	}
	// Carrier-grade NAT space is not routable from the public internet.
	return a2aSharedAddressSpace.Contains(ip)
}

var a2aSharedAddressSpace = func() *net.IPNet {
	_, network, _ := net.ParseCIDR("100.64.0.0/10")
	return network
}()

// a2aBoundedResponses fails a response body that grows past maxBytes instead
// of letting a remote agent exhaust worker memory.
type a2aBoundedResponses struct {
	base     http.RoundTripper
	maxBytes int64
}

func (t a2aBoundedResponses) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &a2aLimitedBody{body: resp.Body, remaining: t.maxBytes}
	return resp, nil
}

type a2aLimitedBody struct {
	body      io.ReadCloser
	remaining int64
}

func (b *a2aLimitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var probe [1]byte
		if n, err := b.body.Read(probe[:]); n == 0 && errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		return 0, fmt.Errorf("external agent response exceeds %d bytes", a2aMaxResponseBytes)
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *a2aLimitedBody) Close() error { return b.body.Close() }
