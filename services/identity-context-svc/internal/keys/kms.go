package keys

import (
	"bytes"
	"context"
	"crypto"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// KMSClient is the slice of Cloud KMS the signer uses.
type KMSClient interface {
	// PublicKey returns the PEM public key and KMS algorithm of a key version.
	PublicKey(ctx context.Context, name string) (pemKey, algorithm string, err error)
	// AsymmetricSign signs a SHA-256 digest; the result is the raw KMS
	// signature (PKCS#1 v1.5 for RSA, ASN.1 DER for EC).
	AsymmetricSign(ctx context.Context, name string, digest []byte) ([]byte, error)
}

type kmsSigner struct {
	kid    string
	name   string
	alg    string
	pub    crypto.PublicKey
	client KMSClient
}

// NewKMSSigner builds a signer over a Cloud KMS asymmetric key version
// ("projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/n").
// The key's protection level (SOFTWARE or HSM) is a property of the KMS key;
// production uses HSM. Only the public half is ever held here.
func NewKMSSigner(kid, name string, client KMSClient) (Signer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pemKey, algorithm, err := client.PublicKey(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("key %q: read KMS public key: %w", kid, err)
	}
	pub, err := ParsePublicPEM([]byte(pemKey))
	if err != nil {
		return nil, fmt.Errorf("key %q: KMS public key: %w", kid, err)
	}
	var alg string
	switch {
	case strings.HasPrefix(algorithm, "RSA_SIGN_PKCS1_") && strings.HasSuffix(algorithm, "_SHA256"):
		alg = "RS256"
	case algorithm == "EC_SIGN_P256_SHA256":
		alg = "ES256"
	default:
		return nil, fmt.Errorf("key %q: KMS algorithm %s is not RS256 or ES256", kid, algorithm)
	}
	return &kmsSigner{kid: kid, name: name, alg: alg, pub: pub, client: client}, nil
}

func (k *kmsSigner) Kid() string              { return k.kid }
func (k *kmsSigner) Alg() string              { return k.alg }
func (k *kmsSigner) Public() crypto.PublicKey { return k.pub }

func (k *kmsSigner) SignDigest(digest []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sig, err := k.client.AsymmetricSign(ctx, k.name, digest)
	if err != nil {
		return nil, err
	}
	if k.alg != "ES256" {
		return sig, nil
	}
	var der struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(sig, &der); err != nil {
		return nil, fmt.Errorf("KMS EC signature: %w", err)
	}
	return joseECDSA(der.R, der.S), nil
}

// GCPKMS is a Cloud KMS REST client authenticated by the workload's service
// account through the metadata server (GKE Workload Identity / GCE).
type GCPKMS struct {
	Endpoint     string // default https://cloudkms.googleapis.com/v1/
	MetadataHost string // default metadata.google.internal
	HTTP         *http.Client

	mu    sync.Mutex
	token string
	until time.Time
}

// NewGCPKMS builds the client.
func NewGCPKMS() *GCPKMS {
	return &GCPKMS{Endpoint: "https://cloudkms.googleapis.com/v1/", MetadataHost: "metadata.google.internal",
		HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (g *GCPKMS) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && time.Now().Before(g.until) {
		return g.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+g.MetadataHost+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata token: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AccessToken == "" {
		return "", errors.New("metadata token: no access_token")
	}
	g.token = out.AccessToken
	g.until = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - time.Minute)
	return g.token, nil
}

func (g *GCPKMS) call(ctx context.Context, method, url string, body any, out any) error {
	tok, err := g.accessToken(ctx)
	if err != nil {
		return err
	}
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cloud kms: status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// PublicKey implements KMSClient.
func (g *GCPKMS) PublicKey(ctx context.Context, name string) (string, string, error) {
	var out struct {
		Pem       string `json:"pem"`
		Algorithm string `json:"algorithm"`
	}
	if err := g.call(ctx, http.MethodGet, g.Endpoint+name+"/publicKey", nil, &out); err != nil {
		return "", "", err
	}
	return out.Pem, out.Algorithm, nil
}

// AsymmetricSign implements KMSClient.
func (g *GCPKMS) AsymmetricSign(ctx context.Context, name string, digest []byte) ([]byte, error) {
	var out struct {
		Signature string `json:"signature"`
	}
	body := map[string]any{"digest": map[string]string{"sha256": base64.StdEncoding.EncodeToString(digest)}}
	if err := g.call(ctx, http.MethodPost, g.Endpoint+name+":asymmetricSign", body, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Signature)
}
