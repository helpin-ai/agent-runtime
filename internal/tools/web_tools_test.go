package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
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

func testTinyFishSearchClient(status int, calls *int) *TinyFishSearchClient {
	return &TinyFishSearchClient{
		apiKey: "tinyfish-key",
		apiURL: "https://api.search.tinyfish.test",
		httpClient: &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			(*calls)++
			body := `{"query":"release notes","results":[{"position":1,"site_name":"example.com","title":"TinyFish result","url":"https://tinyfish.example","snippet":"Fresh result"}],"total_results":1,"page":0}`
			if status != http.StatusOK {
				body = `{"error":{"code":"UNAVAILABLE","message":"temporarily unavailable"}}`
			}
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		now: time.Now,
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
		BraveSearch:    NewBraveSearchClient("brave-key"),
		ExaSearch:      NewExaSearchClient("exa-key"),
		TinyFishSearch: NewTinyFishSearchClient("tinyfish-key"),
	})

	for _, name := range []string{"web_search", "fetch_url", "crawl_url"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s definition", name)
		}
		if def.Mutating {
			t.Fatalf("expected %s to be read-only", name)
		}
		if name == "web_search" && !strings.Contains(def.Description, "TinyFish is preferred") {
			t.Fatalf("web_search description omits provider precedence: %q", def.Description)
		}
	}
	for _, oldName := range []string{"web_search_brave", "web_search_exa"} {
		if _, ok := registry.Definition(oldName); ok {
			t.Fatalf("superseded tool %s must not be registered", oldName)
		}
	}
}

func TestRegisterWebToolsRegistersSearchWithOnlyTinyFish(t *testing.T) {
	registry := &Registry{
		state: &registryState{
			defs:        map[string]Definition{},
			handlers:    map[string]Handler{},
			appDefs:     map[string]map[string]Definition{},
			appHandlers: map[string]map[string]Handler{},
		},
	}
	RegisterWebTools(registry, WebToolsConfig{TinyFishSearch: NewTinyFishSearchClient("tinyfish-key")})
	if _, ok := registry.Definition("web_search"); !ok {
		t.Fatal("expected web_search definition with only TinyFish configured")
	}
}

