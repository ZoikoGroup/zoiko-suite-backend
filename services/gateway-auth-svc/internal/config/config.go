// Package config loads gateway-auth-svc configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds this service's runtime settings. There is no database or
// message broker here — this service is a stateless JWT verifier called by
// Traefik's ForwardAuth middleware on every gated request.
type Config struct {
	// OTELExporterEndpoint is the OTLP/HTTP collector traces are shipped to.
	// Tracing failing to initialise is logged and the service starts anyway —
	// a collector outage must not take down the gateway every request depends
	// on.
	OTELExporterEndpoint string

	Port int

	// JWKSURL is identity-context-svc's public key endpoint. Envelope JWTs
	// are signed there with RS256; this service only ever holds the public
	// half, fetched over the network, never a private key.
	JWKSURL      string
	JWKSCacheTTL time.Duration

	// ExpectedIssuer/ExpectedAudience must match identity-context-svc's
	// JWTIssuer / JWTAudienceInternal so a token minted for a different
	// purpose can't be replayed here.
	ExpectedIssuer   string
	ExpectedAudience string

	// AU-01 revocation: each verified envelope's session is checked at
	// identity-context-svc's introspection endpoint. IdentityRevocation "off"
	// is refused unless ENV=local; RevocationTTL caps how long a live answer is
	// reused (≤ 60 s, the revocation SLO); RevocationFail is "closed" (503 when
	// the status is unknown) or "open".
	IdentityRevocation    string
	IdentityIntrospectURL string
	RevocationTTL         time.Duration
	RevocationFail        string

	// CartaServiceURL is carta-svc — continuous session-risk scoring
	// (Doc 05 §3.11/§5.4's "authentication is not a one-time event").
	// Empty disables the call: risk scoring degrades to "not evaluated",
	// never to a fabricated ALLOW.
	CartaServiceURL string

	// SIEMServiceURL is siem-integration-svc. Empty disables streaming:
	// security-event export is best-effort and tenant-opt-in (a tenant with
	// no configured exporter is normal, not an error), so its absence must
	// never affect whether a request is let through.
	SIEMServiceURL string

	// IdentityJWKSMTLSEnabled/IdentityJWKSMTLSURL wire the JWKS client into
	// the mTLS pilot for calling identity-context-svc. Disabled by default —
	// plain HTTP JWKSURL keeps being used unless explicitly turned on.
	IdentityJWKSMTLSEnabled bool
	IdentityJWKSMTLSURL     string

	// TenantRegistryMTLSEnabled/TenantRegistryMTLSURL wire the tenant context
	// resolver into the mTLS pilot for calling tenant-entity-registry-svc.
	// Disabled by default.
	TenantRegistryMTLSEnabled bool
	TenantRegistryMTLSURL     string

	// MTLSManagementServiceURL is the mtls-management-svc endpoint used to
	// provision this service's client identity for mTLS calls.
	MTLSManagementServiceURL string

	// MTLSBootstrapTokenPath is the shared provisioning token file
	// mtls-management-svc authenticates a self-provisioning service by.
	// Without it every provisioning call is refused.
	MTLSBootstrapTokenPath string

	// TenantRegistryURL is tenant-entity-registry-svc — the tenant and legal
	// entity master GOV-01 resolves context against. Empty disables resolution
	// entirely: the gateway then verifies the token and forwards, exactly as it
	// did before, rather than failing every request closed on a dependency the
	// deployment has not configured yet.
	//
	// Once set, resolution is enforced: an unknown or suspended tenant is
	// denied and an unreachable registry blocks material writes. That is the
	// intended end state — see internal/tenantctx.
	TenantRegistryURL string

	// TenantContextTTL bounds how long a resolved context is reused before the
	// registry is consulted again. Short enough that a suspension takes effect
	// promptly, long enough that the registry is not in the hot path of every
	// gated request.
	TenantContextTTL time.Duration

	// TenantContextStaleGrace bounds how far past TenantContextTTL an entry may
	// still answer a NON-MATERIAL READ while the registry is unreachable
	// (ZS-SVC-A-001 §4 GOV-01 degradation contract). Zero disables stale reads.
	// It never applies to writes.
	TenantContextStaleGrace time.Duration

	// ── Admission (ZS-SVC-M-001 INT-01) ──────────────────────────────────────
	// Every limit has a safe default; 0 disables that one protection.

	// MaxBodyBytes caps a declared request Content-Length.
	MaxBodyBytes int64
	// AllowedRequestEncodings are the Content-Encoding values a request body may
	// carry. Empty (the default) refuses every compressed request body.
	AllowedRequestEncodings []string
	// MaxCompressedBodyBytes caps the declared length of an allowed compressed
	// body. 0 uses MaxBodyBytes.
	MaxCompressedBodyBytes int64
	// SupportedAPIVersions are the versions ("v1") the gateway will admit.
	SupportedAPIVersions []string

	// Per-process token-bucket quotas. IP applies before authentication, to
	// protect token verification itself; Tenant applies after it. With N
	// replicas the effective ceiling is N times these; the edge proxy's own
	// rate limit is the cross-replica ceiling.
	RateIPPerSecond     float64
	RateIPBurst         int
	RateTenantPerSecond float64
	RateTenantBurst     int

	// MaxInFlight sheds /verify load above this many concurrent requests.
	MaxInFlight int

	// DecisionWindow is how many recent refusals are kept in memory.
	DecisionWindow int
	// AbuseThresholdPerMinute is how many refusals one client may collect in a
	// minute before api_abuse_threshold_exceeded is raised.
	AbuseThresholdPerMinute int

	// AdminAddr, with AdminTokenFile, enables the read-only decision query
	// listener (GetGatewayDecision, ListDeniedRequests, GetClientUsage,
	// GetRateLimitState, GetRouteVersionStatus). Off by default. The token is
	// read from a FILE, never from the environment, so no secret sits in
	// configuration. The listener is a second port of this same service.
	AdminAddr      string
	AdminTokenFile string
}

