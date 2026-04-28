// Package apitoken implements the on-the-wire format for gocdnext
// API tokens — generation, hashing, parsing, equality. Tokens are
// 32 bytes of crypto-random material, encoded as base32 (no
// padding) for URL-safety, and prefixed so a leak in a log is
// scannable. Two prefixes:
//
//	gnk_<base32>      — user-owned token
//	gnk_sa_<base32>   — service-account-owned token
//
// The plaintext value is shown to the user EXACTLY ONCE at
// creation time. The database stores SHA-256(body) only — there's
// no way to recover the plaintext from the DB. Validation: hash
// the incoming bearer body, look up by hash, check expiry +
// revoke status.
package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	UserPrefix = "gnk_"
	SAPrefix   = "gnk_sa_"
	// 32 bytes of randomness → 52 base32 chars (no padding). The
	// resulting token is ~57 chars including the user prefix or
	// ~60 with the SA prefix — comfortable for shell scripts +
	// HTTP headers.
	bodyBytes = 32
	// PrefixDisplayChars is what the UI shows for audit ("which
	// token authenticated this call"). Long enough for ops to
	// disambiguate, short enough that a leaked log line doesn't
	// give away the secret.
	PrefixDisplayChars = 8
)

var (
	// b32 uses the RFC-4648 lowercase no-padding encoder so the
	// token reads cleanly without `=`s — easier on shell
	// expansion + URL-safe.
	b32 = base32.HexEncoding.WithPadding(base32.NoPadding)

	ErrMalformed = errors.New("apitoken: malformed bearer value")
)

// Kind tags which family of token this is. Surfaces in audit
// logs ("user token X used" vs "service account token Y used").
type Kind int

const (
	KindUser Kind = iota
	KindSA
)

// Generated is what NewUser/NewSA hand back — the plaintext (to
// show the user once), the hash (stored in the DB), the prefix
// (kept in the clear for audit), the kind.
type Generated struct {
	Plaintext string
	Hash      string
	Prefix    string
	Kind      Kind
}

// NewUser mints a fresh user-owned token. Crypto/rand failure is
// surfaced; callers treat it as a 500 (the platform can't generate
// secure tokens, no point falling through).
func NewUser() (Generated, error) {
	body, err := randomBody()
	if err != nil {
		return Generated{}, err
	}
	plain := UserPrefix + body
	return assemble(plain, body, KindUser), nil
}

// NewSA mints a service-account token.
func NewSA() (Generated, error) {
	body, err := randomBody()
	if err != nil {
		return Generated{}, err
	}
	plain := SAPrefix + body
	return assemble(plain, body, KindSA), nil
}

// Parse splits an incoming bearer value into (kind, body) without
// validating. The body is what gets hashed; the kind tells the
// caller which DB column to filter on later. ErrMalformed when
// the prefix is wrong — the bearer middleware treats this as
// "not our token" and falls through to the cookie path.
func Parse(bearer string) (kind Kind, body string, err error) {
	switch {
	case strings.HasPrefix(bearer, SAPrefix):
		// Service account check has to come BEFORE user prefix
		// because UserPrefix ("gnk_") is a substring of SAPrefix
		// ("gnk_sa_").
		return KindSA, bearer[len(SAPrefix):], nil
	case strings.HasPrefix(bearer, UserPrefix):
		return KindUser, bearer[len(UserPrefix):], nil
	default:
		return 0, "", ErrMalformed
	}
}

// Hash returns the SHA-256 hex digest of `body`. Stable across
// runs + processes, used for both insertion (`InsertAPIToken`)
// and lookup (`GetAPITokenByHash`).
func Hash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// PrefixOf returns the first PrefixDisplayChars of `body` — what
// gets stored in `api_tokens.prefix` and shown in the UI for
// audit purposes.
func PrefixOf(body string) string {
	if len(body) <= PrefixDisplayChars {
		return body
	}
	return body[:PrefixDisplayChars]
}

func randomBody() (string, error) {
	buf := make([]byte, bodyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b32.EncodeToString(buf), nil
}

func assemble(plain, body string, k Kind) Generated {
	return Generated{
		Plaintext: plain,
		Hash:      Hash(body),
		Prefix:    PrefixOf(body),
		Kind:      k,
	}
}
