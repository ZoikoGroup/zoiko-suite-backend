# authorization-svc — administration and governance API

The contract for every route outside the five Doc 03 §8.3 inbound APIs, which
until 7 Oct 2026 existed only in code. For the evaluation endpoints see
`progress.md` and ZS-IAM-001 §8.1/§8.2/§21.

## Conventions (all routes)

- **Identity (register A-01, 14 Oct 2026).** Every route here (all but the evaluation
  routes and the probes) identifies the caller by the identity envelope identity-context-svc
  minted, sent as `Authorization: Bearer <envelope>` and verified against its JWKS (RS256,
  issuer, audience, expiry). Principal and tenant come from the envelope; an
  `X-Principal-Id` / `X-Tenant-Id` that names anyone else is `403 identity_mismatch` /
  `403 tenant_scope_mismatch`. A presented envelope that does not verify is
  `401 identity_assertion_invalid` in every mode. With none, a registered delegate service
  (`AUTHZ_ADMIN_DELEGATES`, its mTLS certificate CN — never a header) may act for the
  principal it forwards; otherwise `AUTHZ_IDENTITY_ASSERTION=require` (the default outside
  ENV=local) answers `401 identity_assertion_required`, and `warn` admits the request marked
  `X-Identity-Assertion: missing`. No route takes the caller or tenant from the body or query
  string. A malformed tenant is `400`.
- **Permission.** Each admin route requires the named Authorization Standard
  (ZS-IAM-001 Appendix A) permission, held at tenant scope unless the row says
  *platform*. Missing it is `403 authorization_denied`, and the check is itself
  recorded as a decision.
- **Errors.** `{"error": "<code>", "message": "...", "error_class": "<§16 class>"}`.
  `error` is the stable code callers already match on. `error_class` is the
  Governance Control Plane §16 class (`CONTEXT_UNRESOLVED`,
  `AUTHORIZATION_DENIED`, `SOD_CONFLICT`, `APPROVAL_INVALIDATED`,
  `CONCURRENCY_CONFLICT`, `IDEMPOTENCY_MISMATCH`), present only where one applies.
- **Idempotency.** Every material write honours `Idempotency-Key`: a retry
  returns the first response (`X-Idempotent-Replay: true`, `201` answered as
  `200`); the same key with a different body is `409 idempotency_mismatch`.
- **§16 command fields.** Privileged and destructive commands (marked **R**)
  take `reason_code` and/or `purpose`, recorded with the change in
  `authz_config_history`. With `AUTHZ_COMMAND_CONTRACT=enforce` (the default) a
  missing reason is `400`; with `warn` it is admitted and marked
  `X-Command-Contract: violated`.
- **Optimistic concurrency.** Commands marked **V** take an optional
  `expected_version`; a stale one is `409 version_conflict`. Every role, bundle,
  SoD and ABAC rule carries `version`.
- **Events.** Every change below is announced through the transactional outbox
  on `zoiko.authorization.events` (ZS-IAM-001 §23 names): `iam.role.published`,
  `iam.assignment.granted` / `.revoked`, `iam.delegation.granted` / `.revoked`,
  `iam.sod_policy.published`, `iam.policy_set.published`,
  `sod.compensating_control.approved` / `.revoked`, `sod.exception.expired`,
  `authorization.cache.invalidated`.

## Roles and permission bundles

| Route | Permission | Notes |
|---|---|---|
| `POST /v1/admin/roles` | `iam.role.manage` | Idempotent on (tenant, role_code): `201` / `200`. |
| `GET /v1/admin/roles` | `iam.role.read` | Tenant-scoped catalogue. |
| `POST /v1/admin/roles/{role_id}/retire` · `/reactivate` | `iam.role.manage` | **R V**. Reactivating a role you hold is `403 self_grant_not_allowed`; reactivating one that would give a holder an SoD conflict is `409 sod_conflict` listing the holders. |
| `POST /v1/admin/roles/{role_id}/permission-bundles` | `iam.permission_bundle.manage` (*platform* if the bundle names a protected platform-admin permission — `SOD_RULE_MANAGE_GLOBAL`, `ABAC_RULE_MANAGE_GLOBAL`, `security.*`, `platform.*`) | **V** (replace only). `201` created / `200` replaced. Refused on a role you hold (`403`) or if it gives a holder an SoD conflict (`409`). |
| `GET /v1/admin/roles/{role_id}/permission-bundles` | `iam.role.read` | Includes retired bundles. |
| `POST /v1/admin/permission-bundles/{id}/retire` · `/reactivate` | `iam.permission_bundle.manage` | **R V**. Reactivation checks as above. |

