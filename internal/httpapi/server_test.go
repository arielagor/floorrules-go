package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arielagor/floorrules-go/internal/adapter"
	"github.com/arielagor/floorrules-go/internal/auth"
	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/metrics"
	"github.com/arielagor/floorrules-go/internal/service"
	"github.com/arielagor/floorrules-go/internal/store"
	"github.com/arielagor/floorrules-go/internal/store/memstore"
)

var key = []byte("test-only-hmac-key-0123456789abcdef")

const (
	issuer   = "https://issuer.test"
	audience = "floorrules"
)

type env struct {
	srv   *httptest.Server
	ssp   *adapter.Mock
	m     *metrics.Registry
	ready *atomic.Bool
	logs  *bytes.Buffer
}

func newEnv(t *testing.T, st store.Store) env {
	t.Helper()
	if st == nil {
		st = memstore.New()
	}
	ssp := adapter.NewMock()
	m := metrics.New()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	retry := adapter.DefaultRetryPolicy()
	retry.Sleep = func(context.Context, time.Duration) error { return nil }
	svc, err := service.New(st, ssp, service.Config{Retry: retry, Metrics: m, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewHMACVerifier(key, issuer, audience)
	if err != nil {
		t.Fatal(err)
	}
	ready := &atomic.Bool{}
	ready.Store(true)
	srv := httptest.NewServer(NewHandler(Deps{Service: svc, Verifier: v, Log: log, Metrics: m, Ready: ready}))
	t.Cleanup(srv.Close)
	return env{srv: srv, ssp: ssp, m: m, ready: ready, logs: logs}
}

func token(t *testing.T, scope string, pubs ...string) string {
	t.Helper()
	tok, err := auth.SignHS256(key, auth.Claims{
		Subject: "user:ops-1", Issuer: issuer, Audience: auth.Audience{audience},
		ExpiresAt: time.Now().Add(time.Hour).Unix(), Scope: scope, Publishers: pubs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

const allScopes = "rules:read rules:write plans:write plans:apply audit:read"

type call struct {
	method, path, tok, body string
	headers                 map[string]string
}

// response is what tests inspect. do() reads and closes the body itself, so
// no *http.Response with an open body escapes the helper.
type response struct {
	StatusCode int
	Header     http.Header
}

func (e env) do(t *testing.T, c call) (response, map[string]any) {
	t.Helper()
	var body io.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	}
	req, err := http.NewRequest(c.method, e.srv.URL+c.path, body)
	if err != nil {
		t.Fatal(err)
	}
	if c.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if len(raw) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("bad JSON %q: %v", raw, err)
		}
	}
	return response{StatusCode: resp.StatusCode, Header: resp.Header}, out
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

const ruleBody = `{"segment":{"device":"ctv","geo":"US","demand_partner":"magnite"},"floor_micros":4500000}`

func TestEndToEnd_RulePlanApplyReplayAudit(t *testing.T) {
	e := newEnv(t, nil)
	tok := token(t, allScopes, "acme-tv")

	resp, body := e.do(t, call{method: "POST", path: "/v1/publishers/acme-tv/rules", tok: tok, body: ruleBody})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create rule = %d %v", resp.StatusCode, body)
	}
	ruleID, _ := body["id"].(string)

	resp, body = e.do(t, call{method: "POST", path: "/v1/publishers/acme-tv/rules", tok: tok, body: ruleBody})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate rule = %d %v", resp.StatusCode, body)
	}

	resp, body = e.do(t, call{method: "GET", path: "/v1/publishers/acme-tv/rules", tok: tok})
	if resp.StatusCode != 200 || len(body["rules"].([]any)) != 1 {
		t.Fatalf("list rules = %d %v", resp.StatusCode, body)
	}

	resp, body = e.do(t, call{method: "POST", path: "/v1/publishers/acme-tv/plans", tok: tok})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create plan = %d %v", resp.StatusCode, body)
	}
	planID := body["id"].(string)

	resp, body = e.do(t, call{method: "GET", path: "/v1/publishers/acme-tv/plans/" + planID, tok: tok})
	if resp.StatusCode != 200 || body["status"] != "pending" {
		t.Fatalf("get plan = %d %v", resp.StatusCode, body)
	}

	applyPath := "/v1/publishers/acme-tv/plans/" + planID + "/apply"
	resp, body = e.do(t, call{method: "POST", path: applyPath, tok: tok})
	if resp.StatusCode != http.StatusBadRequest || errCode(body) != "invalid_idempotency_key" {
		t.Fatalf("apply without key = %d %v", resp.StatusCode, body)
	}

	idem := map[string]string{"Idempotency-Key": "op-2026-10-03-0001"}
	resp, body = e.do(t, call{method: "POST", path: applyPath, tok: tok, headers: idem})
	if resp.StatusCode != 200 || body["status"] != "applied" || resp.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("apply = %d %v", resp.StatusCode, body)
	}
	resp, body = e.do(t, call{method: "POST", path: applyPath, tok: tok, headers: idem})
	if resp.StatusCode != 200 || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay = %d %v", resp.StatusCode, body)
	}
	if e.ssp.Calls("SetFloor") != 1 {
		t.Fatalf("SetFloor called %d times, want 1", e.ssp.Calls("SetFloor"))
	}

	resp, body = e.do(t, call{method: "DELETE", path: "/v1/publishers/acme-tv/rules/" + ruleID, tok: tok})
	if resp.StatusCode != 200 || body["status"] != "disabled" {
		t.Fatalf("disable = %d %v", resp.StatusCode, body)
	}

	resp, body = e.do(t, call{method: "GET", path: "/v1/publishers/acme-tv/audit?limit=10", tok: tok})
	// rule.create, plan.create, plan.apply.started, plan.apply, rule.disable
	if resp.StatusCode != 200 || len(body["entries"].([]any)) != 5 {
		t.Fatalf("audit = %d %v", resp.StatusCode, body)
	}
	first := body["entries"].([]any)[0].(map[string]any)
	if first["action"] != "rule.disable" || first["actor"] != "user:ops-1" {
		t.Fatalf("newest audit entry = %v", first)
	}

	if got := e.m.Get("http_requests_total", "route", "POST /v1/publishers/{pub}/plans/{id}/apply", "code", "200"); got != 2 {
		t.Fatalf("http_requests_total for apply 200 = %v, want 2", got)
	}
}

