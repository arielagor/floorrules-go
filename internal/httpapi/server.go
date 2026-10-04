// Package httpapi is the REST surface: routing, authN/Z, input decoding,
// error mapping, request IDs, access logs and request metrics.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/arielagor/floorrules-go/internal/auth"
	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/id"
	"github.com/arielagor/floorrules-go/internal/metrics"
	"github.com/arielagor/floorrules-go/internal/service"
)

const (
	maxBodyBytes   = 64 << 10
	requestTimeout = 30 * time.Second
)

// Deps are the handler's collaborators.
type Deps struct {
	Service  *service.Service
	Verifier auth.Verifier
	Log      *slog.Logger
	Metrics  *metrics.Registry
	// Ready is flipped to false at the start of graceful shutdown so the load
	// balancer stops sending traffic before the listener closes.
	Ready *atomic.Bool
}

type server struct{ Deps }

// NewHandler builds the routed, middleware-wrapped handler.
func NewHandler(d Deps) http.Handler {
	s := &server{d}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", d.Metrics)

	mux.Handle("POST /v1/publishers/{pub}/rules", s.authed(auth.ScopeRulesWrite, s.createRule))
	mux.Handle("GET /v1/publishers/{pub}/rules", s.authed(auth.ScopeRulesRead, s.listRules))
	mux.Handle("DELETE /v1/publishers/{pub}/rules/{id}", s.authed(auth.ScopeRulesWrite, s.disableRule))
	mux.Handle("POST /v1/publishers/{pub}/plans", s.authed(auth.ScopePlansWrite, s.createPlan))
	mux.Handle("GET /v1/publishers/{pub}/plans/{id}", s.authed(auth.ScopeRulesRead, s.getPlan))
	mux.Handle("POST /v1/publishers/{pub}/plans/{id}/apply", s.authed(auth.ScopePlansApply, s.applyPlan))
	mux.Handle("GET /v1/publishers/{pub}/audit", s.authed(auth.ScopeAuditRead, s.listAudit))

	return s.recoverer(s.requestID(s.accessLog(mux)))
}

// ---- middleware ----

type ctxKey int

const requestIDKey ctxKey = iota

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func (s *server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if !safeRequestID.MatchString(rid) { // never echo attacker-shaped values into logs
			rid = id.New()
		}
		w.Header().Set("X-Request-ID", rid)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, rid)))
	})
}

func requestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (s *server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		route := r.Pattern // set by ServeMux on the request it routed
		if route == "" {
			route = "unmatched"
		}
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		s.Metrics.Inc("http_requests_total", "route", route, "code", strconv.Itoa(status))
		// Authorization is never logged; only method, route template and outcome.
		s.Log.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

func (s *server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				// ErrAbortHandler is net/http's deliberate "abort this response"
				// signal; let the server handle it as designed.
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				s.Log.Error("panic", "request_id", requestIDFrom(r.Context()), "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type authedHandler func(w http.ResponseWriter, r *http.Request, p auth.Principal, pub string)

// authed enforces, in order: a valid bearer token, the route's scope, a
// well-formed publisher ID, and that the token is granted that publisher.
func (s *server) authed(scope string, h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="floorrules"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "missing bearer token", nil)
			return
		}
		p, err := s.Verifier.Verify(r.Context(), strings.TrimSpace(raw))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="floorrules", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid token", nil)
			return
		}
		if !p.HasScope(scope) {
			writeError(w, http.StatusForbidden, "insufficient_scope", "token lacks scope "+scope, nil)
			return
		}
		pub := r.PathValue("pub")
		if !domain.ValidPublisherID(pub) {
			writeError(w, http.StatusBadRequest, "invalid_publisher", "malformed publisher id", nil)
			return
		}
		if !p.CanAccess(pub) {
			writeError(w, http.StatusForbidden, "publisher_forbidden", "token is not granted this publisher", nil)
			return
		}
		ctx, cancel := context.WithTimeout(auth.WithPrincipal(r.Context(), p), requestTimeout)
		defer cancel()
		h(w, r.WithContext(ctx), p, pub)
	})
}

// ---- handlers ----

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.Ready != nil && !s.Ready.Load() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", "draining", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Service.Ready(ctx); err != nil {
		s.Log.Warn("readiness check failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "dependency unavailable", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type createRuleRequest struct {
	Segment     domain.Segment `json:"segment"`
	FloorMicros int64          `json:"floor_micros"`
	Currency    string         `json:"currency"`
}

