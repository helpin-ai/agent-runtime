package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type recordingBraveSearchClient struct {
	calls   int
	results []WebSearchResult
	err     error
}

func (c *recordingBraveSearchClient) Search(_ context.Context, _ WebSearchQuery) ([]WebSearchResult, error) {
	c.calls++
	return c.results, c.err
}

func testExaSearchClient(status int, calls *int) *ExaSearchClient {
	return &ExaSearchClient{
		apiKey: "exa-key",
		apiURL: "https://api.exa.test/search",
		httpClient: &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			(*calls)++
			body := `{"requestId":"req-1","searchType":"fast","results":[{"title":"Exa result","url":"https://exa.example"}]}`
			if status != http.StatusOK {
				body = `{"error":"temporarily unavailable"}`
			}
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
}

func webSearchProvider(t *testing.T, output json.RawMessage) (string, string) {
	t.Helper()
	var payload struct {
		Provider     string `json:"provider"`
		FallbackFrom string `json:"fallback_from"`
	}
	if err := json.Unmarshal(output, &payload); err != nil {
		t.Fatalf("decode web_search output: %v; raw=%s", err, output)
	}
	return payload.Provider, payload.FallbackFrom
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

	for _, name := range []string{"web_search", "fetch_url", "crawl_url"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s definition", name)
		}
		if def.Mutating {
			t.Fatalf("expected %s to be read-only", name)
		}
		if name == "web_search" && !strings.Contains(def.Description, "Exa is preferred") {
			t.Fatalf("web_search description omits provider precedence: %q", def.Description)
		}
	}
	for _, oldName := range []string{"web_search_brave", "web_search_exa"} {
		if _, ok := registry.Definition(oldName); ok {
			t.Fatalf("superseded tool %s must not be registered", oldName)
		}
	}
}

func TestWebSearchSelectsConfiguredProviderWithExaPrecedence(t *testing.T) {
	tests := []struct {
		name          string
		withExa       bool
		withBrave     bool
		wantProvider  string
		wantExaCalls  int
		wantBraveCall int
	}{
		{name: "Exa only", withExa: true, wantProvider: "exa", wantExaCalls: 1},
		{name: "Brave only", withBrave: true, wantProvider: "brave", wantBraveCall: 1},
		{name: "both prefer Exa", withExa: true, withBrave: true, wantProvider: "exa", wantExaCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exaCalls := 0
			var exa *ExaSearchClient
			if test.withExa {
				exa = testExaSearchClient(http.StatusOK, &exaCalls)
			}
			brave := &recordingBraveSearchClient{results: []WebSearchResult{{Title: "Brave result", URL: "https://brave.example"}}}
			var braveClient WebSearchClient
			if test.withBrave {
				braveClient = brave
			}

			out, err := toolWebSearch(context.Background(), braveClient, exa, json.RawMessage(`{"query":"release notes"}`))
			if err != nil {
				t.Fatalf("web_search returned error: %v", err)
			}
			provider, fallbackFrom := webSearchProvider(t, out)
			if provider != test.wantProvider || fallbackFrom != "" {
				t.Fatalf("provider = %q fallback_from = %q, want provider %q", provider, fallbackFrom, test.wantProvider)
			}
			if exaCalls != test.wantExaCalls || brave.calls != test.wantBraveCall {
				t.Fatalf("calls: Exa=%d Brave=%d; want Exa=%d Brave=%d", exaCalls, brave.calls, test.wantExaCalls, test.wantBraveCall)
			}
		})
	}
}

func TestWebSearchFallsBackToBraveForCompatibleFastExaFailure(t *testing.T) {
	exaCalls := 0
	brave := &recordingBraveSearchClient{results: []WebSearchResult{{Title: "Fallback", URL: "https://fallback.example"}}}
	out, err := toolWebSearch(context.Background(), brave, testExaSearchClient(http.StatusServiceUnavailable, &exaCalls), json.RawMessage(`{"query":"release notes"}`))
	if err != nil {
		t.Fatalf("web_search returned error: %v", err)
	}
	provider, fallbackFrom := webSearchProvider(t, out)
	if provider != "brave" || fallbackFrom != "exa" || exaCalls != 1 || brave.calls != 1 {
		t.Fatalf("provider=%q fallback_from=%q calls: Exa=%d Brave=%d", provider, fallbackFrom, exaCalls, brave.calls)
	}
}

func TestWebSearchDoesNotDowngradeExaOnlyRequests(t *testing.T) {
	exaCalls := 0
	brave := &recordingBraveSearchClient{results: []WebSearchResult{{Title: "Fallback", URL: "https://fallback.example"}}}
	_, err := toolWebSearch(context.Background(), brave, testExaSearchClient(http.StatusServiceUnavailable, &exaCalls), json.RawMessage(`{"query":"release notes","mode":"deep"}`))
	if err == nil || !strings.Contains(err.Error(), "exa search API returned status 503") {
		t.Fatalf("expected Exa error, got %v", err)
	}
	if exaCalls != 1 || brave.calls != 0 {
		t.Fatalf("calls: Exa=%d Brave=%d; deep search must not downgrade", exaCalls, brave.calls)
	}
}

func TestWebSearchReportsBothProviderFailures(t *testing.T) {
	exaCalls := 0
	brave := &recordingBraveSearchClient{err: errors.New("brave unavailable")}
	_, err := toolWebSearch(context.Background(), brave, testExaSearchClient(http.StatusServiceUnavailable, &exaCalls), json.RawMessage(`{"query":"release notes"}`))
	if err == nil || !strings.Contains(err.Error(), "exa search failed") || !strings.Contains(err.Error(), "brave fallback failed") {
		t.Fatalf("expected combined provider error, got %v", err)
	}
}

func TestApplyWebSearchFreshnessToExa(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	params := webSearchToolInput{Freshness: "pw"}
	if err := applyWebSearchFreshnessToExa(&params, now); err != nil {
		t.Fatalf("apply freshness: %v", err)
	}
	if params.StartPublishedDate != "2026-08-06T12:00:00Z" {
		t.Fatalf("start_published_date = %q", params.StartPublishedDate)
	}
	params = webSearchToolInput{Freshness: "pw", exaSearchToolInput: exaSearchToolInput{StartPublishedDate: "2026-08-01T00:00:00Z"}}
	if err := applyWebSearchFreshnessToExa(&params, now); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected conflicting freshness error, got %v", err)
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
