package jwks

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A nil *http.Client (mTLS off for the peer) must still fetch, not panic.
func TestNewClientWithHTTPClient_NilClientFetches(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer srv.Close()
	pub, err := NewClientWithHTTPClient(srv.URL, time.Minute, nil).PublicKey(context.Background(), "k1")
	if err != nil || pub.N.Cmp(key.N) != 0 {
		t.Fatalf("nil client: %v", err)
	}
}
