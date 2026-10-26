package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// AuthPlaneConfig holds the settings added by the 15 Oct 2026 auth closure
// (docs/audit_files/Auth-Service-audit-2026-10-14.md, AU-01 to AU-27).
type AuthPlaneConfig struct {
	// ── Signing keyring (AU-05) ──────────────────────────────────────────────

	// SigningKeys is JWT_SIGNING_KEYS: "kid=source,kid=source", where source
	// is a PEM path or gcpkms://projects/…/cryptoKeyVersions/N. Empty falls
	// back to JWT_KEY_ID=JWT_SIGNING_PRIVATE_KEY_PATH.
	SigningKeys string
	// SigningActiveKid picks the signer (default: the first key).
	SigningActiveKid string
	// VerifyOnlyKeys are retired keys still published: "kid=public.pem,…".
	VerifyOnlyKeys string

	// ── OIDC provider (AU-06, AU-10, AU-12) ──────────────────────────────────

	// IssuerURL is the public base URL of this service as clients reach it;
	// discovery, ID tokens and redirects use it.
	IssuerURL string
	// ClientsFile registers OAuth clients (JSON; see internal/oidc).
	ClientsFile     string
	CodeTTL         time.Duration
	IDTokenTTL      time.Duration
	RefreshTTL      time.Duration // absolute lifetime of a refresh family
	RefreshIdleTTL  time.Duration // a refresh token unused this long expires
	LoginSessionMax time.Duration // an auth session (sid) ends after this
	// PasswordEndpoint governs POST /v1/authenticate, the password-for-token
	// exchange IAM §18 prohibits as a client grant: "internal" (only mTLS
	// peers and local tooling), "open" (local only), "disabled".
	PasswordEndpoint string
	// LoginMFA is "enrolled" (a principal with a factor must use it at
	// login) or "required" (no password-only login at all).
	LoginMFA string
	// ExchangeAudiences are the audiences token exchange may mint; empty
	// admits any "<name>-svc".
	ExchangeAudiences []string
	// ExchangeActors are the workloads (client-certificate names) that may
	// exchange an envelope; empty admits MTLSAllowedClients.
	ExchangeActors       []string
	ExchangeRequireActor bool
	ExchangeTTL          time.Duration

	// ── Login throttling (AU-04) ─────────────────────────────────────────────

	// TrustedProxyCIDRs are the proxies whose X-Forwarded-For is believed.
	// Empty believes none: the client is the TCP peer.
	TrustedProxyCIDRs  []*net.IPNet
	AuthRatePerIPMin   int
	AuthFailIPLimit    int
	AuthFailTenantMax  int
	AuthFailWindow     time.Duration
	ResetRequestPerIPH int

	// ── Subject lifecycle (AU-09) ────────────────────────────────────────────

	PublicAppURL        string
	InviteTTL           time.Duration
	ResetTTL            time.Duration
	PasswordMinLength   int
	PasswordHistory     int
	BreachCheck         string // local | hibp | off
	BreachListFile      string
	HIBPURL             string
	NotificationURL     string
	ResetApprovals      int
	ActivationLinkPath  string
	PasswordResetPath   string
	DeprovisionOnDelete bool

	// ── Federation (AU-07) ───────────────────────────────────────────────────

	FederationSecretsDir string
	SAMLSPEntityID       string

	// ── WebAuthn (AU-11) ─────────────────────────────────────────────────────

	WebAuthnRPID    string
	WebAuthnOrigins []string
	WebAuthnRPName  string

	// ── SCIM (AU-08) ─────────────────────────────────────────────────────────

	SCIMEnabled bool
}

