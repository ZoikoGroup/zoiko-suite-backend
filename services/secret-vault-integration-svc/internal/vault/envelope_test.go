package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeKMS stands in for the KMS: it holds the master key, and the backend
// under test only ever sees what Wrap/Unwrap return.
type fakeKMS struct {
	master []byte
	down   bool
	calls  int
}

func newFakeKMS(t *testing.T) *fakeKMS {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return &fakeKMS{master: k}
}

func (f *fakeKMS) KeyRef() string { return "fake:kek/1" }

func (f *fakeKMS) Wrap(_ context.Context, dek []byte) (string, error) {
	f.calls++
	if f.down {
		return "", ErrKEKUnavailable
	}
	gcm, _ := newGCM(f.master)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(append(nonce, gcm.Seal(nil, nonce, dek, nil)...)), nil
}

func (f *fakeKMS) Unwrap(_ context.Context, wrapped string) ([]byte, error) {
	f.calls++
	if f.down {
		return nil, ErrKEKUnavailable
	}
	raw, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return nil, err
	}
	gcm, _ := newGCM(f.master)
	n := gcm.NonceSize()
	return gcm.Open(nil, raw[:n], raw[n:], nil)
}

func TestEnvelope_RoundTripAndNoKeyOrPlaintextAtRest(t *testing.T) {
	ctx := context.Background()
	kms := newFakeKMS(t)
	path := filepath.Join(t.TempDir(), "store")
	b, err := NewEnvelopeFileVaultBackend(ctx, path, kms)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("db-password-correct-horse")
	if err := b.Put(ctx, "db/primary", secret); err != nil {
		t.Fatal(err)
	}
	got, err := b.GetMaterial(ctx, "db/primary")
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("round trip: %q, %v", got, err)
	}

	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, secret) {
		t.Fatal("plaintext material found in the store file")
	}
	if bytes.Contains(raw, []byte(hex.EncodeToString(kms.master))) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(kms.master))) {
		t.Fatal("master key found in the store file")
	}
	var st envelopeStore
	_ = json.Unmarshal(raw, &st)
	rec := st.Records["db/primary"]
	if rec.Alg != AlgEnvelopeAES256GCMv1 || rec.KeyRef != "fake:kek/1" || rec.WrappedDEK == "" {
		t.Fatalf("record does not carry its algorithm and KEK reference: %+v", rec)
	}
}

// SEC-INV-07 in its negative form: the process holds nothing that decrypts
// the store on its own. Reopening the file with a different KMS master key
// (e.g. a copied store file on another host) must fail to boot.
func TestEnvelope_StoreUnreadableWithoutTheKMS(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	b, _ := NewEnvelopeFileVaultBackend(ctx, path, newFakeKMS(t))
	_ = b.Put(ctx, "db/primary", []byte("s3cret"))

	if _, err := NewEnvelopeFileVaultBackend(ctx, path, newFakeKMS(t)); err == nil {
		t.Fatal("a store opened under a different KMS master key must not unwrap")
	}
}

func TestEnvelope_KMSOutageFailsClosed(t *testing.T) {
	ctx := context.Background()
	kms := newFakeKMS(t)
	b, _ := NewEnvelopeFileVaultBackend(ctx, filepath.Join(t.TempDir(), "store"), kms)
	_ = b.Put(ctx, "db/primary", []byte("s3cret"))
	kms.down = true
	if _, err := b.Get(ctx, "db/primary", "req-1", time.Now().Add(time.Minute)); !errors.Is(err, ErrKEKUnavailable) {
		t.Fatalf("KMS outage must refuse the lease, got %v", err)
	}
	if err := b.Put(ctx, "db/other", []byte("x")); !errors.Is(err, ErrKEKUnavailable) {
		t.Fatalf("KMS outage must refuse writes, got %v", err)
	}
}

func TestEnvelope_RecordMovedToAnotherPathDoesNotOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	b, _ := NewEnvelopeFileVaultBackend(ctx, path, newFakeKMS(t))
	_ = b.Put(ctx, "db/primary", []byte("primary"))
	_ = b.Put(ctx, "db/replica", []byte("replica"))

	raw, _ := os.ReadFile(path)
	var st envelopeStore
	_ = json.Unmarshal(raw, &st)
	st.Records["db/replica"] = st.Records["db/primary"]
	_ = b.save(st)

	if _, err := b.GetMaterial(ctx, "db/replica"); err == nil {
		t.Fatal("a record copied under another path must fail its AAD check")
	}
}

func TestEnvelope_LeaseTokensSurviveRestartAndAreNotSignedWithKEK(t *testing.T) {
	ctx := context.Background()
	kms := newFakeKMS(t)
	path := filepath.Join(t.TempDir(), "store")
	b, _ := NewEnvelopeFileVaultBackend(ctx, path, kms)
	_ = b.Put(ctx, "db/primary", []byte("s3cret"))
	tok, err := b.Get(ctx, "db/primary", "req-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	b2, err := NewEnvelopeFileVaultBackend(ctx, path, kms)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b2.Verify(ctx, tok); err != nil {
		t.Fatalf("token must verify after restart (signing key persisted wrapped): %v", err)
	}
	if bytes.Equal(b.signingKey, kms.master) {
		t.Fatal("lease signing key must not be the master key")
	}
	forged, _ := mintLeaseToken(kms.master, "db/primary", "req-1", time.Now().Add(time.Hour))
	if _, err := b.Verify(ctx, forged); !errors.Is(err, ErrLeaseTokenInvalid) {
		t.Fatalf("a token signed with the KEK must not verify, got %v", err)
	}
}

