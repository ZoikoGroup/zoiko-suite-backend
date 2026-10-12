# authorization-svc — administration and governance API

The contract for every route outside the five Doc 03 §8.3 inbound APIs, which
until 7 Oct 2026 existed only in code. For the evaluation endpoints see
`progress.md` and ZS-IAM-001 §8.1/§8.2/§21.

## Conventions (all routes)

- **Identity.** `X-Principal-Id` and `X-Tenant-Id` are required and verified
  (the gateway sets them). A missing one is `401`; a malformed tenant is `400`.
  No route takes the caller or tenant from the body or query string.
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
  `authz_config_history`. With `AUTHZ_COMMAND_CONTRACT=warn` (default) a missing
  reason is admitted and marked `X-Command-Contract: violated`; with `enforce`
  it is `400`.
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

## Sessions and reviews

Privileged, break-glass and support session registers need the permission that
administers them (`iam.pam.manage`, `iam.break_glass.manage`,
`iam.support.manage`); your own sessions are readable without it, and another
principal's session by id is `404`. `GET /v1/iam/access-reviews` and
`POST /v1/iam/access-reviews/{id}/decide` act for the verified caller only.

## Read surfaces

`GET /v1/{resource}/{id}/available-actions` and `GET /v1/me/capabilities` use
the verified caller only; the former answers for another principal with
`iam.assignment.read`. Available actions are classified by the same pipeline as
`/v1/authorize`. `POST /v1/entity-scope/validate` for another principal needs
`iam.assignment.read`.
