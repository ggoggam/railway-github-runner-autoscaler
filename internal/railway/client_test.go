package railway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c := NewClient("token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.Endpoint = url
	c.Sleep = func(context.Context, time.Duration) error { return nil } // no real backoff
	return c
}

func TestSetReplicasUsesMultiRegionConfig(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		got = req.Variables["input"].(map[string]any)
		_, _ = w.Write([]byte(`{"data":{"serviceInstanceUpdate":true}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.SetReplicas(t.Context(), "svc", "env", []string{"asia-southeast1-eqsg3a"}, 3); err != nil {
		t.Fatalf("SetReplicas: %v", err)
	}

	mrc, ok := got["multiRegionConfig"].(map[string]any)
	if !ok {
		t.Fatalf("want multiRegionConfig in input, got %#v", got)
	}
	region, ok := mrc["asia-southeast1-eqsg3a"].(map[string]any)
	if !ok {
		t.Fatalf("want region key, got %#v", mrc)
	}
	if region["numReplicas"] != float64(3) {
		t.Fatalf("want numReplicas 3, got %v", region["numReplicas"])
	}
	if _, legacy := got["numReplicas"]; legacy {
		t.Fatal("must not send legacy numReplicas alongside multiRegionConfig")
	}
}

func TestSetReplicasFallsBackToLegacyNumReplicas(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		got = req.Variables["input"].(map[string]any)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.SetReplicas(t.Context(), "svc", "env", nil, 2); err != nil {
		t.Fatalf("SetReplicas: %v", err)
	}
	if got["numReplicas"] != float64(2) {
		t.Fatalf("want legacy numReplicas 2, got %#v", got)
	}
}

// Railway reports transient internal faults as HTTP 200 with an errors array
// ("Problem processing request"), which is what broke scaling in production.
func TestRetriesTransientGraphQLError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			_, _ = w.Write([]byte(`{"errors":[{"message":"Problem processing request"}],"data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"serviceInstanceUpdate":true}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.SetReplicas(t.Context(), "svc", "env", nil, 1); err != nil {
		t.Fatalf("want success after retries, got %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("want 3 attempts, got %d", n)
	}
}

func TestRetriesServerErrorsAndRateLimits(t *testing.T) {
	for name, status := range map[string]int{
		"500": http.StatusInternalServerError,
		"502": http.StatusBadGateway,
		"429": http.StatusTooManyRequests,
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) < 2 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(`{"data":{}}`))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			if err := c.SetReplicas(t.Context(), "svc", "env", nil, 1); err != nil {
				t.Fatalf("want recovery after %s, got %v", name, err)
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("want 2 attempts, got %d", n)
			}
		})
	}
}

func TestDoesNotRetryAuthFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.SetReplicas(t.Context(), "svc", "env", nil, 1); err == nil {
		t.Fatal("want error on 401")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("401 is permanent and must not be retried, got %d attempts", n)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.SetReplicas(t.Context(), "svc", "env", nil, 1); err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if n := calls.Load(); int(n) != maxAttempts {
		t.Fatalf("want %d attempts, got %d", maxAttempts, n)
	}
}

func TestDiscoverServiceStateReadsMultiRegionConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"serviceInstance":{
			"region":null,
			"numReplicas":1,
			"latestDeployment":{"meta":{"serviceManifest":{"deploy":{
				"multiRegionConfig":{"us-west2":{"numReplicas":2},"asia-southeast1-eqsg3a":{"numReplicas":3}}
			}}}}
		}}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	state, err := c.DiscoverServiceState(t.Context(), "svc", "env")
	if err != nil {
		t.Fatalf("DiscoverServiceState: %v", err)
	}
	want := []string{"asia-southeast1-eqsg3a", "us-west2"} // sorted
	if !reflect.DeepEqual(state.Regions, want) {
		t.Fatalf("want regions %v, got %v", want, state.Regions)
	}
	if state.Replicas != 5 {
		t.Fatalf("want summed replicas 5, got %d", state.Replicas)
	}
}

func TestDiscoverServiceStateFallsBackToRegionField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"serviceInstance":{
			"region":"us-west2","numReplicas":4,"latestDeployment":null
		}}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	state, err := c.DiscoverServiceState(t.Context(), "svc", "env")
	if err != nil {
		t.Fatalf("DiscoverServiceState: %v", err)
	}
	if !reflect.DeepEqual(state.Regions, []string{"us-west2"}) {
		t.Fatalf("want [us-west2], got %v", state.Regions)
	}
	if state.Replicas != 4 {
		t.Fatalf("want 4 replicas, got %d", state.Replicas)
	}
}

