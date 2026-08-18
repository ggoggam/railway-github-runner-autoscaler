package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecret = "s3cr3t"

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func jobPayload(action string, id int64, labels ...string) string {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = `"` + l + `"`
	}
	return fmt.Sprintf(`{"action":%q,"workflow_job":{"id":%d,"labels":[%s]}}`,
		action, id, strings.Join(quoted, ","))
}

func newTestHandler(t *testing.T) (*Handler, *Autoscaler) {
	t.Helper()
	cfg := testConfig()
	cfg.WebhookSecret = testSecret
	a, _, _ := newTestAutoscaler(t, cfg, 1)
	return NewHandler(cfg, a, discardLogger()), a
}

func post(h *Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)
	return rr
}

func TestRejectsInvalidSignature(t *testing.T) {
	h, a := newTestHandler(t)
	body := jobPayload("queued", 1, "railway")

	for name, sig := range map[string]string{
		"wrong secret": "sha256=" + strings.Repeat("00", 32),
		"missing":      "",
		"malformed":    "sha256=nothex",
		"no prefix":    strings.TrimPrefix(sign(body), "sha256="),
	} {
		t.Run(name, func(t *testing.T) {
			rr := post(h, body, map[string]string{
				"X-GitHub-Event":      "workflow_job",
				"X-Hub-Signature-256": sig,
			})
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d", rr.Code)
			}
		})
	}

	if s := a.Stats(); s.Queued != 0 {
		t.Fatalf("unsigned request mutated state: %+v", s)
	}
}

func TestAcceptsValidSignatureAndTracksJob(t *testing.T) {
	h, a := newTestHandler(t)
	body := jobPayload("queued", 42, "railway")

	rr := post(h, body, map[string]string{
		"X-GitHub-Event":      "workflow_job",
		"X-Hub-Signature-256": sign(body),
		"X-GitHub-Delivery":   "d1",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if s := a.Stats(); s.Queued != 1 {
		t.Fatalf("want 1 queued job, got %+v", s)
	}
}

// GitHub retries deliveries; replaying one must not double-count.
func TestDuplicateDeliveryIgnored(t *testing.T) {
	h, a := newTestHandler(t)
	body := jobPayload("queued", 7, "railway")
	headers := map[string]string{
		"X-GitHub-Event":      "workflow_job",
		"X-Hub-Signature-256": sign(body),
		"X-GitHub-Delivery":   "same-id",
	}

	post(h, body, headers)
	post(h, body, headers)

	if s := a.Stats(); s.Queued != 1 {
		t.Fatalf("duplicate delivery double-counted: %+v", s)
	}
}

func TestIgnoresJobsForOtherRunners(t *testing.T) {
	h, a := newTestHandler(t)
	body := jobPayload("queued", 9, "ubuntu-latest")

	rr := post(h, body, map[string]string{
		"X-GitHub-Event":      "workflow_job",
		"X-Hub-Signature-256": sign(body),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if s := a.Stats(); s.Queued != 0 {
		t.Fatalf("tracked a job meant for another runner: %+v", s)
	}
}

func TestPingAcknowledged(t *testing.T) {
	h, _ := newTestHandler(t)
	body := `{"zen":"hello"}`

	rr := post(h, body, map[string]string{
		"X-GitHub-Event":      "ping",
		"X-Hub-Signature-256": sign(body),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 for ping, got %d", rr.Code)
	}
}

func TestNonPostWebhookRejected(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("GET /webhook should not be accepted, got %d", rr.Code)
	}
}

func TestHealthAndStatus(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, path := range []string{"/health", "/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: want 200, got %d", path, rr.Code)
		}
	}
}

func TestMatchesLabels(t *testing.T) {
	for _, tc := range []struct {
		name     string
		job      []string
		required []string
		want     bool
	}{
		{"exact", []string{"railway"}, []string{"railway"}, true},
		{"case insensitive", []string{"Railway"}, []string{"railway"}, true},
		{"job has extras", []string{"self-hosted", "railway", "linux"}, []string{"railway"}, true},
		{"missing one required", []string{"railway"}, []string{"self-hosted", "railway"}, false},
		{"no overlap", []string{"ubuntu-latest"}, []string{"railway"}, false},
		{"empty job labels", nil, []string{"railway"}, false},
		{"no required labels", []string{"railway"}, nil, false},
		{"whitespace padded", []string{" railway "}, []string{"railway"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesLabels(tc.job, tc.required); got != tc.want {
				t.Fatalf("matchesLabels(%v, %v) = %v, want %v", tc.job, tc.required, got, tc.want)
			}
		})
	}
}

func TestValidateSignatureRejectsEmptySecret(t *testing.T) {
	body := []byte("{}")
	mac := hmac.New(sha256.New, []byte(""))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if validateSignature(body, sig, "") {
		t.Fatal("an empty secret must never validate")
	}
}