func (s *server) createRule(w http.ResponseWriter, r *http.Request, p auth.Principal, pub string) {
	var req createRuleRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	rule, err := s.Service.CreateRule(r.Context(), p.Subject, domain.Rule{
		PublisherID: pub, Segment: req.Segment, FloorMicros: req.FloorMicros, Currency: req.Currency,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (s *server) listRules(w http.ResponseWriter, r *http.Request, _ auth.Principal, pub string) {
	rules, err := s.Service.ListRules(r.Context(), pub)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func (s *server) disableRule(w http.ResponseWriter, r *http.Request, p auth.Principal, pub string) {
	ruleID := r.PathValue("id")
	if !id.Valid(ruleID) {
		s.fail(w, r, service.ErrNotFound)
		return
	}
	rule, err := s.Service.DisableRule(r.Context(), p.Subject, pub, ruleID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (s *server) createPlan(w http.ResponseWriter, r *http.Request, p auth.Principal, pub string) {
	plan, err := s.Service.CreatePlan(r.Context(), p.Subject, pub)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, plan)
}

func (s *server) getPlan(w http.ResponseWriter, r *http.Request, _ auth.Principal, pub string) {
	planID := r.PathValue("id")
	if !id.Valid(planID) {
		s.fail(w, r, service.ErrNotFound)
		return
	}
	plan, err := s.Service.GetPlan(r.Context(), pub, planID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type applyRequest struct {
	AcknowledgeRisky bool `json:"acknowledge_risky"`
}

func (s *server) applyPlan(w http.ResponseWriter, r *http.Request, p auth.Principal, pub string) {
	planID := r.PathValue("id")
	if !id.Valid(planID) {
		s.fail(w, r, service.ErrNotFound)
		return
	}
	var req applyRequest
	if !s.decode(w, r, &req, false) {
		return
	}
	res, replayed, err := s.Service.ApplyPlan(r.Context(), p.Subject, pub, planID, r.Header.Get("Idempotency-Key"), req.AcknowledgeRisky)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	// The status is derived from the stored result, so a replay returns the
	// same code as the original response.
	code := http.StatusOK
	switch res.Status {
	case domain.PlanStale:
		code = http.StatusConflict
	case domain.PlanFailed, domain.PlanPartiallyApplied:
		code = http.StatusBadGateway
	}
	writeJSON(w, code, res)
}

func (s *server) listAudit(w http.ResponseWriter, r *http.Request, _ auth.Principal, pub string) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1-500", nil)
			return
		}
		limit = n
	}
	entries, err := s.Service.ListAudit(r.Context(), pub, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// ---- encoding and errors ----

// decode reads a single JSON object, bounded in size, rejecting unknown
// fields and trailing data. With required=false an empty body is allowed.
func (s *server) decode(w http.ResponseWriter, r *http.Request, into any, required bool) bool {
	if r.ContentLength == 0 && !required {
		return true
	}
	if ct := r.Header.Get("Content-Type"); ct != "" || required {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", nil)
			return false
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) && !required {
			return true
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large", nil)
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON for this endpoint", nil)
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be a single JSON object", nil)
		return false
	}
	return true
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *domain.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, "validation_failed", "rule is invalid", ve.Fields)
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found", nil)
	case errors.Is(err, service.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error(), nil)
	case errors.Is(err, service.ErrPlanNotPending), errors.Is(err, service.ErrApplyInProgress):
		writeError(w, http.StatusConflict, "conflict", err.Error(), nil)
	case errors.Is(err, service.ErrInvalidIdempotencyKey):
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", err.Error(), nil)
	case errors.Is(err, service.ErrIdempotencyKeyReused), errors.Is(err, service.ErrRiskyNotAcknowledged),
		errors.Is(err, domain.ErrPlanTooLarge):
		writeError(w, http.StatusUnprocessableEntity, "unprocessable", err.Error(), nil)
	case errors.Is(err, service.ErrPlatformUnavailable):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "platform_unavailable", "ad platform unavailable, retry later", nil)
	default:
		// Internal detail goes to the log with the request ID, never to the client.
		s.Log.Error("request failed", "request_id", requestIDFrom(r.Context()), "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
	}
}

type errorBody struct {
	Error struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields,omitempty"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string, fields map[string]string) {
	var b errorBody
	b.Error.Code, b.Error.Message, b.Error.Fields = code, msg, fields
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
