package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

const (
	braveSearchAPIURL        = "https://api.search.brave.com/res/v1/web/search"
	exaSearchAPIURL          = "https://api.exa.ai/search"
	tinyFishSearchAPIURL     = "https://api.search.tinyfish.ai"
	defaultWebFetchUserAgent = "AgentRuntime/1.0"
	maxWebFetchBodyBytes     = 2 * 1024 * 1024
	maxWebFetchLinks         = 80
	maxTinyFishResponseBytes = 1024 * 1024
	tinyFishRequestTimeout   = 5 * time.Second
	tinyFishFailureCooldown  = 30 * time.Second
	tinyFishRateCooldown     = time.Minute
	tinyFishAuthCooldown     = 5 * time.Minute
	tinyFishMaxCooldown      = 5 * time.Minute
)

var webFetchHTTPClient = &http.Client{Timeout: 20 * time.Second}
var allowPrivateWebFetchHostsForTests bool

var allowedExaSearchTypes = map[string]struct{}{
	"auto":           {},
	"neural":         {},
	"fast":           {},
	"instant":        {},
	"deep-lite":      {},
	"deep":           {},
	"deep-reasoning": {},
}

var allowedExaCategories = map[string]struct{}{
	"company":          {},
	"research paper":   {},
	"news":             {},
	"personal site":    {},
	"financial report": {},
	"people":           {},
}

type WebSearchClient interface {
	Search(ctx context.Context, query WebSearchQuery) ([]WebSearchResult, error)
}

type WebSearchQuery struct {
	Query           string
	Count           int
	Freshness       string
	DomainAllowlist []string
}

type WebSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet"`
	PublishedAt string `json:"published_at,omitempty"`
}

type BraveSearchClient struct {
	apiKey     string
	httpClient *http.Client
}

type ExaSearchClient struct {
	apiKey     string
	apiURL     string
	httpClient *http.Client
}

type TinyFishSearchClient struct {
	apiKey        string
	apiURL        string
	httpClient    *http.Client
	now           func() time.Time
	cooldownMu    sync.Mutex
	cooldownUntil time.Time
}

type TinyFishSearchRequest struct {
	Query          string
	Location       string
	MaxResults     int
	IncludeDomains []string
	ExcludeDomains []string
}

type tinyFishSearchResponse struct {
	Results *[]struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	} `json:"results"`
}

type tinyFishSearchError struct {
	category   string
	statusCode int
	retryAfter time.Duration
}

func (e *tinyFishSearchError) Error() string {
	if e == nil {
		return "tinyfish search failed"
	}
	if e.statusCode != 0 {
		return fmt.Sprintf("tinyfish search API returned status %d", e.statusCode)
	}
	switch e.category {
	case "cooldown":
		return "tinyfish search is temporarily unavailable"
	case "network":
		return "tinyfish search request failed"
	case "response":
		return "tinyfish search returned an invalid response"
	default:
		return "tinyfish search failed"
	}
}

type WebFetchClient struct {
	directClient *http.Client
	proxyURLs    []*url.URL
	proxyClients []*http.Client
	nextProxy    atomic.Uint64
}

type WebToolsConfig struct {
	BraveSearch    *BraveSearchClient
	ExaSearch      *ExaSearchClient
	TinyFishSearch *TinyFishSearchClient
	WebFetch       *WebFetchClient
}

type ExaSearchRequest struct {
	Query              string             `json:"query"`
	AdditionalQueries  []string           `json:"additionalQueries,omitempty"`
	SystemPrompt       string             `json:"systemPrompt,omitempty"`
	Type               string             `json:"type,omitempty"`
	Category           string             `json:"category,omitempty"`
	UserLocation       string             `json:"userLocation,omitempty"`
	NumResults         int                `json:"numResults,omitempty"`
	IncludeDomains     []string           `json:"includeDomains,omitempty"`
	ExcludeDomains     []string           `json:"excludeDomains,omitempty"`
	StartPublishedDate string             `json:"startPublishedDate,omitempty"`
	EndPublishedDate   string             `json:"endPublishedDate,omitempty"`
	StartCrawlDate     string             `json:"startCrawlDate,omitempty"`
	EndCrawlDate       string             `json:"endCrawlDate,omitempty"`
	Moderation         bool               `json:"moderation,omitempty"`
	Contents           *ExaSearchContents `json:"contents,omitempty"`
	OutputSchema       map[string]any     `json:"outputSchema,omitempty"`
}

type ExaSearchContents struct {
	Text          *ExaTextConfig       `json:"text,omitempty"`
	Highlights    *ExaHighlightsConfig `json:"highlights,omitempty"`
	Summary       *ExaSummaryConfig    `json:"summary,omitempty"`
	MaxAgeHours   *int                 `json:"maxAgeHours,omitempty"`
	Subpages      *int                 `json:"subpages,omitempty"`
	SubpageTarget any                  `json:"subpageTarget,omitempty"`
	Extras        *ExaExtrasConfig     `json:"extras,omitempty"`
}

type ExaTextConfig struct {
	MaxCharacters   int  `json:"maxCharacters,omitempty"`
	IncludeHTMLTags bool `json:"includeHtmlTags,omitempty"`
}

type ExaHighlightsConfig struct {
	MaxCharacters int    `json:"maxCharacters,omitempty"`
	Query         string `json:"query,omitempty"`
}

type ExaSummaryConfig struct {
	Query  string         `json:"query,omitempty"`
	Schema map[string]any `json:"schema,omitempty"`
}

type ExaExtrasConfig struct {
	Links      *int `json:"links,omitempty"`
	ImageLinks *int `json:"imageLinks,omitempty"`
}

type ExaSearchResponse struct {
	RequestID   string            `json:"requestId"`
	SearchType  string            `json:"searchType"`
	Results     []ExaSearchResult `json:"results"`
	Output      *ExaOutput        `json:"output,omitempty"`
	CostDollars map[string]any    `json:"costDollars,omitempty"`
}

type ExaSearchResult struct {
	Title           string            `json:"title"`
	URL             string            `json:"url"`
	ID              string            `json:"id"`
	PublishedDate   *string           `json:"publishedDate,omitempty"`
	Author          *string           `json:"author,omitempty"`
	Image           string            `json:"image,omitempty"`
	Favicon         string            `json:"favicon,omitempty"`
	Text            string            `json:"text,omitempty"`
	Highlights      []string          `json:"highlights,omitempty"`
	HighlightScores []float64         `json:"highlightScores,omitempty"`
	Summary         string            `json:"summary,omitempty"`
	Subpages        []ExaSearchResult `json:"subpages,omitempty"`
	Extras          map[string]any    `json:"extras,omitempty"`
}

type ExaOutput struct {
	Content   any            `json:"content,omitempty"`
	Grounding []ExaGrounding `json:"grounding,omitempty"`
}

type ExaGrounding struct {
	Field      string        `json:"field,omitempty"`
	Citations  []ExaCitation `json:"citations,omitempty"`
	Confidence string        `json:"confidence,omitempty"`
}