func TestNewTinyFishSearchClientRequiresKey(t *testing.T) {
	if client := NewTinyFishSearchClient(" \n\t"); client != nil {
		t.Fatalf("client=%+v, want nil", client)
	}
	client := NewTinyFishSearchClient(" key ")
	if client == nil || client.apiKey != "key" || client.httpClient.Timeout != 5*time.Second || client.httpClient.CheckRedirect == nil {
		t.Fatalf("unexpected configured client: %+v", client)
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

			out, err := toolWebSearch(context.Background(), nil, exa, braveClient, json.RawMessage(`{"query":"release notes"}`))
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

func TestWebSearchPrefersTinyFishForCompatibleFastSearch(t *testing.T) {
	tinyFishCalls := 0
	exaCalls := 0
	brave := &recordingBraveSearchClient{results: []WebSearchResult{{Title: "Brave result", URL: "https://brave.example"}}}

	out, err := toolWebSearch(
		context.Background(),
		testTinyFishSearchClient(http.StatusOK, &tinyFishCalls),
		testExaSearchClient(http.StatusOK, &exaCalls),
		brave,
		json.RawMessage(`{"query":"release notes"}`),
	)
	if err != nil {
		t.Fatalf("web_search returned error: %v", err)
	}
	provider, fallbackFrom := webSearchProvider(t, out)
	if provider != "tinyfish" || fallbackFrom != "" {
		t.Fatalf("provider=%q fallback_from=%q", provider, fallbackFrom)
	}
	if tinyFishCalls != 1 || exaCalls != 0 || brave.calls != 0 {
		t.Fatalf("calls: TinyFish=%d Exa=%d Brave=%d", tinyFishCalls, exaCalls, brave.calls)
	}
}

func TestWebSearchFallsBackFromTinyFishToExa(t *testing.T) {
	tinyFishCalls := 0
	exaCalls := 0
	out, err := toolWebSearch(
		context.Background(),
		testTinyFishSearchClient(http.StatusServiceUnavailable, &tinyFishCalls),
		testExaSearchClient(http.StatusOK, &exaCalls),
		nil,
		json.RawMessage(`{"query":"release notes"}`),
	)
	if err != nil {
		t.Fatalf("web_search returned error: %v", err)
	}
	provider, fallbackFrom := webSearchProvider(t, out)
	if provider != "exa" || fallbackFrom != "tinyfish" {
		t.Fatalf("provider=%q fallback_from=%q", provider, fallbackFrom)
	}
	if tinyFishCalls != 1 || exaCalls != 1 {
		t.Fatalf("calls: TinyFish=%d Exa=%d", tinyFishCalls, exaCalls)
	}
}

func TestWebSearchFallsThroughTinyFishAndExaToBrave(t *testing.T) {
	tinyFishCalls := 0
	exaCalls := 0
	brave := &recordingBraveSearchClient{results: []WebSearchResult{{Title: "Brave result", URL: "https://brave.example"}}}
	out, err := toolWebSearch(
		context.Background(),
		testTinyFishSearchClient(http.StatusServiceUnavailable, &tinyFishCalls),
		testExaSearchClient(http.StatusServiceUnavailable, &exaCalls),
		brave,
		json.RawMessage(`{"query":"release notes"}`),
	)
	if err != nil {
		t.Fatalf("web_search returned error: %v", err)
	}
	provider, fallbackFrom := webSearchProvider(t, out)
	if provider != "brave" || fallbackFrom != "exa" {
		t.Fatalf("provider=%q fallback_from=%q", provider, fallbackFrom)
	}
	if tinyFishCalls != 1 || exaCalls != 1 || brave.calls != 1 {
		t.Fatalf("calls: TinyFish=%d Exa=%d Brave=%d", tinyFishCalls, exaCalls, brave.calls)
	}
}

func TestWebSearchDoesNotDowngradeLocationSearchToBrave(t *testing.T) {
	tinyFishCalls := 0
	brave := &recordingBraveSearchClient{results: []WebSearchResult{{Title: "Brave result", URL: "https://brave.example"}}}
	_, err := toolWebSearch(
		context.Background(),
		testTinyFishSearchClient(http.StatusServiceUnavailable, &tinyFishCalls),
		nil,
		brave,
		json.RawMessage(`{"query":"release notes","user_location":"US"}`),
	)
	if err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("error=%v", err)
	}
	if tinyFishCalls != 1 || brave.calls != 0 {
		t.Fatalf("calls: TinyFish=%d Brave=%d", tinyFishCalls, brave.calls)
	}
}

func TestWebSearchSkipsTinyFishForExaOnlyRequest(t *testing.T) {
	tests := []struct {
		name  string
		input json.RawMessage
	}{
		{name: "deep", input: json.RawMessage(`{"query":"release notes","mode":"deep"}`)},
		{name: "freshness", input: json.RawMessage(`{"query":"release notes","freshness":"pw"}`)},
		{name: "contents", input: json.RawMessage(`{"query":"release notes","contents":{"highlights":{"max_characters":1000}}}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tinyFishCalls := 0
			exaCalls := 0
			out, err := toolWebSearch(
				context.Background(),
				testTinyFishSearchClient(http.StatusOK, &tinyFishCalls),
				testExaSearchClient(http.StatusOK, &exaCalls),
				nil,
				test.input,
			)
			if err != nil {
				t.Fatalf("web_search returned error: %v", err)
			}
			provider, _ := webSearchProvider(t, out)
			if provider != "exa" || tinyFishCalls != 0 || exaCalls != 1 {
				t.Fatalf("provider=%q calls: TinyFish=%d Exa=%d", provider, tinyFishCalls, exaCalls)
			}
		})
	}
}

func TestWebSearchTinyFishEmptyResultsDoNotFallback(t *testing.T) {
	tinyFishCalls := 0
	exaCalls := 0
	client := testTinyFishSearchClient(http.StatusOK, &tinyFishCalls)
	client.httpClient.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		tinyFishCalls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"results":[]}`))}, nil
	})
	out, err := toolWebSearch(context.Background(), client, testExaSearchClient(http.StatusOK, &exaCalls), nil, json.RawMessage(`{"query":"nothing obscure"}`))
	if err != nil {
		t.Fatalf("web_search returned error: %v", err)
	}
	provider, _ := webSearchProvider(t, out)
	if provider != "tinyfish" || tinyFishCalls != 1 || exaCalls != 0 {
		t.Fatalf("provider=%q calls: TinyFish=%d Exa=%d", provider, tinyFishCalls, exaCalls)
	}
}

