package federation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// supportedAlgs is every algorithm a connection may accept. Asymmetric only: a symmetric algorithm
// would mean the tenant's IdP shares a secret with this service, and "none" would mean no signature.
var supportedAlgs = map[string]bool{
	"RS256": true, "RS384": true, "RS512": true,
	"PS256": true, "PS384": true, "PS512": true,
	"ES256": true, "ES384": true,
}

var claimName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_:./-]{0,63}$`)

// Config is a connection version's trust configuration: who the issuer is, who the token must be for,
// where its keys come from, and what a login may and may not claim.
type Config struct {
	Issuer       string   `json:"issuer"`
	Audience     string   `json:"audience"`
	JWKSURI      string   `json:"jwks_uri"`
	AllowedAlgs  []string `json:"allowed_algs"`
	SubjectClaim string   `json:"subject_claim"`
	EmailClaim   string   `json:"email_claim"`
	MFAAMRValues []string `json:"mfa_amr_values"`
	MFAACRValues []string `json:"mfa_acr_values"`
	// NonceOptional lets a connection accept a token without a request-bound nonce. The default (false)
	// requires one: a configuration that says nothing about it must not be the weaker one.
	NonceOptional      bool   `json:"nonce_optional"`
	MaxTokenAgeSeconds int    `json:"max_token_age_seconds"`
	ClockSkewSeconds   int    `json:"clock_skew_seconds"`
	OutageBehavior     string `json:"outage_behavior"`
	KeyGraceSeconds    int    `json:"key_grace_seconds"`
	JITEnabled         bool   `json:"jit_enabled"`
}

// URLPolicy is the egress check applied to the JWKS location. *egress.Policy satisfies it.
type URLPolicy interface {
	ValidateSyntax(raw string) (string, error)
}

// Normalize applies defaults and canonical ordering, so equal configurations have equal fingerprints.
func (c Config) Normalize() Config {
	c.Issuer = strings.TrimSpace(c.Issuer)
	c.Audience = strings.TrimSpace(c.Audience)
	c.JWKSURI = strings.TrimSpace(c.JWKSURI)
	if len(c.AllowedAlgs) == 0 {
		c.AllowedAlgs = []string{"RS256"}
	}
	c.AllowedAlgs = sortedUnique(c.AllowedAlgs)
	if c.SubjectClaim == "" {
		c.SubjectClaim = "sub"
	}
	if c.EmailClaim == "" {
		c.EmailClaim = "email"
	}
	c.MFAAMRValues = sortedUnique(c.MFAAMRValues)
	c.MFAACRValues = sortedUnique(c.MFAACRValues)
	if c.MaxTokenAgeSeconds == 0 {
		c.MaxTokenAgeSeconds = 300
	}
	if c.ClockSkewSeconds == 0 {
		c.ClockSkewSeconds = 60
	}
	if c.OutageBehavior == "" {
		c.OutageBehavior = OutageFailClosed
	}
	return c
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Validate checks a normalized configuration. It never reaches the network; the JWKS location is
// checked syntactically against the egress policy (scheme, port, internal names, address literals) and
// resolved again, with the connection pinned, at every fetch.
func (c Config) Validate(policy URLPolicy) error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }

	u, err := url.Parse(c.Issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return bad("issuer must be an https URL without query, fragment or credentials")
	}
	if len(c.Issuer) > 512 {
		return bad("issuer is too long")
	}
	if c.Audience == "" || len(c.Audience) > 255 {
		return bad("audience (the client id the token must be issued to) is required")
	}
	if c.JWKSURI == "" {
		return bad("jwks_uri is required")
	}
	if policy != nil {
		if _, err := policy.ValidateSyntax(c.JWKSURI); err != nil {
			return bad("jwks_uri is not an allowed destination")
		}
	}
	if len(c.AllowedAlgs) == 0 || len(c.AllowedAlgs) > 4 {
		return bad("allowed_algs must list between one and four algorithms")
	}
	for _, a := range c.AllowedAlgs {
		if !supportedAlgs[a] {
			return bad("algorithm %q is not accepted: only asymmetric RS*, PS* and ES* are", a)
		}
	}
	for _, n := range []string{c.SubjectClaim, c.EmailClaim} {
		if !claimName.MatchString(n) {
			return bad("claim names must be simple identifiers")
		}
	}
	if len(c.MFAAMRValues) > 10 || len(c.MFAACRValues) > 10 {
		return bad("at most ten mfa values are allowed")
	}
	if c.MaxTokenAgeSeconds < 30 || c.MaxTokenAgeSeconds > 3600 {
		return bad("max_token_age_seconds must be between 30 and 3600")
	}
	if c.ClockSkewSeconds < 0 || c.ClockSkewSeconds > 120 {
		return bad("clock_skew_seconds must be between 0 and 120")
	}
	switch c.OutageBehavior {
	case OutageFailClosed:
		if c.KeyGraceSeconds != 0 {
			return bad("key_grace_seconds applies only to CACHED_KEYS")
		}
	case OutageCachedKeys:
		if c.KeyGraceSeconds < 1 || c.KeyGraceSeconds > 86400 {
			return bad("key_grace_seconds must be between 1 and 86400 for CACHED_KEYS")
		}
	default:
		return bad("outage_behavior must be FAIL_CLOSED or CACHED_KEYS")
	}
	return nil
}

// Fingerprint is the SHA-256 of the canonical form of the configuration. An approver approves exactly
// this value; a version whose fingerprint differs is a different trust decision.
func (c Config) Fingerprint() string {
	b, _ := json.Marshal(c.Normalize())
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