func TestDistribute(t *testing.T) {
	for _, tc := range []struct {
		n       int
		regions []string
		want    map[string]int
	}{
		{3, []string{"a"}, map[string]int{"a": 3}},
		{4, []string{"a", "b"}, map[string]int{"a": 2, "b": 2}},
		{5, []string{"a", "b"}, map[string]int{"a": 3, "b": 2}},
		{0, []string{"a", "b"}, map[string]int{"a": 0, "b": 0}},
		{2, []string{"a", "b", "c"}, map[string]int{"a": 1, "b": 1, "c": 0}},
	} {
		if got := distribute(tc.n, tc.regions); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("distribute(%d,%v) = %v, want %v", tc.n, tc.regions, got, tc.want)
		}
	}
}

func TestRetryAfterParsing(t *testing.T) {
	if got := retryAfter("2"); got != 2*time.Second {
		t.Errorf("retryAfter(\"2\") = %v, want 2s", got)
	}
	if got := retryAfter(""); got != 0 {
		t.Errorf("retryAfter(\"\") = %v, want 0", got)
	}
	if got := retryAfter("garbage"); got != 0 {
		t.Errorf("retryAfter(garbage) = %v, want 0", got)
	}
	if got := retryAfter("99999"); got != maxBackoff {
		t.Errorf("retryAfter must cap at maxBackoff, got %v", got)
	}
}

func TestDefaultEndpointIsRailwayDotCom(t *testing.T) {
	// backboard.railway.app is the legacy host.
	want := "https://backboard.railway.com/graphql/v2"
	if DefaultEndpoint != want {
		t.Fatalf("want %s, got %s", want, DefaultEndpoint)
	}
}

// servicesResponse renders the project services payload the resolver reads.
func servicesResponse(services map[string]string) string {
	type node struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var edges []map[string]node
	for name, id := range services {
		edges = append(edges, map[string]node{"node": {ID: id, Name: name}})
	}
	body, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"project": map[string]any{
				"services": map[string]any{"edges": edges},
			},
		},
	})
	return string(body)
}

func TestResolveServiceIDByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(servicesResponse(map[string]string{
			"autoscaler":    "svc-auto",
			"github-runner": "svc-runner",
		})))
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv.URL).ResolveServiceID(t.Context(), "proj", "github-runner")
	if err != nil {
		t.Fatalf("ResolveServiceID: %v", err)
	}
	if got != "svc-runner" {
		t.Errorf("got %q, want %q", got, "svc-runner")
	}
}

// Service names are user-visible, so a user who re-cases one should not silently
// lose autoscaling.
func TestResolveServiceIDMatchesCaseInsensitively(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(servicesResponse(map[string]string{"GitHub-Runner": "svc-runner"})))
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv.URL).ResolveServiceID(t.Context(), "proj", "github-runner")
	if err != nil {
		t.Fatalf("ResolveServiceID: %v", err)
	}
	if got != "svc-runner" {
		t.Errorf("got %q, want %q", got, "svc-runner")
	}
}

// An exact match must win outright, or a project holding both "github-runner"
// and "GitHub-Runner" would resolve by iteration order.
func TestResolveServiceIDPrefersExactMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(servicesResponse(map[string]string{
			"github-runner": "svc-exact",
			"GitHub-Runner": "svc-loose",
		})))
	}))
	defer srv.Close()

	for range 20 { // iteration order over the response varies run to run
		got, err := newTestClient(t, srv.URL).ResolveServiceID(t.Context(), "proj", "github-runner")
		if err != nil {
			t.Fatalf("ResolveServiceID: %v", err)
		}
		if got != "svc-exact" {
			t.Fatalf("got %q, want %q", got, "svc-exact")
		}
	}
}

// A missing service is retryable at startup, because a template creates every
// service at once and the autoscaler may query before the runner is visible.
func TestResolveServiceIDNotFoundIsDistinguishable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(servicesResponse(map[string]string{"autoscaler": "svc-auto"})))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ResolveServiceID(t.Context(), "proj", "github-runner")
	if !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("err = %v, want ErrServiceNotFound", err)
	}
	// The names that were present are what tells a user they mistyped one.
	if !strings.Contains(err.Error(), "autoscaler") {
		t.Errorf("error should list the services found, got %v", err)
	}
}

// Two case-insensitive matches must fail loudly rather than scale a coin flip.
func TestResolveServiceIDRejectsAmbiguousMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(servicesResponse(map[string]string{
			"github-runner": "svc-a",
			"GITHUB-RUNNER": "svc-b",
		})))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ResolveServiceID(t.Context(), "proj", "GitHub-Runner")
	if err == nil {
		t.Fatal("want error when two services match case-insensitively")
	}
	if errors.Is(err, ErrServiceNotFound) {
		t.Error("ambiguity is not retryable and must not report as not-found")
	}
}
