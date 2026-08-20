// Package github is a small client for the parts of GitHub's REST API needed to
// observe a repository's runner pool and its outstanding Actions work.
//
// The autoscaler's primary input is webhooks. This package exists to provide a
// second, authoritative input: webhooks can be dropped (GitHub does not retry a
// failed delivery), and a replica can exist without a live runner inside it. In
// both cases the in-memory view drifts from reality and nothing in the
// webhook path can notice.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is GitHub's public REST API.
const DefaultEndpoint = "https://api.github.com"

const (
	maxRespBodyRead = 4 << 20 // 4MiB
	perPage         = 100
	// maxRunPages bounds the queued/in-progress run sweep. A repository with
	// more outstanding runs than this is already far past MaxRunners, so the
	// scale decision is unaffected by the tail.
	maxRunPages = 3
)

// Client talks to GitHub's REST API for a single repository or organization.
type Client struct {
	Token string
	// Owner and Repo identify the repository at repo scope. Empty at org scope.
	Owner string
	Repo  string
	// Org identifies the organization at org scope; non-empty means the
	// client reads the org-level runner registrations instead of a repo's.
	Org      string
	Endpoint string
	HTTP     *http.Client
	Logger   *slog.Logger
}

// NewClient returns a Client scoped to owner/repo.
func NewClient(token, owner, repo string, logger *slog.Logger) *Client {
	return &Client{
		Token:    token,
		Owner:    owner,
		Repo:     repo,
		Endpoint: DefaultEndpoint,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Logger:   logger,
	}
}

// NewOrgClient returns a Client scoped to an organization's runner pool.
func NewOrgClient(token, org string, logger *slog.Logger) *Client {
	return &Client{
		Token:    token,
		Org:      org,
		Endpoint: DefaultEndpoint,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Logger:   logger,
	}
}

// OrgScoped reports whether the client reads org-level registrations.
func (c *Client) OrgScoped() bool { return c.Org != "" }

// SplitRepository splits an "owner/repo" string.
func SplitRepository(s string) (owner, repo string, err error) {
	owner, repo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || owner == "" || repo == "" {
		return "", "", fmt.Errorf("expected owner/repo, got %q", s)
	}
	return owner, repo, nil
}

// get issues a GET and decodes the JSON body into out.
//
// Unlike the Railway client this does not retry. Every call site runs on the
// resync ticker, so a transient failure is retried by the next tick a few
// seconds later; burning attempts inside a reconcile would only delay it.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.Endpoint + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github api transport: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBodyRead))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// 403 is also how GitHub reports a spent rate limit; surface the
		// remaining quota so the cause is obvious from the log line.
		return fmt.Errorf("github api %d (check GITHUB_API_TOKEN scopes; rate limit remaining %q): %s",
			resp.StatusCode, resp.Header.Get("X-RateLimit-Remaining"), truncate(body, 200))
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("github api %d: %s", resp.StatusCode, truncate(body, 200))
	}

	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}

// Runner is a self-hosted runner registered against the repository.
type Runner struct {
	ID     int64
	Name   string
	Status string // "online" or "offline"
	Busy   bool
	Labels []string
}

// Online reports whether the runner is connected and able to accept work.
func (r Runner) Online() bool { return r.Status == "online" }

type runnersResponse struct {
	TotalCount int `json:"total_count"`
	Runners    []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Busy   bool   `json:"busy"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"runners"`
}

// ListRunners returns every self-hosted runner registered to the repository.
//
// Registrations outlive the process that created them: an ephemeral runner that
// dies without deregistering is still listed, with status "offline". Callers
// that want capacity must count only online runners.
func (c *Client) ListRunners(ctx context.Context) ([]Runner, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runners", c.Owner, c.Repo)
	if c.OrgScoped() {
		path = fmt.Sprintf("/orgs/%s/actions/runners", c.Org)
	}

	var resp runnersResponse
	err := c.get(ctx, path, url.Values{"per_page": {strconv.Itoa(perPage)}}, &resp)
	if err != nil {
		return nil, err
	}

	out := make([]Runner, 0, len(resp.Runners))
	for _, r := range resp.Runners {
		runner := Runner{ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy}
		for _, l := range r.Labels {
			runner.Labels = append(runner.Labels, strings.ToLower(strings.TrimSpace(l.Name)))
		}
		out = append(out, runner)
	}
	return out, nil
}

// Job is an Actions job that has not reached a terminal state.
type Job struct {
	ID     int64
	Status string // "queued", "in_progress", or "waiting"
	Labels []string
}

type runsResponse struct {
	TotalCount   int `json:"total_count"`
	WorkflowRuns []struct {
		ID int64 `json:"id"`
	} `json:"workflow_runs"`
}

type jobsResponse struct {
	TotalCount int `json:"total_count"`
	Jobs       []struct {
		ID     int64    `json:"id"`
		Status string   `json:"status"`
		Labels []string `json:"labels"`
	} `json:"jobs"`
}

// ListActiveJobs returns the repository's queued and in-progress jobs.
//
// GitHub exposes no endpoint listing a repository's jobs directly, so this
// walks the unfinished workflow runs and reads each one's jobs. Runs are
// fetched per status because the runs endpoint accepts only a single status
// filter.
//
// Repo scope only: workflow runs live on repositories, and GitHub has no
// org-level equivalent, so an org-scoped client cannot enumerate jobs.
func (c *Client) ListActiveJobs(ctx context.Context) ([]Job, error) {
	if c.OrgScoped() {
		return nil, fmt.Errorf("org scope: GitHub has no org-level jobs API")
	}
	runIDs, err := c.activeRunIDs(ctx)
	if err != nil {
		return nil, err
	}

	var out []Job
	for _, runID := range runIDs {
		var resp jobsResponse
		err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs", c.Owner, c.Repo, runID),
			url.Values{"per_page": {strconv.Itoa(perPage)}, "filter": {"latest"}}, &resp)
		if err != nil {
			return nil, fmt.Errorf("jobs for run %d: %w", runID, err)
		}
		for _, j := range resp.Jobs {
			if j.Status != "queued" && j.Status != "in_progress" && j.Status != "waiting" {
				continue
			}
			job := Job{ID: j.ID, Status: j.Status}
			for _, l := range j.Labels {
				job.Labels = append(job.Labels, strings.ToLower(strings.TrimSpace(l)))
			}
			out = append(out, job)
		}
	}
	return out, nil
}

// activeRunIDs collects the IDs of workflow runs that still have work to do,
// de-duplicated across status filters.
func (c *Client) activeRunIDs(ctx context.Context) ([]int64, error) {
	seen := make(map[int64]struct{})
	var ids []int64

	for _, status := range []string{"queued", "in_progress"} {
		for page := 1; page <= maxRunPages; page++ {
			var resp runsResponse
			err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/actions/runs", c.Owner, c.Repo), url.Values{
				"status":   {status},
				"per_page": {strconv.Itoa(perPage)},
				"page":     {strconv.Itoa(page)},
			}, &resp)
			if err != nil {
				return nil, fmt.Errorf("runs with status %q: %w", status, err)
			}
			for _, r := range resp.WorkflowRuns {
				if _, dup := seen[r.ID]; dup {
					continue
				}
				seen[r.ID] = struct{}{}
				ids = append(ids, r.ID)
			}
			if len(resp.WorkflowRuns) < perPage {
				break
			}
		}
	}
	return ids, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