## Role assignments (maker-checker)

| Route | Permission | Notes |
|---|---|---|
| `POST /v1/admin/role-assignments` | `iam.assignment.grant` (*platform* for `legal_entity_id` = the platform scope) | Body: `principal_id`, `role_id`, `legal_entity_id?`, `book_id?`, `org_unit_id?`, `effective_from`, `effective_to?`, `approval_reference?`. Self-assignment `403`; SoD conflict at the assignment's scope `409`. A **privileged** role (one granting any `iam.*` or protected platform action) or a platform-scope assignment by a caller without `iam.assignment.approve_privileged` is recorded `PENDING_APPROVAL` (`202`) and grants nothing until approved; with it, `201` `APPROVED`, `approved_by` = caller. |
| `GET /v1/admin/role-assignments` | `iam.assignment.read` | Shows `approval_status`. |
| `POST /v1/admin/role-assignments/{id}/approve` · `/reject` | `iam.assignment.approve_privileged` (*platform* for a platform-scope assignment) | **R**. The checker must be neither the requester nor the target (`403 checker_not_independent`). Approval re-runs the SoD check. Pending requests expire after 72 h (`409 approval_expired`). |
| `POST /v1/admin/role-assignments/{id}/revoke` | `iam.assignment.revoke` (*platform* for a platform-scope assignment) | **R**. `{"effective_to": ...}` schedules the end instead of ending now. |

## Delegations (ZS-IAM-001 §11)

| Route | Permission | Notes |
|---|---|---|
| `POST /v1/admin/delegated-authorities` | `iam.delegation.grant` | The delegator must be the caller; no self-delegation. `delegated_actions` may not name an `iam.*` or protected platform action (`400 protected_privilege_not_delegable`), and a full-authority delegation never confers them. `effective_to` is required under `enforce`; under `warn` it defaults to 90 days. `reason` (**R**) and `approval_reference` are recorded. |
| `GET /v1/admin/delegated-authorities` | `iam.delegation.read` | |
| `POST /v1/admin/delegated-authorities/{id}/revoke` | `iam.delegation.revoke` | **R**. Delegator only. |

## Segregation of duties (GOV-04)

| Route | Permission | Notes |
|---|---|---|
| `POST /v1/admin/sod-rules` | `iam.sod_rule.manage`; *platform* `SOD_RULE_MANAGE_GLOBAL` for a rule with no `tenant_id` | |
| `GET /v1/admin/sod-rules` | `iam.sod_rule.read` | Tenant and platform-wide rules. |
| `POST /v1/admin/sod-rules/{id}/retire` · `/reactivate` | `iam.sod_rule.manage` | **R V**. A principal holding either action of the rule cannot retire it (`403 sod_self_interest`). |
| `POST /v1/admin/sod-exceptions` | `iam.sod_rule.manage` | Compensating-control exception: `sod_rule_id`, `principal_id`, `compensating_control`, `reason`, `effective_from?`, `expires_at` (**required** — exceptions always expire). `REQUESTED`. |
| `GET /v1/admin/sod-exceptions?principal_id=&status=` | `iam.sod_rule.read` | |
| `POST /v1/admin/sod-exceptions/{id}/approve` · `/reject` | `iam.sod_rule.publish` | **R**. Never by the requester or the subject (also a database CHECK). While `APPROVED` and in its period the conflict permits with obligation `COMPENSATING_CONTROL:<control>` and reason code `SOD_EXCEPTION_APPLIED`; it stops the instant it expires. |
| `POST /v1/admin/sod-exceptions/{id}/revoke` | `iam.sod_rule.manage` | **R**. |
| `GET /v1/sod/conflicting-permissions?action_type=` | — | GOV-04 ListConflictingPermissions. |

