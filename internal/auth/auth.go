// Package auth generates and verifies secret tokens. Tokens are random,
// high-entropy strings; only their SHA-256 hashes are stored.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
)

// Token prefixes make leaked tokens recognizable.
const (
	PrefixApp        = "plt_app_"
	PrefixWorker     = "plt_wrk_"
	PrefixEnrollment = "plt_enr_"
	PrefixAdmin      = "plt_adm_"
	PrefixProxy      = "plt_pxy_"
)

const hashPrefix = "sha256:"

// NewToken returns a new random token with the prefix.
func NewToken(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Hash returns the storage form of a token: "sha256:" followed by the hex
// encoded digest. A plain hash is sufficient because tokens carry 256 bits
// of entropy.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hashPrefix + hex.EncodeToString(sum[:])
}

// Verify reports whether token matches the stored hash, in constant time.
func Verify(token, hash string) bool {
	if token == "" || hash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(Hash(token)), []byte(hash)) == 1
}

// ValidHash reports whether s looks like a value produced by Hash.
func ValidHash(s string) bool {
	if !strings.HasPrefix(s, hashPrefix) {
		return false
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, hashPrefix))
	return err == nil && len(b) == sha256.Size
}

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewID returns a random identifier for records such as jobs.
func NewID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return idEncoding.EncodeToString(b)
}

// BearerToken extracts the token of an "Authorization: Bearer" header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}