func Load() (*Config, error) {
	port, err := intEnv("PORT", 8092)
	if err != nil {
		return nil, err
	}
	ttlSeconds, err := intEnv("JWKS_CACHE_TTL_SECONDS", 300)
	if err != nil {
		return nil, err
	}
	ctxTTL, err := intEnv("TENANT_CONTEXT_TTL_SECONDS", 30)
	if err != nil {
		return nil, err
	}
	ctxGrace, err := intEnv("TENANT_CONTEXT_STALE_GRACE_SECONDS", 120)
	if err != nil {
		return nil, err
	}

	adm, err := loadAdmission()
	if err != nil {
		return nil, err
	}

cfg := &Config{
		Port:                      port,
		JWKSURL:                   strEnv("IDENTITY_JWKS_URL", "http://identity-svc:8080/.well-known/jwks.json"),
		JWKSCacheTTL:              time.Duration(ttlSeconds) * time.Second,
		ExpectedIssuer:            strEnv("EXPECTED_ISSUER", "identity-context-svc"),
		ExpectedAudience:          strEnv("EXPECTED_AUDIENCE", "zoiko-internal"),
		IdentityRevocation:        strings.ToLower(strEnv("IDENTITY_REVOCATION", "on")),
		IdentityIntrospectURL:     strEnv("IDENTITY_INTROSPECT_URL", "http://identity-svc:8080/v1/context/session/introspect"),
		RevocationFail:            strings.ToLower(strEnv("IDENTITY_REVOCATION_FAIL", "closed")),
		CartaServiceURL:           strEnv("CARTA_SERVICE_URL", ""),
		SIEMServiceURL:            strEnv("SIEM_SERVICE_URL", ""),
		IdentityJWKSMTLSEnabled:   strEnv("IDENTITY_JWKS_MTLS_ENABLED", "false") == "true",
		IdentityJWKSMTLSURL:       strEnv("IDENTITY_JWKS_MTLS_URL", "https://identity-svc:8449/.well-known/jwks.json"),
		TenantRegistryMTLSEnabled: strEnv("TENANT_REGISTRY_MTLS_ENABLED", "false") == "true",
		TenantRegistryMTLSURL:     strEnv("TENANT_REGISTRY_MTLS_URL", "https://tenant-entity-registry-svc:8449"),
		MTLSManagementServiceURL:  strEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		MTLSBootstrapTokenPath:    strEnv("MTLS_BOOTSTRAP_TOKEN_PATH", "/bootstrap/token"),
		TenantRegistryURL:         strEnv("TENANT_REGISTRY_URL", ""),
		OTELExporterEndpoint:      strEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		TenantContextTTL:          time.Duration(ctxTTL) * time.Second,
		TenantContextStaleGrace:   time.Duration(ctxGrace) * time.Second,
	}

	revTTL, err := intEnv("IDENTITY_REVOCATION_TTL_SECONDS", 5)
	if err != nil {
		return nil, err
	}
	if revTTL < 1 || revTTL > 60 {
		return nil, fmt.Errorf("IDENTITY_REVOCATION_TTL_SECONDS must be 1-60 (the revocation SLO), got %d", revTTL)
	}
	cfg.RevocationTTL = time.Duration(revTTL) * time.Second
	switch cfg.IdentityRevocation {
	case "on":
	case "off":
		if env := strings.ToLower(strEnv("ENV", "local")); env != "local" {
			return nil, fmt.Errorf("IDENTITY_REVOCATION=off is refused outside ENV=local: a logged-out session would keep passing the edge until its envelope expired (AU-01)")
		}
	default:
		return nil, fmt.Errorf("IDENTITY_REVOCATION must be on or off, got %q", cfg.IdentityRevocation)
	}
	if cfg.RevocationFail != "closed" && cfg.RevocationFail != "open" {
		return nil, fmt.Errorf("IDENTITY_REVOCATION_FAIL must be closed or open, got %q", cfg.RevocationFail)
	}
	adm.apply(cfg)

	// Enabling mTLS for a peer switches that peer's calls to its mTLS URL.
	// The 25 Sep wiring read the *_MTLS_URL values and never used them: the
	// mTLS client was handed the plain http:// URL, and Go sends plain HTTP to
	// an http:// URL whatever its TLS config says. So "enabled" changed
	// nothing on the wire. A non-https mTLS URL is refused at boot for the
	// same reason, instead of passing silently as plaintext.
	if cfg.IdentityJWKSMTLSEnabled {
		if !strings.HasPrefix(cfg.IdentityJWKSMTLSURL, "https://") {
			return nil, fmt.Errorf("IDENTITY_JWKS_MTLS_ENABLED=true requires an https:// IDENTITY_JWKS_MTLS_URL, got %q", cfg.IdentityJWKSMTLSURL)
		}
		cfg.JWKSURL = cfg.IdentityJWKSMTLSURL
	}
	if cfg.TenantRegistryMTLSEnabled {
		if !strings.HasPrefix(cfg.TenantRegistryMTLSURL, "https://") {
			return nil, fmt.Errorf("TENANT_REGISTRY_MTLS_ENABLED=true requires an https:// TENANT_REGISTRY_MTLS_URL, got %q", cfg.TenantRegistryMTLSURL)
		}
		cfg.TenantRegistryURL = cfg.TenantRegistryMTLSURL
	}
	return cfg, nil
}

func strEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return n, nil
}

type admission struct {
	maxBody, maxCompressed     int64
	encodings, versions        []string
	ipRPS, tenantRPS           float64
	ipBurst, tenantBurst       int
	maxInFlight, window, abuse int
	adminAddr, adminTokenFile  string
}

func (a admission) apply(c *Config) {
	c.MaxBodyBytes, c.MaxCompressedBodyBytes = a.maxBody, a.maxCompressed
	c.AllowedRequestEncodings, c.SupportedAPIVersions = a.encodings, a.versions
	c.RateIPPerSecond, c.RateIPBurst = a.ipRPS, a.ipBurst
	c.RateTenantPerSecond, c.RateTenantBurst = a.tenantRPS, a.tenantBurst
	c.MaxInFlight, c.DecisionWindow, c.AbuseThresholdPerMinute = a.maxInFlight, a.window, a.abuse
	c.AdminAddr, c.AdminTokenFile = a.adminAddr, a.adminTokenFile
}

// loadAdmission reads the INT-01 admission settings. A negative or malformed
// value is refused at boot: a limit that silently fell back to a default would
// leave the operator believing a protection was set that was not.
func loadAdmission() (admission, error) {
	var a admission
	var err error
	if a.maxBody, err = int64Env("GATEWAY_MAX_BODY_BYTES", 10<<20); err != nil {
		return a, err
	}
	if a.maxCompressed, err = int64Env("GATEWAY_MAX_COMPRESSED_BODY_BYTES", 1<<20); err != nil {
		return a, err
	}
	a.encodings = listEnv("GATEWAY_ALLOWED_REQUEST_ENCODINGS", nil)
	a.versions = listEnv("GATEWAY_SUPPORTED_API_VERSIONS", []string{"v1"})
	if a.ipRPS, err = floatEnv("GATEWAY_RATE_IP_PER_SECOND", 200); err != nil {
		return a, err
	}
	if a.ipBurst, err = intEnv("GATEWAY_RATE_IP_BURST", 400); err != nil {
		return a, err
	}
	if a.tenantRPS, err = floatEnv("GATEWAY_RATE_TENANT_PER_SECOND", 2000); err != nil {
		return a, err
	}
	if a.tenantBurst, err = intEnv("GATEWAY_RATE_TENANT_BURST", 4000); err != nil {
		return a, err
	}
	if a.maxInFlight, err = intEnv("GATEWAY_MAX_INFLIGHT", 2000); err != nil {
		return a, err
	}
	if a.window, err = intEnv("GATEWAY_DECISION_WINDOW", 2048); err != nil {
		return a, err
	}
	if a.abuse, err = intEnv("GATEWAY_ABUSE_THRESHOLD_PER_MINUTE", 100); err != nil {
		return a, err
	}
	a.adminAddr = strEnv("GATEWAY_ADMIN_ADDR", "")
	a.adminTokenFile = strEnv("GATEWAY_ADMIN_TOKEN_FILE", "")
	if (a.adminAddr == "") != (a.adminTokenFile == "") {
		return a, fmt.Errorf("GATEWAY_ADMIN_ADDR and GATEWAY_ADMIN_TOKEN_FILE must be set together: " +
			"the query listener is never served without authentication")
	}
	for name, v := range map[string]float64{
		"GATEWAY_MAX_BODY_BYTES": float64(a.maxBody), "GATEWAY_MAX_COMPRESSED_BODY_BYTES": float64(a.maxCompressed),
		"GATEWAY_RATE_IP_PER_SECOND": a.ipRPS, "GATEWAY_RATE_IP_BURST": float64(a.ipBurst),
		"GATEWAY_RATE_TENANT_PER_SECOND": a.tenantRPS, "GATEWAY_RATE_TENANT_BURST": float64(a.tenantBurst),
		"GATEWAY_MAX_INFLIGHT": float64(a.maxInFlight), "GATEWAY_DECISION_WINDOW": float64(a.window),
		"GATEWAY_ABUSE_THRESHOLD_PER_MINUTE": float64(a.abuse),
	} {
		if v < 0 {
			return a, fmt.Errorf("%s must not be negative (use 0 to disable)", name)
		}
	}
	if (a.ipRPS > 0 && a.ipBurst < 1) || (a.tenantRPS > 0 && a.tenantBurst < 1) {
		return a, fmt.Errorf("an enabled rate limit needs a burst of at least 1")
	}
	return a, nil
}

func int64Env(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return n, nil
}

func floatEnv(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return n, nil
}

// listEnv reads a comma-separated list. An unset variable yields def; a set
// but empty one yields an empty list, which is how an operator says "none".
func listEnv(key string, def []string) []string {
	v, set := os.LookupEnv(key)
	if !set {
		return def
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