func TestAuthZ(t *testing.T) {
	e := newEnv(t, nil)
	cases := []struct {
		name, tok, path string
		want            int
		code            string
	}{
		{"no token", "", "/v1/publishers/acme-tv/rules", 401, "unauthenticated"},
		{"garbage token", "abc.def.ghi", "/v1/publishers/acme-tv/rules", 401, "unauthenticated"},
		{"missing scope", token(t, "rules:read", "acme-tv"), "/v1/publishers/acme-tv/audit", 403, "insufficient_scope"},
		{"other publisher", token(t, allScopes, "acme-tv"), "/v1/publishers/rival-tv/rules", 403, "publisher_forbidden"},
		{"bad publisher id", token(t, allScopes, "*"), "/v1/publishers/ACME%20TV/rules", 400, "invalid_publisher"},
		{"wildcard grant", token(t, "rules:read", "*"), "/v1/publishers/rival-tv/rules", 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := e.do(t, call{method: "GET", path: c.path, tok: c.tok})
			if resp.StatusCode != c.want || errCode(body) != c.code {
				t.Fatalf("status=%d code=%q, want %d %q (%v)", resp.StatusCode, errCode(body), c.want, c.code, body)
			}
		})
	}
	// A read-only token cannot write.
	resp, _ := e.do(t, call{method: "POST", path: "/v1/publishers/acme-tv/rules", tok: token(t, "rules:read", "acme-tv"), body: ruleBody})
	if resp.StatusCode != 403 {
		t.Fatalf("read-only token wrote a rule: %d", resp.StatusCode)
	}
	// Tokens never appear in logs.
	if strings.Contains(e.logs.String(), "Bearer") || strings.Contains(e.logs.String(), "eyJ") {
		t.Fatal("token material found in logs")
	}
}

