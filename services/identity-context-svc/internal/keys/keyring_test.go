package keys

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	idenv "zoiko.io/identity-envelope"
)

// fakeKMS signs like Cloud KMS: the private key stays inside it and an EC
// signature comes back ASN.1 DER.
type fakeKMS struct {
	key *ecdsa.PrivateKey
}

func (f *fakeKMS) PublicKey(context.Context, string) (string, string, error) {
	der, _ := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), "EC_SIGN_P256_SHA256", nil
}

func (f *fakeKMS) AsymmetricSign(_ context.Context, _ string, digest []byte) ([]byte, error) {
	return ecdsa.SignASN1(rand.Reader, f.key, digest)
}

func TestKMSSignedEnvelopeVerifies(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s, err := NewKMSSigner("kms-1", "projects/p/locations/l/keyRings/r/cryptoKeys/c/cryptoKeyVersions/1", &fakeKMS{key: k})
	if err != nil {
		t.Fatal(err)
	}
	ring, err := NewKeyring(s)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, err := ring.Sign(map[string]any{"iss": "identity-context-svc", "aud": "zoiko-internal", "iat": now.Unix(),
		"exp": now.Add(time.Minute).Unix(), "tenant_id": "t1",
		"principal": map[string]string{"principal_id": "p1"}}, "JWT")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := idenv.NewVerifier(idenv.VerifierOptions{Keys: ring.KeySource(), Issuer: "identity-context-svc", AcceptLegacyAudience: true})
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("KMS-signed (DER→JOSE) envelope did not verify: %v", err)
	}
}

func TestLoadPicksActiveAndPublishesRetired(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b *pem.Block) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(b), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, _ := rsa.GenerateKey(rand.Reader, 2048)
	b, _ := rsa.GenerateKey(rand.Reader, 2048)
	old, _ := rsa.GenerateKey(rand.Reader, 2048)
	oldPub, _ := x509.MarshalPKIXPublicKey(&old.PublicKey)
	pa := write("a.pem", &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(a)})
	pb := write("b.pem", &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(b)})
	po := write("old.pub", &pem.Block{Type: "PUBLIC KEY", Bytes: oldPub})

	specs, err := ParseSpecs("a=" + pa + ", b=" + pb)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := Load(Options{Signing: specs, ActiveKid: "b", VerifyOnly: []Spec{{Kid: "old", Source: po}}})
	if err != nil {
		t.Fatal(err)
	}
	if ring.ActiveKid() != "b" {
		t.Fatalf("active = %s", ring.ActiveKid())
	}
	set, _ := ring.JWKS()
	if len(set) != 3 {
		t.Fatalf("published %d keys, want 3", len(set))
	}
	if _, err := Load(Options{Signing: specs, ActiveKid: "zzz"}); err == nil {
		t.Fatal("an active kid that is not configured must be refused")
	}
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	if _, err := NewPEMSigner("s", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(small)})); err == nil {
		t.Fatal("a 1024-bit key must be refused")
	}
}

var _ crypto.PublicKey = (*rsa.PublicKey)(nil)
