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
