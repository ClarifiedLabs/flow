// Package token mints cryptographically-random, URL-safe bearer tokens shared by
// Flow's credential, web-session, and worker-config issuers.
package token

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Generate returns a 256-bit (32-byte) base64url-encoded random token.
func Generate() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}