type ExaCitation struct {
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

type exaSearchToolInput struct {
	Query              string                  `json:"query"`
	Type               string                  `json:"type"`
	NumResults         int                     `json:"num_results"`
	Category           string                  `json:"category"`
	UserLocation       string                  `json:"user_location"`
	IncludeDomains     []string                `json:"include_domains"`
	ExcludeDomains     []string                `json:"exclude_domains"`
	StartPublishedDate string                  `json:"start_published_date"`
	EndPublishedDate   string                  `json:"end_published_date"`
	StartCrawlDate     string                  `json:"start_crawl_date"`
	EndCrawlDate       string                  `json:"end_crawl_date"`
	AdditionalQueries  []string                `json:"additional_queries"`
	SystemPrompt       string                  `json:"system_prompt"`
	Moderation         bool                    `json:"moderation"`
	Contents           *exaSearchContentsInput `json:"contents"`
	OutputSchema       map[string]any          `json:"output_schema"`
}

type exaSearchContentsInput struct {
	Text          *exaTextInput       `json:"text"`
	Highlights    *exaHighlightsInput `json:"highlights"`
	Summary       *exaSummaryInput    `json:"summary"`
	MaxAgeHours   *int                `json:"max_age_hours"`
	Subpages      *int                `json:"subpages"`
	SubpageTarget any                 `json:"subpage_target"`
	Extras        *exaExtrasInput     `json:"extras"`
}

type exaTextInput struct {
	MaxCharacters   int  `json:"max_characters"`
	IncludeHTMLTags bool `json:"include_html_tags"`
}

type exaHighlightsInput struct {
	MaxCharacters int    `json:"max_characters"`
	Query         string `json:"query"`
}

type exaSummaryInput struct {
	Query  string         `json:"query"`
	Schema map[string]any `json:"schema"`
}

type exaExtrasInput struct {
	Links      *int `json:"links"`
	ImageLinks *int `json:"image_links"`
}

type fetchURLToolInput struct {
	URL           string `json:"url"`
	MaxCharacters int    `json:"max_characters"`
	IncludeHTML   bool   `json:"include_html"`
}

type crawlURLToolInput struct {
	URL                  string   `json:"url"`
	MaxPages             int      `json:"max_pages"`
	MaxDepth             int      `json:"max_depth"`
	MaxCharactersPerPage int      `json:"max_characters_per_page"`
	IncludePatterns      []string `json:"include_patterns"`
	PathKeywords         []string `json:"path_keywords"`
}

type exaToolResponse struct {
	Query       string                  `json:"query"`
	RequestID   string                  `json:"request_id,omitempty"`
	SearchType  string                  `json:"search_type,omitempty"`
	ResultCount int                     `json:"result_count"`
	Results     []exaToolResponseResult `json:"results"`
	Output      *ExaOutput              `json:"output,omitempty"`
	CostDollars map[string]any          `json:"cost_dollars,omitempty"`
}

type exaToolResponseResult struct {
	Title           string                  `json:"title"`
	URL             string                  `json:"url"`
	ID              string                  `json:"id,omitempty"`
	PublishedDate   *string                 `json:"published_date,omitempty"`
	Author          *string                 `json:"author,omitempty"`
	Image           string                  `json:"image,omitempty"`
	Favicon         string                  `json:"favicon,omitempty"`
	Text            string                  `json:"text,omitempty"`
	Highlights      []string                `json:"highlights,omitempty"`
	HighlightScores []float64               `json:"highlight_scores,omitempty"`
	Summary         string                  `json:"summary,omitempty"`
	Subpages        []exaToolResponseResult `json:"subpages,omitempty"`
	Extras          map[string]any          `json:"extras,omitempty"`
}

type fetchURLToolResponse struct {
	URL         string                 `json:"url"`
	FinalURL    string                 `json:"final_url,omitempty"`
	Status      int                    `json:"status"`
	ContentType string                 `json:"content_type,omitempty"`
	Title       string                 `json:"title,omitempty"`
	Description string                 `json:"description,omitempty"`
	Text        string                 `json:"text,omitempty"`
	HTML        string                 `json:"html,omitempty"`
	Links       []webPageLink          `json:"links,omitempty"`
	Truncated   bool                   `json:"truncated,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
}

type crawlURLToolResponse struct {
	URL            string                 `json:"url"`
	MaxPages       int                    `json:"max_pages"`
	MaxDepth       int                    `json:"max_depth"`
	PageCount      int                    `json:"page_count"`
	Pages          []fetchURLToolResponse `json:"pages"`
	SkippedLinks   int                    `json:"skipped_links,omitempty"`
	DiscoveredURLs []string               `json:"discovered_urls,omitempty"`
}

type webPageLink struct {
	Text string `json:"text,omitempty"`
	URL  string `json:"url"`
}

type fetchedWebPage struct {
	response fetchURLToolResponse
	links    []webPageLink
}

func NewBraveSearchClient(apiKey string) *BraveSearchClient {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	return &BraveSearchClient{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func NewExaSearchClient(apiKey string) *ExaSearchClient {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	return &ExaSearchClient{
		apiKey: apiKey,
		apiURL: exaSearchAPIURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func NewTinyFishSearchClient(apiKey string) *TinyFishSearchClient {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	return &TinyFishSearchClient{
		apiKey: apiKey,
		apiURL: tinyFishSearchAPIURL,
		httpClient: &http.Client{
			Timeout: tinyFishRequestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}
}

func NewWebFetchClient(proxyURLs string) *WebFetchClient {
	client := &WebFetchClient{
		directClient: webFetchHTTPClient,
	}
	for _, proxyURL := range parseWebFetchProxyURLs(proxyURLs) {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = http.ProxyURL(proxyURL)
		client.proxyURLs = append(client.proxyURLs, proxyURL)
		client.proxyClients = append(client.proxyClients, &http.Client{
			Timeout:   20 * time.Second,
			Transport: transport,
		})
	}
	return client
}

func RegisterWebToolsFromEnv(r *Registry) {
	RegisterWebTools(r, WebToolsConfig{
		BraveSearch:    NewBraveSearchClient(firstNonEmptyString(os.Getenv("BRAVE_SEARCH_API_KEY"), os.Getenv("BRAVE_API_KEY"))),
		ExaSearch:      NewExaSearchClient(os.Getenv("EXA_API_KEY")),
		TinyFishSearch: NewTinyFishSearchClient(os.Getenv("TINYFISH_API_KEY")),
		WebFetch:       NewWebFetchClient(os.Getenv("WEB_FETCH_PROXY_URLS")),
	})
}

func RegisterWebTools(r *Registry, cfg WebToolsConfig) {
	if r == nil {
		return
	}
	if cfg.TinyFishSearch != nil || cfg.ExaSearch != nil || cfg.BraveSearch != nil {
		r.Register(Definition{
			Name:        "web_search",
			Description: "Search the public web through the automatically selected configured provider. TinyFish is preferred for compatible fast searches, Exa handles advanced searches and is the first fallback, and Brave remains the final compatible fallback. Use fast mode for ordinary lookup and deep mode for neural search, extraction, filters, or synthesized output.",
			Category:    "Web Search",
			InputSchema: webSearchToolSchema(),
			Mutating:    false,
		}, func(ctx context.Context, _ CallContext, input json.RawMessage) (json.RawMessage, error) {
			return toolWebSearch(ctx, cfg.TinyFishSearch, cfg.ExaSearch, cfg.BraveSearch, input)
		})
	}
	if cfg.WebFetch == nil {
		cfg.WebFetch = NewWebFetchClient("")
	}
	r.Register(Definition{
		Name:        "fetch_url",
		Description: fetchURLToolDescription(),
		Category:    "Web Search",
		InputSchema: fetchURLToolSchema(),
		Mutating:    false,
	}, func(ctx context.Context, _ CallContext, input json.RawMessage) (json.RawMessage, error) {
		return toolFetchURL(ctx, cfg.WebFetch, input)
	})
	r.Register(Definition{
		Name:        "crawl_url",
		Description: crawlURLToolDescription(),
		Category:    "Web Search",
		InputSchema: crawlURLToolSchema(),
		Mutating:    false,
	}, func(ctx context.Context, _ CallContext, input json.RawMessage) (json.RawMessage, error) {
		return toolCrawlURL(ctx, cfg.WebFetch, input)
	})
}

type webSearchToolInput struct {
	exaSearchToolInput
	Mode       string `json:"mode"`
	MaxResults int    `json:"max_results"`
	Freshness  string `json:"freshness"`
}

func toolWebSearch(ctx context.Context, tinyFish *TinyFishSearchClient, exa *ExaSearchClient, brave WebSearchClient, input json.RawMessage) (json.RawMessage, error) {
	var params webSearchToolInput
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
	}
	params.Query = strings.TrimSpace(params.Query)
	if params.Query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if params.Mode == "" {
		params.Mode = "fast"
	}
	if params.Mode != "fast" && params.Mode != "deep" {
		return nil, fmt.Errorf("mode must be fast or deep")
	}
	if params.MaxResults == 0 {
		params.MaxResults = 5
	}
	if params.MaxResults < 1 || params.MaxResults > 10 {
		return nil, fmt.Errorf("max_results must be between 1 and 10")
	}
	tinyFishCompatible := webSearchTinyFishCompatible(params)
	braveCompatible := webSearchBraveCompatible(params)
	var tinyFishErr error
	if tinyFish != nil && tinyFishCompatible {
		startedAt := time.Now()
		raw, err := toolWebSearchTinyFish(ctx, tinyFish, params)
		if err == nil {
			return annotateWebSearchProvider(raw, "tinyfish", nil)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		tinyFishErr = err
		if exa != nil {
			logWebSearchFallback(ctx, "tinyfish", "exa", err, time.Since(startedAt))
		} else if brave != nil && braveCompatible {
			logWebSearchFallback(ctx, "tinyfish", "brave", err, time.Since(startedAt))
		}
	}

	if exa != nil {
		if err := applyWebSearchFreshnessToExa(&params, time.Now().UTC()); err != nil {
			return nil, err
		}
		params.Type = "fast"
		if params.Mode == "deep" {
			params.Type = "deep"
		}
		params.NumResults = params.MaxResults
		exaInput, _ := json.Marshal(params.exaSearchToolInput)
		exaStartedAt := time.Now()
		raw, exaErr := toolWebSearchExa(ctx, exa, exaInput)
		if exaErr == nil {
			if tinyFishErr != nil {
				return annotateWebSearchFallback(raw, "exa", "tinyfish")
			}
			return annotateWebSearchProvider(raw, "exa", nil)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if brave == nil || !braveCompatible {
			if tinyFishErr != nil {
				return nil, fmt.Errorf("tinyfish search failed: %v; exa fallback failed: %w", tinyFishErr, exaErr)
			}
			return nil, exaErr
		}
		logWebSearchFallback(ctx, "exa", "brave", exaErr, time.Since(exaStartedAt))
		raw, braveErr := toolWebSearchBrave(ctx, brave, braveWebSearchInput(params))
		if braveErr != nil {
			if tinyFishErr != nil {
				return nil, fmt.Errorf("tinyfish search failed: %v; exa fallback failed: %v; brave fallback failed: %w", tinyFishErr, exaErr, braveErr)
			}
			return nil, fmt.Errorf("exa search failed: %v; brave fallback failed: %w", exaErr, braveErr)
		}
		return annotateWebSearchFallback(raw, "brave", "exa")
	}
	if brave != nil && braveCompatible {
		raw, err := toolWebSearchBrave(ctx, brave, braveWebSearchInput(params))
		if err == nil && tinyFishErr != nil {
			return annotateWebSearchFallback(raw, "brave", "tinyfish")
		}
		if err != nil && tinyFishErr != nil {
			return nil, fmt.Errorf("tinyfish search failed: %v; brave fallback failed: %w", tinyFishErr, err)
		}
		return annotateWebSearchProvider(raw, "brave", err)
	}
	if tinyFishErr != nil {
		return nil, tinyFishErr
	}
	if !tinyFishCompatible && !braveCompatible {
		return nil, fmt.Errorf("web_search deep mode and advanced filters require the Exa provider")
	}
	if tinyFishCompatible && !braveCompatible {
		return nil, fmt.Errorf("web_search location and excluded-domain filters require TinyFish or Exa")
	}
	if !tinyFishCompatible && braveCompatible {
		return nil, fmt.Errorf("web_search freshness filters require Exa or Brave")
	}
	return nil, fmt.Errorf("web_search is not configured on this worker")
}

func webSearchTinyFishCompatible(params webSearchToolInput) bool {
	return params.Mode == "fast" && params.Category == "" && params.Freshness == "" &&
		params.StartPublishedDate == "" && params.EndPublishedDate == "" &&
		params.StartCrawlDate == "" && params.EndCrawlDate == "" && len(params.AdditionalQueries) == 0 &&
		params.SystemPrompt == "" && !params.Moderation && params.Contents == nil && len(params.OutputSchema) == 0
}

func webSearchBraveCompatible(params webSearchToolInput) bool {
	return params.Mode == "fast" && params.Category == "" && params.UserLocation == "" &&
		len(params.ExcludeDomains) == 0 && params.StartPublishedDate == "" && params.EndPublishedDate == "" &&
		params.StartCrawlDate == "" && params.EndCrawlDate == "" && len(params.AdditionalQueries) == 0 &&
		params.SystemPrompt == "" && !params.Moderation && params.Contents == nil && len(params.OutputSchema) == 0
}

func braveWebSearchInput(params webSearchToolInput) json.RawMessage {
	return mustMarshalJSON(map[string]interface{}{
		"query": params.Query, "count": params.MaxResults, "freshness": params.Freshness, "domain_allowlist": params.IncludeDomains,
	})
}

func applyWebSearchFreshnessToExa(params *webSearchToolInput, now time.Time) error {
	freshness := strings.TrimSpace(params.Freshness)
	if freshness == "" {
		return nil
	}
	if strings.TrimSpace(params.StartPublishedDate) != "" {
		return fmt.Errorf("use freshness or start_published_date, not both")
	}
	var start time.Time
	switch freshness {
	case "pd":
		start = now.Add(-24 * time.Hour)
	case "pw":
		start = now.AddDate(0, 0, -7)
	case "pm":
		start = now.AddDate(0, -1, 0)
	case "py":
		start = now.AddDate(-1, 0, 0)
	default:
		return fmt.Errorf("freshness must be one of pd, pw, pm, or py")
	}
	params.StartPublishedDate = start.Format(time.RFC3339)
	return nil
}

func mustMarshalJSON(value interface{}) json.RawMessage {
	payload, _ := json.Marshal(value)
	return payload
}

func annotateWebSearchProvider(raw json.RawMessage, provider string, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	var payload map[string]interface{}
	if json.Unmarshal(raw, &payload) != nil {
		return raw, nil
	}
	payload["provider"] = provider
	return json.Marshal(payload)
}

func annotateWebSearchFallback(raw json.RawMessage, provider, fallbackFrom string) (json.RawMessage, error) {
	annotated, err := annotateWebSearchProvider(raw, provider, nil)
	if err != nil {
		return nil, err
	}
	var payload map[string]interface{}
	if json.Unmarshal(annotated, &payload) != nil {
		return annotated, nil
	}
	payload["fallback_from"] = fallbackFrom
	return json.Marshal(payload)
}

func parseWebFetchProxyURLs(raw string) []*url.URL {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	proxies := make([]*url.URL, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, err := url.Parse(part)
		if err != nil {
			continue
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "socks5" {
			continue
		}
		if strings.TrimSpace(parsed.Host) == "" {
			continue
		}
		proxies = append(proxies, parsed)
	}
	return proxies
}

func (c *WebFetchClient) httpClient() *http.Client {
	if c != nil && len(c.proxyClients) > 0 {
		index := int(c.nextProxy.Add(1)-1) % len(c.proxyClients)
		return c.proxyClients[index]
	}
	if c != nil && c.directClient != nil {
		return c.directClient
	}
	return webFetchHTTPClient
}

func (c *BraveSearchClient) Search(ctx context.Context, query WebSearchQuery) ([]WebSearchResult, error) {
	if c == nil || c.apiKey == "" {
		return nil, fmt.Errorf("brave search is not configured")
	}

	count := query.Count
	if count <= 0 {
		count = 5
	}
	if count > 10 {
		count = 10
	}

	apiURL, err := url.Parse(braveSearchAPIURL)
	if err != nil {
		return nil, fmt.Errorf("parse brave search URL: %w", err)
	}

	params := apiURL.Query()
	params.Set("q", formatWebSearchQuery(query.Query, query.DomainAllowlist))
	params.Set("count", fmt.Sprintf("%d", count))
	if freshness := strings.TrimSpace(query.Freshness); freshness != "" {
		params.Set("freshness", freshness)
	}
	apiURL.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create brave search request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send brave search request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave search API returned status %d", resp.StatusCode)
	}

	var payload struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				Age         string `json:"age"`
				PageAge     string `json:"page_age"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode brave search response: %w", err)
	}

	results := make([]WebSearchResult, 0, len(payload.Web.Results))
	for _, item := range payload.Web.Results {
		title := strings.TrimSpace(item.Title)
		resultURL := strings.TrimSpace(item.URL)
		snippet := strings.TrimSpace(item.Description)
		if title == "" || resultURL == "" {
			continue
		}
		results = append(results, WebSearchResult{
			Title:       title,
			URL:         resultURL,
			Snippet:     snippet,
			PublishedAt: firstNonEmptyString(strings.TrimSpace(item.PageAge), strings.TrimSpace(item.Age)),
		})
	}

	return results, nil
}

