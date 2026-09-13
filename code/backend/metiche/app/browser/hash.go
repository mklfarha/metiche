// Package browser is metiche's browser sign-in: the single-use sign-in link an
// agent mints (docs/BOARD_LOGIN.md §2.3), the exchange that turns it into a
// browser session (§2.4), the per-request session check (§2.7) and sign out /
// revocation (§2.8), plus the in-cluster REST handlers for /v1/browser/*.
//
// It is the ONLY implementation of "whose browser session is this?", in the
// same way app/mcp.IdentityByToken is the only implementation of "whose token
// is this?". app/authz (the board gate) and app/mcp (open_board,
// sign_out_browsers) both import this package; it imports neither, so no
// cycle can form. Its hashing is defined here with crypto/sha256 and
// crypto/subtle rather than imported from app/mcp, and hash_parity_test.go
// pins it to mcp.HashToken / mcp.VerifyToken.
//
// # Secrets
//
// A link secret is "mbl_" + base64url(32 bytes of crypto/rand); a session
// secret is "mbs_" + the same. Only Hash(secret) is stored. Neither is ever
// logged, placed in an error, or accepted from a URL: the link arrives in a
// JSON body, the session in the X-Metiche-Browser-Session header.
//
// # Errors
//
// Every function here reports refusals with exactly one opaque sentinel per
// operation (ErrLinkNotUsable, ErrUnauthenticated, ErrSessionNotFound,
// ErrMintRefused, ErrTooManyLinks), and every database failure as an error
// wrapping ErrUnavailable. A caller must map ErrUnavailable to 503 and never to
// 401/404: a board that reads "database down" as "session invalid" clears a
// good cookie.
package browser

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// LinkPrefix marks a sign-in link secret, so a leaked one is greppable.
	LinkPrefix = "mbl_"
	// SessionPrefix marks a browser session secret (the cookie value).
	SessionPrefix = "mbs_"
	// secretEntropyBytes is 256 bits from crypto/rand, as app/mcp.MintToken.
	secretEntropyBytes = 32
)

// Hash is the stored form of a link or session secret: sha256, hex, of the
// whitespace-trimmed secret. It is byte-for-byte the definition of
// app/mcp.HashToken (hash_parity_test.go asserts it).
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])
}

// Verify reports whether a presented secret matches a stored hash, in constant
// time with respect to the hash contents. Same semantics as
// app/mcp.VerifyToken: an empty secret or a stored hash that is not 64
// characters never verifies.
func Verify(secret, storedHash string) bool {
	if secret == "" || len(storedHash) != sha256.Size*2 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(Hash(secret)), []byte(storedHash)) == 1
}

// MintLinkSecret returns a fresh "mbl_" link secret and its stored hash.
func MintLinkSecret() (secret, hash string, err error) { return mintSecret(LinkPrefix) }

// MintSessionSecret returns a fresh "mbs_" session secret and its stored hash.
func MintSessionSecret() (secret, hash string, err error) { return mintSecret(SessionPrefix) }

func mintSecret(prefix string) (string, string, error) {
	buf := make([]byte, secretEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("could not generate a secret: %w", err)
	}
	secret := prefix + base64.RawURLEncoding.EncodeToString(buf)
	return secret, Hash(secret), nil
}