## ABAC rules

| Route | Permission | Notes |
|---|---|---|
| `POST /v1/admin/abac-rules` | `iam.abac_rule.manage`; *platform* `ABAC_RULE_MANAGE_GLOBAL` for a platform-wide rule | |
| `GET /v1/admin/abac-rules` | `iam.policy.read` | |
| `POST /v1/admin/abac-rules/{id}/retire` · `/reactivate` | `iam.abac_rule.manage` | **R V**. |

## GOV-03 commands

| Route | Permission | Notes |
|---|---|---|
| `POST /v1/admin/authorization-cache/invalidate` (alias `/internal/v1/gov03/commands/invalidateAuthorizationCache`) | `iam.policy.publish` | **R**. Drops this replica's cache for the tenant and emits `authorization.cache.invalidated`; every replica's invalidator (own consumer group per replica) drops its own. |
| `POST /v1/admin/subjects/{principal_id}/effective-access/recompute` (alias `/internal/v1/gov03/commands/recomputeSubjectEffectiveAccess`, `principal_id` in body) | `iam.assignment.read` for another subject | Recomputes from the store: each assignment, whether it grants now, its actions, and the union. |

## Decision log (GOV-03 ExplainAuthorization)

| Route | Permission | Notes |
|---|---|---|
| `GET /v1/access-decisions` | `iam.policy.read` for the tenant's log | Without it: only the caller's own decisions, with `decision_basis` redacted to `"redacted"` and matched grants / delegation removed — the reason codes remain (ZS-IAM-001 §25). Asking for another principal is `403`. |
| `GET /v1/access-decisions/{id}` | as above | Another principal's decision without `iam.policy.read` is `404`. |

Each decision carries `decision` (`PERMIT` / `DENY` / `STEP_UP` /
`REQUIRE_APPROVAL`), `policy_set_version` (`cfg.<n>`, the configuration
watermark — replay it from `authz_config_history`), `obligations`,
`reason_codes`, `matched_grants`, the resource, an attributes digest (never the
values), session assurance, `on_behalf_of` / `delegation_id` for a delegated
grant, and `expires_at`. The log is append-only in the database.

## Sessions (ZS-IAM-001 §13–15) and reviews

Each route exists under `/admin/v1/…` and `/v1/admin/…` (support under `/v1/support/sessions`).
Registers need the administering permission; your own sessions are readable without it,
another principal's session by id is `404`. A session grants only to its subject, only while
ACTIVE and unexpired, and only the actions it names — no wildcard is accepted anywhere.

| Route | Permission | Rules |
|---|---|---|
| `POST …/privileged-sessions` | `iam.pam.manage` | Body: `principal_id` (default the caller), `requested_actions`, `ticket_ref`, `reason`, `duration_seconds`, `resource_scope`, `target_environment` — all mandatory (§13). The subject must hold `iam.pam.eligible`. Any high-impact action (`iam.*`, protected platform, payment / posting / filing) → `202` `PENDING_APPROVAL`, granting nothing; otherwise `201` ACTIVE. **Step-up** (see below). Max `AUTHZ_MAX_JIT_SECONDS` (8 h); longer is `400 duration_exceeds_maximum`. |
| `POST …/privileged-sessions/{id}/approve` · `/reject` | `iam.pam.approve` | **R**. Never the subject or the requester (`403 checker_not_independent`; also a DB CHECK). Approval needs the approver's own **step-up**; rejection does not. The clock starts at approval. Pending requests expire after 24 h. |
| `POST …/privileged-sessions/{id}/revoke` | `iam.pam.manage` | **R**. |
| `POST …/break-glass-sessions` | `iam.break_glass.manage` | `incident_id` (A16), `reason`, `requested_actions`. The subject must hold `iam.break_glass.eligible`. No payment / posting / filing power (§14 "separately constrained"). CRITICAL SIEM alert on issue. **Step-up**. Max `AUTHZ_MAX_BREAK_GLASS_SECONDS` (2 h); longer is `400`. |
| `POST …/break-glass-sessions/{id}/revoke` | `iam.break_glass.manage` | **R**. |
| `POST /v1/support/sessions` | `iam.support.manage` | `ticket_ref`, `purpose`, and `tenant_consent_obtained` stated explicitly (400 if omitted). Read-only by default: grants read verbs only (`…READ/VIEW/LIST/GET/SEARCH/QUERY/LOOKUP/INSPECT/DISCOVER`). A writable session also grants only the writes named in `allowed_actions`. No consent → nothing granted. Bulk export off unless `allow_bulk_export`. Max `AUTHZ_MAX_SUPPORT_SECONDS` (4 h); longer is `400`. |
| `POST /v1/support/sessions/{id}/revoke` | `iam.support.manage` | **R**. |
| `POST …/privileged-sessions/{id}/review` · `…/break-glass-sessions/{id}/review` | `iam.session.review` | `{"outcome": "ATTESTED" \| "FLAGGED", "notes"}`; FLAGGED needs notes and raises a CRITICAL SIEM alert. Reviewer is never the subject or the requester. A review is opened when a high-impact JIT session or any break-glass session ends (revoked or expired). |