func TestEnvelope_LegacyStoreRefusedThenMigrated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	keyFile := filepath.Join(dir, "legacy.key")
	legacyKey := make([]byte, 32)
	_, _ = rand.Read(legacyKey)
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(legacyKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := NewLocalFileVaultBackendFromFile(store, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	_ = legacy.Put(ctx, "db/primary", []byte("old-secret"))
	_ = legacy.Put(ctx, "format", []byte("path named like a header field"))

	kms := newFakeKMS(t)
	if _, err := NewEnvelopeFileVaultBackend(ctx, store, kms); !errors.Is(err, ErrLegacyStoreFormat) {
		t.Fatalf("legacy store must be refused, not reinterpreted: %v", err)
	}
	n, err := MigrateLocalStore(ctx, store, keyFile, kms)
	if err != nil || n != 2 {
		t.Fatalf("migrate: n=%d err=%v", n, err)
	}
	b, err := NewEnvelopeFileVaultBackend(ctx, store, kms)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"db/primary": "old-secret", "format": "path named like a header field"} {
		got, err := b.GetMaterial(ctx, path)
		if err != nil || string(got) != want {
			t.Fatalf("%s after migration: %q, %v", path, got, err)
		}
	}
	if _, err := os.Stat(store + ".legacy-bak"); err != nil {
		t.Fatalf("legacy backup must be kept: %v", err)
	}
}

func TestTransitKeyWrapper_ProtocolAndFailure(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "tok-1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/v1/transit/encrypt/svi-kek":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"ciphertext": "vault:v1:" + in["plaintext"]}})
		case "/v1/transit/decrypt/svi-kek":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": strings.TrimPrefix(in["ciphertext"], "vault:v1:")}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	tokFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokFile, []byte("tok-1\n"), 0o600)

	tw := &TransitKeyWrapper{Addr: srv.URL, KeyName: "svi-kek", TokenFile: tokFile}
	wrapped, err := tw.Wrap(context.Background(), []byte("dek"))
	if err != nil || !strings.HasPrefix(wrapped, "vault:v1:") {
		t.Fatalf("wrap: %q %v", wrapped, err)
	}
	dek, err := tw.Unwrap(context.Background(), wrapped)
	if err != nil || string(dek) != "dek" {
		t.Fatalf("unwrap: %q %v", dek, err)
	}
	fail = true
	if _, err := tw.Wrap(context.Background(), []byte("dek")); !errors.Is(err, ErrKEKUnavailable) {
		t.Fatalf("Vault 500 must be ErrKEKUnavailable, got %v", err)
	}
	_ = os.WriteFile(tokFile, []byte("wrong"), 0o600)
	fail = false
	if _, err := tw.Wrap(context.Background(), []byte("dek")); !errors.Is(err, ErrKEKUnavailable) {
		t.Fatalf("a rejected token must be ErrKEKUnavailable, got %v", err)
	}
}

func TestGCPKMSKeyWrapper_ProtocolAndFailure(t *testing.T) {
	var tokenFetches int
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		tokenFetches++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.x", "expires_in": 3600})
	}))
	defer meta.Close()
	const key = "projects/p/locations/l/keyRings/r/cryptoKeys/k"
	var down bool
	kms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.x" || down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/v1/" + key + ":encrypt":
			_ = json.NewEncoder(w).Encode(map[string]string{"ciphertext": in["plaintext"]})
		case "/v1/" + key + ":decrypt":
			_ = json.NewEncoder(w).Encode(map[string]string{"plaintext": in["ciphertext"]})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer kms.Close()

	g := &GCPKMSKeyWrapper{KeyName: key, Endpoint: kms.URL, MetadataURL: meta.URL}
	wrapped, err := g.Wrap(context.Background(), []byte("dek"))
	if err != nil {
		t.Fatal(err)
	}
	dek, err := g.Unwrap(context.Background(), wrapped)
	if err != nil || string(dek) != "dek" {
		t.Fatalf("unwrap: %q %v", dek, err)
	}
	if tokenFetches != 1 {
		t.Fatalf("access token must be cached, fetched %d times", tokenFetches)
	}
	down = true
	if _, err := g.Wrap(context.Background(), []byte("dek")); !errors.Is(err, ErrKEKUnavailable) {
		t.Fatalf("KMS 503 must be ErrKEKUnavailable, got %v", err)
	}
}

func TestTransitKeyWrapper_VerifyNonExportable(t *testing.T) {
	policy := map[string]any{"type": "aes256-gcm96", "exportable": false, "allow_plaintext_backup": false}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/transit/keys/svi-kek" || r.Header.Get("X-Vault-Token") != "tok-1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": policy})
	}))
	defer srv.Close()
	tokFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokFile, []byte("tok-1"), 0o600)
	tw := &TransitKeyWrapper{Addr: srv.URL, KeyName: "svi-kek", TokenFile: tokFile}

	if err := tw.VerifyNonExportable(context.Background()); err != nil {
		t.Fatalf("a non-exportable aes key must pass: %v", err)
	}
	policy["exportable"] = true
	if err := tw.VerifyNonExportable(context.Background()); err == nil || !strings.Contains(err.Error(), "exportable") {
		t.Fatalf("an exportable key must be refused, got %v", err)
	}
	policy["exportable"], policy["allow_plaintext_backup"] = false, true
	if err := tw.VerifyNonExportable(context.Background()); err == nil {
		t.Fatal("a key allowing plaintext backup must be refused")
	}
	policy["allow_plaintext_backup"], policy["type"] = false, "rsa-2048"
	if err := tw.VerifyNonExportable(context.Background()); err == nil {
		t.Fatal("a non-symmetric key must be refused as a KEK")
	}
}
