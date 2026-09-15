package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

const protocolVersion = "agent-runtime-cli/v1alpha1"

type Descriptor struct {
	ProtocolVersion string   `json:"protocol_version"`
	AppID           string   `json:"app_id"`
	Name            string   `json:"name"`
	APIBaseURL      string   `json:"api_base_url"`
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"client_id"`
	Resource        string   `json:"resource"`
	Scopes          []string `json:"scopes"`
}
type Connection struct {
	URL             string     `json:"url"`
	Descriptor      Descriptor `json:"descriptor"`
	CredentialStore string     `json:"credential_store"`
}
type token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int       `json:"expires_in"`
	ExpiresAt    time.Time `json:"expires_at"`
}
type oauthMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

var profileName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var httpClient = &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}

func secureURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("invalid endpoint URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("endpoint must use HTTPS (HTTP is allowed only for loopback IPs)")
		}
	}
	return nil
}
func readJSON(ctx context.Context, method, endpoint, bearer string, body io.Reader, dst interface{}) error {
	if err := secureURL(endpoint); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("host returned HTTP %d", res.StatusCode)
	}
	if dst == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(dst)
}
func connectionPath(home, name string) (string, error) {
	if !profileName.MatchString(name) {
		return "", errors.New("connection name must contain only letters, numbers, underscores or hyphens")
	}
	return filepath.Join(home, "connections", name+".json"), nil
}
func loadConnection(home, name string) (Connection, error) {
	var c Connection
	p, e := connectionPath(home, name)
	if e != nil {
		return c, e
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return c, e
	}
	e = json.Unmarshal(b, &c)
	return c, e
}
func saveJSON(path string, v interface{}) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func credentialKey(home, name string) string {
	sum := sha256.Sum256([]byte(home + "\x00" + name))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func saveToken(home, name string, c Connection, t token) error {
	b, e := json.Marshal(t)
	if e != nil {
		return e
	}
	if c.CredentialStore == "file" {
		return saveJSON(filepath.Join(home, "credentials", name+".json"), t)
	}
	if e = keyring.Set("agent-runtime-cli", credentialKey(home, name), string(b)); e != nil {
		return fmt.Errorf("OS keyring unavailable: %w; reconnect with --credential-store file to explicitly use a protected local file", e)
	}
	return nil
}
func loadToken(home, name string, c Connection) (token, error) {
	var t token
	var b []byte
	var e error
	if c.CredentialStore == "file" {
		b, e = os.ReadFile(filepath.Join(home, "credentials", name+".json"))
	} else {
		var s string
		s, e = keyring.Get("agent-runtime-cli", credentialKey(home, name))
		b = []byte(s)
	}
	if e != nil {
		return t, fmt.Errorf("login required for %s", name)
	}
	e = json.Unmarshal(b, &t)
	return t, e
}
func randomString() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func metadata(ctx context.Context, c Connection) (oauthMetadata, error) {
	var m oauthMetadata
	issuer := strings.TrimSuffix(c.Descriptor.Issuer, "/")
	u, e := url.Parse(issuer)
	if e != nil {
		return m, e
	}
	endpoint := u.Scheme + "://" + u.Host + "/.well-known/oauth-authorization-server" + strings.TrimSuffix(u.Path, "/")
	e = readJSON(ctx, "GET", endpoint, "", nil, &m)
	if e != nil {
		return m, e
	}
	if m.Issuer != c.Descriptor.Issuer {
		return m, errors.New("OAuth issuer mismatch")
	}
	if e = secureURL(m.AuthorizationEndpoint); e != nil {
		return m, e
	}
	if e = secureURL(m.TokenEndpoint); e != nil {
		return m, e
	}
	supported := false
	for _, s := range m.CodeChallengeMethodsSupported {
		if s == "S256" {
			supported = true
		}
	}
	if !supported {
		return m, errors.New("issuer must support PKCE S256")
	}
	return m, nil
}
func exchange(ctx context.Context, endpoint string, form url.Values) (token, error) {
	var t token
	req, e := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if e != nil {
		return t, e
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, e := httpClient.Do(req)
	if e != nil {
		return t, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return t, fmt.Errorf("OAuth token exchange returned HTTP %d", res.StatusCode)
	}
	e = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&t)
	if e != nil {
		return t, e
	}
	if t.AccessToken == "" || !strings.EqualFold(t.TokenType, "Bearer") {
		return t, errors.New("invalid OAuth token response")
	}
	if t.ExpiresIn > 0 {
		t.ExpiresAt = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	return t, nil
}
func accessToken(ctx context.Context, home, name string, c Connection) (string, error) {
	unlock, e := lockCredentials(ctx, home, name)
	if e != nil {
		return "", e
	}
	defer unlock()
	t, e := loadToken(home, name, c)
	if e != nil {
		return "", e
	}
	if !t.ExpiresAt.IsZero() && time.Until(t.ExpiresAt) < time.Minute {
		if t.RefreshToken == "" {
			return "", errors.New("access token expired; login again")
		}
		m, e := metadata(ctx, c)
		if e != nil {
			return "", e
		}
		next, e := exchange(ctx, m.TokenEndpoint, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}, "client_id": {c.Descriptor.ClientID}, "resource": {c.Descriptor.Resource}})
		if e != nil {
			return "", e
		}
		if next.RefreshToken == "" {
			next.RefreshToken = t.RefreshToken
		}
		if e = saveToken(home, name, c, next); e != nil {
			return "", e
		}
		t = next
	}
	return t.AccessToken, nil
}
func login(ctx context.Context, home, name string, c Connection, out io.Writer) error {
	m, e := metadata(ctx, c)
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return e
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	state := randomString()
	verifier := randomString()
	sum := sha256.Sum256([]byte(verifier))
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		q := r.URL.Query()
		if r.Method != "GET" || q.Get("state") != state || q.Get("code") == "" {
			http.Error(w, "Invalid OAuth callback", 400)
			return
		}
		if issuer := q.Get("iss"); issuer != "" && issuer != m.Issuer {
			http.Error(w, "Issuer mismatch", 400)
			return
		}
		select {
		case codes <- q.Get("code"):
			fmt.Fprint(w, "Signed in. You may close this tab.")
		default:
			http.Error(w, "Callback already received", 409)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go server.Serve(listener)
	u, _ := url.Parse(m.AuthorizationEndpoint)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.Descriptor.ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("state", state)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("scope", strings.Join(c.Descriptor.Scopes, " "))
	q.Set("resource", c.Descriptor.Resource)
	u.RawQuery = q.Encode()
	fmt.Fprintln(out, "Open this URL to sign in:\n"+u.String())
	browserOpener(u.String())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case code := <-codes:
		t, e := exchange(ctx, m.TokenEndpoint, url.Values{"grant_type": {"authorization_code"}, "client_id": {c.Descriptor.ClientID}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {c.Descriptor.Resource}})
		if e != nil {
			return e
		}
		if e = saveToken(home, name, c, t); e != nil {
			return e
		}
		fmt.Fprintln(out, "Signed in to", safeText(c.Descriptor.Name))
		return nil
	}
}

var browserOpener = openBrowser

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}
func connectionCommand(ctx context.Context, home string, args []string, out io.Writer) error {
	if args[0] == "connections" {
		files, e := filepath.Glob(filepath.Join(home, "connections", "*.json"))
		if e != nil {
			return e
		}
		for _, f := range files {
			name := strings.TrimSuffix(filepath.Base(f), ".json")
			c, e := loadConnection(home, name)
			if e != nil {
				return e
			}
			fmt.Fprintf(out, "%s  %s  %s\n", name, safeText(c.Descriptor.Name), safeText(c.URL))
		}
		return nil
	}
	if len(args) < 2 {
		return errors.New("connection name or URL required")
	}
	if args[0] == "connect" {
		name := "default"
		credentialStore := "keyring"
		for i := 2; i < len(args); i++ {
			if i+1 >= len(args) {
				return errors.New("flag value required")
			}
			switch args[i] {
			case "--name":
				name = args[i+1]
			case "--credential-store":
				credentialStore = args[i+1]
			default:
				return fmt.Errorf("unknown flag %s", args[i])
			}
			i++
		}
		if credentialStore != "keyring" && credentialStore != "file" {
			return errors.New("credential store must be keyring or file")
		}
		p, e := connectionPath(home, name)
		if e != nil {
			return e
		}
		if _, e = os.Stat(p); e == nil {
			return errors.New("connection already exists; choose another name")
		}
		base := strings.TrimSuffix(args[1], "/")
		var d Descriptor
		if e = readJSON(ctx, "GET", base+"/agent-runtime/cli.json", "", nil, &d); e != nil {
			return e
		}
		if d.ProtocolVersion != protocolVersion {
			return fmt.Errorf("unsupported host protocol %q", d.ProtocolVersion)
		}
		if d.AppID == "" || d.ClientID == "" || d.Resource == "" {
			return errors.New("host descriptor is missing app_id, client_id, or resource")
		}
		for _, u := range []string{d.APIBaseURL, d.Issuer, d.Resource} {
			if e = secureURL(u); e != nil {
				return e
			}
		}
		c := Connection{URL: base, Descriptor: d, CredentialStore: credentialStore}
		if e = saveJSON(p, c); e != nil {
			return e
		}
		fmt.Fprintf(out, "Connected %s (%s). Run: agent-runtime-cli login %s\n", safeText(d.Name), name, name)
		return nil
	}
	name := args[1]
	c, e := loadConnection(home, name)
	if e != nil {
		return e
	}
	switch args[0] {
	case "login":
		return login(ctx, home, name, c, out)
	case "logout":
		if c.CredentialStore == "file" {
			e = os.Remove(filepath.Join(home, "credentials", name+".json"))
			if os.IsNotExist(e) {
				e = nil
			}
		} else {
			e = keyring.Delete("agent-runtime-cli", credentialKey(home, name))
		}
		return e
	case "agents", "whoami":
		t, e := accessToken(ctx, home, name, c)
		if e != nil {
			return e
		}
		var result json.RawMessage
		endpoint := args[0]
		if endpoint == "whoami" {
			endpoint = "me"
		}
		if e = readJSON(ctx, "GET", strings.TrimSuffix(c.Descriptor.APIBaseURL, "/")+"/"+endpoint, t, nil, &result); e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(result)
	}
	return errors.New("unknown connection command")
}
