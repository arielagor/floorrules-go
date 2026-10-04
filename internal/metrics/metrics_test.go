package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistry_TextFormat(t *testing.T) {
	r := New()
	r.Describe("http_requests_total", "HTTP requests by route and code.")
	r.Inc("http_requests_total", "route", "GET /healthz", "code", "200")
	r.Inc("http_requests_total", "code", "200", "route", "GET /healthz") // label order must not matter
	r.Add("apply_total", 3, "status", `we"ird\value`)

	if got := r.Get("http_requests_total", "route", "GET /healthz", "code", "200"); got != 2 {
		t.Fatalf("counter = %v, want 2", got)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# HELP http_requests_total HTTP requests by route and code.",
		"# TYPE http_requests_total counter",
		`http_requests_total{code="200",route="GET /healthz"} 2`,
		`apply_total{status="we\"ird\\value"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}
}

func TestRegistry_GaugeIsSetNotAdded(t *testing.T) {
	r := New()
	r.Set("outbox_pending", 7)
	r.Set("outbox_pending", 3)
	if got := r.Get("outbox_pending"); got != 3 {
		t.Fatalf("gauge = %v, want 3", got)
	}
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "# TYPE outbox_pending gauge\noutbox_pending 3\n") {
		t.Fatalf("gauge not rendered as a gauge:\n%s", b.String())
	}
}

func TestRegistry_NilIsNoop(t *testing.T) {
	var r *Registry
	r.Inc("x") // must not panic
	r.Set("g", 1)
	// Day-2 review nit: Get (and Describe) were not nil-safe while the
	// package documents a nil registry as a no-op.
	r.Describe("x", "help")
	if got := r.Get("x"); got != 0 {
		t.Fatalf("nil Get = %v, want 0", got)
	}
}
