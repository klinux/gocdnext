package buildcache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Minted (per-run) build-cache tokens let the scheduler auto-wire a build
// without ever handling the project's static token (only its hash is stored).
// A token is a short-lived, stateless bearer credential:
//
//	bc1.<base64url(projectID[16] || expUnixSec[8] || flags[1])>.<base64url(HMAC-SHA256)>
//
// flags bit 0 (flagWrite) grants PUT (push); the rest are reserved.
//
// The endpoint verifies the HMAC with the server signing key, checks expiry,
// and confirms the embedded project id matches the project the request path
// (slug) resolves to — so a token minted for one project can't be replayed
// against another. Verification is bound to the project's current credential
// generation (its token hash), which the endpoint looks up by slug, so a
// rotate/disable revokes outstanding tokens. The "bc1." prefix makes it cheap
// to tell a minted token from a static project token (43-char base64url).
const (
	mintedPrefix  = "bc1."
	payloadLen    = 16 + 8 + 1 // projectID + expUnix + flags
	mintSigLen    = 32         // full HMAC-SHA256
	mintedTTLSkew = 30 * time.Second
	flagWrite     = 0x01 // may PUT (push) entries, not only GET
)

// MintToken produces a per-run token for projectID valid for ttl. write grants
// PUT (push) access — mint it true only for trusted runs (e.g. the default
// branch); PR/branch runs get a read-only token so they cannot poison or bloat
// the shared cache. key is the server build-cache signing secret; an empty key
// disables minting (returns "").
//
// gen is the project's current credential generation (its cache_token_hash). It
// is folded into the signature — NOT carried in the token — so a token is only
// valid while the project's token is unchanged. Disabling then re-enabling (or
// rotating) the cache changes/clears the generation, which invalidates every
// outstanding minted token immediately, even before its TTL. An empty gen
// (cache disabled) mints nothing.
func MintToken(projectID uuid.UUID, write bool, ttl time.Duration, key, gen []byte) string {
	if len(key) == 0 || len(gen) == 0 || ttl <= 0 {
		return ""
	}
	var payload [payloadLen]byte
	copy(payload[:16], projectID[:])
	binary.BigEndian.PutUint64(payload[16:24], uint64(time.Now().Add(ttl).Unix()))
	if write {
		payload[24] = flagWrite
	}
	sig := mintSign(key, gen, payload[:])
	return mintedPrefix +
		base64.RawURLEncoding.EncodeToString(payload[:]) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyMintedToken checks a token's signature + expiry and returns the project
// id it was minted for plus whether it carries write (push) access. gen must be
// the project's CURRENT credential generation (cache_token_hash); a token minted
// under a different (rotated) or empty (disabled) generation fails. ok=false for
// any malformed/forged/expired/stale-generation token.
func VerifyMintedToken(token string, key, gen []byte, now time.Time) (id uuid.UUID, write bool, ok bool) {
	if len(key) == 0 || len(gen) == 0 || !strings.HasPrefix(token, mintedPrefix) {
		return uuid.Nil, false, false
	}
	parts := strings.Split(token[len(mintedPrefix):], ".")
	if len(parts) != 2 {
		return uuid.Nil, false, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(payload) != payloadLen {
		return uuid.Nil, false, false
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(gotSig) != mintSigLen {
		return uuid.Nil, false, false
	}
	wantSig := mintSign(key, gen, payload)
	if !hmac.Equal(gotSig, wantSig) {
		return uuid.Nil, false, false
	}
	exp := int64(binary.BigEndian.Uint64(payload[16:24]))
	if now.Add(-mintedTTLSkew).Unix() > exp {
		return uuid.Nil, false, false
	}
	copy(id[:], payload[:16])
	return id, payload[24]&flagWrite != 0, true
}

// LooksLikeMinted reports whether a credential is a minted token (vs a static
// project token), so the handler routes it to HMAC verification.
func LooksLikeMinted(token string) bool {
	return strings.HasPrefix(token, mintedPrefix)
}

func mintSign(key, gen, payload []byte) []byte {
	mac := hmac.New(sha256.New, key)
	// Domain-separate from any other HMAC use of the same key, then bind the
	// signature to the project's credential generation (gen) so a rotate/disable
	// invalidates the token. Length-prefix gen so it can't be confused with the
	// payload that follows.
	mac.Write([]byte("gocdnext-buildcache\x00"))
	var genLen [8]byte
	binary.BigEndian.PutUint64(genLen[:], uint64(len(gen)))
	mac.Write(genLen[:])
	mac.Write(gen)
	mac.Write(payload)
	return mac.Sum(nil)
}