**Step-up (register A-03, ZS-IAM-001 §3/§13/§14).** Issuing a JIT session, approving one,
and issuing a break-glass session need the caller's verified envelope to carry
`session_trust_posture.mfa_verified=true` with an `auth_time` no older than
`AUTHZ_SESSION_STEP_UP_MAX_AGE_SECONDS` (900). Otherwise `401 step_up_required` with
`reason` (`no_verified_identity`, `delegated_caller`, `single_factor`, `step_up_stale`,
`authn_time_unknown`, `authn_time_in_future`, `assertion_principal_mismatch`),
`required_assurance: "MFA"`, `max_authn_age_seconds`, and
`WWW-Authenticate: Bearer error="insufficient_user_authentication"`. The remedy is
identity-context-svc `POST /v1/authenticate/step-up` (a TOTP code), then
`/v1/context/resolve`, then retry with the new envelope. Every check is recorded as
`iam.session.step_up.<issue_jit|approve_jit|issue_break_glass>`. What was proven is stored on
the session (000033) and returned as `requester_step_up` / `approver_step_up`
(`assurance`, `authn_at`, `session_context_id`). `AUTHZ_SESSION_STEP_UP=off` exists for
ENV=local only.

Events (outbox, 000030): `security.privileged_session.started` / `.ended`,
`security.break_glass.started` / `.ended`, `support.session.started` / `.ended`; a revoke's
reason travels as `command_reason`. The expiry sweep (every minute) ends sessions that ran out.

`POST /internal/authorization/decisions` is the ZS-IAM-001 §8 canonical decision API: the same
pipeline as `/v1/authorize`, with `subject_id`, `action`, `resource_*`, `environment` and the
optional `session_id` (JIT), `break_glass_session_id` and `support_session_id`; it returns the
§8.2 decision.

Access reviews (§24) are **not** served here. They belong to access-control-svc (campaigns,
items, decisions; `GET /v1/iam/access-reviews` there). This service's copy of the routes was
removed on 13 Oct 2026 (Governance audit N-03 / N-10): it had no way to create a review, and a
REVOKE decision discarded every revocation error while marking the review completed.

## Workload bindings (ZS-IAM-001 §16)

| Route | Permission | Notes |
|---|---|---|
| `POST /v1/admin/workload-bindings` | `iam.workload.manage` | **R**. `{workload_id, allowed_audience, allowed_actions[]}`; creates or replaces the binding in the caller's tenant. Wildcard audience or action, or no actions → `400`. |
| `GET /v1/admin/workload-bindings` | `iam.workload.manage` | The tenant's bindings, active and not. |
| `POST /v1/admin/workload-bindings/{workload_id}/deactivate` · `/reactivate` | `iam.workload.manage` | **R**. Unknown binding → `404`. |

