// Package fly is a small client for the parts of the Fly Machines API needed to
// run a pool of ephemeral runners: list an app's Machines, start them, and stop
// them.
package fly

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
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is the public Machines API.
const DefaultEndpoint = "https://api.machines.dev"

const (
	maxAttempts     = 4
	baseBackoff     = 500 * time.Millisecond
	maxBackoff      = 8 * time.Second
	maxRespBodyRead = 1 << 20 // 1MiB
)

// Machine states, as reported by the Machines API.
const (
	StateCreated   = "created"
	StateStarting  = "starting"
	StateStarted   = "started"
	StateStopping  = "stopping"
	StateStopped   = "stopped"
	StateSuspended = "suspended"
)

// Machine is one Machine in the runner app.
type Machine struct {
	ID     string
	Name   string
	State  string
	Region string
}

// Active reports whether the Machine is running, or on its way to running, and
// so already counts as capacity.
func (m Machine) Active() bool { return m.State == StateStarting || m.State == StateStarted }

// Startable reports whether the Machine is parked and can be started.
func (m Machine) Startable() bool {
	return m.State == StateStopped || m.State == StateSuspended || m.State == StateCreated
}

// Client talks to the Fly Machines API for a single app.
type Client struct {
	Token    string
	App      string
	Endpoint string
	HTTP     *http.Client
	Logger   *slog.Logger

	// Sleep is swappable so tests do not wait on real backoff.
	Sleep func(context.Context, time.Duration) error
}

// NewClient returns a Client pointed at the public Machines API.
func NewClient(token, app string, logger *slog.Logger) *Client {
	return &Client{
		Token:    token,
		App:      app,
		Endpoint: DefaultEndpoint,
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

// authorization formats the token for the Authorization header. Tokens from
// `fly tokens create` already carry their own "FlyV1 " scheme and must be sent
// as-is; anything else is a bearer token.
func authorization(token string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, "FlyV1 ") {
		return token
	}
	return "Bearer " + token
}

// retryable marks a failure worth another attempt.
type retryable struct {
	err   error
	after time.Duration // server-provided delay, if any
}

func (e *retryable) Error() string { return e.err.Error() }
func (e *retryable) Unwrap() error { return e.err }

// do executes a request, retrying transient failures.
//
// Start and stop are idempotent in effect (starting a started Machine or
// stopping a stopped one changes nothing), so retrying is safe.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			delay := backoffFor(attempt)
			var re *retryable
			if errors.As(lastErr, &re) && re.after > 0 {
				delay = re.after
			}
			c.Logger.Warn("fly api retry",
				"attempt", attempt+1, "of", maxAttempts, "delay", delay, "err", lastErr)
			if err := c.Sleep(ctx, delay); err != nil {
				return fmt.Errorf("%w (last error: %v)", err, lastErr)
			}
		}

		err := c.attempt(ctx, method, path, body, out)
		if err == nil {
			return nil
		}
		lastErr = err

		var re *retryable
		if !errors.As(err, &re) {
			return err // permanent; do not burn attempts
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		}
	}
	return fmt.Errorf("fly api failed after %d attempts: %w", maxAttempts, lastErr)
}

func backoffFor(attempt int) time.Duration {
	d := time.Duration(math.Pow(2, float64(attempt-1))) * baseBackoff
	return min(d, maxBackoff)
}

func (c *Client) attempt(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Endpoint, "/")+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", authorization(c.Token))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Transport-level failures are worth another go.
		return &retryable{err: fmt.Errorf("fly api transport: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBodyRead))
	if err != nil {
		return &retryable{err: fmt.Errorf("read response: %w", err)}
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &retryable{
			err:   fmt.Errorf("fly api rate limited (429): %s", truncate(respBody, 200)),
			after: retryAfter(resp.Header.Get("Retry-After")),
		}
	case resp.StatusCode >= 500:
		return &retryable{err: fmt.Errorf("fly api %d: %s", resp.StatusCode, truncate(respBody, 200))}
	case resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusPreconditionFailed:
		// The Machine is leased by someone else, typically a `fly deploy` in
		// flight. The lease is short-lived, so wait it out.
		return &retryable{err: fmt.Errorf("fly api %d (machine busy): %s", resp.StatusCode, truncate(respBody, 200))}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("fly api %d (check FLY_API_TOKEN): %s", resp.StatusCode, truncate(respBody, 200))
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("fly api %d: %s", resp.StatusCode, truncate(respBody, 200))
	}

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
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

func (c *Client) machinesPath() string {
	return "/v1/apps/" + url.PathEscape(c.App) + "/machines"
}

// ListMachines returns every live Machine in the app.
func (c *Client) ListMachines(ctx context.Context) ([]Machine, error) {
	var resp []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		State  string `json:"state"`
		Region string `json:"region"`
	}
	if err := c.do(ctx, http.MethodGet, c.machinesPath(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]Machine, 0, len(resp))
	for _, m := range resp {
		out = append(out, Machine{ID: m.ID, Name: m.Name, State: m.State, Region: m.Region})
	}
	return out, nil
}

// Start boots a stopped Machine.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, c.machinesPath()+"/"+url.PathEscape(id)+"/start", nil, nil)
}

// Stop shuts a Machine down with the app's configured kill signal, giving the
// runner the chance to deregister itself.
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, c.machinesPath()+"/"+url.PathEscape(id)+"/stop", nil, nil)
}