func (c *ExaSearchClient) Search(ctx context.Context, query ExaSearchRequest) (*ExaSearchResponse, error) {
	if c == nil || c.apiKey == "" {
		return nil, fmt.Errorf("exa search is not configured")
	}

	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("marshal exa search request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create exa search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send exa search request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			return nil, fmt.Errorf("exa search API returned status %d", resp.StatusCode)
		}
		message := strings.TrimSpace(string(body))
		if message == "" {
			return nil, fmt.Errorf("exa search API returned status %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("exa search API returned status %d: %s", resp.StatusCode, message)
	}

	var payload ExaSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode exa search response: %w", err)
	}
	return &payload, nil
}

func (c *TinyFishSearchClient) Search(ctx context.Context, query TinyFishSearchRequest) ([]WebSearchResult, error) {
	if c == nil || strings.TrimSpace(c.apiKey) == "" {
		return nil, fmt.Errorf("tinyfish search is not configured")
	}
	if remaining := c.cooldownRemaining(); remaining > 0 {
		return nil, &tinyFishSearchError{category: "cooldown", retryAfter: remaining}
	}

	apiURL, err := url.Parse(c.apiURL)
	if err != nil {
		return nil, fmt.Errorf("parse tinyfish search URL: %w", err)
	}
	params := apiURL.Query()
	params.Set("query", formatTinyFishSearchQuery(query.Query, query.IncludeDomains, query.ExcludeDomains))
	if location := strings.ToUpper(strings.TrimSpace(query.Location)); location != "" {
		params.Set("location", location)
	}
	apiURL.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create tinyfish search request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.startCooldown(tinyFishFailureCooldown)
		return nil, &tinyFishSearchError{category: "network", retryAfter: tinyFishFailureCooldown}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		providerErr := tinyFishHTTPError(resp.StatusCode, resp.Header.Get("Retry-After"), c.currentTime())
		if providerErr.retryAfter > 0 {
			c.startCooldown(providerErr.retryAfter)
		}
		return nil, providerErr
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTinyFishResponseBytes+1))
	if err != nil || len(body) > maxTinyFishResponseBytes {
		c.startCooldown(tinyFishFailureCooldown)
		return nil, &tinyFishSearchError{category: "response", retryAfter: tinyFishFailureCooldown}
	}
	var payload tinyFishSearchResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		c.startCooldown(tinyFishFailureCooldown)
		return nil, &tinyFishSearchError{category: "response", retryAfter: tinyFishFailureCooldown}
	}
	if payload.Results == nil {
		c.startCooldown(tinyFishFailureCooldown)
		return nil, &tinyFishSearchError{category: "response", retryAfter: tinyFishFailureCooldown}
	}

	maxResults := query.MaxResults
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 10 {
		maxResults = 10
	}
	results := make([]WebSearchResult, 0, minInt(len(*payload.Results), maxResults))
	for _, item := range *payload.Results {
		title := strings.TrimSpace(item.Title)
		resultURL := strings.TrimSpace(item.URL)
		if title == "" || resultURL == "" {
			continue
		}
		results = append(results, WebSearchResult{
			Title:   title,
			URL:     resultURL,
			Snippet: strings.TrimSpace(item.Snippet),
		})
		if len(results) == maxResults {
			break
		}
	}
	return results, nil
}