func TestWebSearchFallsBackToBraveForCompatibleFastExaFailure(t *testing.T) {
	exaCalls := 0
	brave := &recordingBraveSearchClient{results: []WebSearchResult{{Title: "Fallback", URL: "https://fallback.example"}}}
	out, err := toolWebSearch(context.Background(), nil, testExaSearchClient(http.StatusServiceUnavailable, &exaCalls), brave, json.RawMessage(`{"query":"release notes"}`))
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
	_, err := toolWebSearch(context.Background(), nil, testExaSearchClient(http.StatusServiceUnavailable, &exaCalls), brave, json.RawMessage(`{"query":"release notes","mode":"deep"}`))
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
	_, err := toolWebSearch(context.Background(), nil, testExaSearchClient(http.StatusServiceUnavailable, &exaCalls), brave, json.RawMessage(`{"query":"release notes"}`))
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

func TestTinyFishSearchClientBuildsRequestAndNormalizesResults(t *testing.T) {
	var requestURL *url.URL
	var apiKey, accept string
	client := &TinyFishSearchClient{
		apiKey: "tinyfish-key",
		apiURL: "https://api.search.tinyfish.test",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requestURL = req.URL
			apiKey = req.Header.Get("X-API-Key")
			accept = req.Header.Get("Accept")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"results":[
					{"title":" First ","url":" https://one.example ","snippet":" One snippet "},
					{"title":"","url":"https://invalid.example","snippet":"missing title"},
					{"title":"Second","url":"https://two.example","snippet":"Two snippet"}
				]}`)),
			}, nil
		})},
		now: time.Now,
	}

	results, err := client.Search(context.Background(), TinyFishSearchRequest{
		Query:          " release notes ",
		Location:       "us",
		MaxResults:     1,
		IncludeDomains: []string{"https://Example.com/docs"},
		ExcludeDomains: []string{"spam.example"},
	})
	if err != nil {
		t.Fatalf("TinyFish Search returned error: %v", err)
	}
	if apiKey != "tinyfish-key" || accept != "application/json" {
		t.Fatalf("headers: X-API-Key=%q Accept=%q", apiKey, accept)
	}
	if requestURL == nil {
		t.Fatal("TinyFish Search did not issue a request")
	}
	if got := requestURL.Query().Get("location"); got != "US" {
		t.Fatalf("location=%q", got)
	}
	query := requestURL.Query().Get("query")
	if !strings.Contains(query, "site:example.com") || !strings.Contains(query, "-site:spam.example") || !strings.Contains(query, "release notes") {
		t.Fatalf("query=%q", query)
	}
	if len(results) != 1 || results[0].Title != "First" || results[0].URL != "https://one.example" || results[0].Snippet != "One snippet" {
		t.Fatalf("results=%+v", results)
	}
}

func TestTinyFishSearchClientCooldownHonorsRetryAfter(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	calls := 0
	client := &TinyFishSearchClient{
		apiKey: "tinyfish-key",
		apiURL: "https://api.search.tinyfish.test",
		httpClient: &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			calls++
			header := make(http.Header)
			header.Set("Retry-After", "90")
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: header, Body: io.NopCloser(strings.NewReader(`{"error":"rate limited"}`))}, nil
		})},
		now: func() time.Time { return now },
	}

	_, err := client.Search(context.Background(), TinyFishSearchRequest{Query: "release notes"})
	var firstErr *tinyFishSearchError
	if !errors.As(err, &firstErr) || firstErr.category != "rate_limited" || firstErr.retryAfter != 90*time.Second {
		t.Fatalf("first error=%#v", err)
	}
	_, err = client.Search(context.Background(), TinyFishSearchRequest{Query: "release notes"})
	var cooldownErr *tinyFishSearchError
	if !errors.As(err, &cooldownErr) || cooldownErr.category != "cooldown" || calls != 1 {
		t.Fatalf("cooldown error=%#v calls=%d", err, calls)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.Search(context.Background(), TinyFishSearchRequest{Query: "release notes"})
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("concurrent cooldown calls reached provider: %d", calls)
	}
	now = now.Add(91 * time.Second)
	_, _ = client.Search(context.Background(), TinyFishSearchRequest{Query: "release notes"})
	if calls != 2 {
		t.Fatalf("calls after cooldown=%d", calls)
	}
}

func TestTinyFishSearchClientSanitizesProviderErrors(t *testing.T) {
	calls := 0
	client := testTinyFishSearchClient(http.StatusUnauthorized, &calls)
	_, err := client.Search(context.Background(), TinyFishSearchRequest{Query: "secret query"})
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(err.Error(), "UNAVAILABLE") || strings.Contains(err.Error(), "secret query") || strings.Contains(err.Error(), "tinyfish-key") {
		t.Fatalf("provider error leaked request details: %v", err)
	}
}

func TestTinyFishSearchClientRejectsMalformedAndOversizedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"results":`},
		{name: "missing results", body: `{"query":"release notes"}`},
		{name: "oversized", body: strings.Repeat("x", maxTinyFishResponseBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &TinyFishSearchClient{
				apiKey: "tinyfish-key",
				apiURL: "https://api.search.tinyfish.test",
				httpClient: &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}, nil
				})},
				now: time.Now,
			}
			_, err := client.Search(context.Background(), TinyFishSearchRequest{Query: "release notes"})
			var responseErr *tinyFishSearchError
			if !errors.As(err, &responseErr) || responseErr.category != "response" {
				t.Fatalf("error=%#v", err)
			}
			_, _ = client.Search(context.Background(), TinyFishSearchRequest{Query: "release notes"})
			if calls != 1 {
				t.Fatalf("provider calls during response cooldown=%d", calls)
			}
		})
	}
}

