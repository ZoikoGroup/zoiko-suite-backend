package idenv

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// EnvOptions are the parts of a gate that environment variables cannot carry.
type EnvOptions struct {
	// HTTPClient fetches keys and session status (an mTLS client in
	// production). nil uses plain clients.
	HTTPClient *http.Client
	Observer   Observer
	Exempt     func(r *http.Request) bool
	PeerName   func(r *http.Request) string
	// DefaultPeers apply when IDENTITY_PEERS is unset.
	DefaultPeers []string
}

// Settings is what NewGateFromEnv read, for the startup log.
type Settings struct {
	Mode               Mode
	JWKSURL            string
	Issuer             string
	Audiences          []string
	IntrospectURL      string
	RevocationTTL      time.Duration
	RevocationFail     FailMode
	RevocationDisabled bool
	EnforceBinding     bool
	Peers              []string
	TrustAllPeers      bool
	TokenURL           string
}

// Fields renders s for a zap log line.
func (s Settings) Fields() []zap.Field {
	return []zap.Field{
		zap.String("mode", string(s.Mode)), zap.String("jwks_url", s.JWKSURL), zap.String("issuer", s.Issuer),
		zap.Strings("audiences", s.Audiences), zap.String("introspect_url", s.IntrospectURL),
		zap.Duration("revocation_ttl", s.RevocationTTL), zap.String("revocation_fail", string(s.RevocationFail)),
		zap.Bool("revocation_disabled", s.RevocationDisabled), zap.Bool("enforce_sender_constraint", s.EnforceBinding),
		zap.Strings("peers", s.Peers), zap.Bool("trust_all_mtls_peers", s.TrustAllPeers),
	}
}

// LocalEnvironment reports whether ENVIRONMENT (or ENV / APP_ENV) names a
// developer tier, where the defaults relax to warn.
func LocalEnvironment() bool {
	for _, k := range []string{"ENVIRONMENT", "ENV", "APP_ENV"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
		case "local", "dev", "development", "test":
			return true
		case "":
			continue
		default:
			return false
		}
	}
	return true
}

