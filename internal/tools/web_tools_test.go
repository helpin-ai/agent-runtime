package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRegisterWebToolsRegistersFetchAndConfiguredSearchTools(t *testing.T) {
	registry := &Registry{
		state: &registryState{
			defs:        map[string]Definition{},
			handlers:    map[string]Handler{},
			appDefs:     map[string]map[string]Definition{},
			appHandlers: map[string]map[string]Handler{},
		},
	}
	RegisterWebTools(registry, WebToolsConfig{
		BraveSearch: NewBraveSearchClient("brave-key"),
		ExaSearch:   NewExaSearchClient("exa-key"),
	})

	for _, name := range []string{"web_search_brave", "web_search_exa", "fetch_url", "crawl_url"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s definition", name)
		}
		if def.Mutating {
			t.Fatalf("expected %s to be read-only", name)
		}
	}
}

func TestWebSearchExaNormalizesRequestAndReturnsResults(t *testing.T) {
	var body map[string]any
	client := &ExaSearchClient{
		apiKey: "exa-key",
		apiURL: "https://api.exa.test/search",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("x-api-key"); got != "exa-key" {
				t.Fatalf("x-api-key = %q", got)
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{
					"requestId":"req-1",
					"searchType":"auto",
					"results":[{"title":"Example","url":"https://example.com","id":"r1","highlights":["one"]}]
				}`)),
			}, nil
		})},
	}

	out, err := toolWebSearchExa(context.Background(), client, json.RawMessage(`{
		"query":" release notes ",
		"num_results":20,
		"include_domains":["https://Example.com/docs", "example.com"],
		"contents":{"text":{"max_characters":0}}
	}`))
	if err != nil {
		t.Fatalf("web_search_exa returned error: %v", err)
	}
	if body["query"] != "release notes" {
		t.Fatalf("query = %v", body["query"])
	}
	if body["numResults"] != float64(10) {
		t.Fatalf("numResults = %v", body["numResults"])
	}
	domains, _ := body["includeDomains"].([]any)
	if len(domains) != 1 || domains[0] != "example.com" {
		t.Fatalf("includeDomains = %#v", body["includeDomains"])
	}
	var payload exaToolResponse
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if payload.RequestID != "req-1" || payload.ResultCount != 1 || payload.Results[0].URL != "https://example.com" {
		t.Fatalf("unexpected output: %s", string(out))
	}
}

func TestFetchURLRejectsPrivateHostsBeforeRequest(t *testing.T) {
	called := false
	client := &WebFetchClient{
		directClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(nil)),
			}, nil
		})},
	}

	_, err := toolFetchURL(context.Background(), client, json.RawMessage(`{"url":"http://127.0.0.1/private"}`))
	if err == nil || !strings.Contains(err.Error(), "private or local IP") {
		t.Fatalf("expected private-host error, got %v", err)
	}
	if called {
		t.Fatal("fetch_url should reject private hosts before issuing a request")
	}
}