func TestTinyFishSearchCancellationDoesNotFallBack(t *testing.T) {
	tinyFishCalls := 0
	exaCalls := 0
	client := testTinyFishSearchClient(http.StatusOK, &tinyFishCalls)
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		tinyFishCalls++
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := toolWebSearch(ctx, client, testExaSearchClient(http.StatusOK, &exaCalls), nil, json.RawMessage(`{"query":"release notes"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if exaCalls != 0 {
		t.Fatalf("Exa calls=%d after cancellation", exaCalls)
	}
}

func TestTinyFishRetryAfterIsBounded(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	if got := parseTinyFishRetryAfter("", now); got != time.Minute {
		t.Fatalf("default Retry-After=%s", got)
	}
	if got := parseTinyFishRetryAfter("9999", now); got != 5*time.Minute {
		t.Fatalf("bounded Retry-After=%s", got)
	}
	if got := parseTinyFishRetryAfter("999999999999999999", now); got != 5*time.Minute {
		t.Fatalf("overflow-safe Retry-After=%s", got)
	}
	if got := parseTinyFishRetryAfter("not-a-date", now); got != time.Minute {
		t.Fatalf("invalid Retry-After=%s", got)
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

	_, err := toolFetchURL(context.Background(), client, CallContext{}, json.RawMessage(`{"url":"http://127.0.0.1/private"}`))
	if err == nil || !strings.Contains(err.Error(), "private or local IP") {
		t.Fatalf("expected private-host error, got %v", err)
	}
	if called {
		t.Fatal("fetch_url should reject private hosts before issuing a request")
	}
}

func TestFetchURLSavesPublicResponseInWorkspace(t *testing.T) {
	previous := allowPrivateWebFetchHostsForTests
	allowPrivateWebFetchHostsForTests = true
	defer func() { allowPrivateWebFetchHostsForTests = previous }()

	client := &WebFetchClient{directClient: &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/csv"}},
			Body:       io.NopCloser(strings.NewReader("region,total\nnorth,42\n")),
		}, nil
	})}}
	root := t.TempDir()
	call := CallContext{Run: &agentcore.AgentRun{WorkspaceLease: &agentcore.WorkspaceLease{RootPath: root}}}
	out, err := toolFetchURL(context.Background(), client, call, json.RawMessage(`{"url":"https://data.example/results.csv","output_path":"inputs/results.csv"}`))
	if err != nil {
		t.Fatalf("fetch and save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "inputs", "results.csv"))
	if err != nil || string(data) != "region,total\nnorth,42\n" {
		t.Fatalf("saved response = %q, %v", data, err)
	}
	var response fetchURLToolResponse
	if err := json.Unmarshal(out, &response); err != nil || response.SavedPath != "inputs/results.csv" {
		t.Fatalf("response = %#v, %v", response, err)
	}
}

func TestFetchURLRejectsPrivateRedirect(t *testing.T) {
	client := &WebFetchClient{directClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/start" {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"http://127.0.0.1/private"}},
				Body:       io.NopCloser(strings.NewReader("redirect")),
			}, nil
		}
		t.Fatal("private redirect reached transport")
		return nil, nil
	})}}
	_, err := toolFetchURL(context.Background(), client, CallContext{}, json.RawMessage(`{"url":"http://8.8.8.8/start"}`))
	if err == nil || !strings.Contains(err.Error(), "private or local IP") {
		t.Fatalf("expected private redirect rejection, got %v", err)
	}
}