func (c *TinyFishSearchClient) currentTime() time.Time {
	if c != nil && c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func (c *TinyFishSearchClient) cooldownRemaining() time.Duration {
	if c == nil {
		return 0
	}
	now := c.currentTime()
	c.cooldownMu.Lock()
	defer c.cooldownMu.Unlock()
	if !c.cooldownUntil.After(now) {
		return 0
	}
	return c.cooldownUntil.Sub(now)
}

func (c *TinyFishSearchClient) startCooldown(duration time.Duration) {
	if c == nil || duration <= 0 {
		return
	}
	until := c.currentTime().Add(duration)
	c.cooldownMu.Lock()
	if until.After(c.cooldownUntil) {
		c.cooldownUntil = until
	}
	c.cooldownMu.Unlock()
}

func tinyFishHTTPError(status int, retryAfter string, now time.Time) *tinyFishSearchError {
	err := &tinyFishSearchError{category: "http", statusCode: status}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		err.category = "authentication"
		err.retryAfter = tinyFishAuthCooldown
	case http.StatusTooManyRequests:
		err.category = "rate_limited"
		err.retryAfter = parseTinyFishRetryAfter(retryAfter, now)
	default:
		if status >= 500 {
			err.category = "server"
			err.retryAfter = tinyFishFailureCooldown
		}
	}
	return err
}

func parseTinyFishRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	duration := tinyFishRateCooldown
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds >= int64(tinyFishMaxCooldown/time.Second) {
			duration = tinyFishMaxCooldown
		} else {
			duration = time.Duration(seconds) * time.Second
		}
	} else if retryAt, err := http.ParseTime(raw); err == nil {
		duration = retryAt.Sub(now)
	}
	if duration < time.Second {
		return time.Second
	}
	if duration > tinyFishMaxCooldown {
		return tinyFishMaxCooldown
	}
	return duration
}

