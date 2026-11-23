# identity-context-svc: spec deviations

Where this service's behaviour differs from a specification, and why (implementation plan §2 R15).

## ZS-API-001 §12: problem details on protocol endpoints

ZS-API-001 §12 asks for every error to be an RFC 9457 `application/problem+json` document. The service
follows that on every `/v1/...` route. Three families of routes keep the error format their own protocol
mandates instead, because the clients that call them are written against that protocol:

| Routes | Error format kept | Mandated by |
|---|---|---|
| `/oauth2/*` (authorize, token, introspect, revoke, userinfo, login) | `{"error": "...", "error_description": "..."}` as `application/json`, plus `WWW-Authenticate` on 401 | RFC 6749 §5.2, RFC 6750 §3, OpenID Connect Core |
| `/scim/v2/*` | SCIM error schema (`urn:ietf:params:scim:api:messages:2.0:Error`) | RFC 7644 §3.12 |
| `/.well-known/*`, `/health`, `/readyz` | discovery documents and probe bodies | OIDC Discovery; the orchestrator reads only the status |

The API-2 review of 27 Oct 2026 exempted these paths and no others: every other 4xx/5xx in `openapi.yaml`
references `#/components/schemas/Problem` as `application/problem+json`.

## AU-07 / ZS-IAM-001 §17: SAML federation

AU-07 and IAM §17 name OIDC or SAML for enterprise federation. Federation here is OIDC only
(`internal/federation`: discovery, JWKS with rotation, signed id_token, nonce, claim-to-subject mapping,
SCIM provisioning). A `saml_assertion` at `POST /v1/context/resolve` is refused with 400
`saml_assertion is not supported`, deliberately not 401: no assertion a caller could send would work.
SAML waits on decision D-4 (build or buy the IdP, Auth-Service audit §D); a tenant whose IdP speaks only SAML
federates through a broker that issues OIDC until then.

## ZS-IAM-001 §3 and §13: passkey attestation

Passkeys are registered with attestation conveyance `none` and no FIDO Metadata Service: the service proves
possession of the key, user presence and user verification, and the origin and relying party, but not the
authenticator's make or model. That is the WebAuthn default for consumer and workforce passkeys (synced
passkeys carry no attestation). An allow-list of authenticator models (AAGUIDs) for administrators is a
Security decision; the library supports one (a metadata provider and AAGUID filtering) for `internal/webauthn`'s
config to pass through once it is made.