func TestInputHardening(t *testing.T) {
	e := newEnv(t, nil)
	tok := token(t, allScopes, "acme-tv")
	rules := "/v1/publishers/acme-tv/rules"
	cases := []struct {
		name, body string
		ctype      string
		want       int
		code       string
	}{
		{"validation", `{"segment":{"device":"fridge","geo":"USA"},"floor_micros":1}`, "", 400, "validation_failed"},
		{"unknown field", `{"segment":{},"floor_micros":4500000,"publisher_id":"rival-tv"}`, "", 400, "invalid_json"},
		{"trailing data", ruleBody + `{}`, "", 400, "invalid_json"},
		{"not json", `floor=4.5`, "", 400, "invalid_json"},
		{"float micros", `{"segment":{},"floor_micros":4.5}`, "", 400, "invalid_json"},
		{"wrong content type", ruleBody, "text/plain", 415, "unsupported_media_type"},
		{"too large", `{"segment":{"genre":"` + strings.Repeat("a", 70_000) + `"},"floor_micros":1}`, "", 413, "body_too_large"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := map[string]string{}
			if c.ctype != "" {
				h["Content-Type"] = c.ctype
			}
			resp, body := e.do(t, call{method: "POST", path: rules, tok: tok, body: c.body, headers: h})
			if resp.StatusCode != c.want || errCode(body) != c.code {
				t.Fatalf("status=%d code=%q, want %d %q", resp.StatusCode, errCode(body), c.want, c.code)
			}
		})
	}
	// Malformed IDs are a 404, never a database error.
	for _, p := range []string{"/v1/publishers/acme-tv/plans/not-a-uuid", "/v1/publishers/acme-tv/plans/'%3B--"} {
		resp, body := e.do(t, call{method: "GET", path: p, tok: tok})
		if resp.StatusCode != 404 {
			t.Fatalf("GET %s = %d %v", p, resp.StatusCode, body)
		}
	}
	resp, _ := e.do(t, call{method: "DELETE", path: rules + "/nope", tok: tok})
	if resp.StatusCode != 404 {
		t.Fatalf("DELETE bad id = %d", resp.StatusCode)
	}
	resp, _ = e.do(t, call{method: "GET", path: "/v1/publishers/acme-tv/audit?limit=100000", tok: tok})
	if resp.StatusCode != 400 {
		t.Fatalf("unbounded audit limit accepted: %d", resp.StatusCode)
	}
}

func TestApplyStatusCodes(t *testing.T) {
	e := newEnv(t, nil)
	tok := token(t, allScopes, "acme-tv")
	mk := func() string {
		_, body := e.do(t, call{method: "POST", path: "/v1/publishers/acme-tv/plans", tok: tok})
		return body["id"].(string)
	}
	e.do(t, call{method: "POST", path: "/v1/publishers/acme-tv/rules", tok: tok, body: ruleBody})

	// Drift -> 409 with a stale result.
	stale := mk()
	// Drift on the planned segment itself (a hand-set floor appears there).
	e.ssp.Seed("acme-tv", domain.PlatformFloor{Segment: domain.Segment{Device: "ctv", Geo: "US", Genre: "*", DemandPartner: "magnite"}, FloorMicros: 1_000_000, ManagedBy: "human"})
	resp, body := e.do(t, call{method: "POST", path: "/v1/publishers/acme-tv/plans/" + stale + "/apply", tok: tok, headers: map[string]string{"Idempotency-Key": "stale-key-0001"}})
	if resp.StatusCode != 409 || body["status"] != "stale" {
		t.Fatalf("stale apply = %d %v", resp.StatusCode, body)
	}
	// Remove the hand-set floor so the next plans are plain creates again.
	if err := e.ssp.DeleteFloor(context.Background(), "acme-tv", domain.Segment{Device: "ctv", Geo: "US", Genre: "*", DemandPartner: "magnite"}); err != nil {
		t.Fatal(err)
	}

	// Permanent SSP failure -> 502, and the replay returns the same 502.
	failing := mk()
	e.ssp.FailNext("SetFloor", &adapter.StatusError{Status: 400})
	path := "/v1/publishers/acme-tv/plans/" + failing + "/apply"
	h := map[string]string{"Idempotency-Key": "fail-key-00001"}
	resp, body = e.do(t, call{method: "POST", path: path, tok: tok, headers: h})
	if resp.StatusCode != 502 || body["status"] != "failed" {
		t.Fatalf("failed apply = %d %v", resp.StatusCode, body)
	}
	resp, _ = e.do(t, call{method: "POST", path: path, tok: tok, headers: h})
	if resp.StatusCode != 502 || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replayed failure = %d", resp.StatusCode)
	}

	// Same key, different body -> 422.
	resp, body = e.do(t, call{method: "POST", path: path, tok: tok, headers: h, body: `{"acknowledge_risky":true}`})
	if resp.StatusCode != 422 {
		t.Fatalf("key reuse = %d %v", resp.StatusCode, body)
	}

	// Platform down while planning -> 503 with Retry-After.
	fail := make([]error, 10)
	for i := range fail {
		fail[i] = &adapter.StatusError{Status: 503}
	}
	e.ssp.FailNext("ListFloors", fail...)
	resp, body = e.do(t, call{method: "POST", path: "/v1/publishers/acme-tv/plans", tok: tok})
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("plan with platform down = %d %v", resp.StatusCode, body)
	}
}

