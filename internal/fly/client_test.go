package fly

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestClient(t *testing.T, url, token string) *Client {
	t.Helper()
	c := NewClient(token, "runners", discardLogger())
	c.Endpoint = url
	c.Sleep = func(context.Context, time.Duration) error { return nil } // no real backoff
	return c
}

func TestListMachinesParsesStates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/runners/machines" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"id":"a1","name":"one","state":"started","region":"nrt"},
			{"id":"b2","name":"two","state":"stopped","region":"nrt"},
			{"id":"c3","name":"three","state":"starting","region":"nrt"}
		]`))
	}))
	defer srv.Close()

	ms, err := newTestClient(t, srv.URL, "tok").ListMachines(t.Context())
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(ms) != 3 {
		t.Fatalf("want 3 machines, got %d", len(ms))
	}
	if !ms[0].Active() || ms[1].Active() || !ms[2].Active() {
		t.Errorf("Active() wrong: %+v", ms)
	}
	if ms[0].Startable() || !ms[1].Startable() {
		t.Errorf("Startable() wrong: %+v", ms)
	}
}

func TestAuthorizationHeader(t *testing.T) {
	for _, tc := range []struct{ token, want string }{
		{"plain", "Bearer plain"},
		{"FlyV1 fm2_abc,fm2_def", "FlyV1 fm2_abc,fm2_def"},
		{"  FlyV1 fm2_abc\n", "FlyV1 fm2_abc"},
	} {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("Authorization")
		}))
		if err := newTestClient(t, srv.URL, tc.token).Start(t.Context(), "m"); err != nil {
			t.Fatalf("Start: %v", err)
		}
		srv.Close()
		if got != tc.want {
			t.Errorf("token %q: Authorization = %q, want %q", tc.token, got, tc.want)
		}
	}
}

func TestStartAndStopHitMachinePaths(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
		}
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "tok")
	if err := c.Start(t.Context(), "m1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Stop(t.Context(), "m2"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	want := []string{"/v1/apps/runners/machines/m1/start", "/v1/apps/runners/machines/m2/stop"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusConflict, http.StatusPreconditionFailed} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}))
		if err := newTestClient(t, srv.URL, "tok").Start(t.Context(), "m"); err != nil {
			t.Errorf("status %d: want success after retries, got %v", status, err)
		}
		if calls.Load() != 3 {
			t.Errorf("status %d: want 3 calls, got %d", status, calls.Load())
		}
		srv.Close()
	}
}

func TestDoesNotRetryAuthFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := newTestClient(t, srv.URL, "tok").Start(t.Context(), "m")
	if err == nil || !strings.Contains(err.Error(), "FLY_API_TOKEN") {
		t.Fatalf("want auth error naming FLY_API_TOKEN, got %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("want 1 call, got %d", calls.Load())
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := newTestClient(t, srv.URL, "tok").Stop(t.Context(), "m"); err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if calls.Load() != maxAttempts {
		t.Errorf("want %d calls, got %d", maxAttempts, calls.Load())
	}
}
