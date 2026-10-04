// Package id generates random RFC 4122 version 4 UUIDs from crypto/rand.
package id

import (
	"crypto/rand"
	"fmt"
	"regexp"
)

// New returns a new random UUID in canonical text form.
func New() string {
	var b [16]byte
	// crypto/rand.Read never returns an error on supported platforms (Go 1.24+).
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Valid reports whether s is a canonical lowercase UUID. Handlers use it to
// reject malformed path IDs before they reach the database.
func Valid(s string) bool { return uuidRe.MatchString(s) }
