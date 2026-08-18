package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// DefaultRailwayEndpoint is Railway's public GraphQL API.
//
// Note the .com host: the older backboard.railway.app address is legacy.
const DefaultRailwayEndpoint = "https://backboard.railway.com/graphql/v2"

const (
	maxAttempts     = 4
	baseBackoff     = 500 * time.Millisecond
	maxBackoff      = 8 * time.Second
	maxRespBodyRead = 1 << 20 // 1MiB
)

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlError struct {
	Message string `json:"message"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// RailwayClient talks to Railway's public GraphQL API.
type RailwayClient struct {
	Token    string
	Endpoint string
	HTTP     *http.Client
	Logger   *slog.Logger

	// Sleep is swappable so tests do not wait on real backoff.
	Sleep func(context.Context, time.Duration) error
}

func NewRailwayClient(token string, logger *slog.Logger) *RailwayClient {
	return &RailwayClient{
		Token:    token,
		Endpoint: DefaultRailwayEndpoint,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Logger:   logger,
		Sleep:    sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryableError marks a failure worth another attempt.
type retryableError struct {
	err   error
	after time.Duration // server-provided delay, if any
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// Do executes a GraphQL document, retrying transient failures.
//
// Every mutation this client issues is idempotent (it sets an absolute replica
// count rather than applying a delta), so retrying is safe.
func (c *RailwayClient) Do(ctx context.Context, req gqlRequest, out any) error {
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			delay := backoffFor(attempt)
			var re *retryableError
			if errors.As(lastErr, &re) && re.after > 0 {
				delay = re.after
			}
			c.Logger.Warn("railway api retry",
				"attempt", attempt+1, "of", maxAttempts, "delay", delay, "err", lastErr)
			if err := c.Sleep(ctx, delay); err != nil {
				return fmt.Errorf("%w (last error: %v)", err, lastErr)
			}
		}

		err := c.attempt(ctx, req, out)
		if err == nil {
			return nil
		}
		lastErr = err

		var re *retryableError
		if !errors.As(err, &re) {
			return err // permanent; do not burn attempts
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		}
	}
	return fmt.Errorf("railway api failed after %d attempts: %w", maxAttempts, lastErr)
}

func backoffFor(attempt int) time.Duration {
	d := time.Duration(math.Pow(2, float64(attempt-1))) * baseBackoff
	return min(d, maxBackoff)
}

func (c *RailwayClient) attempt(ctx context.Context, req gqlRequest, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		// Transport-level failures are worth another go.
		return &retryableError{err: fmt.Errorf("railway api transport: %w", err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBodyRead))
	if err != nil {
		return &retryableError{err: fmt.Errorf("read response: %w", err)}
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &retryableError{
			err:   fmt.Errorf("railway api rate limited (429): %s", truncate(respBody, 200)),
			after: retryAfter(resp.Header.Get("Retry-After")),
		}
	case resp.StatusCode >= 500:
		return &retryableError{err: fmt.Errorf("railway api %d: %s", resp.StatusCode, truncate(respBody, 200))}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("railway api %d (check RAILWAY_API_TOKEN): %s", resp.StatusCode, truncate(respBody, 200))
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("railway api %d: %s", resp.StatusCode, truncate(respBody, 200))
	}

	var gqlResp gqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return &retryableError{err: fmt.Errorf("unmarshal response: %w", err)}
	}
	if len(gqlResp.Errors) > 0 {
		// Railway surfaces transient internal faults as a 200 with an errors
		// array (e.g. "Problem processing request"), so these retry too.
		return &retryableError{err: fmt.Errorf("railway graphql: %s", gqlResp.Errors[0].Message)}
	}

	if out != nil && gqlResp.Data != nil {
		if err := json.Unmarshal(gqlResp.Data, out); err != nil {
			return fmt.Errorf("unmarshal data: %w", err)
		}
	}
	return nil
}

func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return min(time.Duration(secs)*time.Second, maxBackoff)
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, maxBackoff)
		}
	}
	return 0
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

const discoverQuery = `
query RunnerServiceInstance($serviceId: String!, $environmentId: String!) {
  serviceInstance(serviceId: $serviceId, environmentId: $environmentId) {
    region
    numReplicas
    latestDeployment { meta }
  }
}`

// ServiceState is what the runner service looks like right now.
type ServiceState struct {
	// Regions the service deploys to. Empty means the region could not be
	// determined and the legacy numReplicas field should be used.
	Regions []string
	// Replicas currently configured, summed across regions. -1 when unknown.
	Replicas int
}

// DiscoverServiceState reads the runner service's current regions and replica count.
//
// Railway keys replica counts by region under multiRegionConfig; that map is not
// exposed as a readable field, so it is read back out of the latest deployment's
// service manifest.
func (c *RailwayClient) DiscoverServiceState(ctx context.Context, serviceID, envID string) (ServiceState, error) {
	var out struct {
		ServiceInstance struct {
			Region           *string `json:"region"`
			NumReplicas      *int    `json:"numReplicas"`
			LatestDeployment *struct {
				Meta struct {
					ServiceManifest struct {
						Deploy struct {
							MultiRegionConfig map[string]struct {
								NumReplicas int `json:"numReplicas"`
							} `json:"multiRegionConfig"`
						} `json:"deploy"`
					} `json:"serviceManifest"`
				} `json:"meta"`
			} `json:"latestDeployment"`
		} `json:"serviceInstance"`
	}

	state := ServiceState{Replicas: -1}
	if err := c.Do(ctx, gqlRequest{
		Query:     discoverQuery,
		Variables: map[string]any{"serviceId": serviceID, "environmentId": envID},
	}, &out); err != nil {
		return state, err
	}

	si := out.ServiceInstance
	if si.NumReplicas != nil {
		state.Replicas = *si.NumReplicas
	}

	if si.LatestDeployment != nil {
		mrc := si.LatestDeployment.Meta.ServiceManifest.Deploy.MultiRegionConfig
		if len(mrc) > 0 {
			total := 0
			for r, v := range mrc {
				state.Regions = append(state.Regions, r)
				total += v.NumReplicas
			}
			sort.Strings(state.Regions) // deterministic replica distribution
			state.Replicas = total
			return state, nil
		}
	}
	if si.Region != nil && *si.Region != "" {
		state.Regions = []string{*si.Region}
	}
	return state, nil
}

const setReplicasMutation = `
mutation UpdateReplicas($serviceId: String!, $environmentId: String!, $input: ServiceInstanceUpdateInput!) {
  serviceInstanceUpdate(serviceId: $serviceId, environmentId: $environmentId, input: $input)
}`

// SetReplicas sets the runner service to exactly n replicas.
//
// When regions are known the count is written as multiRegionConfig, which is how
// current Railway represents horizontal scaling. Bare numReplicas is the legacy
// path and is only used when no region could be determined.
func (c *RailwayClient) SetReplicas(ctx context.Context, serviceID, envID string, regions []string, n int) error {
	input := map[string]any{}
	if len(regions) > 0 {
		mrc := make(map[string]any, len(regions))
		for region, count := range distribute(n, regions) {
			mrc[region] = map[string]any{"numReplicas": count}
		}
		input["multiRegionConfig"] = mrc
	} else {
		input["numReplicas"] = n
	}

	return c.Do(ctx, gqlRequest{
		Query: setReplicasMutation,
		Variables: map[string]any{
			"serviceId":     serviceID,
			"environmentId": envID,
			"input":         input,
		},
	}, nil)
}

// distribute spreads n replicas across regions as evenly as possible, giving
// the remainder to the earliest regions so the result is stable run to run.
func distribute(n int, regions []string) map[string]int {
	out := make(map[string]int, len(regions))
	if len(regions) == 0 {
		return out
	}
	base, rem := n/len(regions), n%len(regions)
	for i, r := range regions {
		c := base
		if i < rem {
			c++
		}
		out[r] = c
	}
	return out
}