func loadAuthPlane(cfg *Config) error {
	prod := cfg.Environment == "production" || cfg.Environment == "staging"
	a := AuthPlaneConfig{
		SigningKeys:      env("JWT_SIGNING_KEYS", ""),
		SigningActiveKid: env("JWT_SIGNING_ACTIVE_KID", ""),
		VerifyOnlyKeys:   env("JWT_VERIFY_ONLY_KEYS", ""),

		IssuerURL:            strings.TrimRight(env("OIDC_ISSUER", fmt.Sprintf("http://localhost:%d", cfg.Port)), "/"),
		ClientsFile:          env("OIDC_CLIENTS_FILE", ""),
		CodeTTL:              time.Duration(envInt("OIDC_CODE_TTL_SECONDS", 60)) * time.Second,
		IDTokenTTL:           time.Duration(envInt("OIDC_ID_TOKEN_TTL_SECONDS", 300)) * time.Second,
		RefreshTTL:           time.Duration(envInt("OIDC_REFRESH_TTL_SECONDS", 8*3600)) * time.Second,
		RefreshIdleTTL:       time.Duration(envInt("OIDC_REFRESH_IDLE_SECONDS", 3600)) * time.Second,
		LoginSessionMax:      time.Duration(envInt("OIDC_LOGIN_SESSION_MAX_SECONDS", 12*3600)) * time.Second,
		PasswordEndpoint:     strings.ToLower(env("AUTHENTICATE_ENDPOINT", "internal")),
		LoginMFA:             strings.ToLower(env("LOGIN_MFA", "enrolled")),
		ExchangeAudiences:    envList("OIDC_EXCHANGE_AUDIENCES"),
		ExchangeActors:       envList("OIDC_EXCHANGE_ACTORS"),
		ExchangeRequireActor: envBool("OIDC_EXCHANGE_REQUIRE_ACTOR", prod),
		ExchangeTTL:          time.Duration(envInt("OIDC_EXCHANGE_TTL_SECONDS", 300)) * time.Second,

		AuthRatePerIPMin:   envInt("AUTH_RATE_IP_PER_MINUTE", 30),
		AuthFailIPLimit:    envInt("AUTH_FAIL_IP_LIMIT", 10),
		AuthFailTenantMax:  envInt("AUTH_FAIL_TENANT_LIMIT", 100),
		AuthFailWindow:     time.Duration(envInt("AUTH_FAIL_WINDOW_SECONDS", 600)) * time.Second,
		ResetRequestPerIPH: envInt("PASSWORD_RESET_REQUESTS_PER_IP_HOUR", 10),

		InviteTTL:           time.Duration(envInt("INVITE_TTL_HOURS", 72)) * time.Hour,
		ResetTTL:            time.Duration(envInt("PASSWORD_RESET_TTL_MINUTES", 30)) * time.Minute,
		PasswordMinLength:   envInt("PASSWORD_MIN_LENGTH", 12),
		PasswordHistory:     envInt("PASSWORD_HISTORY", 5),
		BreachCheck:         strings.ToLower(env("PASSWORD_BREACH_CHECK", "local")),
		BreachListFile:      env("PASSWORD_BREACH_LIST_FILE", ""),
		HIBPURL:             env("PASSWORD_BREACH_HIBP_URL", "https://api.pwnedpasswords.com/range/"),
		NotificationURL:     env("NOTIFICATION_SERVICE_URL", ""),
		ResetApprovals:      envInt("PASSWORD_RESET_APPROVALS", 1),
		ActivationLinkPath:  env("ACTIVATION_LINK_PATH", "/activate"),
		PasswordResetPath:   env("PASSWORD_RESET_LINK_PATH", "/reset-password"),
		DeprovisionOnDelete: envBool("SCIM_DELETE_DISABLES", true),

		FederationSecretsDir: env("FEDERATION_SECRETS_DIR", ""),

		WebAuthnRPName: env("WEBAUTHN_RP_NAME", "ZoikoSuite"),
		SCIMEnabled:    envBool("SCIM_ENABLED", true),
	}
	a.PublicAppURL = strings.TrimRight(env("PUBLIC_APP_URL", a.IssuerURL), "/")
	a.SAMLSPEntityID = env("SAML_SP_ENTITY_ID", a.IssuerURL+"/saml/metadata")

	issuer, err := url.Parse(a.IssuerURL)
	if err != nil || issuer.Scheme == "" || issuer.Host == "" {
		return fmt.Errorf("OIDC_ISSUER %q is not an absolute URL", a.IssuerURL)
	}
	if prod && issuer.Scheme != "https" {
		return fmt.Errorf("OIDC_ISSUER must be https in %s (RFC 8414 §2)", cfg.Environment)
	}
	a.WebAuthnRPID = env("WEBAUTHN_RP_ID", issuer.Hostname())
	a.WebAuthnOrigins = envList("WEBAUTHN_RP_ORIGINS")
	if len(a.WebAuthnOrigins) == 0 {
		a.WebAuthnOrigins = []string{issuer.Scheme + "://" + issuer.Host}
	}

	for _, c := range envList("TRUSTED_PROXY_CIDRS") {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("TRUSTED_PROXY_CIDRS: %q is not a CIDR", c)
		}
		a.TrustedProxyCIDRs = append(a.TrustedProxyCIDRs, n)
	}

	switch a.PasswordEndpoint {
	case "internal", "disabled":
	case "open":
		if prod {
			return fmt.Errorf("AUTHENTICATE_ENDPOINT=open is refused in %s: a password-for-token grant is prohibited (IAM §18); clients use the authorization code flow", cfg.Environment)
		}
	default:
		return fmt.Errorf("AUTHENTICATE_ENDPOINT %q is invalid: expected internal, open or disabled", a.PasswordEndpoint)
	}
	switch a.LoginMFA {
	case "enrolled", "required":
	default:
		return fmt.Errorf("LOGIN_MFA %q is invalid: expected enrolled or required", a.LoginMFA)
	}
	switch a.BreachCheck {
	case "local", "hibp", "off":
	default:
		return fmt.Errorf("PASSWORD_BREACH_CHECK %q is invalid: expected local, hibp or off", a.BreachCheck)
	}
	if prod && a.BreachCheck == "off" {
		return errors.New("PASSWORD_BREACH_CHECK=off is refused outside local/development (ZS-SEC-001 §10.1)")
	}
	if a.PasswordMinLength < 8 {
		return errors.New("PASSWORD_MIN_LENGTH must be at least 8 (NIST SP 800-63B-4 §3.1.1.2)")
	}
	if a.ResetApprovals < 1 {
		return errors.New("PASSWORD_RESET_APPROVALS must be at least 1: an administrator-initiated reset is independently approved (IAM §3)")
	}
	if a.AuthRatePerIPMin < 1 || a.AuthFailIPLimit < 1 || a.AuthFailTenantMax < 1 || a.AuthFailWindow < time.Minute {
		return errors.New("login throttling limits must be positive and the window at least 60 s (AU-04)")
	}
	if a.CodeTTL <= 0 || a.CodeTTL > 10*time.Minute {
		return errors.New("OIDC_CODE_TTL_SECONDS must be between 1 and 600")
	}
	if a.RefreshTTL <= 0 || a.RefreshIdleTTL <= 0 || a.LoginSessionMax <= 0 {
		return errors.New("refresh and login-session lifetimes must be positive")
	}
	if a.ExchangeTTL <= 0 || a.ExchangeTTL > time.Duration(cfg.EnvelopeJWTTTLSeconds)*time.Second*2 {
		return errors.New("OIDC_EXCHANGE_TTL_SECONDS must be positive and at most twice ENVELOPE_JWT_TTL_SECONDS")
	}
	if len(a.ExchangeActors) == 0 {
		a.ExchangeActors = append([]string{"gateway-auth-svc"}, cfg.MTLSAllowedClients...)
	}
	cfg.AuthPlane = a
	return nil
}
