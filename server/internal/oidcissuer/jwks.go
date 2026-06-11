package oidcissuer

import (
	"encoding/base64"
	"math/big"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// jwk is one RFC 7517 JSON Web Key (RSA public, signing use). n and
// e are base64url WITHOUT padding — verifiers reject padded values.
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// toJWKS converts the store's public keys into the serving shape.
func toJWKS(keys []store.OIDCPublicKey) jwkSet {
	set := jwkSet{Keys: make([]jwk, 0, len(keys))}
	for _, k := range keys {
		set.Keys = append(set.Keys, jwk{
			Kty: "RSA",
			Use: "sig",
			Alg: k.Alg,
			Kid: k.Kid,
			N:   base64.RawURLEncoding.EncodeToString(k.Public.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.Public.E)).Bytes()),
		})
	}
	return set
}