Events: `iam.workload_binding.saved` / `.deactivated` / `.reactivated`, committed with the change.
At decision time a workload may take only the actions its binding lists
(`DENIED workload:action_not_bound`); the binding narrows, it never grants. A principal bound as
a workload in any tenant is evaluated as one whether or not the request declares
`principal_type: WORKLOAD`. `iam.workload.manage` is not yet in access-control-svc's permission
registry; until it is, grant it through this service's own admin API.

## Read surfaces

`GET /v1/{resource}/{id}/available-actions` and `GET /v1/me/capabilities` use
the verified caller only; the former answers for another principal with
`iam.assignment.read`. The legal entity must belong to the caller's tenant, resolved as for
`/v1/authorize` (`403 tenant_scope_mismatch` otherwise; 13 Oct, N-06). Available actions are classified by the same pipeline as
`/v1/authorize`. `POST /v1/entity-scope/validate` for another principal needs
`iam.assignment.read`.

## Command reasons (§16)

`AUTHZ_COMMAND_CONTRACT` defaults to `enforce`: every command marked **R** above needs
`reason_code` or `purpose` in its body (400 `missing_field` otherwise), and a delegation needs
`effective_to` and `reason`. Roles, bundles, SoD and ABAC rules record the reason in
`authz_config_history`; every IAM event carries it as `command_reason` (000029). `warn` admits
and marks such commands (`X-Command-Contract: violated`).

## Tenant scope of `/v1/authorize` (and the canonical decision API)

The tenant is resolved on the server from tenant-entity-registry-svc's
`entity.created` / `entity.status.changed` (projected into
`entity_status_projection`):

| Caller sends | Registry knows the entity | Result |
|---|---|---|
| `X-Tenant-Id` or body `tenant_id` = owner | yes | evaluated in that tenant |
| `X-Tenant-Id` or body `tenant_id` ≠ owner | yes | `403 tenant_scope_mismatch`, nothing evaluated or recorded |
| no tenant | yes | evaluated **and recorded** in the owner's tenant |
| tenant claimed | no | asked of the registry (`AUTHZ_ENTITY_REGISTRY_URL`, `GET /v1/entities/{id}/status` as that tenant): confirmed → evaluated and the owner projected; not confirmed → `403 tenant_scope_mismatch` |
| no tenant | no | the registry is asked about each tenant the principal holds a live approved assignment, live delegation or identity standing in: the one holding the entity scopes and records the decision; none → `403 tenant_scope_mismatch` |
| no tenant, principal holds nothing anywhere | no | `400 missing_tenant_scope` under `AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE=true`; otherwise `403 tenant_required`, nothing evaluated or recorded (13 Oct, C-01) |
| anything | lookup fails | `503 store_unavailable`; registry unreachable or non-200/404 → `503 entity_registry_unavailable` |

With `AUTHZ_ENTITY_REGISTRY_URL` unset the registry rows fall back to the
pre-12-Oct behaviour (claimed tenant used, tenantless evaluated across tenants). Since 13 Oct it
is required outside `ENV=local`: the service will not start without it (N-12).

The platform-scope entity is exempt (tenantless by design).

## Which service may ask for a decision (12 Oct, pass 5)

The evaluation routes are `/v1/authorize`, `/v1/sod/evaluate`, `/v1/sod/validate`,
`/v1/entity-scope/validate`, `/v1/delegated-access/evaluate` and every `/internal/*` route.
They are gated by the caller's client certificate (`internal/workload`). The certificate's CN is
the `service_name` mtls-management-svc issued it for. Admin, session and console routes are
not gated.

| Caller | `AUTHZ_EVALUATION_MTLS=require` (default outside `ENV=local`) | `permit` (local) |
|---|---|---|
| plaintext port, no certificate | `401 client_certificate_required` | admitted |
| mTLS, certificate CN on `AUTHZ_PEP_ALLOWLIST` | admitted; `X-Workload-Id` set to the CN | same |
| mTLS, CN not on the allow-list | `403 workload_not_registered` | same (when a list is set) |
| mTLS, `X-Workload-Id` names someone other than the CN | `403 workload_identity_mismatch` | same |
| mTLS, certificate from another CA, or none | TLS handshake refused | same |

