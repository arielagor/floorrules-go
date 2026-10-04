package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes, test only

func verifier(t *testing.T, now time.Time) *HMACVerifier {
	t.Helper()
	v, err := NewHMACVerifier(testKey, "https://issuer.test", "floorrules")
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return now }
	return v
}

func claims(now time.Time) Claims {
	return Claims{
		Subject: "user:ops-1", Issuer: "https://issuer.test", Audience: Audience{"floorrules"},
		ExpiresAt: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(),
		Scope: "rules:read rules:write", Publishers: []string{"acme-tv"},
	}
}

func sign(t *testing.T, c Claims) string {
	t.Helper()
	tok, err := SignHS256(testKey, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestVerify_Valid(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p, err := verifier(t, now).Verify(context.Background(), sign(t, claims(now)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "user:ops-1" || !p.HasScope(ScopeRulesWrite) || p.HasScope(ScopePlansApply) {
		t.Fatalf("principal = %+v", p)
	}
	if !p.CanAccess("acme-tv") || p.CanAccess("other-pub") {
		t.Fatalf("publisher grants wrong: %+v", p)
	}
}

func TestVerify_AudienceAsString(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// Hand-build a token whose aud is a plain string.
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(
		`{"sub":"s","iss":"https://issuer.test","aud":"floorrules","exp":1800003600,"scope":"rules:read","pubs":["*"]}`))
	tok := resign(h + "." + body)
	p, err := verifier(t, now).Verify(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanAccess("anything") {
		t.Fatal("wildcard publisher grant not honoured")
	}
}

// resign signs an arbitrary header.body with the test key.
func resign(signing string) string {
	mac := hmac.New(sha256.New, testKey)
	mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerify_Rejects(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	good := sign(t, claims(now))
	parts := strings.Split(good, ".")

	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	hs512Header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS512","typ":"JWT"}`))

	mut := func(f func(*Claims)) string {
		c := claims(now)
		f(&c)
		return sign(t, c)
	}
	otherKey, _ := SignHS256([]byte("ffffffffffffffffffffffffffffffff"), claims(now))

	cases := map[string]string{
		"empty":            "",
		"garbage":          "not-a-jwt",
		"two parts":        parts[0] + "." + parts[1],
		"alg none":         noneHeader + "." + parts[1] + ".",
		"alg none signed":  resign(noneHeader + "." + parts[1]),
		"alg HS512":        resign(hs512Header + "." + parts[1]),
		"wrong key":        otherKey,
		"tampered body":    parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin","scope":"plans:apply","pubs":["*"]}`)) + "." + parts[2],
		"bad sig encoding": parts[0] + "." + parts[1] + ".%%%",
		"expired":          mut(func(c *Claims) { c.ExpiresAt = now.Add(-time.Minute).Unix() }),
		"no exp":           mut(func(c *Claims) { c.ExpiresAt = 0 }),
		"not yet valid":    mut(func(c *Claims) { c.NotBefore = now.Add(time.Hour).Unix() }),
		"wrong issuer":     mut(func(c *Claims) { c.Issuer = "https://evil.test" }),
		"wrong audience":   mut(func(c *Claims) { c.Audience = Audience{"other-service"} }),
		"no subject":       mut(func(c *Claims) { c.Subject = "" }),
		"oversized":        strings.Repeat("a", maxTokenBytes+1),
	}
	v := verifier(t, now)
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), tok); err != ErrUnauthenticated {
				t.Fatalf("want ErrUnauthenticated, got %v", err)
			}
		})
	}
}

func TestVerify_LeewayOnExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := claims(now)
	c.ExpiresAt = now.Add(-10 * time.Second).Unix() // within 30s leeway
	if _, err := verifier(t, now).Verify(context.Background(), sign(t, c)); err != nil {
		t.Fatalf("token 10s past exp should pass with 30s leeway: %v", err)
	}
}

func TestNewHMACVerifier_RejectsWeakConfig(t *testing.T) {
	if _, err := NewHMACVerifier([]byte("short"), "i", "a"); err == nil {
		t.Fatal("accepted a short key")
	}
	if _, err := NewHMACVerifier(testKey, "", "a"); err == nil {
		t.Fatal("accepted empty issuer")
	}
}

func TestContextRoundTrip(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty context returned a principal")
	}
	ctx := WithPrincipal(context.Background(), Principal{Subject: "x"})
	if p, ok := FromContext(ctx); !ok || p.Subject != "x" {
		t.Fatalf("got %+v, %v", p, ok)
	}
}
