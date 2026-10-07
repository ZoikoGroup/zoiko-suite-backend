package vault

// Key-encryption-key (KEK) providers — SEC-INV-07 "master key isolated from
// application workloads".
//
// The local backend holds its AES master key in process memory, so anything
// that can read this process's memory, environment or key file can decrypt
// every secret. A KeyWrapper moves the master key out of the process: the
// KMS holds it and only ever returns a wrapped (encrypted) or unwrapped data
// key. This service sees per-secret data keys, transiently, and never the
// key that protects them.
//
// Two providers, both over their plain REST APIs (no SDK dependency):
//   - TransitKeyWrapper — HashiCorp Vault / OpenBao Transit engine.
//   - GCPKMSKeyWrapper  — Google Cloud KMS, authenticated through the GCE/GKE
//     metadata server's workload service-account token.
//
// Which one a deployment uses is infrastructure's decision
// (VAULT_KEK_PROVIDER); nothing above this file depends on it.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrKEKUnavailable wraps every failure to reach or use the KMS, so the
// caller can fail closed (503) rather than treat it as missing material.
var ErrKEKUnavailable = errors.New("vault: key-encryption-key provider unavailable")

// KeyWrapper wraps and unwraps data keys under a master key it never
// releases. KeyRef names the master key (and, where the provider reports it,
// the key version) so a stored record says which key protects it.
type KeyWrapper interface {
	Wrap(ctx context.Context, dek []byte) (wrapped string, err error)
	Unwrap(ctx context.Context, wrapped string) (dek []byte, err error)
	KeyRef() string
}

// defaultKMSTimeout bounds every KMS round-trip so a hung KMS fails the
// request closed instead of pinning a goroutine.
const defaultKMSTimeout = 5 * time.Second

func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultKMSTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKEKUnavailable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		// The response body is not echoed: a KMS error can quote the request.
		return fmt.Errorf("%w: %s returned HTTP %d", ErrKEKUnavailable, url, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: undecodable response: %v", ErrKEKUnavailable, err)
	}
	return nil
}

// ── HashiCorp Vault / OpenBao Transit ────────────────────────────────────────

// TransitKeyWrapper wraps data keys with a Transit named key. The Vault
// token is re-read from TokenFile on every call so an agent-rotated token
// takes effect without a restart.
type TransitKeyWrapper struct {
	Addr      string // e.g. https://vault:8200
	Mount     string // transit mount, default "transit"
	KeyName   string
	TokenFile string
	Client    *http.Client
}

func (t *TransitKeyWrapper) KeyRef() string {
	return "transit:" + t.mount() + "/" + t.KeyName
}

func (t *TransitKeyWrapper) mount() string {
	if t.Mount == "" {
		return "transit"
	}
	return strings.Trim(t.Mount, "/")
}

