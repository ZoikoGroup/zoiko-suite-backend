// Package unsubscribe issues and opens the one-click unsubscribe links of
// RFC 8058 (ZS-SVC-Y-001 §11.1, §11.4, INV-25, NP-46).
//
// The receiver is reached by mail clients, not by ZoikoSuite services, so it
// carries no principal and is exempt from the envelope. The link itself is
// therefore the only credential, and it has to be one: the receiver used to
// accept a bare ?tenant_id=…&email=… and write whatever it was told, so one
// anonymous POST could suppress any address in any tenant — and, because the
// legacy list upserts, rewrite a recorded hard bounce as an unsubscribe,
// which re-opened transactional and security mail to a dead address.
//
// A token is AES-256-GCM over (tenant, address). GCM authenticates, so a
// forged or edited token does not open; and it encrypts, so the recipient's
// address is not written into a URL that mail clients, proxies and provider
// logs record (§11.2). Tokens do not expire: an unsubscribe link must keep
// working for as long as the message that carries it can be read.
package unsubscribe

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrInvalidToken is every way a token can fail to open: malformed, forged,
// edited, sealed under another key, or missing a field. The receiver answers
// all of them the same way and records nothing.
var ErrInvalidToken = errors.New("invalid unsubscribe token")

// keyLabel separates this key from every other use of the configured secret.
const keyLabel = "zoiko/notification-svc/unsubscribe/v1"

// Claims is what a token proves: this tenant mailed this address.
type Claims struct {
	TenantID string `json:"t"`
	Email    string `json:"e"`
}

// Codec seals and opens tokens and builds the RFC 8058 headers.
type Codec struct {
	aead    cipher.AEAD
	baseURL string
}

// New returns a Codec. The secret must be at least 32 bytes; baseURL is the
// externally reachable https origin of this service (mail clients POST to
// it), and must be absolute because a List-Unsubscribe URI cannot be relative.
func New(secret []byte, baseURL string) (*Codec, error) {
	if len(secret) < 32 {
		return nil, errors.New("unsubscribe secret must be at least 32 bytes")
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("unsubscribe base URL %q must be an absolute http(s) URL", baseURL)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(keyLabel))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Codec{aead: aead, baseURL: u.String()}, nil
}

// Seal returns the opaque token for (tenant, address).
func (c *Codec) Seal(tenantID, email string) (string, error) {
	cl := Claims{TenantID: strings.TrimSpace(tenantID), Email: normalize(email)}
	if cl.TenantID == "" || cl.Email == "" {
		return "", errors.New("unsubscribe token needs a tenant and an address")
	}
	plain, err := json.Marshal(cl)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("unsubscribe nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, plain, []byte(keyLabel))), nil
}

// Open verifies a token and returns what it proves.
func (c *Codec) Open(token string) (Claims, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil || len(raw) <= c.aead.NonceSize() {
		return Claims{}, ErrInvalidToken
	}
	n := c.aead.NonceSize()
	plain, err := c.aead.Open(nil, raw[:n], raw[n:], []byte(keyLabel))
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var cl Claims
	if err := json.Unmarshal(plain, &cl); err != nil || cl.TenantID == "" || cl.Email == "" {
		return Claims{}, ErrInvalidToken
	}
	return cl, nil
}

// Headers returns the List-Unsubscribe and List-Unsubscribe-Post headers for
// a message to email in tenantID. There is deliberately no mailto: variant —
// nothing processes replies to an unsubscribe mailbox (OD-09), and a link that
// silently does nothing is worse than none.
func (c *Codec) Headers(tenantID, email string) (map[string]string, error) {
	tok, err := c.Seal(tenantID, email)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"List-Unsubscribe":      "<" + c.baseURL + "/v1/notifications/unsubscribe?token=" + tok + ">",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}, nil
}

func normalize(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
