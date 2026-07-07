package auth

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"math/big"
)

type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksResponse struct {
	Keys []jwk `json:"keys"`
}

// NewJWKSHandler exposes the signer's public key in standard JWKS format
// so any service can verify envelopes independently, without ever holding
// the private key.
func NewJWKSHandler(pub *rsa.PublicKey, kid string) http.HandlerFunc {
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	resp := jwksResponse{Keys: []jwk{{
		Kty: "RSA",
		Use: "sig",
		Kid: kid,
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
	}}}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
func (s *JWTSigner) PublicKey() *rsa.PublicKey {
	return &s.privateKey.PublicKey
}
