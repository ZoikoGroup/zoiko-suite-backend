// Package oidc is identity-context-svc's OpenID Connect provider (AU-06,
// AU-07 entry point, AU-10, AU-12, AU-27; ZS-IAM-001 §17–18; RFC 9700).
//
// Before it, the only login was POST /v1/authenticate: a client sent a
// username and password (and, from 14 Oct, its own secret) and got a token
// back — the resource-owner password grant IAM §18 prohibits. There was no
// authorization server metadata, no authorization code, no PKCE, no refresh
// token and no logout that a relying party could see.
//
// What this package provides:
//
//   - discovery (/.well-known/openid-configuration and RFC 8414 metadata);
//   - the authorization code flow with PKCE S256 for EVERY client, exact
//     redirect-URI matching, state, nonce and the RFC 9207 iss parameter;
//   - a hosted login page (password, then TOTP or a passkey when the
//     principal has one) and optional single sign-on across clients;
//   - refresh tokens with rotation and reuse detection (a reused token
//     revokes its whole family and ends the login session);
//   - DPoP-bound tokens (RFC 9449) for public clients and certificate-bound
//     ones (RFC 8705) for workloads;
//   - token exchange (RFC 8693) to narrow an envelope's audience;
//   - client credentials for workloads only (never for a person);
//   - userinfo, revocation (RFC 7009), introspection (RFC 7662) and
//     RP-initiated logout;
//   - the session status endpoint every relying party calls (AU-01).
//
// POST /v1/authenticate remains for internal tooling only (config
// AUTHENTICATE_ENDPOINT); /v1/context/resolve is unchanged and consumes the
// access token this package issues.
package oidc

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Client types.
const (
	ClientPublic       = "public"
	ClientConfidential = "confidential"
	ClientWorkload     = "workload"
)

// Grant types.
const (
	GrantAuthorizationCode = "authorization_code"
	GrantRefreshToken      = "refresh_token"
	GrantClientCredentials = "client_credentials"
	GrantTokenExchange     = "urn:ietf:params:oauth:grant-type:token-exchange"
)

// Client is one registered OAuth client.
type Client struct {
	ClientID   string `json:"client_id"`
	ClientType string `json:"client_type"`
	// SecretSHA256 is the hex SHA-256 of the client secret (confidential and
	// secret-authenticated workload clients).
	SecretSHA256           string   `json:"secret_sha256,omitempty"`
	RedirectURIs           []string `json:"redirect_uris,omitempty"`
	PostLogoutRedirectURIs []string `json:"post_logout_redirect_uris,omitempty"`
	GrantTypes             []string `json:"grant_types"`
	Scopes                 []string `json:"scopes,omitempty"`
	// DPoPBound requires DPoP on every token request (public browser clients).
	DPoPBound bool `json:"dpop_bound,omitempty"`
	// TLSClientAuthSubject authenticates the client by its mTLS certificate
	// name (RFC 8705 tls_client_auth) and binds its tokens to the certificate.
	TLSClientAuthSubject string `json:"tls_client_auth_subject,omitempty"`
	// AllowedAudiences limit token exchange and client credentials; "*"
	// admits any registered service audience.
	AllowedAudiences []string `json:"allowed_audiences,omitempty"`
	// SourceChannel is the §4 channel a login through this client resolves to.
	SourceChannel string `json:"source_channel,omitempty"`
	// TenantID scopes a workload client's own envelopes (client credentials).
	TenantID string `json:"tenant_id,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`

	digest []byte
}

// Allows reports whether the client may use grant.
func (c *Client) Allows(grant string) bool {
	for _, g := range c.GrantTypes {
		if g == grant {
			return true
		}
	}
	return false
}

// RedirectAllowed is exact matching (RFC 9700 §4.1.3), with RFC 8252 §7.3's
// loopback exception: any port on 127.0.0.1 / [::1] for a registered
// loopback URI.
func (c *Client) RedirectAllowed(uri string) bool {
	for _, r := range c.RedirectURIs {
		if r == uri {
			return true
		}
		if loopbackMatch(r, uri) {
			return true
		}
	}
	return false
}

// PostLogoutAllowed is exact matching on post-logout redirects.
func (c *Client) PostLogoutAllowed(uri string) bool {
	for _, r := range c.PostLogoutRedirectURIs {
		if r == uri {
			return true
		}
	}
	return false
}

// AudienceAllowed reports whether the client may obtain an envelope for aud.
func (c *Client) AudienceAllowed(aud string) bool {
	for _, a := range c.AllowedAudiences {
		if a == "*" || a == aud {
			return true
		}
	}
	return false
}

