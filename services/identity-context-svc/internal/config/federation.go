package config

import (
	"os"
	"strconv"
	"strings"
)

// FederationConfig is INT-10's deployment configuration. Federation is off unless FEDERATION_ENABLED: a
// tenant IdP can only authenticate people on a platform whose operator has turned it on.
type FederationConfig struct {
	Enabled bool

	// Egress rules for fetching a tenant IdP's signing keys. The tenant names the location, so it passes the
	// same SSRF guard every other tenant-configured destination does: https only, public addresses only,
	// pinned dial, no redirects. AllowHTTP is for local development.
	AllowHTTP    bool
	AllowedPorts []int
	PrivateCIDRs []string

	// JWKSRefreshSeconds is how long a fetched key set is trusted before it is fetched again.
	JWKSRefreshSeconds int
	// JWKSMinRefreshSeconds bounds how often an unknown key id may trigger a fetch.
	JWKSMinRefreshSeconds int
}

// LoadFederation reads the federation settings from the environment.
func LoadFederation() FederationConfig {
	return FederationConfig{
		Enabled:               envBool("FEDERATION_ENABLED", false),
		AllowHTTP:             envBool("FEDERATION_EGRESS_ALLOW_HTTP", false),
		AllowedPorts:          envPorts("FEDERATION_EGRESS_PORTS"),
		PrivateCIDRs:          envList("FEDERATION_EGRESS_PRIVATE_CIDRS"),
		JWKSRefreshSeconds:    envInt("FEDERATION_JWKS_REFRESH_SECONDS", 900),
		JWKSMinRefreshSeconds: envInt("FEDERATION_JWKS_MIN_REFRESH_SECONDS", 30),
	}
}

func envPorts(key string) []int {
	var out []int
	for _, p := range strings.Split(os.Getenv(key), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 && n < 65536 {
			out = append(out, n)
		}
	}
	return out
}

// ProvisioningConfig is the directory-provisioning (SCIM) side of INT-10. It is off unless FEDERATION_PROVISIONING_ENABLED
// and requires federation itself to be enabled.
type ProvisioningConfig struct {
	Enabled bool
	// WorkerIntervalSeconds is how often the worker drives queued assignment work, ends sessions that are still live
	// and escalates deprovisioning that has missed its objective. Zero disables the worker (not recommended).
	WorkerIntervalSeconds int
	// ServicePrincipalID is the identity this service acts as when it asks access-control-svc for baseline access. It
	// must be granted ROLE_MANAGE on the entities its mappings name, by an operator.
	ServicePrincipalID string
	// RetryBaseSeconds is the first retry delay for work access control did not answer; it doubles up to 15 minutes for
	// grants and to RevokeRetryMaxSeconds for revocations (taking access away is the priority path).
	RetryBaseSeconds      int
	RevokeRetryMaxSeconds int
}

// LoadProvisioning reads the provisioning settings from the environment.
func LoadProvisioning() ProvisioningConfig {
	return ProvisioningConfig{
		Enabled:               envBool("FEDERATION_PROVISIONING_ENABLED", false),
		WorkerIntervalSeconds: envInt("FEDERATION_WORKER_INTERVAL_SECONDS", 15),
		ServicePrincipalID:    env("IDENTITY_SERVICE_PRINCIPAL_ID", "svc-identity-context"),
		RetryBaseSeconds:      envInt("FEDERATION_RETRY_BASE_SECONDS", 10),
		RevokeRetryMaxSeconds: envInt("FEDERATION_REVOKE_RETRY_MAX_SECONDS", 60),
	}
}