// brokenStore fails readiness and hides a raw database error.
type brokenStore struct{ store.Store }

func (brokenStore) Ping(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: refused") }
func (brokenStore) ListRules(context.Context, string) ([]domain.Rule, error) {
	return nil, errors.New(`pq: relation "rules" password=hunter2`)
}

func TestHealthReadinessAndErrorHiding(t *testing.T) {
	e := newEnv(t, nil)
	if resp, _ := e.do(t, call{method: "GET", path: "/healthz"}); resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, call{method: "GET", path: "/readyz"}); resp.StatusCode != 200 {
		t.Fatalf("readyz = %d", resp.StatusCode)
	}
	e.ready.Store(false) // shutdown has begun
	if resp, _ := e.do(t, call{method: "GET", path: "/readyz"}); resp.StatusCode != 503 {
		t.Fatalf("readyz while draining = %d", resp.StatusCode)
	}

	b := newEnv(t, brokenStore{memstore.New()})
	if resp, _ := b.do(t, call{method: "GET", path: "/readyz"}); resp.StatusCode != 503 {
		t.Fatalf("readyz with db down = %d", resp.StatusCode)
	}
	resp, body := b.do(t, call{method: "GET", path: "/v1/publishers/acme-tv/rules", tok: token(t, allScopes, "acme-tv")})
	raw, _ := json.Marshal(body)
	if resp.StatusCode != 500 || strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "relation") {
		t.Fatalf("internal error leaked to client: %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(b.logs.String(), "request_id") {
		t.Fatal("internal error not logged with request id")
	}
}

// M6 (day-2 review): /metrics was served on the API port, the one the load
// balancer exposes. Metrics live on their own listener (METRICS_ADDR) that
// only the in-cluster scraper can reach.
func TestMetricsAreNotOnTheAPIListener(t *testing.T) {
	e := newEnv(t, nil)
	if resp, _ := e.do(t, call{method: "GET", path: "/metrics"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics on the API handler = %d, want 404", resp.StatusCode)
	}
}

func TestRequestIDAndRecover(t *testing.T) {
	e := newEnv(t, nil)
	resp, _ := e.do(t, call{method: "GET", path: "/healthz", headers: map[string]string{"X-Request-ID": "abc-123"}})
	if resp.Header.Get("X-Request-ID") != "abc-123" {
		t.Fatalf("request id not echoed: %q", resp.Header.Get("X-Request-ID"))
	}
	resp, _ = e.do(t, call{method: "GET", path: "/healthz", headers: map[string]string{"X-Request-ID": "bad id\twith junk"}})
	if got := resp.Header.Get("X-Request-ID"); got == "" || strings.Contains(got, "junk") {
		t.Fatalf("unsafe request id echoed: %q", got)
	}

	s := &server{Deps{Log: slog.New(slog.DiscardHandler), Metrics: metrics.New()}}
	h := s.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 {
		t.Fatalf("panic not recovered to 500: %d", rec.Code)
	}
}
