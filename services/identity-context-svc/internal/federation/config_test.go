package federation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"zoiko.io/eventing/egress"
)

func TestConfigValidation(t *testing.T) {
	policy := egress.Policy{}
	if err := testConfig().Validate(policy); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Config){
		"http issuer":          func(c *Config) { c.Issuer = "http://idp.example.com" },
		"issuer with query":    func(c *Config) { c.Issuer = "https://idp.example.com?x=1" },
		"issuer with fragment": func(c *Config) { c.Issuer = "https://idp.example.com#x" },
		"issuer credentials":   func(c *Config) { c.Issuer = "https://user:pw@idp.example.com" },
		"no issuer":            func(c *Config) { c.Issuer = "" },
		"no audience":          func(c *Config) { c.Audience = "" },
		"no jwks":              func(c *Config) { c.JWKSURI = "" },
		"http jwks":            func(c *Config) { c.JWKSURI = "http://idp.example.com/jwks" },
		"metadata address":     func(c *Config) { c.JWKSURI = "https://169.254.169.254/keys" },
		"loopback jwks":        func(c *Config) { c.JWKSURI = "https://127.0.0.1/keys" },
		"internal name":        func(c *Config) { c.JWKSURI = "https://keys.internal/jwks" },
		"HS256":                func(c *Config) { c.AllowedAlgs = []string{"HS256"} },
		"none":                 func(c *Config) { c.AllowedAlgs = []string{"none"} },
		"mixed with HS":        func(c *Config) { c.AllowedAlgs = []string{"RS256", "HS512"} },
		"bad claim name":       func(c *Config) { c.SubjectClaim = "sub; drop" },
		"too many mfa values":  func(c *Config) { c.MFAAMRValues = strings.Split("a,b,c,d,e,f,g,h,i,j,k", ",") },
		"age too small":        func(c *Config) { c.MaxTokenAgeSeconds = 5 },
		"age too large":        func(c *Config) { c.MaxTokenAgeSeconds = 100000 },
		"skew too large":       func(c *Config) { c.ClockSkewSeconds = 600 },
		"unknown outage":       func(c *Config) { c.OutageBehavior = "SKIP_VERIFICATION" },
		"grace without cache":  func(c *Config) { c.KeyGraceSeconds = 60 },
		"cache without grace":  func(c *Config) { c.OutageBehavior = OutageCachedKeys; c.KeyGraceSeconds = 0 },
		"grace too long":       func(c *Config) { c.OutageBehavior = OutageCachedKeys; c.KeyGraceSeconds = 999999 },
	}
	for name, mut := range cases {
		c := testConfig()
		mut(&c)
		if err := c.Normalize().Validate(policy); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestFingerprintIsCanonical(t *testing.T) {
	a := testConfig()
	b := testConfig()
	b.AllowedAlgs = []string{"ES256", "RS256", "ES256"} // order and duplicates do not matter
	b.MFAAMRValues = []string{}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("equal trust decisions have equal fingerprints")
	}
	for name, mut := range map[string]func(*Config){
		"issuer":   func(c *Config) { c.Issuer = "https://other.example.com" },
		"audience": func(c *Config) { c.Audience = "x" },
		"jwks":     func(c *Config) { c.JWKSURI = "https://idp.example.com/other" },
		"algs":     func(c *Config) { c.AllowedAlgs = []string{"RS256"} },
		"mfa":      func(c *Config) { c.MFAAMRValues = []string{"otp"} },
		"nonce":    func(c *Config) { c.NonceOptional = true },
		"jit":      func(c *Config) { c.JITEnabled = true },
		"outage":   func(c *Config) { c.OutageBehavior = OutageCachedKeys; c.KeyGraceSeconds = 60 },
	} {
		c := testConfig()
		mut(&c)
		if c.Fingerprint() == a.Fingerprint() {
			t.Errorf("a change of %s must change the fingerprint", name)
		}
	}
}

func TestAnUnstatedNonceSettingIsTheStrictOne(t *testing.T) {
	// a configuration that never mentions the nonce must require it: the zero value is the safe one
	var c Config
	raw := `{"issuer":"` + tIssuer + `","audience":"` + tAudience + `","jwks_uri":"https://idp.example.com/j","allowed_algs":["RS256"]}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.Normalize().NonceOptional {
		t.Fatal("omitting the setting must not weaken the connection")
	}
	idp := newTestIdP(t)
	v, _, _ := newVerifier(t, idp)
	cl := idp.claims()
	cl["nonce"] = "something"
	if _, err := v.Verify(context.Background(), testConnection(), c.Normalize(), idp.mint(t, cl), "different"); ReasonOf(err) != "nonce_mismatch" {
		t.Fatalf("a mismatched nonce is refused by default: %v", err)
	}
	if _, err := v.Verify(context.Background(), testConnection(), c.Normalize(), idp.mint(t, cl), ""); ReasonOf(err) != "nonce_missing" {
		t.Fatalf("a missing nonce is refused by default: %v", err)
	}
}