// CheckSecret compares in constant time.
func (c *Client) CheckSecret(secret string) bool {
	if len(c.digest) == 0 || secret == "" {
		return false
	}
	got := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(c.digest, got[:]) == 1
}

func loopbackMatch(registered, presented string) bool {
	for _, host := range []string{"http://127.0.0.1:", "http://[::1]:"} {
		if strings.HasPrefix(registered, host) && strings.HasPrefix(presented, host) {
			rp := registered[strings.Index(registered[len(host):], "/")+len(host):]
			pp := presented[len(host):]
			slash := strings.Index(pp, "/")
			if slash < 0 {
				return false
			}
			port := pp[:slash]
			for _, ch := range port {
				if ch < '0' || ch > '9' {
					return false
				}
			}
			return pp[slash:] == rp
		}
	}
	return false
}

// Registry holds the registered clients.
type Registry struct {
	clients map[string]*Client
}

// LoadRegistry reads a JSON array of clients. An empty path yields an empty
// registry (no client can use the authorization server).
func LoadRegistry(path string) (*Registry, error) {
	r := &Registry{clients: map[string]*Client{}}
	if path == "" {
		return r, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read OIDC_CLIENTS_FILE: %w", err)
	}
	var list []Client
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse OIDC_CLIENTS_FILE: %w", err)
	}
	for i := range list {
		if err := r.Add(list[i]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// NewRegistry builds a registry from clients (tests).
func NewRegistry(clients ...Client) (*Registry, error) {
	r := &Registry{clients: map[string]*Client{}}
	for _, c := range clients {
		if err := r.Add(c); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Add validates and registers c.
func (r *Registry) Add(c Client) error {
	c.ClientID = strings.TrimSpace(c.ClientID)
	if c.ClientID == "" {
		return errors.New("client with empty client_id")
	}
	if _, dup := r.clients[c.ClientID]; dup {
		return fmt.Errorf("client %q registered twice", c.ClientID)
	}
	switch c.ClientType {
	case ClientPublic:
		if c.SecretSHA256 != "" {
			return fmt.Errorf("client %q: a public client has no secret", c.ClientID)
		}
		if c.Allows(GrantClientCredentials) || c.Allows(GrantTokenExchange) {
			return fmt.Errorf("client %q: a public client cannot use client credentials or token exchange", c.ClientID)
		}
	case ClientConfidential:
		if c.SecretSHA256 == "" && c.TLSClientAuthSubject == "" {
			return fmt.Errorf("client %q: a confidential client needs a secret or tls_client_auth_subject", c.ClientID)
		}
		if c.Allows(GrantClientCredentials) {
			return fmt.Errorf("client %q: client credentials are reserved for workload clients (IAM §18)", c.ClientID)
		}
	case ClientWorkload:
		if c.SecretSHA256 == "" && c.TLSClientAuthSubject == "" {
			return fmt.Errorf("client %q: a workload client needs a secret or tls_client_auth_subject", c.ClientID)
		}
		if c.Allows(GrantAuthorizationCode) || c.Allows(GrantRefreshToken) {
			return fmt.Errorf("client %q: a workload client never logs a person in", c.ClientID)
		}
	default:
		return fmt.Errorf("client %q: client_type must be public, confidential or workload", c.ClientID)
	}
	if c.Allows(GrantAuthorizationCode) && len(c.RedirectURIs) == 0 {
		return fmt.Errorf("client %q: authorization_code needs redirect_uris", c.ClientID)
	}
	for _, u := range c.RedirectURIs {
		if strings.ContainsAny(u, "*#") {
			return fmt.Errorf("client %q: redirect URI %q has a wildcard or fragment", c.ClientID, u)
		}
	}
	if c.SecretSHA256 != "" {
		d, err := hex.DecodeString(strings.TrimPrefix(c.SecretSHA256, "sha256:"))
		if err != nil || len(d) != sha256.Size {
			return fmt.Errorf("client %q: secret_sha256 must be a hex SHA-256 digest", c.ClientID)
		}
		c.digest = d
	}
	cc := c
	r.clients[c.ClientID] = &cc
	return nil
}

// Get returns an enabled client.
func (r *Registry) Get(id string) (*Client, bool) {
	c, ok := r.clients[strings.TrimSpace(id)]
	if !ok || c.Disabled {
		return nil, false
	}
	return c, true
}

// Count is the number of registered clients.
func (r *Registry) Count() int { return len(r.clients) }

// Channels maps client id to source channel (the resolver's IDP_CLIENT_CHANNELS).
func (r *Registry) Channels() map[string]string {
	out := map[string]string{}
	for id, c := range r.clients {
		if c.SourceChannel != "" {
			out[id] = c.SourceChannel
		}
	}
	return out
}