func toolWebSearchTinyFish(ctx context.Context, client *TinyFishSearchClient, params webSearchToolInput) (json.RawMessage, error) {
	if client == nil {
		return nil, fmt.Errorf("tinyfish search is not configured on this worker")
	}
	results, err := client.Search(ctx, TinyFishSearchRequest{
		Query:          params.Query,
		Location:       params.UserLocation,
		MaxResults:     params.MaxResults,
		IncludeDomains: params.IncludeDomains,
		ExcludeDomains: params.ExcludeDomains,
	})
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"query":   params.Query,
		"results": results,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal tinyfish search results: %w", err)
	}
	return payload, nil
}

func formatTinyFishSearchQuery(query string, includeDomains, excludeDomains []string) string {
	formatted := formatWebSearchQuery(query, normalizeDomainList(includeDomains))
	for _, domain := range normalizeDomainList(excludeDomains) {
		formatted += " -site:" + domain
	}
	return strings.TrimSpace(formatted)
}

func logWebSearchFallback(ctx context.Context, provider, fallbackProvider string, err error, elapsed time.Duration) {
	attrs := []any{
		"tool", "web_search",
		"provider", provider,
		"fallback_provider", fallbackProvider,
	}
	level := slog.LevelWarn
	if providerErr, ok := err.(*tinyFishSearchError); ok {
		attrs = append(attrs, "failure_category", providerErr.category)
		if providerErr.statusCode != 0 {
			attrs = append(attrs, "http_status", providerErr.statusCode)
		}
		if providerErr.retryAfter > 0 {
			attrs = append(attrs, "cooldown_seconds", int(providerErr.retryAfter.Seconds()))
		}
		if providerErr.category == "cooldown" {
			level = slog.LevelDebug
		}
	}
	if elapsed > 0 {
		attrs = append(attrs, "elapsed_ms", elapsed.Milliseconds())
	}
	slog.Log(ctx, level, "web search provider fallback", attrs...)
}

