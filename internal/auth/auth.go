// Package auth verifies bearer tokens and carries the caller's identity and
// least-privilege grants through the request context.
//
// The sample verifies HS256 JWTs with a shared secret, implemented on the
// standard library so every check is visible and tested. A production
// deployment would put an RS256/ES256 verifier backed by the identity
// provider's JWKS (via a vetted library) behind the same Verifier interface;
// nothing outside this package would change.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Scopes. Each route requires exactly one; tokens should carry the minimum.
const (
	ScopeRulesRead  = "rules:read"
	ScopeRulesWrite = "rules:write"
	ScopePlansWrite = "plans:write"
	ScopePlansApply = "plans:apply"
	ScopeAuditRead  = "audit:read"
)

// AllPublishers is the publisher grant that covers every publisher.
const AllPublishers = "*"

// ErrUnauthenticated covers every token failure. Verify wraps it with a
// fixed reason ("expired", "signature mismatch", ...) for the server log;
// callers match it with errors.Is and send the client one generic 401. A
// reason never contains the token or any value read from it.
var ErrUnauthenticated = errors.New("unauthenticated")

func reject(reason string) error { return fmt.Errorf("%w: %s", ErrUnauthenticated, reason) }

// Principal is the verified caller.
type Principal struct {
	Subject    string
	Scopes     []string
	Publishers []string // publisher IDs this caller may act on, or ["*"]
}

// HasScope reports whether the principal holds scope.
func (p Principal) HasScope(scope string) bool { return slices.Contains(p.Scopes, scope) }

// CanAccess reports whether the principal may act on publisherID. This is
// the tenant boundary: a token for one publisher cannot read or change
// another publisher's floors even with the right scope.
func (p Principal) CanAccess(publisherID string) bool {
	return slices.Contains(p.Publishers, AllPublishers) || slices.Contains(p.Publishers, publisherID)
}

// Verifier turns a raw bearer token into a Principal.
type Verifier interface {
	Verify(ctx context.Context, token string) (Principal, error)
}

// HMACVerifier verifies HS256 JWTs.
type HMACVerifier struct {
	key      []byte
	issuer   string
	audience string
	leeway   time.Duration
	now      func() time.Time
}

// MinKeyBytes is the minimum HMAC secret length accepted (256 bits).
const MinKeyBytes = 32

// NewHMACVerifier validates the configuration up front so a weak or missing
// secret fails at startup, not at the first request.
func NewHMACVerifier(key []byte, issuer, audience string) (*HMACVerifier, error) {
	if len(key) < MinKeyBytes {
		return nil, errors.New("auth: HMAC key must be at least 32 bytes")
	}
	if issuer == "" || audience == "" {
		return nil, errors.New("auth: issuer and audience are required")
	}
	return &HMACVerifier{key: key, issuer: issuer, audience: audience, leeway: 30 * time.Second, now: time.Now}, nil
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Claims is the token body this service understands.
type Claims struct {
	Subject    string   `json:"sub"`
	Issuer     string   `json:"iss"`
	Audience   Audience `json:"aud"`
	ExpiresAt  int64    `json:"exp"`
	NotBefore  int64    `json:"nbf,omitempty"`
	IssuedAt   int64    `json:"iat,omitempty"`
	Scope      string   `json:"scope"`
	Publishers []string `json:"pubs"`
}

// Audience accepts the JWT "aud" claim as a string or an array of strings.
type Audience []string

// UnmarshalJSON implements json.Unmarshaler.
func (a *Audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// maxTokenBytes bounds work done on attacker-supplied input.
const maxTokenBytes = 4096

// Verify implements Verifier.
func (v *HMACVerifier) Verify(_ context.Context, token string) (Principal, error) {
	if token == "" || len(token) > maxTokenBytes {
		return Principal{}, reject("empty or oversized token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, reject("malformed token")
	}

	var h header
	if err := decodeSegment(parts[0], &h); err != nil {
		return Principal{}, reject("malformed header")
	}
	// Pin the algorithm. Never let the token choose it: that is how "none"
	// and RS/HS confusion attacks work.
	if h.Alg != "HS256" {
		return Principal{}, reject("unsupported alg")
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, reject("signature not base64url")
	}
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) { // constant-time compare
		return Principal{}, reject("signature mismatch")
	}

	var c Claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return Principal{}, reject("malformed claims")
	}
	now := v.now()
	switch {
	case c.Subject == "":
		return Principal{}, reject("missing subject")
	case c.Issuer != v.issuer:
		return Principal{}, reject("wrong issuer")
	case !slices.Contains(c.Audience, v.audience):
		return Principal{}, reject("wrong audience")
	case c.ExpiresAt == 0: // exp is mandatory; no immortal tokens
		return Principal{}, reject("missing exp")
	case now.After(time.Unix(c.ExpiresAt, 0).Add(v.leeway)):
		return Principal{}, reject("expired")
	case c.NotBefore != 0 && now.Add(v.leeway).Before(time.Unix(c.NotBefore, 0)):
		return Principal{}, reject("not yet valid")
	}
	return Principal{Subject: c.Subject, Scopes: strings.Fields(c.Scope), Publishers: c.Publishers}, nil
}

func decodeSegment(seg string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

// SignHS256 mints a token. It exists for tests and the local devtoken
// command; the service itself never issues tokens.
func SignHS256(key []byte, c Claims) (string, error) {
	h, err := json.Marshal(header{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(c)

	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal set by the auth middleware.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
