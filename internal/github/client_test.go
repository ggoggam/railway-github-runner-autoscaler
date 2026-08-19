package github

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestClient points a Client at a stub serving canned responses per path.
func newTestClient(t *testing.T, routes map[string]string) (*Client, *[]string) {
	t.Helper()
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)

		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("missing bearer token, got %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got == "" {
			t.Error("missing API version header")
		}

		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c := NewClient("tok", "acme", "widgets", discardLogger())
	c.Endpoint = srv.URL
	return c, &seen
}

func TestSplitRepository(t *testing.T) {
	owner, repo, err := SplitRepository(" acme/widgets ")
	if err != nil || owner != "acme" || repo != "widgets" {
		t.Fatalf("got %q/%q err=%v", owner, repo, err)
	}

	for _, bad := range []string{"", "acme", "/widgets", "acme/", "   "} {
		if _, _, err := SplitRepository(bad); err == nil {
			t.Errorf("want error for %q", bad)
		}
	}
}

// Offline registrations are left behind by runners that died without
// deregistering, so they must never be counted as capacity.
func TestListRunnersReportsStatusAndLabels(t *testing.T) {
	c, _ := newTestClient(t, map[string]string{
		"/repos/acme/widgets/actions/runners": `{
			"total_count": 3,
			"runners": [
				{"id": 1, "name": "dead", "status": "offline", "busy": false,
				 "labels": [{"name": "self-hosted"}, {"name": "Railway"}]},
				{"id": 2, "name": "live", "status": "online", "busy": true,
				 "labels": [{"name": "self-hosted"}, {"name": "railway"}]},
				{"id": 3, "name": "other", "status": "online", "busy": false,
				 "labels": [{"name": "self-hosted"}, {"name": "macos"}]}
			]
		}`,
	})

	runners, err := c.ListRunners(t.Context())
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}
	if len(runners) != 3 {
		t.Fatalf("want 3 runners, got %d", len(runners))
	}

	if runners[0].Online() {
		t.Error("offline runner reported as online")
	}
	if !runners[1].Online() || !runners[1].Busy {
		t.Errorf("want runner 2 online and busy, got %+v", runners[1])
	}
	// Labels are lowercased so matching does not depend on how they were typed.
	if got := strings.Join(runners[0].Labels, ","); got != "self-hosted,railway" {
		t.Errorf("want lowercased labels, got %q", got)
	}
}

func TestListActiveJobsWalksUnfinishedRuns(t *testing.T) {
	c, seen := newTestClient(t, map[string]string{
		"/repos/acme/widgets/actions/runs": `{
			"total_count": 1,
			"workflow_runs": [{"id": 100}]
		}`,
		"/repos/acme/widgets/actions/runs/100/jobs": `{
			"total_count": 3,
			"jobs": [
				{"id": 11, "status": "queued", "labels": ["self-hosted", "railway"]},
				{"id": 12, "status": "in_progress", "labels": ["self-hosted", "railway"]},
				{"id": 13, "status": "completed", "labels": ["self-hosted", "railway"]}
			]
		}`,
	})

	jobs, err := c.ListActiveJobs(t.Context())
	if err != nil {
		t.Fatalf("ListActiveJobs: %v", err)
	}

	// Terminal jobs are filtered out; the run is de-duplicated across the
	// queued and in_progress status sweeps.
	if len(jobs) != 2 {
		t.Fatalf("want 2 active jobs, got %d: %+v", len(jobs), jobs)
	}
	if jobs[0].ID != 11 || jobs[0].Status != "queued" {
		t.Errorf("unexpected first job: %+v", jobs[0])
	}
	if jobs[1].ID != 12 || jobs[1].Status != "in_progress" {
		t.Errorf("unexpected second job: %+v", jobs[1])
	}

	var jobCalls int
	for _, s := range *seen {
		if strings.Contains(s, "/jobs") {
			jobCalls++
		}
	}
	if jobCalls != 1 {
		t.Errorf("want the shared run fetched once, got %d job calls in %v", jobCalls, *seen)
	}
}

// A spent rate limit arrives as a 403; the message must say so, because
// otherwise it is indistinguishable from a bad token.
func TestUnauthorizedErrorMentionsTokenAndRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()

	c := NewClient("tok", "acme", "widgets", discardLogger())
	c.Endpoint = srv.URL

	_, err := c.ListRunners(t.Context())
	if err == nil {
		t.Fatal("want an error on 403")
	}
	for _, want := range []string{"GITHUB_TOKEN", "rate limit remaining", "403"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestNonOKStatusIsAnError(t *testing.T) {
	c, _ := newTestClient(t, nil) // every path 404s

	if _, err := c.ListRunners(t.Context()); err == nil {
		t.Fatal("want an error on 404")
	}
}