func toolWebSearchBrave(ctx context.Context, client WebSearchClient, input json.RawMessage) (json.RawMessage, error) {
	if client == nil {
		return nil, fmt.Errorf("brave search is not configured on this worker")
	}

	var params struct {
		Query           string   `json:"query"`
		Count           int      `json:"count"`
		Freshness       string   `json:"freshness"`
		DomainAllowlist []string `json:"domain_allowlist"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if strings.TrimSpace(params.Query) == "" {
		return nil, fmt.Errorf("query is required")
	}

	results, err := client.Search(ctx, WebSearchQuery{
		Query:           strings.TrimSpace(params.Query),
		Count:           params.Count,
		Freshness:       strings.TrimSpace(params.Freshness),
		DomainAllowlist: params.DomainAllowlist,
	})
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]any{
		"query":   strings.TrimSpace(params.Query),
		"results": results,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal search results: %w", err)
	}
	return payload, nil
}

func toolWebSearchExa(ctx context.Context, client *ExaSearchClient, input json.RawMessage) (json.RawMessage, error) {
	if client == nil {
		return nil, fmt.Errorf("exa search is not configured on this worker")
	}

	var params exaSearchToolInput
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}

	request, err := buildExaSearchRequest(params)
	if err != nil {
		return nil, err
	}

	results, err := client.Search(ctx, request)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(exaToolResponse{
		Query:       request.Query,
		RequestID:   strings.TrimSpace(results.RequestID),
		SearchType:  strings.TrimSpace(results.SearchType),
		ResultCount: len(results.Results),
		Results:     normalizeExaToolResults(results.Results),
		Output:      results.Output,
		CostDollars: results.CostDollars,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal exa search results: %w", err)
	}
	return payload, nil
}

func toolFetchURL(ctx context.Context, client *WebFetchClient, input json.RawMessage) (json.RawMessage, error) {
	var params fetchURLToolInput
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if client == nil {
		client = NewWebFetchClient("")
	}
	page, err := client.Fetch(ctx, params.URL, clampWebFetchCharacters(params.MaxCharacters), params.IncludeHTML)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(page.response)
	if err != nil {
		return nil, fmt.Errorf("marshal fetch_url result: %w", err)
	}
	return payload, nil
}

func toolCrawlURL(ctx context.Context, client *WebFetchClient, input json.RawMessage) (json.RawMessage, error) {
	var params crawlURLToolInput
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if client == nil {
		client = NewWebFetchClient("")
	}
	root, err := parseAllowedWebFetchURL(params.URL)
	if err != nil {
		return nil, err
	}
	maxPages := clampCrawlMaxPages(params.MaxPages)
	maxDepth := clampCrawlMaxDepth(params.MaxDepth)
	maxChars := clampCrawlPageCharacters(params.MaxCharactersPerPage)
	includePatterns := normalizeCrawlPatterns(params.IncludePatterns)
	pathKeywords := normalizeCrawlPatterns(params.PathKeywords)
	if len(pathKeywords) == 0 {
		pathKeywords = defaultCrawlPathKeywords()
	}

	type queuedURL struct {
		url   string
		depth int
	}
	queue := []queuedURL{{url: root.String(), depth: 0}}
	seen := map[string]bool{canonicalWebURL(root): true}
	pages := make([]fetchURLToolResponse, 0, maxPages)
	discovered := make([]string, 0)
	skipped := 0

	for len(queue) > 0 && len(pages) < maxPages {
		next := queue[0]
		queue = queue[1:]
		page, err := client.Fetch(ctx, next.url, maxChars, false)
		if err != nil {
			skipped++
			continue
		}
		if page.response.Status >= http.StatusBadRequest {
			skipped++
			continue
		}
		pages = append(pages, page.response)
		if next.depth >= maxDepth {
			continue
		}
		candidates := filterCrawlLinks(root, page.links, includePatterns, pathKeywords)
		for _, link := range candidates {
			parsed, err := parseAllowedWebFetchURL(link.URL)
			if err != nil || !sameWebHost(root, parsed) {
				skipped++
				continue
			}
			key := canonicalWebURL(parsed)
			if seen[key] {
				continue
			}
			seen[key] = true
			queue = append(queue, queuedURL{url: parsed.String(), depth: next.depth + 1})
			if len(discovered) < maxWebFetchLinks {
				discovered = append(discovered, parsed.String())
			}
		}
	}

	result := crawlURLToolResponse{
		URL:            root.String(),
		MaxPages:       maxPages,
		MaxDepth:       maxDepth,
		PageCount:      len(pages),
		Pages:          pages,
		SkippedLinks:   skipped,
		DiscoveredURLs: discovered,
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal crawl_url result: %w", err)
	}
	return payload, nil
}

func webSearchBraveToolDescription() string {
	return "Search the public web with Brave Search. Use this for market context, standards, competitors, and external evidence. Returns normalized JSON results."
}

func webSearchToolSchema() map[string]interface{} {
	schema := webSearchExaToolSchema()
	properties := schema["properties"].(map[string]interface{})
	delete(properties, "type")
	delete(properties, "num_results")
	properties["mode"] = map[string]interface{}{
		"type": "string", "enum": []string{"fast", "deep"},
		"description": "Search mode. fast is the default; deep enables neural search and advanced extraction.",
	}
	properties["max_results"] = map[string]interface{}{
		"type": "integer", "minimum": 1, "maximum": 10,
		"description": "Maximum results. Defaults to 5.",
	}
	properties["freshness"] = map[string]interface{}{
		"type": "string", "description": "Optional fast-search freshness hint such as pd, pw, pm, or py.",
	}
	schema["additionalProperties"] = false
	return schema
}

func webSearchBraveToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Search query to run",
			},
			"count": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of results to return (default 5, max 10)",
			},
			"freshness": map[string]interface{}{
				"type":        "string",
				"description": "Optional freshness hint such as pd, pw, pm, or py",
			},
			"domain_allowlist": map[string]interface{}{
				"type":        "array",
				"description": "Optional list of domains to prioritize",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
		},
		"required": []string{"query"},
	}
}

func fetchURLToolDescription() string {
	return "Fetch a specific public URL and return extracted title, description, readable text, and links. Use this after finding or knowing an exact changelog, release notes, blog, docs, or source URL."
}

func fetchURLToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "Public http(s) URL to fetch directly.",
			},
			"max_characters": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum extracted text characters to return. Defaults to 12000, max 30000.",
			},
			"include_html": map[string]interface{}{
				"type":        "boolean",
				"description": "When true, include a truncated raw HTML excerpt. Prefer false unless structure matters.",
			},
		},
		"required": []string{"url"},
	}
}

func crawlURLToolDescription() string {
	return "Crawl a small number of same-host public pages from a starting URL, prioritizing changelog, release notes, updates, announcements, docs, and roadmap paths. Use this to discover official update pages when search results are thin."
}

func crawlURLToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "Public http(s) URL to start crawling from.",
			},
			"max_pages": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum pages to fetch. Defaults to 8, max 20.",
			},
			"max_depth": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum same-host link depth. Defaults to 1, max 2.",
			},
			"max_characters_per_page": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum extracted text characters per page. Defaults to 6000, max 15000.",
			},
			"include_patterns": map[string]interface{}{
				"type":        "array",
				"description": "Optional URL/text substrings to include, such as changelog or release-notes.",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"path_keywords": map[string]interface{}{
				"type":        "array",
				"description": "Optional path/link-text keywords to prioritize. Defaults to common update-page keywords.",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
		},
		"required": []string{"url"},
	}
}

func webSearchExaToolDescription() string {
	return "Search the web with Exa's neural search engine. Supports category filters, semantic search types, content extraction, domain filters, freshness controls, and synthesized structured output. Returns JSON results with titles, URLs, and extracted content."
}

func webSearchExaToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Natural-language search query",
			},
			"type": map[string]interface{}{
				"type":        "string",
				"description": "Search type: auto (default), neural, fast, instant, deep-lite, deep, or deep-reasoning",
				"enum":        []string{"auto", "neural", "fast", "instant", "deep-lite", "deep", "deep-reasoning"},
			},
			"num_results": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of results to return (default 5, max 10 in this tool)",
			},
			"category": map[string]interface{}{
				"type":        "string",
				"description": "Optional category: company, research paper, news, personal site, financial report, or people",
				"enum":        []string{"company", "research paper", "news", "personal site", "financial report", "people"},
			},
			"user_location": map[string]interface{}{
				"type":        "string",
				"description": "Optional two-letter ISO country code such as US to bias results geographically",
			},
			"include_domains": map[string]interface{}{
				"type":        "array",
				"description": "Only return results from these domains",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"exclude_domains": map[string]interface{}{
				"type":        "array",
				"description": "Exclude results from these domains. Not supported for company or people categories",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"start_published_date": map[string]interface{}{
				"type":        "string",
				"description": "Only return links published after this ISO 8601 timestamp",
			},
			"end_published_date": map[string]interface{}{
				"type":        "string",
				"description": "Only return links published before this ISO 8601 timestamp",
			},
			"start_crawl_date": map[string]interface{}{
				"type":        "string",
				"description": "Only return links crawled after this ISO 8601 timestamp",
			},
			"end_crawl_date": map[string]interface{}{
				"type":        "string",
				"description": "Only return links crawled before this ISO 8601 timestamp",
			},
			"additional_queries": map[string]interface{}{
				"type":        "array",
				"description": "Optional extra query variations for deep-search modes",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"system_prompt": map[string]interface{}{
				"type":        "string",
				"description": "Optional synthesis instructions for output shaping and source preferences",
			},
			"moderation": map[string]interface{}{
				"type":        "boolean",
				"description": "Filter unsafe content from results",
			},
			"output_schema": map[string]interface{}{
				"type":        "object",
				"description": "Optional JSON schema for synthesized output.content",
			},
			"contents": map[string]interface{}{
				"type":        "object",
				"description": "Optional content extraction settings. Choose exactly one of text, highlights, or summary. Defaults to highlights with max_characters 4000 when omitted.",
				"properties": map[string]interface{}{
					"text": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"max_characters":    map[string]interface{}{"type": "integer"},
							"include_html_tags": map[string]interface{}{"type": "boolean"},
						},
					},
					"highlights": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"max_characters": map[string]interface{}{"type": "integer"},
							"query":          map[string]interface{}{"type": "string"},
						},
					},
					"summary": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"query":  map[string]interface{}{"type": "string"},
							"schema": map[string]interface{}{"type": "object"},
						},
					},
					"max_age_hours": map[string]interface{}{
						"type":        "integer",
						"description": "0 always livecrawls, -1 uses cache only",
					},
					"subpages": map[string]interface{}{
						"type":        "integer",
						"description": "Optional number of subpages to crawl per result",
					},
					"subpage_target": map[string]interface{}{
						"description": "Optional keyword or structure used to prioritize subpages",
					},
					"extras": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"links":       map[string]interface{}{"type": "integer"},
							"image_links": map[string]interface{}{"type": "integer"},
						},
					},
				},
			},
		},
		"required": []string{"query"},
	}
}

func formatWebSearchQuery(query string, domainAllowlist []string) string {
	query = strings.TrimSpace(query)
	if len(domainAllowlist) == 0 {
		return query
	}

	siteTerms := make([]string, 0, len(domainAllowlist))
	for _, domain := range domainAllowlist {
		domain = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(domain, "https://"), "http://"))
		domain = strings.TrimSuffix(domain, "/")
		if domain == "" {
			continue
		}
		siteTerms = append(siteTerms, "site:"+domain)
	}
	if len(siteTerms) == 0 {
		return query
	}

	return fmt.Sprintf("(%s) %s", strings.Join(siteTerms, " OR "), query)
}

func buildExaSearchRequest(input exaSearchToolInput) (ExaSearchRequest, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return ExaSearchRequest{}, fmt.Errorf("query is required")
	}

	request := ExaSearchRequest{
		Query:              query,
		AdditionalQueries:  normalizeStringList(input.AdditionalQueries),
		SystemPrompt:       strings.TrimSpace(input.SystemPrompt),
		Type:               firstNonEmptyString(strings.TrimSpace(input.Type), "auto"),
		Category:           strings.TrimSpace(input.Category),
		UserLocation:       strings.ToUpper(strings.TrimSpace(input.UserLocation)),
		NumResults:         clampExaNumResults(input.NumResults),
		IncludeDomains:     normalizeDomainList(input.IncludeDomains),
		ExcludeDomains:     normalizeDomainList(input.ExcludeDomains),
		StartPublishedDate: strings.TrimSpace(input.StartPublishedDate),
		EndPublishedDate:   strings.TrimSpace(input.EndPublishedDate),
		StartCrawlDate:     strings.TrimSpace(input.StartCrawlDate),
		EndCrawlDate:       strings.TrimSpace(input.EndCrawlDate),
		Moderation:         input.Moderation,
		OutputSchema:       normalizeObject(input.OutputSchema),
		Contents:           buildExaContents(input.Contents),
	}

	if err := validateExaSearchRequest(request); err != nil {
		return ExaSearchRequest{}, err
	}
	return request, nil
}

func buildExaContents(input *exaSearchContentsInput) *ExaSearchContents {
	if input == nil {
		return defaultExaContents()
	}

	contents := &ExaSearchContents{
		MaxAgeHours:   input.MaxAgeHours,
		Subpages:      input.Subpages,
		SubpageTarget: input.SubpageTarget,
	}
	if input.Text != nil {
		contents.Text = &ExaTextConfig{
			MaxCharacters:   input.Text.MaxCharacters,
			IncludeHTMLTags: input.Text.IncludeHTMLTags,
		}
		if contents.Text.MaxCharacters <= 0 {
			contents.Text.MaxCharacters = 10000
		}
	}
	if input.Highlights != nil {
		contents.Highlights = &ExaHighlightsConfig{
			MaxCharacters: input.Highlights.MaxCharacters,
			Query:         strings.TrimSpace(input.Highlights.Query),
		}
		if contents.Highlights.MaxCharacters <= 0 {
			contents.Highlights.MaxCharacters = 4000
		}
	}
	if input.Summary != nil {
		contents.Summary = &ExaSummaryConfig{
			Query:  strings.TrimSpace(input.Summary.Query),
			Schema: normalizeObject(input.Summary.Schema),
		}
	}
	if input.Extras != nil {
		contents.Extras = &ExaExtrasConfig{
			Links:      input.Extras.Links,
			ImageLinks: input.Extras.ImageLinks,
		}
	}

	if contents.Text == nil && contents.Highlights == nil && contents.Summary == nil {
		contents.Highlights = &ExaHighlightsConfig{MaxCharacters: 4000}
	}
	return contents
}

func defaultExaContents() *ExaSearchContents {
	return &ExaSearchContents{
		Highlights: &ExaHighlightsConfig{
			MaxCharacters: 4000,
		},
	}
}

func validateExaSearchRequest(request ExaSearchRequest) error {
	if _, ok := allowedExaSearchTypes[request.Type]; !ok {
		return fmt.Errorf("type must be one of auto, neural, fast, instant, deep-lite, deep, or deep-reasoning")
	}

	if request.Category != "" {
		if _, ok := allowedExaCategories[request.Category]; !ok {
			return fmt.Errorf("category must be one of company, research paper, news, personal site, financial report, or people")
		}
	}

	if err := validateExaContents(request.Contents); err != nil {
		return err
	}

	switch request.Category {
	case "company", "people":
		if len(request.ExcludeDomains) > 0 {
			return fmt.Errorf("exclude_domains is not supported for category %q", request.Category)
		}
		if request.StartPublishedDate != "" || request.EndPublishedDate != "" || request.StartCrawlDate != "" || request.EndCrawlDate != "" {
			return fmt.Errorf("published and crawl date filters are not supported for category %q", request.Category)
		}
	}

	if request.Category == "people" {
		for _, domain := range request.IncludeDomains {
			if !isLinkedInDomain(domain) {
				return fmt.Errorf("include_domains only supports LinkedIn domains for category %q", request.Category)
			}
		}
	}

	return nil
}

func validateExaContents(contents *ExaSearchContents) error {
	if contents == nil {
		return nil
	}

	contentModeCount := 0
	if contents.Text != nil {
		contentModeCount++
	}
	if contents.Highlights != nil {
		contentModeCount++
	}
	if contents.Summary != nil {
		contentModeCount++
	}
	if contentModeCount > 1 {
		return fmt.Errorf("contents must specify only one of text, highlights, or summary")
	}

	return nil
}

func normalizeStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeDomainList(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized := normalizeDomain(value)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeDomain(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "https://")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if slash := strings.Index(trimmed, "/"); slash >= 0 {
		trimmed = trimmed[:slash]
	}
	return strings.TrimSpace(trimmed)
}

func fetchWebPage(ctx context.Context, rawURL string, maxCharacters int, includeHTML bool) (*fetchedWebPage, error) {
	return NewWebFetchClient("").Fetch(ctx, rawURL, maxCharacters, includeHTML)
}

func (c *WebFetchClient) Fetch(ctx context.Context, rawURL string, maxCharacters int, includeHTML bool) (*fetchedWebPage, error) {
	parsed, err := parseAllowedWebFetchURL(rawURL)
	if err != nil {
		return nil, err
	}
	if err := validateWebFetchHost(ctx, parsed.Hostname()); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build fetch_url request: %w", err)
	}
	req.Header.Set("User-Agent", defaultWebFetchUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,text/plain;q=0.7,*/*;q=0.1")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request fetch_url: %w", err)
	}
	defer resp.Body.Close()

	finalURL := parsed.String()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	mediaType := ""
	if contentType != "" {
		mediaType, _, _ = mime.ParseMediaType(contentType)
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxWebFetchBodyBytes))
	if readErr != nil {
		return nil, fmt.Errorf("read fetch_url response: %w", readErr)
	}
	decoded := body
	if reader, err := charset.NewReader(bytes.NewReader(body), contentType); err == nil {
		if converted, err := io.ReadAll(io.LimitReader(reader, maxWebFetchBodyBytes)); err == nil {
			decoded = converted
		}
	}

	result := fetchURLToolResponse{
		URL:         parsed.String(),
		FinalURL:    finalURL,
		Status:      resp.StatusCode,
		ContentType: contentType,
		Metadata: map[string]interface{}{
			"bytes_read": len(body),
		},
	}

	content := string(decoded)
	switch {
	case mediaType == "" || strings.HasPrefix(mediaType, "text/html") || strings.Contains(strings.ToLower(content[:minInt(len(content), 512)]), "<html"):
		doc, err := html.Parse(strings.NewReader(content))
		if err != nil {
			return nil, fmt.Errorf("parse fetch_url html: %w", err)
		}
		title, description, text, links := extractFetchedHTML(parsed, doc)
		result.Title = title
		result.Description = description
		result.Text, result.Truncated = truncateWithFlag(text, maxCharacters)
		result.Links = limitWebPageLinks(links, maxWebFetchLinks)
		if includeHTML {
			result.HTML, _ = truncateWithFlag(content, 20000)
		}
		return &fetchedWebPage{response: result, links: links}, nil
	case strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "json") || strings.Contains(mediaType, "xml"):
		result.Text, result.Truncated = truncateWithFlag(normalizeFetchedWhitespace(content), maxCharacters)
		return &fetchedWebPage{response: result}, nil
	default:
		result.Metadata["unsupported_media_type"] = mediaType
		return &fetchedWebPage{response: result}, nil
	}
}

func parseAllowedWebFetchURL(rawURL string) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, fmt.Errorf("url is required")
	}
	if !strings.Contains(trimmed, "://") && strings.Contains(trimmed, ".") && !strings.ContainsAny(trimmed, " \t\r\n") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("url must use http or https")
	}
	if strings.TrimSpace(parsed.Hostname()) == "" {
		return nil, fmt.Errorf("url host is required")
	}
	parsed.Fragment = ""
	return parsed, nil
}

func validateWebFetchHost(ctx context.Context, host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("url host is required")
	}
	lowerHost := strings.ToLower(host)
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".local") {
		return fmt.Errorf("fetch_url cannot access localhost or .local hosts")
	}
	if allowPrivateWebFetchHostsForTests {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateWebFetchIP(ip) {
			return fmt.Errorf("fetch_url cannot access private or local IP addresses")
		}
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return fmt.Errorf("resolve fetch_url host: %w", err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve fetch_url host: no addresses")
	}
	for _, addr := range addrs {
		if isPrivateWebFetchIP(addr.IP) {
			return fmt.Errorf("fetch_url cannot access hosts resolving to private or local IP addresses")
		}
	}
	return nil
}

func isPrivateWebFetchIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

func extractFetchedHTML(baseURL *url.URL, doc *html.Node) (string, string, string, []webPageLink) {
	var title string
	var description string
	textParts := make([]string, 0, 256)
	links := make([]webPageLink, 0, 64)
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, hidden bool) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Noscript, atom.Svg:
				hidden = true
			case atom.Title:
				title = firstNonEmptyString(title, normalizeFetchedWhitespace(nodeText(n)))
				hidden = true
			case atom.Meta:
				name := strings.ToLower(strings.TrimSpace(htmlNodeAttr(n, "name")))
				property := strings.ToLower(strings.TrimSpace(htmlNodeAttr(n, "property")))
				if name == "description" || property == "og:description" || property == "twitter:description" {
					description = firstNonEmptyString(description, normalizeFetchedWhitespace(htmlNodeAttr(n, "content")))
				}
			case atom.A:
				if href := strings.TrimSpace(htmlNodeAttr(n, "href")); href != "" {
					if resolved := resolveWebLink(baseURL, href); resolved != "" {
						links = append(links, webPageLink{
							Text: normalizeFetchedWhitespace(nodeText(n)),
							URL:  resolved,
						})
					}
				}
			}
		}
		if n.Type == html.TextNode && !hidden {
			if text := normalizeFetchedWhitespace(n.Data); text != "" {
				textParts = append(textParts, text)
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child, hidden)
		}
	}
	walk(doc, false)
	return title, description, normalizeFetchedWhitespace(strings.Join(textParts, " ")), dedupeWebPageLinks(links)
}

func nodeText(n *html.Node) string {
	var parts []string
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current == nil {
			return
		}
		if current.Type == html.TextNode {
			parts = append(parts, current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.Join(parts, " ")
}

func htmlNodeAttr(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, key) {
			return strings.TrimSpace(attr.Val)
		}
	}
	return ""
}

func resolveWebLink(baseURL *url.URL, href string) string {
	if baseURL == nil {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return ""
	}
	resolved := baseURL.ResolveReference(parsed)
	if resolved == nil || (resolved.Scheme != "http" && resolved.Scheme != "https") {
		return ""
	}
	resolved.Fragment = ""
	return resolved.String()
}

func dedupeWebPageLinks(links []webPageLink) []webPageLink {
	if len(links) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(links))
	out := make([]webPageLink, 0, len(links))
	for _, link := range links {
		link.URL = strings.TrimSpace(link.URL)
		link.Text = truncateString(normalizeFetchedWhitespace(link.Text), 120)
		if link.URL == "" || seen[link.URL] {
			continue
		}
		seen[link.URL] = true
		out = append(out, link)
	}
	return out
}

func limitWebPageLinks(links []webPageLink, limit int) []webPageLink {
	if limit <= 0 || len(links) <= limit {
		return links
	}
	return append([]webPageLink(nil), links[:limit]...)
}

func filterCrawlLinks(root *url.URL, links []webPageLink, includePatterns, pathKeywords []string) []webPageLink {
	if len(links) == 0 {
		return nil
	}
	scored := make([]struct {
		link  webPageLink
		score int
	}, 0, len(links))
	for _, link := range links {
		parsed, err := parseAllowedWebFetchURL(link.URL)
		if err != nil || !sameWebHost(root, parsed) {
			continue
		}
		haystack := strings.ToLower(parsed.Path + " " + parsed.RawQuery + " " + link.Text)
		score := 0
		for _, pattern := range includePatterns {
			if pattern != "" && strings.Contains(haystack, pattern) {
				score += 10
			}
		}
		for _, keyword := range pathKeywords {
			if keyword != "" && strings.Contains(haystack, keyword) {
				score += 5
			}
		}
		if score == 0 && len(includePatterns) > 0 {
			continue
		}
		if score == 0 {
			score = 1
		}
		scored = append(scored, struct {
			link  webPageLink
			score int
		}{link: webPageLink{Text: link.Text, URL: parsed.String()}, score: score})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score == scored[j].score {
			return scored[i].link.URL < scored[j].link.URL
		}
		return scored[i].score > scored[j].score
	})
	out := make([]webPageLink, 0, len(scored))
	for _, item := range scored {
		out = append(out, item.link)
	}
	return out
}

func sameWebHost(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Hostname(), b.Hostname())
}

func canonicalWebURL(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}
	clone := *parsed
	clone.Fragment = ""
	if clone.Path == "" {
		clone.Path = "/"
	}
	return strings.ToLower(clone.String())
}

func normalizeCrawlPatterns(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func defaultCrawlPathKeywords() []string {
	return []string{"changelog", "release", "releases", "release-notes", "updates", "whats-new", "what's new", "announcements", "roadmap", "docs", "blog"}
}

func clampWebFetchCharacters(value int) int {
	switch {
	case value <= 0:
		return 12000
	case value > 30000:
		return 30000
	default:
		return value
	}
}

func clampCrawlPageCharacters(value int) int {
	switch {
	case value <= 0:
		return 6000
	case value > 15000:
		return 15000
	default:
		return value
	}
}

func clampCrawlMaxPages(value int) int {
	switch {
	case value <= 0:
		return 8
	case value > 20:
		return 20
	default:
		return value
	}
}

func clampCrawlMaxDepth(value int) int {
	switch {
	case value <= 0:
		return 1
	case value > 2:
		return 2
	default:
		return value
	}
}

func normalizeFetchedWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateWithFlag(value string, limit int) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if limit <= 0 || len(trimmed) <= limit {
		return trimmed, false
	}
	return strings.TrimSpace(trimmed[:limit]), true
}

func truncateString(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return strings.TrimSpace(value[:limit])
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isLinkedInDomain(value string) bool {
	normalized := normalizeDomain(value)
	return normalized == "linkedin.com" || strings.HasSuffix(normalized, ".linkedin.com")
}

func normalizeObject(value map[string]any) map[string]any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func clampExaNumResults(value int) int {
	switch {
	case value <= 0:
		return 5
	case value > 10:
		return 10
	default:
		return value
	}
}

func normalizeExaToolResults(results []ExaSearchResult) []exaToolResponseResult {
	if len(results) == 0 {
		return []exaToolResponseResult{}
	}

	out := make([]exaToolResponseResult, 0, len(results))
	for _, result := range results {
		item := exaToolResponseResult{
			Title:           strings.TrimSpace(result.Title),
			URL:             strings.TrimSpace(result.URL),
			ID:              strings.TrimSpace(result.ID),
			PublishedDate:   result.PublishedDate,
			Author:          result.Author,
			Image:           strings.TrimSpace(result.Image),
			Favicon:         strings.TrimSpace(result.Favicon),
			Text:            strings.TrimSpace(result.Text),
			Highlights:      result.Highlights,
			HighlightScores: result.HighlightScores,
			Summary:         strings.TrimSpace(result.Summary),
			Extras:          result.Extras,
		}
		if item.Title == "" || item.URL == "" {
			continue
		}
		if len(result.Subpages) > 0 {
			item.Subpages = normalizeExaToolResults(result.Subpages)
		}
		out = append(out, item)
	}
	return out
}