// NewGateFromEnv builds the gate for service from the standard variables:
//
//	IDENTITY_VERIFY                  require | warn | off (default require; warn in local/dev)
//	IDENTITY_JWKS_URL                identity-context's JWKS
//	IDENTITY_ISSUER                  envelope iss (identity-context-svc)
//	IDENTITY_AUDIENCE                this service's audience (default: service)
//	IDENTITY_EXTRA_AUDIENCES         further accepted audiences (CSV)
//	IDENTITY_ACCEPT_LEGACY_AUDIENCE  accept "zoiko-internal" (default true; AU-12 cutover sets false)
//	IDENTITY_INTROSPECT_URL          session status endpoint (AU-01)
//	IDENTITY_REVOCATION              on | off (off refused outside local/dev)
//	IDENTITY_REVOCATION_TTL_SECONDS  live-answer cache, the lag ceiling (default 5)
//	IDENTITY_REVOCATION_FAIL         closed | open (default closed)
//	IDENTITY_SENDER_CONSTRAINT       enforce | off (default enforce; AU-27)
//	IDENTITY_PEERS                   workloads that may vouch for X-Principal-Id over mTLS (CSV)
//	IDENTITY_TRUST_MTLS_PEERS        any verified client certificate is a peer (default false)
func NewGateFromEnv(service string, log *zap.Logger, o EnvOptions) (*Gate, Settings, error) {
	local := LocalEnvironment()
	s := Settings{
		Mode:           ParseMode(envOr("IDENTITY_VERIFY", map[bool]string{true: "warn", false: "require"}[local])),
		JWKSURL:        envOr("IDENTITY_JWKS_URL", "http://identity-context-svc:8080/.well-known/jwks.json"),
		Issuer:         envOr("IDENTITY_ISSUER", "identity-context-svc"),
		IntrospectURL:  envOr("IDENTITY_INTROSPECT_URL", "http://identity-context-svc:8080/v1/context/session/introspect"),
		RevocationTTL:  time.Duration(envInt("IDENTITY_REVOCATION_TTL_SECONDS", 5)) * time.Second,
		RevocationFail: FailMode(strings.ToLower(envOr("IDENTITY_REVOCATION_FAIL", string(FailClosed)))),
		EnforceBinding: !strings.EqualFold(envOr("IDENTITY_SENDER_CONSTRAINT", "enforce"), "off"),
		Peers:          csv(os.Getenv("IDENTITY_PEERS")),
		TrustAllPeers:  envBool("IDENTITY_TRUST_MTLS_PEERS", false),
		TokenURL:       envOr("IDENTITY_TOKEN_URL", "http://identity-context-svc:8080/oauth2/token"),
	}
	if len(s.Peers) == 0 {
		s.Peers = o.DefaultPeers
	}
	if s.RevocationFail != FailOpen {
		s.RevocationFail = FailClosed
	}
	if strings.EqualFold(os.Getenv("IDENTITY_REVOCATION"), "off") {
		if !local {
			return nil, s, fmt.Errorf("IDENTITY_REVOCATION=off is refused outside local/dev: a logged-out session would keep working until it expires (AU-01)")
		}
		s.RevocationDisabled = true
	}
	if s.Mode == ModeOff && !local {
		return nil, s, fmt.Errorf("IDENTITY_VERIFY=off is refused outside local/dev")
	}
	if s.RevocationTTL > time.Minute {
		return nil, s, fmt.Errorf("IDENTITY_REVOCATION_TTL_SECONDS above 60 exceeds the revocation SLO")
	}

	gate := GateOptions{Service: service, Mode: s.Mode, EnforceBinding: s.EnforceBinding, Peers: s.Peers,
		TrustAllPeers: s.TrustAllPeers, Exempt: o.Exempt, PeerName: o.PeerName, Log: log, Observer: o.Observer}
	if s.Mode != ModeOff {
		v, err := NewVerifier(VerifierOptions{
			Keys:                 NewJWKS(s.JWKSURL, time.Duration(envInt("IDENTITY_JWKS_TTL_SECONDS", 300))*time.Second, o.HTTPClient),
			Issuer:               s.Issuer,
			Audience:             envOr("IDENTITY_AUDIENCE", service),
			ExtraAudiences:       csv(os.Getenv("IDENTITY_EXTRA_AUDIENCES")),
			AcceptLegacyAudience: envBool("IDENTITY_ACCEPT_LEGACY_AUDIENCE", true),
		})
		if err != nil {
			return nil, s, err
		}
		s.Audiences = v.Audiences()
		gate.Verifier = v
		if !s.RevocationDisabled {
			gate.Revocation = NewRevocation(RevocationOptions{Source: NewIntrospector(s.IntrospectURL, o.HTTPClient),
				ActiveTTL: s.RevocationTTL, Fail: s.RevocationFail, Observer: o.Observer})
		}
	}
	g, err := NewGate(gate)
	return g, s, err
}

// NewExchangerFromEnv builds the audience exchanger from IDENTITY_TOKEN_URL,
// IDENTITY_CLIENT_ID and IDENTITY_CLIENT_SECRET (or _FILE).
func NewExchangerFromEnv(client *http.Client) *Exchanger {
	secret := os.Getenv("IDENTITY_CLIENT_SECRET")
	if f := os.Getenv("IDENTITY_CLIENT_SECRET_FILE"); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			secret = strings.TrimSpace(string(b))
		}
	}
	return NewExchanger(envOr("IDENTITY_TOKEN_URL", "http://identity-context-svc:8080/oauth2/token"),
		os.Getenv("IDENTITY_CLIENT_ID"), secret, client)
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(k))); err == nil {
		return v
	}
	return def
}

func csv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
