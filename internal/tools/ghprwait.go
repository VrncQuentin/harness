package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	ghPRWaitDefaultTimeout = 300  // seconds
	ghPRWaitMaxTimeout     = 1800 // seconds
	ghPRWaitInitialPoll    = 10 * time.Second
	ghPRWaitMaxPoll        = 60 * time.Second
)

// ghAPIBase is the GitHub API root. It is a var so tests can patch it with a
// local httptest.Server without network access.
var ghAPIBase = "https://api.github.com"

// ghPRWaitTool polls the GitHub Checks API until the PR's CI reaches a
// terminal state or the wait ceiling is exceeded.
// Tier-1 treatment: read-only, no approval gate. Uses the network.
type ghPRWaitTool struct{}

var _ Tool = (*ghPRWaitTool)(nil)

func (t *ghPRWaitTool) ID() string { return "gh_pr_wait" }

func (t *ghPRWaitTool) Description() string {
	return "Polls the GitHub Checks API until the PR's CI reaches a terminal state. " +
		"Returns {state: green}, {state: red, failed: [...]}, or {state: timed_out}. " +
		"Requires GITHUB_TOKEN in the environment. Blocks — expected-blocking per schema."
}

func (t *ghPRWaitTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"owner":     map[string]any{"type": "string", "description": "Repository owner (org or username)"},
			"repo":      map[string]any{"type": "string", "description": "Repository name"},
			"pr_number": map[string]any{"type": "integer", "description": "Pull request number"},
			"timeout_seconds": map[string]any{
				"type":               "integer",
				"description":        fmt.Sprintf("Maximum wait time in seconds (default %d, max %d)", ghPRWaitDefaultTimeout, ghPRWaitMaxTimeout),
				"x-expected-blocking": true,
			},
		},
		"required": []string{"owner", "repo", "pr_number"},
	}
}

func (t *ghPRWaitTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	owner, ok := args["owner"].(string)
	if !ok || strings.TrimSpace(owner) == "" {
		return Result{Error: "gh_pr_wait: owner is required"}
	}
	repo, ok := args["repo"].(string)
	if !ok || strings.TrimSpace(repo) == "" {
		return Result{Error: "gh_pr_wait: repo is required"}
	}
	prNum, ok := args["pr_number"].(float64)
	if !ok || prNum <= 0 {
		return Result{Error: "gh_pr_wait: pr_number must be a positive integer"}
	}

	timeoutSec := ghPRWaitDefaultTimeout
	if ts, ok := args["timeout_seconds"].(float64); ok && ts > 0 {
		timeoutSec = int(ts)
		if timeoutSec > ghPRWaitMaxTimeout {
			timeoutSec = ghPRWaitMaxTimeout
		}
	}

	token := ""
	if c.GHTokenFn != nil {
		token = c.GHTokenFn()
	}
	if token == "" {
		return Result{Error: "gh_pr_wait: GITHUB_TOKEN environment variable is not set"}
	}

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	pr := int(prNum)

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	poll := ghPRWaitInitialPoll

	// Resolve PR head SHA once.
	headSHA, err := prHeadSHA(ctx, hc, token, owner, repo, pr)
	if err != nil {
		return Result{Error: "gh_pr_wait: resolve PR head: " + err.Error()}
	}

	for {
		state, failed, err := checkRunsState(ctx, hc, token, owner, repo, headSHA)
		if err != nil {
			return Result{Error: "gh_pr_wait: check runs: " + err.Error()}
		}
		if state == "green" {
			return Result{Content: `{"state":"green"}`}
		}
		if state == "red" {
			out, _ := json.Marshal(map[string]any{"state": "red", "failed": failed})
			return Result{Content: string(out)}
		}
		// state == "pending" — keep waiting
		if time.Now().After(deadline) {
			return Result{Content: `{"state":"timed_out"}`}
		}
		select {
		case <-ctx.Done():
			return Result{Error: "gh_pr_wait: cancelled"}
		case <-time.After(poll):
		}
		poll *= 2
		if poll > ghPRWaitMaxPoll {
			poll = ghPRWaitMaxPoll
		}
	}
}

func prHeadSHA(ctx context.Context, hc *http.Client, token, owner, repo string, pr int) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", ghAPIBase, owner, repo, pr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	setGHHeaders(req, token)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from pulls API", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	var prResp struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &prResp); err != nil {
		return "", err
	}
	if prResp.Head.SHA == "" {
		return "", fmt.Errorf("empty head SHA in API response")
	}
	return prResp.Head.SHA, nil
}

// checkRunsState returns "green", "red", or "pending" plus any failed check names.
func checkRunsState(ctx context.Context, hc *http.Client, token, owner, repo, sha string) (string, []string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs?per_page=100", ghAPIBase, owner, repo, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, err
	}
	setGHHeaders(req, token)
	resp, err := hc.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("HTTP %d from check-runs API", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", nil, err
	}

	var result struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", nil, err
	}
	if len(result.CheckRuns) == 0 {
		return "pending", nil, nil // CI hasn't started yet
	}

	var failed []string
	allDone := true
	for _, cr := range result.CheckRuns {
		if cr.Status != "completed" {
			allDone = false
			continue
		}
		switch cr.Conclusion {
		case "success", "skipped", "neutral":
			// ok
		default:
			failed = append(failed, cr.Name)
		}
	}
	if !allDone {
		return "pending", nil, nil
	}
	if len(failed) > 0 {
		return "red", failed, nil
	}
	return "green", nil, nil
}

func setGHHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}