Require mode will not start without `MTLS_ENABLED=true` and a non-empty `AUTHZ_PEP_ALLOWLIST`.
A PEP joins by setting its own `AUTHZ_MTLS_ENABLED=true` / `AUTHZ_MTLS_URL` and being added
to the list.

## Changes of 13 Oct 2026 (Governance audit fresh-audit fixes)

- **Admin gates check standing (N-01, N-13).** Every admin, approval, eligibility and read
  gate reads the caller's projected principal status first. A SUSPENDED / DISABLED /
  TERMINATED principal is refused (`403`, recorded with basis `principal_status:<status>`)
  wherever they hold the permission. Every gate check is recorded, including approvals and
  read gates.
- **Delegations end with the delegator's standing (N-02).** A delegation from a principal
  who is not ACTIVE in the delegation's tenant confers nothing, and flows again on
  reinstatement.
- **Step-up is the server's (N-07).** Reopen-class actions and the actions in
  `AUTHZ_STEP_UP_ACTIONS` need a recent login: at most `AUTHZ_STEP_UP_MAX_AUTHN_AGE_SECONDS`
  (300) old, and a caller may only tighten that. A request that states no authentication age
  (`X-Authn-Age-Seconds`, `attributes.authn_age_seconds`, `environment.authn_age_seconds`) is
  answered `STEP_UP` under `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up` (the default), or treated as
  recent under `permit`. Compose sets `permit`, because no PEP forwards an age yet (see the
  compose comment).
- **JIT approval is default-deny (N-08).** Any requested action that is not a pure read
  (last token READ / VIEW / LIST / GET / SEARCH / QUERY / LOOKUP / INSPECT / DISCOVER) makes
  the session `PENDING_APPROVAL` (`202`) until an independent `iam.pam.approve` holder approves.
- **Outbox (N-04, N-14).** The relay publishes a claimed batch in one Kafka write and drains
  a backlog without waiting a tick; the shared writer flushes after 10 ms and hashes the
  message key. The retention sweep purges published outbox rows after
  `AUTHZ_OUTBOX_RETENTION_HOURS` (168) and Idempotency-Key records after
  `AUTHZ_IDEMPOTENCY_RETENTION_HOURS` (168); 0 keeps them. Migration **000031**.
- **Principal status ordering (N-05).** A status event without `status_changed_at` is
  ordered by its envelope `emitted_at`, so a replayed or late event cannot undo a newer one.
- **mTLS identity renews (N-11).** The server certificate is re-provisioned at two thirds of
  its lifetime; new handshakes use the new certificate.
- **Replica id (N-15).** `AUTHZ_REPLICA_ID` names this replica's cache-invalidation consumer
  group; it defaults to the hostname.

## Changes of 14 Oct 2026 (auth plane closure: A-01, A-03, B-01)

- **The caller is the identity envelope (A-01).** See *Identity* under Conventions.
  `internal/identity` (gate + JWKS client); config `AUTHZ_IDENTITY_ASSERTION`,
  `AUTHZ_IDENTITY_JWKS_URL` (required unless `off`), `AUTHZ_IDENTITY_ISSUER`,
  `AUTHZ_IDENTITY_AUDIENCE`, `AUTHZ_ADMIN_DELEGATES`. An https JWKS URL is fetched with this
  service's own mTLS certificate (identity-context-svc's :8449 listener allow-lists it).
  Unknown-kid refetches are rate-limited to one per 10 s.
- **Step-up before privileged sessions (A-03).** See *Step-up* under Sessions. Migration
  **000033** (evidence columns, paired by CHECK; down refuses while evidence exists).
- **Session maximums (B-01).** Configurable, refused rather than shortened: JIT 8 h,
  break-glass 2 h, support 4 h (chosen 14 Oct 2026; Security sign-off pending). The store
  refuses too, so a caller that skips the handler cannot be clamped.
- **access-control-svc** forwards the person's envelope when its call acts in that person's
  name, and calls over mTLS (when enabled) so it can be admitted as a delegate otherwise.
