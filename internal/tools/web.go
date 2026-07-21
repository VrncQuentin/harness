package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type webSearchTool struct{}

var _ Tool = (*webSearchTool)(nil)

func (t *webSearchTool) ID() string { return "web_search" }
func (t *webSearchTool) Description() string {
	return "Search the web using a network request. Returns short result titles, URLs, and snippets."
}
func (t *webSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query to send over the network",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results to return, from 1 to 5",
			},
		},
		"required": []string{"query"},
	}
}

func (t *webSearchTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	query, ok := args["query"].(string)
	query = strings.TrimSpace(query)
	if !ok || query == "" {
		return Result{Error: "web_search: missing or invalid query argument"}
	}
	maxResults := 3
	if raw, ok := args["max_results"].(float64); ok {
		maxResults = min(max(int(raw), 1), 5)
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	reqURL := "https://api.duckduckgo.com/?" + url.Values{
		"q":             []string{query},
		"format":        []string{"json"},
		"no_html":       []string{"1"},
		"skip_disambig": []string{"1"},
		"no_redirect":   []string{"1"},
		"t":             []string{"harness"},
		"pretty":        []string{"0"},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return Result{Error: fmt.Sprintf("web_search: build request: %v", err)}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "harness-local/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return Result{Error: fmt.Sprintf("web_search: network request failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{Error: fmt.Sprintf("web_search: network request returned HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{Error: fmt.Sprintf("web_search: read response: %v", err)}
	}
	results, err := parseDuckDuckGoResults(body, maxResults)
	if err != nil {
		return Result{Error: fmt.Sprintf("web_search: parse response: %v", err)}
	}
	if len(results) == 0 {
		return Result{Content: fmt.Sprintf("Network request used for web_search query %q.\nNo results returned.", query)}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Network request used for web_search query %q.\n", query)
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n%s\n%s\n", i+1, r.Title, r.URL, r.Snippet)
	}
	return Result{Content: strings.TrimSpace(b.String())}
}

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

func parseDuckDuckGoResults(body []byte, maxResults int) ([]searchResult, error) {
	var payload struct {
		AbstractText   string `json:"AbstractText"`
		AbstractSource string `json:"AbstractSource"`
		AbstractURL    string `json:"AbstractURL"`
		Heading        string `json:"Heading"`
		RelatedTopics  []struct {
			FirstURL string `json:"FirstURL"`
			Text     string `json:"Text"`
			Topics   []struct {
				FirstURL string `json:"FirstURL"`
				Text     string `json:"Text"`
			} `json:"Topics"`
		} `json:"RelatedTopics"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	results := make([]searchResult, 0, maxResults)
	if payload.AbstractText != "" || payload.AbstractURL != "" {
		title := payload.Heading
		if title == "" {
			title = payload.AbstractSource
		}
		addSearchResult(&results, maxResults, title, payload.AbstractURL, payload.AbstractText)
	}
	for _, topic := range payload.RelatedTopics {
		addSearchResult(&results, maxResults, topicTitle(topic.Text), topic.FirstURL, topic.Text)
		for _, child := range topic.Topics {
			addSearchResult(&results, maxResults, topicTitle(child.Text), child.FirstURL, child.Text)
		}
		if len(results) >= maxResults {
			break
		}
	}
	return results, nil
}

func addSearchResult(results *[]searchResult, maxResults int, title, resultURL, snippet string) {
	if len(*results) >= maxResults || (strings.TrimSpace(title) == "" && strings.TrimSpace(snippet) == "") {
		return
	}
	*results = append(*results, searchResult{
		Title:   fallback(strings.TrimSpace(title), "Untitled result"),
		URL:     strings.TrimSpace(resultURL),
		Snippet: strings.TrimSpace(snippet),
	})
}

func topicTitle(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if i := strings.Index(text, " - "); i > 0 {
		return text[:i]
	}
	if len(text) > 80 {
		return text[:80] + "..."
	}
	return text
}

func fallback(value, def string) string {
	if value == "" {
		return def
	}
	return value
}
