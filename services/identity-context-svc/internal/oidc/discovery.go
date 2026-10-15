package oidc

import (
	"encoding/json"
	"net/http"
)

// Discovery serves OpenID Provider metadata (OIDC Discovery §3) and RFC 8414
// authorization server metadata (IAM §18 "publish standards-based metadata").
func (p *Provider) Discovery(w http.ResponseWriter, r *http.Request) {
	iss := p.issuer
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSONCacheable(w, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                iss + "/oauth2/authorize",
		"token_endpoint":                        iss + "/oauth2/token",
		"userinfo_endpoint":                     iss + "/oauth2/userinfo",
		"jwks_uri":                              iss + "/.well-known/jwks.json",
		"revocation_endpoint":                   iss + "/oauth2/revoke",
		"introspection_endpoint":                iss + "/oauth2/introspect",
		"end_session_endpoint":                  iss + "/oauth2/logout",
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{GrantAuthorizationCode, GrantRefreshToken, GrantClientCredentials, GrantTokenExchange},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
		"claims_supported": []string{"sub", "iss", "aud", "exp", "iat", "auth_time", "nonce", "acr", "amr", "azp",
			"sid", "at_hash", "tenant_id", "email", "name"},
		"token_endpoint_auth_methods_supported":            []string{"client_secret_basic", "client_secret_post", "tls_client_auth", "none"},
		"revocation_endpoint_auth_methods_supported":       []string{"client_secret_basic", "client_secret_post", "tls_client_auth", "none"},
		"introspection_endpoint_auth_methods_supported":    []string{"client_secret_basic", "client_secret_post", "tls_client_auth"},
		"code_challenge_methods_supported":                 []string{"S256"},
		"dpop_signing_alg_values_supported":                []string{"ES256", "RS256", "PS256"},
		"tls_client_certificate_bound_access_tokens":       true,
		"authorization_response_iss_parameter_supported":   true,
		"claims_parameter_supported":                       false,
		"request_parameter_supported":                      false,
		"request_uri_parameter_supported":                  false,
		"require_pushed_authorization_requests":            false,
		"frontchannel_logout_supported":                    false,
		"backchannel_logout_supported":                     false,
		"acr_values_supported":                             []string{acrSingle, acrMFA, acrPhishingResistant},
		"prompt_values_supported":                          []string{"none", "login"},
		"ui_locales_supported":                             []string{"en"},
	})
}

func writeJSONCacheable(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