func (t *TransitKeyWrapper) token() (string, error) {
	raw, err := os.ReadFile(t.TokenFile)
	if err != nil {
		return "", fmt.Errorf("%w: cannot read Vault token file: %v", ErrKEKUnavailable, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("%w: Vault token file is empty", ErrKEKUnavailable)
	}
	return tok, nil
}

func (t *TransitKeyWrapper) call(ctx context.Context, op string, in, out any) error {
	tok, err := t.token()
	if err != nil {
		return err
	}
	url := strings.TrimRight(t.Addr, "/") + "/v1/" + t.mount() + "/" + op + "/" + t.KeyName
	return postJSON(ctx, httpClient(t.Client), url, map[string]string{"X-Vault-Token": tok}, in, out)
}

func (t *TransitKeyWrapper) Wrap(ctx context.Context, dek []byte) (string, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := t.call(ctx, "encrypt", map[string]string{"plaintext": base64.StdEncoding.EncodeToString(dek)}, &out); err != nil {
		return "", err
	}
	if out.Data.Ciphertext == "" {
		return "", fmt.Errorf("%w: transit encrypt returned no ciphertext", ErrKEKUnavailable)
	}
	return out.Data.Ciphertext, nil
}

func (t *TransitKeyWrapper) Unwrap(ctx context.Context, wrapped string) ([]byte, error) {
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := t.call(ctx, "decrypt", map[string]string{"ciphertext": wrapped}, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Data.Plaintext)
}

// VerifyNonExportable reads the Transit key's policy and refuses a master key
// that Vault would hand out: exportable (the /export endpoint releases it) or
// allow_plaintext_backup (a backup carries it in the clear). Either one puts
// the key a single privileged API call away from leaving the KMS, which is
// the isolation SEC-INV-07 exists for. Cloud KMS keys cannot be exported at
// all, so GCPKMSKeyWrapper needs no equivalent.
func (t *TransitKeyWrapper) VerifyNonExportable(ctx context.Context) error {
	tok, err := t.token()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultKMSTimeout)
	defer cancel()
	url := strings.TrimRight(t.Addr, "/") + "/v1/" + t.mount() + "/keys/" + t.KeyName
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", tok)
	resp, err := httpClient(t.Client).Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKEKUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: read transit key policy: HTTP %d", ErrKEKUnavailable, resp.StatusCode)
	}
	var out struct {
		Data struct {
			Type                 string `json:"type"`
			Exportable           bool   `json:"exportable"`
			AllowPlaintextBackup bool   `json:"allow_plaintext_backup"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return fmt.Errorf("%w: undecodable transit key policy", ErrKEKUnavailable)
	}
	switch {
	case out.Data.Exportable:
		return fmt.Errorf("vault: transit key %s is exportable — the master key can leave the KMS; recreate it with exportable=false", t.KeyRef())
	case out.Data.AllowPlaintextBackup:
		return fmt.Errorf("vault: transit key %s allows plaintext backup — the master key can leave the KMS in the clear", t.KeyRef())
	case out.Data.Type != "aes256-gcm96" && out.Data.Type != "chacha20-poly1305":
		return fmt.Errorf("vault: transit key %s has type %q; want a symmetric encryption key (aes256-gcm96)", t.KeyRef(), out.Data.Type)
	}
	return nil
}

// ── Google Cloud KMS ─────────────────────────────────────────────────────────

// GCPKMSKeyWrapper wraps data keys with a Cloud KMS symmetric key, as the
// workload's own service account (metadata-server token), so no credential
// for the KMS is configured into this service either.
type GCPKMSKeyWrapper struct {
	// KeyName is the full resource name:
	// projects/P/locations/L/keyRings/R/cryptoKeys/K
	KeyName     string
	Endpoint    string // default https://cloudkms.googleapis.com
	MetadataURL string // default http://metadata.google.internal
	Client      *http.Client

	mu     sync.Mutex
	tok    string
	tokExp time.Time
}

func (g *GCPKMSKeyWrapper) KeyRef() string { return "gcpkms:" + g.KeyName }

func (g *GCPKMSKeyWrapper) accessToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tok != "" && time.Now().Before(g.tokExp) {
		return g.tok, nil
	}
	base := g.MetadataURL
	if base == "" {
		base = "http://metadata.google.internal"
	}
	ctx, cancel := context.WithTimeout(ctx, defaultKMSTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(base, "/")+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := httpClient(g.Client).Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: metadata token: %v", ErrKEKUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: metadata token HTTP %d", ErrKEKUnavailable, resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("%w: metadata token undecodable", ErrKEKUnavailable)
	}
	g.tok = out.AccessToken
	// Refresh a minute early so a token never expires mid-call.
	g.tokExp = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - time.Minute)
	return g.tok, nil
}

func (g *GCPKMSKeyWrapper) call(ctx context.Context, op string, in, out any) error {
	tok, err := g.accessToken(ctx)
	if err != nil {
		return err
	}
	ep := g.Endpoint
	if ep == "" {
		ep = "https://cloudkms.googleapis.com"
	}
	url := strings.TrimRight(ep, "/") + "/v1/" + g.KeyName + ":" + op
	return postJSON(ctx, httpClient(g.Client), url, map[string]string{"Authorization": "Bearer " + tok}, in, out)
}

func (g *GCPKMSKeyWrapper) Wrap(ctx context.Context, dek []byte) (string, error) {
	var out struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err := g.call(ctx, "encrypt", map[string]string{"plaintext": base64.StdEncoding.EncodeToString(dek)}, &out); err != nil {
		return "", err
	}
	if out.Ciphertext == "" {
		return "", fmt.Errorf("%w: cloud kms encrypt returned no ciphertext", ErrKEKUnavailable)
	}
	return out.Ciphertext, nil
}

func (g *GCPKMSKeyWrapper) Unwrap(ctx context.Context, wrapped string) ([]byte, error) {
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	if err := g.call(ctx, "decrypt", map[string]string{"ciphertext": wrapped}, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Plaintext)
}

func httpClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return http.DefaultClient
}
