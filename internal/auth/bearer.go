package auth

import (
	"crypto/subtle"
	"strings"
)

const bearerPrefix = "Bearer "

// BearerHeader formats a shared secret for the Authorization header.
func BearerHeader(secret string) string {
	return bearerPrefix + secret
}

// ParseBearerToken extracts the token from an Authorization header.
func ParseBearerToken(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, bearerPrefix) {
		return "", false
	}
	// TrimSpace on the full header already turns a bare "Bearer " into "Bearer",
	// which fails the prefix check above, so the remaining suffix is non-empty.
	return strings.TrimSpace(strings.TrimPrefix(header, bearerPrefix)), true
}

// BearerTokenMatches reports whether header carries the expected bearer secret.
// The comparison is constant time so a wrong token does not leak how much of
// it was right.
func BearerTokenMatches(header, secret string) bool {
	got, ok := ParseBearerToken(header)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}
