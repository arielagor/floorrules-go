// Command devtoken mints a short-lived HS256 token for local testing. It
// reads the same AUTH_HMAC_SECRET / AUTH_HMAC_SECRET_FILE as the service. It
// is a development tool; production tokens come from the identity provider.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/arielagor/floorrules-go/internal/auth"
)

func main() {
	sub := flag.String("sub", "user:local-dev", "subject")
	scope := flag.String("scope", "rules:read", "space-separated scopes")
	pubs := flag.String("pubs", "", "comma-separated publisher IDs, or *")
	ttl := flag.Duration("ttl", 15*time.Minute, "lifetime (max 1h)")
	flag.Parse()

	secret := os.Getenv("AUTH_HMAC_SECRET")
	if path := os.Getenv("AUTH_HMAC_SECRET_FILE"); path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's own secret mount, by design.
		if err != nil {
			fail("cannot read AUTH_HMAC_SECRET_FILE")
		}
		secret = strings.TrimSpace(string(b))
	}
	if len(secret) < auth.MinKeyBytes {
		fail("AUTH_HMAC_SECRET must be at least 32 bytes")
	}
	if *pubs == "" {
		fail("-pubs is required (least privilege: name the publishers)")
	}
	if *ttl <= 0 || *ttl > time.Hour {
		fail("-ttl must be between 0 and 1h")
	}
	now := time.Now()
	tok, err := auth.SignHS256([]byte(secret), auth.Claims{
		Subject: *sub, Issuer: os.Getenv("AUTH_ISSUER"), Audience: auth.Audience{envOr("AUTH_AUDIENCE", "floorrules")},
		IssuedAt: now.Unix(), ExpiresAt: now.Add(*ttl).Unix(),
		Scope: *scope, Publishers: strings.Split(*pubs, ","),
	})
	if err != nil {
		fail(err.Error())
	}
	fmt.Println(tok)
}

func envOr(k, v string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return v
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "devtoken:", msg)
	os.Exit(2)
}
