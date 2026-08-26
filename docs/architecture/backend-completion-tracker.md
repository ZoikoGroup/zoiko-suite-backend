# Backend Completion Tracker

Single, consolidated list of everything confirmed **not built / not complete** against
`docs/original_doc/` (the 7-document spec: Doc 01–06 series + Doc 07 commercial standard),
verified against the real codebase on 2026-08-21 (not just re-stated from older tracking
docs — see "Verification method" per item).

This supersedes nothing — `known-gaps.md`, `doc7-implementation-backlog.md`, and
`full-architecture-gap-analysis.md` remain the detailed narrative record of *how* each
finding was discovered. This file exists to be worked top-to-bottom, one row at a time.

## Working rule

**Exactly one row at a time.** For each row:
1. Move it to `In Progress`.
2. Implement the fix for that row only.
3. Run the full verification loop: `gofmt -w .` → `go build ./...` → `go vet ./...` →
   `go test ./... -count=1` on every service touched.
4. Write/extend a test that specifically proves the fix (not just that nothing broke).
5. Commit + push to `satyaprakash-changes` (no PR — per standing instruction, one
   consolidated PR happens at the end of the whole tracker, not per row).
6. Mark the row `Done`, with the commit hash and a one-line note on what was verified.
7. Only then move to the next row.

Do not start a second row before the current one is `Done`. Do not batch multiple rows
into one commit.

## Status values
`Not Started` / `In Progress` / `Done` / `Blocked` (needs a decision this tracker can't make)

---

## Priority 1 — Tier-0 governance services with zero row-level security — ✅ COMPLETE

The Doc 03 §06 "must exist before broad functional expansion" services that had a
`tenant_id` column and **no `CREATE POLICY` / `ENABLE ROW LEVEL SECURITY` at all** — verified
by grepping every migration file, not inferred. Pattern to copy: `governance-decision-log-svc`'s
`000002_add_rls.up.sql` + `000006_force_rls.up.sql`.

**Closed 2026-08-22.** 7 real services fixed (row 4 was a false positive — genuinely
platform-wide reference data). All 7 verified against a real Postgres 16 as a purpose-created
`NOSUPERUSER NOBYPASSRLS` role, each with a negative control.

### What this tier actually taught us — read before starting Priority 2

Four lessons that cost real time here and will recur:

1. **A superuser bypasses RLS unconditionally, `FORCE` included.** `TEST_DATABASE_URL` points
   at `postgres`. An isolation test over that connection proves only that the application
   predicate works — it never touches the policy. Every row here needed a purpose-created
   ordinary role. The first version of row 6's test passed for exactly this wrong reason.
2. **Always run a negative control.** Remove the migration; the test must fail. Row 8 also
   showed the *reverse* control matters: an over-restrictive policy is a real failure mode
   too (it hid global defaults), and only a test that checks for it will catch it.
3. **Three services needed a deliberate exemption, and getting it wrong is an outage, not a
   tightening.** audit-event-store-svc (Kafka writer, global hash chain — a naive policy
   stops the evidence store accepting events *silently*), authorization-svc
   (`FindGrantedActions` is the core `/v1/authorize` path — scoping it breaks all
   authorization platform-wide), secret-vault-integration-svc (cross-tenant admin actions).
   Check for a legitimate cross-tenant caller *before* writing the policy, not after.
4. **Test harnesses that name migrations individually silently skip new ones.** Rows 7 and 8
   both had this: the migration under test would not have been applied at all. Check the
   suite globs `*.up.sql` before trusting a green run.

| # | Service | Spec ref | Status | Notes |
|---|---|---|---|---|
| 1 | identity-context-svc | Doc 03 §06, Doc 04 §2.2 | Done | `045ae84`. Also found & fixed a real cross-tenant hole: GET/PUT /v1/principals/{id} routes had no X-Tenant-Id check at all. RLS (ENABLE+FORCE+WITH CHECK) added on all 4 tables; 4 isolation tests + 2 handler tests live-verified against real Postgres 16. |
| 2 | secret-vault-integration-svc | Doc 03 §06, Doc 04 §2.2 | Done | `6343434`. Real bug: ListVersionHistory had zero tenant scoping. RLS added with a documented `app.platform_scope` bypass for the 2 genuinely cross-tenant admin actions (ActivateVersion, Rotate). 9 tests live-verified against real Postgres 16, including 2 proving the bypass itself works. |
| 3 | policy-svc | Doc 03 §06, Doc 04 §2.2 | Done | `14602c6`. Real bug: ListVersionHistory had zero tenant scoping (same shape as row 2's fix). RLS on policy_versions only (other 4 tables are platform-wide, no tenant_id). No platform-scope bypass needed — ActivateVersion here is genuinely tenant-scoped. 7 tests live-verified against real Postgres 16. |
| 4 | jurisdiction-rules-svc | Doc 03 §06 | **Not applicable** | False positive in the original audit — the only "tenant_id" hit in its migrations is a comment ("...reference data, not per-tenant data. No tenant_id column."), not a real column. This service is genuinely platform-wide reference data (matches Doc 03's own design — jurisdiction-rules-svc *is* the jurisdiction concept). No RLS is possible or correct here; fabricating a tenant boundary would violate the "never fabricate a signal with nothing real to populate it" doctrine. Removed from the count of 8 — real Tier-0 count is 7. |
| 5 | authorization-svc | Doc 03 §06, Doc 04 §2.2 | Done | `de2dfc9`. Real, severe bug found beyond the RLS gap: all 7 `/v1/admin/*` write routes had NO authentication at all — tenant_id and actor attribution came straight from the request body. Fixed all 7 (tenant verification, actor from X-Principal-Id, delegator-must-be-caller). RLS added on `roles`/`sod_rules` only (the only 2 tables with a real tenant_id). Two reads (`FindRoleByID`, `FindGrantedActions`) needed a deliberate platform-scope bypass — `FindGrantedActions` is the core `/v1/authorize` path called on nearly every request platform-wide; scoping it by tenant would have silently broken all authorization. 37 tests live-verified against real Postgres 16. |
| 6 | workflow-svc | Doc 03 §06, Doc 04 §2.2 | Done | `9a5f748`. Two real bugs beyond RLS: (1) `FindWorkflowByID` — the choke point all Store methods route through — fell back to an UNSCOPED lookup when X-Tenant-Id was omitted (document-vault-svc's "filter that disables itself" shape); (2) `initiated_by`/`actor_principal_id` came from the request body on every route, making the existing SoD checks self-declared rather than load-bearing. RLS on `workflow_instances`. 31 tests live-verified against real Postgres 16 — including a purpose-created NOSUPERUSER NOBYPASSRLS role for the no-tenant probe (a superuser bypasses RLS unconditionally, so the first version of that test passed for the wrong reason) plus an explicit negative-control run with the migration removed. |
| 7 | audit-event-store-svc | Doc 03 §06, Doc 04 §2.2 | Done | `f02b270`. Different shape from rows 1–6: a naive tenant-only FORCE policy here would have taken the platform's append-only evidence store **offline** — no tenant-context plumbing exists (Kafka consumer, no X-Tenant-Id), the hash chain is deliberately global (Doc 04 §15.4), and `sequence_number` is UNIQUE, so the chain-tip SELECT would match zero rows and WITH CHECK would reject every insert, silently (DLQ absorbs a consumer's error). Proven by negative control, not assumed. RLS added with an explicit `app.platform_scope` exemption set only by `PgStore.Store`. Also replaced the test suite's hand-maintained `const schema` copy with `applyMigrations()` reading the real files — the copy would have made this very migration untested. 4 integration tests live-verified against real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role. |
| 8 | configuration-feature-flag-svc | Doc 03 §06, Doc 04 §2.2 | Done | `7058337`. Simplest row — the app layer was already correct (all 6 handlers call `requireTenant`, every store method already takes the tenant, list routes already refuse a foreign `?tenant_id=`), so this was purely the DB backstop with no application bug alongside. RLS on both tables, keeping NULL-tenant_id global defaults readable by everyone — a plain `tenant_id = app.tenant_id` policy would hide every global default and turn applicable config into "not found". No platform-scope bypass needed (no legitimate cross-tenant caller exists). Also fixed the suite's setup naming only `000001`, which would have left this migration untested. 16/16 tests pass against real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role, with negative controls in **both** directions (missing policy → leak; over-restrictive policy → global defaults hidden). |

**Verification method per row**: add a `TestPgStore_RLS_TenantIsolation`-style test (same
pattern as tenant-entity-registry-svc's) that creates two tenants and proves a query scoped
to tenant A cannot see tenant B's rows, against a real Postgres instance.

⚠️ **Two ways an RLS test passes for the wrong reason** — both hit during row 6, both worth
checking before marking any row Done:

1. **Connected as a superuser.** `TEST_DATABASE_URL` normally points at `postgres`, and a
   SUPERUSER bypasses row-level security *unconditionally* — `FORCE` does not change this.
   A test asserting isolation while connected as the superuser proves only that the
   application-level `WHERE tenant_id = $n` predicate works, and nothing at all about the
   policy the row adds. For any assertion that the *policy itself* closes a gap (e.g. a
   missing-tenant fallback the app predicate deliberately leaves open), connect as a
   purpose-created `NOSUPERUSER NOBYPASSRLS` role — see workflow-svc's `appRolePool` helper
   for the pattern. This mirrors the platform's real runtime role (`zoiko_app`).
2. **The app predicate already covered it.** If the test would pass with the migration
   deleted, it is testing the handler/store code, not the RLS policy. Run the negative
   control explicitly: temporarily remove the `_add_rls.up.sql` file, confirm the test
   fails, restore it, confirm it passes.

## Priority 1b — Fabricated tenant identity: the `"default-tenant"` fallback — ✅ COMPLETE

**Closed 2026-08-25.** 15 services fixed, 1 false positive (row 8i). `grep -rn "default-tenant" --include=*.go services/` now returns **zero** real fabrication sites estate-wide (only explanatory comments remain).

### What this tier taught us

1. **A fabricated tenant is not a weaker missing check — it is the opposite of one.** A missing check makes a request *fail*. This made it *succeed*, into a tenant that does not exist, shared by every header-less caller.
2. **A correctly-scoped store makes it worse, not better.** Every service in this tier had a store that filtered by tenant properly — so it enforced the fake tenant just as faithfully as a real one. Correct code plus one fabricated identity produced shared control planes over key material, mTLS trust, and SIEM credentials.
3. **Where RLS already existed, the policy was SATISFIED, not bypassed.** Five services pushed the fabricated value through `set_config(app.tenant_id)` into a live `USING (tenant_id = current_setting(...))` policy. Postgres did exactly as told. `pg_class` reports the policy exists; tests passing a real tenant go green. This is why 1b was resequenced ahead of the remaining RLS work — adding RLS first would have produced a green, audited, useless boundary.
4. **A blanket tenant gate breaks legitimately tenant-less routes.** Applying the middleware with `r.Use()` wrapped `/healthz` in five services; probes carry no tenant, so every liveness check would have 401d and the orchestrator would restart the container in a loop. Fixed with `r.With()` on the `/v1` subtree — never a path comparison inside the middleware, which is a classic bypass source. Each service now has a test asserting a header-less probe still returns 200.
5. **This grep produces false positives.** 3 of 16 candidates were comments, not code (rows 4, 19, 8i). Read the file; never count the grep.

**Found 2026-08-22 during Priority 2 reconnaissance. This blocks meaningful RLS on 6 of
Priority 2's rows, so it is sequenced ahead of them.**

16 services silently substitute the literal string `"default-tenant"` when `X-Tenant-ID` is
absent, in request-path middleware (`internal/middleware/tenant.go`) or directly in a handler
— not in dev seeding. Verified in each:

```go
tenantID := r.Header.Get("X-Tenant-ID")
if tenantID == "" {
    tenantID = "default-tenant"     // ← fabricated identity
}
...
func GetTenantID(ctx context.Context) string {
    if val, ok := ctx.Value(TenantIDKey).(string); ok && val != "" { return val }
    return "default-tenant"          // ← and again, as a getter default
}
```

**Why this is worse than a missing tenant check.** A header-less request does not fail — it
succeeds, attributed to a tenant that does not exist. Every header-less request from every
caller lands in the *same* `"default-tenant"` bucket, so those rows are co-mingled, and any
caller can read another's data by simply *omitting* the header. Omitting it is the easier
request to make, which makes the insecure path the path of least resistance — the same shape
as document-vault-svc's "filter that disables itself" and payroll-exceptions-svc's `"GLOBAL"`
sentinel, and a direct violation of the platform's own "never fabricate a signal with nothing
real to populate it" doctrine.

**Why it must be fixed before/with RLS on the affected services, not after.** RLS compares
`tenant_id = app.tenant_id`. The application would set `app.tenant_id = 'default-tenant'`, so
every header-less caller would still read and write each other's rows — inside the policy,
legitimately. Tests that pass a real tenant would go green. Adding RLS alone there produces a
control that *looks* like tenant isolation and is not one: security theater, which is worse
than a known gap because it stops anyone looking.

Not a bug: the header is spelled `X-Tenant-ID` here vs the platform's `X-Tenant-Id`. Go
canonicalises header keys, so `Header.Get` matches either — checked before reporting.

Fix per service: fail closed (401, as `requireTenant` does in the Tier-0 services) instead of
inventing an identity. Both the middleware default and the getter default must go — leaving
either one keeps the hole.

| # | Service | In Priority 2? | Status |
|---|---|---|---|
| 8a | banking-connector-svc | yes (row 10) | **Done** `02421f7` — fixed together with row 10 |
| 8b | connectivity-api-bridge-svc | yes (row 12) | **Done** — fixed together with row 12 |
| 8c | esignature-integration-svc | yes (row 13) | **Done** — fixed together with row 13 |
| 8d | external-data-feed-svc | yes (row 15) | **Done** `5b3bb97` — fixed together with row 15 |
| 8e | hris-connector-svc | yes (row 16) | **Done** `3c97c3e` — fixed together with row 16 |
| 8f | tax-authority-interface-svc | yes (row 20) | **Done** `f7da196` — fixed together with row 20 |
| 8g | carta-svc | no | **Done** `342461b` |
| 8h | compliance-risk-scoring-svc | no | **Done** `342461b` |
| 8i | evidence-requirements-svc | no | **Not applicable** — false positive (3rd from this grep, after jurisdiction-rules-svc and source-authority-svc). Both hits are *comments* explaining that this service rejects a missing tenant instead of fabricating one; handlers call `requireTenant`. Its comment also names offboarding-severance-svc and workforce-compliance-svc as fabricating — verified, they no longer do, so that comment is stale. |
| 8j | forecasting-svc | no | **Done** `342461b` |
| 8k | key-management-svc | no | **Done** `4afed19` — store was already correctly scoped on every method, so the fabricated tenant produced a *shared control plane over key material*: header-less callers could list each other's keys and, via RotateKey/DisableKey, disable them. No RLS applicable (no migrations, no Postgres — see row 82). Negative control prints the bug in its own output. |
| 8l | migration-integrity-svc | no | **Done** `342461b` |
| 8m | mtls-management-svc | no | **Done** `e119065` — same shape; the shared bucket was a control plane over mutual-TLS trust (RevokeCert/RotateCert scoped like the reads, so header-less callers could revoke each other's certificates and break their service-to-service auth). Also fixed a latent store bug: `CreateCert` accepted a `tenantID` param and **silently dropped it**, never setting `cert.TenantID` — it worked only because the handler happened to populate the field, so any future caller that forgot would create an unattributed certificate. No RLS applicable. |
| 8n | reconciliation-intelligence-svc | no | **Done** `342461b` |
| 8o | reporting-orchestration-svc | no | **Done** `342461b` |
| 8p | siem-integration-svc | no | **Done** `e119065` — **worst payload of the three security services.** A `SIEMExporter` carries `endpoint_url` AND `auth_token`, stored as supplied and **never redacted on read**, so the shared bucket exposed a live credential for another tenant's SIEM destination — the secret itself, not metadata. `ListEvents` was shared too: a tenant's security event stream readable by anyone omitting a header, i.e. the pipeline meant to detect this. One of my own tests was a **false pass** (asserted only "not 201", actually failing on a 400 from a wrong field name, so it would have stayed green with the tenant check removed) — now asserts 404 specifically. No RLS applicable. See 8p-a. |

Note 8k/8m/8p (key-management, mtls-management, siem-integration) are **security** services —
a fabricated tenant on key material, certificate issuance, or the security-event pipeline is
the worst placement of this defect in the estate. Worth doing those even though they are
outside Priority 2.

## Priority 1c — Caller-declared tenant identity (candidate sweep, NOT yet a confirmed count)

Surfaced by row 9 (ai-governance-svc), and it is a **different, more severe defect class** than
Priority 1b. Priority 1b was a *fabricated* tenant: a header-less request landed in a shared
synthetic `"default-tenant"` bucket. This one is a *declared* tenant: the handler reads
`tenant_id` from the caller's own request body or query string, so the caller chooses which
tenant it operates on. No leak or guessed id is needed — you just type the value. On
ai-governance-svc that meant naming the tenant whose **autonomy allowlist** you were creating
or resolving against, which doc7 §G7 rules out in as many words ("not broad delegated
authority").

A sweep for `req.TenantID` / `q.Get("tenant_id")` in non-test handler code returns these
services, ordered by hit count:

```
10  secret-vault-integration-svc      4  evidence-requirements-svc     2  purchase-order-svc
 8  policy-svc                        4  document-vault-svc            2  evidence-manifest-svc
 8  kill-switch-registry-svc          3  workflow-svc                  2  accounts-receivable-svc
 8  configuration-feature-flag-svc    3  ai-governance-svc             2  accounts-payable-svc
 8  authorization-svc                 2  workflow-history-svc          1  governance-decision-log-svc
 7  retention-registry-svc            2  purchase-request-svc          1  bank-reconciliation-svc
 5  general-ledger-svc
```

**This is a candidate list, not a verdict** — the grep cannot tell "trusted" from "checked", and
two spot-checks already prove both directions:

- **ai-governance-svc still shows 3 hits after being fixed.** `req.TenantID` is now passed
  *into* `requireTenant` to be compared against the verified header, not trusted. A fixed
  service still matches.
- **secret-vault-integration-svc (10 hits) is already clean.** It has `refuseForeignTenant`
  and rejects `?tenant_id=` that disagrees with the verified scope — done as part of Priority 1.

So each service needs reading, not counting. Do not convert this table into a defect count.
Rows 11/14/17/18 below get this check as part of their own work, since they are already queued;
the rest need a pass of their own once Priority 2 closes.

## Priority 2 — Remaining non-Tier-0 services with zero row-level security — ✅ COMPLETE

**Closed 2026-08-22.** All 11 real services fixed (row 19 was a false positive). Every one
verified against real Postgres 16 as a purpose-created `NOSUPERUSER NOBYPASSRLS` role with
negative controls.

### What this tier taught, beyond Priority 1's four lessons

1. **"Lower severity than Tier-0" was wrong as a blanket statement.** The premise below was
   written before the work. In practice this tier held the worst defects found anywhere:
   evidence-manifest-svc served another tenant's **assembled evidence bundles** to anyone with
   a manifest id (row 14), and retention-registry-svc's legal holds gate **irreversible
   deletion** (row 18). Severity tracks what a table *holds*, not which tier the service sits in.
2. **The obvious policy is sometimes the dangerous one.** Three services (rows 8, 17, 18) have a
   nullable `tenant_id` where NULL means "applies to everyone". Tightening those to a plain
   tenant equality *looks* like hardening and produces: a wrong config value (row 8), an
   emergency stop that does not stop (row 17), or deletion of records under legal hold (row 18).
   **Every nullable-tenant policy needs an over-restrictive negative control**, not just the
   usual "remove the migration" one.
3. **Derived tables couple to their parent, in a direction that is not intuitive.** Rows 11c/14:
   `DROP POLICY` on the parent fails *closed* (Postgres reads RLS-enabled-with-no-policy as
   deny-all); `DISABLE ROW LEVEL SECURITY` fails *open*. One `ALTER TABLE` can widen seven
   tables while looking identical in a diff to a change that merely causes an outage.
4. **Our own fixes broke callers, and the breakage misdirected the diagnosis.** Row 14a: making
   workflow-svc require a tenant (Priority 1 row 6) silently broke evidence-manifest-svc's
   aggregator, which surfaced as `ErrSourceUnavailable` — reading as a downstream outage rather
   than a missing header. **Sweep service-to-service clients for `X-Tenant-Id` forwarding
   before Priority 3.**
5. **Test doubles that ignore `ctx` cannot fail.** Found in five services. A stub taking
   `_ context.Context` makes every handler-level isolation assertion vacuous, which is the most
   likely reason these defects survived review at all.

Original premise, retained for the record: *same defect, lower severity (not on the governance
critical path), still a real gap.*
Re-verified individually (2026-08-21) — 6 of these use `NNN_init.sql` naming rather than
golang-migrate's `NNNNNN_name.up.sql`, which the original audit's glob pattern would have
missed if re-run naively; checked their actual file contents directly instead.

| # | Service | Status | Notes |
|---|---|---|---|
| 9 | ai-governance-svc | Done | `95a5832`. **A different and more severe defect class than the connectors: caller-declared tenant identity.** Three routes took `tenant_id` from the caller's own request body or `?tenant_id=` query string — `POST /v1/automation-policies` (create an autonomy allowlist entry in *a tenant you name*), `GET /v1/automation-policies/resolve` (the may-this-run decision, which also reports `kill_switch_engaged`), and `POST /v1/automation-actions`. Not a leak reachable through a missing predicate: the caller simply declares the tenant. Doc7 §G7 makes allowlists "per tenant, role, risk class and tool" so agentic execution is "a controlled execution model, **not broad delegated authority**" — caller-declared tenant is that delegation. Now bound to the verified `X-Tenant-Id`; the body/query field survives for compatibility but may only *agree* (403 on disagreement, not a silent reinterpretation). Also `DecideAutomationAction` was an unscoped **write** on the human-authority gate — a caller with another tenant's action id could approve their pending autonomous action, i.e. grant agentic execution authority inside someone else's tenant; worse than esignature row 13, which forged a record rather than authorizing an action to run. Plus unscoped `GetAIRun` (exposing `source_refs`/`evidence_refs`/`recommended_action` — how a governed decision was reached) and `GetAutomationAction`. **Enforcement is in the handlers, not blanket middleware** — see row 9a. Migration `000002` covers only the three tenant tables. Four negative controls on real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role, including one at the handler layer: with the stub store's tenant checks stripped the new isolation tests fail, so they are not passing vacuously. |
| 9a | ai-governance-svc — platform-scope tables | **Not applicable (verified against the doc)** | `action_risk_classifications`, `model_provider_registrations` and `policy_change_approvals` carry **no** `tenant_id`, and that is correct. Doc7 §G2 makes the risk taxonomy one shared taxonomy, §G6 the provider registry platform-wide, and §G3 is explicit that policy changes "alter governance truth **across tenants** and historical evaluation" — so approving one is platform administration. Adding a tenant column to make the schema look uniform would invent a boundary the doc rules out (same trap as row 19 source-authority-svc). The controls that *do* belong on `policy_change_approvals` are authorization plus doc7 §H2/§H3's self-approval block, and the handler already enforces both. A test (`TestRLS_PlatformTablesHaveNoPolicy`) pins the asymmetry so a later uniformity change has to argue with a failing test rather than land quietly. This is also why the connectors' blanket-401 middleware was **not** reused here: it would have broken these routes. |
| 10 | banking-connector-svc | Done | `02421f7`. **Three defects, done together with row 8a** — RLS here is only load-bearing once the fabricated identity is gone. (1) `default-tenant` fabrication → now 401. (2) **Cross-tenant leak of bank data**, the worst of the three: all 3 reads had no tenant predicate — `GetConnectionByID`/`ListStatements` filtered on id alone (exposing `bank_name`, `bic`, `account_number`, balances), and `ListConnections`' only filter disabled itself when `legal_entity_id` was omitted. (3) RLS added (ENABLE+FORCE+WITH CHECK, no exemption needed). Also fixed `MemoryStore`, which had the identical unscoped reads — the handler tests run against it, so isolation assertions were passing against a fake that could not fail. Verified against real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role with negative controls on all three. |
| 11a | commercial-account-svc — exposed reads | Done | `40e6ac7`. **Split from row 11 because this half is the actually-open door and worth shipping alone.** Two defects that compose: (1) `ListMemberships` took the organization from the `{organizationID}` **URL path**, passed it to the store, compared it to nothing, and has **no authz check** — so `GET /v1/organizations/<any-org>/memberships` returned that org's full roster (principal ids, workspace/legal-entity ids, roles, effective dates). Fixed structurally: the store parameter is **removed**, not checked. (2) `GetCommercialAccount`/`GetMembership` wrote the tenant predicate as `($2 = '' OR organization_id::text = $2)` where `$2` is the context tenant — so a header-less caller **disabled its own tenant predicate**. The fail-open sibling of the fabricated-`default-tenant` bug: rather than inventing a tenant, it removes the boundary. **Writes were NOT exposed** — every mutating route authorizes against the resource's own organization and authorization-svc defaults to `DENIED`/`no_grant`. `requireTenant` is applied per-handler, not as blanket middleware, because the catalog/plan routes are platform-scope (doc7 §U1). Stub store took `_ context.Context` and ignored it — a fake that could not fail; now mirrors PgStore. **The test routers never installed `TenantContext()`**, so the tenant path was never exercised at all; `TestListMemberships` was reading org-test-02's roster while carrying org-test-01's identity, and passing. Three negative controls — including one that **passed**, recorded rather than glossed: with `requireTenant` in place the self-disabling predicate is unreachable over HTTP, so no handler test can prove that fix matters; row 11b's store-level test pins it. |
| 11b | commercial-account-svc — RLS + subscription-store scoping | Done | `7973761`. Migration 000005 over 9 tables in **three classes**, which is the substance: **direct** (`commercial_accounts`, `memberships` — plain equality); **platform scope** (`price_catalogs`, `plans`, `entitlement_limits` — no tenant column and must not get one, doc7 §U1: every tenant reads the same approved catalog, so RLS there would hide it from *everybody*; pinned by `TestRLS_PlatformTablesHaveNoPolicy`); **derived** (7 tables with no tenant column — 3 one hop via `commercial_account_id`, 4 two hops via `subscription_id`, using subquery policies that resolve through a chain of two policies). Plus all 15 previously-unscoped `subscription_store.go` methods, now routed through `withTenant`/`setTenantOnTx` (22 methods total across both files; the 6 catalog methods deliberately excluded). **I predicted the coupling wrong and the test caught it** — see row 11c. One documented exception at `withScope`: `CreateCommercialAccount`/`CreateMembership` are onboarding, where the target organization is legitimately *not* the caller's tenant (a new org's first account has no prior tenant context), so they scope from the validated request and their real control is the per-organization authz check, not `WITH CHECK` — stated plainly rather than left for a reader to reverse-engineer. Includes `TestRLS_TenantlessContext_SeesNothing`, the store-level test 11a provably could not write. Negative controls: migration removed → ENABLE/FORCE + derived-isolation fail; down migration round-trips 9 policies → 0 → 9. Verified on Postgres 16 as a `NOSUPERUSER NOBYPASSRLS` role — pointed here, since this service's own store comment had justified having no RLS on the superuser observation, drawn from the test DSN rather than production. |
| 11c | **Corrected finding — how the derived-table policies couple to their parent** | Done | Recorded as its own row because it contradicts the intuitive reading and I got it wrong first. The 7 derived tables carry no tenant column; their policies resolve through `commercial_accounts`. I predicted that **dropping** that parent policy would *widen* the children. **The opposite is true.** Postgres treats RLS-enabled-with-no-applicable-policy as **deny-all**, so dropping the parent policy empties the subquery and the children become *more* restrictive — even the owning organization loses its own rows. It is an outage, not a breach. The actual widening path is **`ALTER TABLE commercial_accounts DISABLE ROW LEVEL SECURITY`**, which opens all 7 derived tables at once. So: **one `ALTER TABLE` on one table is a seven-table breach, while dropping that same table's policy is merely an outage — and the two look equally innocuous in a diff.** `TestRLS_ParentPolicyCoupling` asserts both directions; migration 000005's header records the verified behaviour in place of the original guess. Generalizes to any service using subquery policies over FK-derived tables — worth checking before Priority 3. |
| 12 | connectivity-api-bridge-svc | Done | Same three defects as row 10, done with row 8b. Unscoped reads exposed another tenant's `endpoint_url`/`auth_type` (`GetBridgeByID`), and `ListIngestionLogs` exposed payload summaries + error messages — the contents flowing through their integration. `ListBridges`' only filter disabled itself. MemoryStore fixed too. RLS + negative control verified on real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role. |
| 13 | esignature-integration-svc | Done | **Worst variant found so far: an unscoped WRITE.** `UpdateEnvelopeStatus` was `WHERE envelope_id = $4` alone, so any caller holding another tenant's envelope_id could mark that tenant's document SIGNED/COMPLETED and set `external_ref`. Doc 03 §16.5 makes this the governed execution path for contracts, board resolutions and legal artifacts, so a forged transition is a legal-integrity issue. Reads also exposed `signer_email`/`signer_name` (personal data). Negative control established RLS alone is sufficient AND that with both controls removed the forged mutation really lands — my first attempt at that control was invalid (a `sed` left `$5` bound, so Postgres errored on parameter count and the test passed for the wrong reason); redone correctly. |
| 14 | evidence-manifest-svc | Done | `2a12cfa`. **The worst read exposure of the tier.** The service had NO tenant plumbing at all: `X-Tenant-Id` was never read, the tenant existed only as a POST body field (validated non-empty, otherwise trusted), and **both GET routes had no tenant input whatsoever** — a manifest id from the URL was the entire argument, with no authz anywhere in the service to fall back on. `GET /{id}/records` returns `manifest_records.record_snapshot`, a **verbatim JSON copy** of each source record (governance decisions, access decisions, workflow instances), snapshotted in full so a manifest survives a source outage. Doc 03 §14.4 makes these the bundles handed to an auditor/regulator/legal-discovery request — so a manifest id was one tenant's assembled evidence, in full. Not metadata about evidence: the evidence. Writes were as bad in the other direction: `FinalizeFailed` was an unscoped UPDATE, so any caller could mark another tenant's in-flight bundle FAILED — **terminal**, so a retry makes a new manifest rather than repairing it. Doc 03 §22 requires evidence to fail safe; failing somebody else's on demand is the inverse. Fixed: new `internal/middleware` (blanket refusal — no platform-scope routes here, but mounted on the API group only so `/healthz`/`/readyz` still answer), every read/write scoped via `withTenant`, body `tenant_id` may only agree with the header, migration 000003 (direct policy + subquery policy on the derived table). Stub store took `_ context.Context` and ignored it — a fake that could not fail; now mirrors PgStore. See rows 14a and 14b for two findings this surfaced. |
| 14a | **evidence-manifest-svc's aggregator sent no tenant header — manifest generation was NON-FUNCTIONAL** | Done | Fixed in `2a12cfa` alongside row 14, because the isolation fix is what made the correct value available. The aggregator set **no headers at all** on any downstream call. `governance-decision-log-svc` answers **400** `missing_tenant_id` without `X-Tenant-Id`; `workflow-svc` answers **401** `missing_tenant_scope` — the latter a direct consequence of our own **Priority 1 row 6 fix**, which correctly stopped its by-id read falling back to an unscoped lookup. `getByID` maps any non-200 to `ErrSourceUnavailable` and `collectRecords` fails closed on the first source error, so **the whole manifest failed for 2 of 3 sources** and reported it as "source unavailable". A missing header presenting as a downstream outage is the expensive part — nobody debugging that would find it by looking at the source service. Lesson worth generalizing: **when a Priority 1/2 fix makes a service require a tenant, every in-platform caller of that service becomes a candidate break.** Worth an explicit sweep of service-to-service clients for missing `X-Tenant-Id` forwarding before Priority 3. |
| 14b | evidence-manifest-svc has no authorization at all | Not Started | Found during row 14, deliberately left out of it. No `internal/authz` package, no `CheckAllowed` on any route — so after row 14 cross-tenant access is refused, but **within** a tenant every principal can read and generate every evidence manifest. RLS is a tenant boundary, not a permission model, and conflating the two would make the control look stronger than it is. Needs an authz client plus action constants (`EVIDENCE_MANIFEST_GENERATE`, `EVIDENCE_MANIFEST_READ`) and its own tests. Same class as row 84b (commercial-account-svc's ungated by-id GETs). |
| 15 | external-data-feed-svc | Done | `5b3bb97` (+ `3b14878` comment reword). Same three defects, done with row 8d. Worst leak was `ListEvents`: its only predicate was `feed_id`, self-disabling, so calling it with no `feed_id` returned up to 500 of **every** tenant's feed events **including the payload** — the actual market/reference data flowing through their subscriptions, not just metadata. MemoryStore fixed too. RLS + negative control verified on real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role. Follow-up commit only reworded two doc comments: gofmt's doc-comment formatter rewrites `''` into a curly quote, mangling the SQL snippet. |
| 16 | hris-connector-svc | Done | `3c97c3e`. Same three defects, done with row 8e. Unscoped reads exposed another tenant's `provider_name` + `api_endpoint` — the address of the system of record for their workforce data — and `GetSyncJobByID`'s `error_message`, which can carry provider detail. `ListSyncJobs`' only filter disabled itself, returning every tenant's sync history. MemoryStore fixed too. RLS + negative control verified on real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role. |
| 17 | kill-switch-registry-svc | Done | `56a2d0e`. **The one service where the OBVIOUS policy is a silent safety bypass, not a leak or an outage.** `tenant_id` is nullable and NULL means *platform-wide kill switch*; `ResolveKillSwitch` reaches it via `AND (tenant_id IS NULL OR tenant_id = $4::uuid)`. A policy written the natural way — plain tenant equality — hides every platform-wide switch from every tenant-scoped resolution, so `ResolveKillSwitch` answers **"not engaged" during an incident for an action class that has been globally stopped**. The caller proceeds to charge / run the automation / publish the claim. Nothing errors, nothing logs, and the operations view still shows it on. doc7 §32.1 calls this control "privileged, logged, approval-controlled". Hence the IS NULL branch in both USING and WITH CHECK, and `TestRLS_PlatformWideSwitchStaysVisibleToTenants` as an **over-restrictive** control: swapping in the plain-equality policy makes it and the operations-view test fail, so "hardening" this policy now yields a red build instead of an unenforced emergency stop. Same shape as row 8's global defaults, but there a wrong policy gave a wrong config value; here it gives an emergency stop that does not stop anything. Read fixes: `ResolveKillSwitch`/`ListHistoryForScope` took `tenant_id` from the **query string** unverified (any caller could read whether a given tenant's charging was stopped, plus the incident reason text — during an incident, a live feed of another customer's trouble), now bound to the verified header with disagreement refused; `ListCurrentStates` took no parameters and had no authz, returning **every** tenant's switch state as a cross-tenant incident map, now policy-bounded. All 5 store methods routed through `withTenant`. Two handler tests were passing `?tenant_id=` with no header and still getting that tenant's answer. Two negative controls in **opposite** directions (over-restrictive → safety tests fail; migration removed → isolation + WITH CHECK fail). |
| 17a | **Documented non-guarantee: RLS does not guard a platform-wide ENGAGE** | Done | Recorded as its own row so it is not mistaken for an oversight. Because WITH CHECK must keep the `tenant_id IS NULL` branch (see row 17), **RLS permits any caller to insert a platform-wide kill-switch event** — i.e. to engage the platform's emergency stop. That is a deliberate consequence, not a gap in the policy. The control on that path is the handler's per-scope authorization, which falls back to `platformScopeID` when the request names no tenant, so a platform-wide ENGAGE requires a `KILL_SWITCH_ENGAGE` grant at platform scope (authorization-svc defaults to DENIED / `no_grant`); migration 000001's append-only design means a forged event cannot erase the history showing it. An `app.platform_scope` GUC gating NULL-tenant inserts — the audit-event-store-svc pattern — was **considered and rejected**: it would put the platform-wide ENGAGE path behind a second store-level switch an incident responder cannot see, and during an incident a control nobody can find is worse than one guarded by a legible authz grant. Revisit only if platform-scope grants turn out to be widely held. |
| 18 | retention-registry-svc | Done | `b5cae56`. **Closes Priority 2.** Same nullable-tenant shape as row 17, but here a wrong policy is **irreversible**. This service answers "is it safe to delete/export/migrate this?" for every other service, and both halves reach platform-wide rules via `tenant_id IS NULL OR tenant_id = $n`. Tighten to plain equality — which looks exactly like hardening — and: a platform-wide **retention policy** becomes invisible, so Resolve reports no applicable policy and the caller concludes it may delete (doc7 §J2 forbids automatic destructive deletion of governed records); a platform-wide **legal hold** becomes invisible, so the deletion path proceeds on records under a legal preservation obligation (doc7 §J3). **Destroying records under hold is spoliation and cannot be undone by re-engaging anything** — unlike a missed kill switch. Hence **two** over-restrictive controls (one per table) plus a third asserting the IS NULL branch *adds* platform-wide rules rather than overriding a tenant's own more-specific one. Read fixes: `Resolve` took `tenant_id` from the query string with no authz (any caller could learn what legal holds applied to any tenant — i.e. that they are under litigation), `GetLegalHold` had no tenant input and no authz at all (exposing `scope_description`, `custodians_objects`, `authority`). All 7 store methods through `withTenant`. **Precise about what was NOT an open door:** `ReleaseLegalHold`'s handler fetches the hold first and authorizes against *that hold's* tenant, so cross-tenant release was already refused; the unscoped store UPDATE was defence-in-depth, and the negative control confirms RLS now carries it (migration removed → tenant B reads authority "court order 2026-CV-1234" **and** releases the hold). |
| 18a | **Documented non-guarantee: RLS does not guard creating a platform-wide retention rule or hold** | Done | Recorded like row 17a so it is not read as an oversight. WITH CHECK keeps the `tenant_id IS NULL` branch, so any caller may insert a platform-wide retention policy or legal hold. For **this** service that is the *safe* direction of the asymmetry — an unauthorized extra hold **blocks** a deletion, it never permits one — which is why no platform-scope GUC was introduced. The dangerous operation is **release**, and release is authz-gated per hold against that hold's own tenant. Creating a platform-wide rule still requires a platform-scope grant, since both create handlers authorize against the request's `tenant_id` and fall back to `platformScopeID` when absent. Contrast row 17a, where the equivalent NULL-insert engages a platform-wide kill switch and the same reasoning does **not** make it safe — there the guard is the authz grant alone. |
| 19 | source-authority-svc | **Not applicable** | False positive — its only "tenant_id" mention is a comment comparing its real column (`entity_ref`, free-text, no tenant dimension) to kill-switch-registry-svc's design. Genuinely platform-wide reference data; no fix needed. |
| 20 | tax-authority-interface-svc | Done | `f7da196`. Same three defects, done with row 8f — last of the six connectors. Most sensitive read in the tier: `GetSubmissionByID` filtered on `submission_id` alone, exposing another tenant's `tax_amount`, `tax_period`, `filing_type` and the authority's `ack_reference` — their actual filed tax figures. `ListSubmissions`' only predicate was `interface_id`, self-disabling, so omitting it returned every tenant's filings. MemoryStore fixed too. **Three** negative controls this time, on real Postgres 16 as a NOSUPERUSER NOBYPASSRLS role: migration removed → ENABLE/FORCE + WITH CHECK fail; Go predicate removed with migration present → isolation still holds (RLS is load-bearing alone); both removed → tenant B really reads tenant A's filing amount and ack_reference, so the test detects the leak rather than passing vacuously. |


## Priority 2b — Services with NO authorization on any route

Opened 2026-08-25, after Priorities 1/1b/2 closed the tenant boundary estate-wide.

**Why this is now the weakest link.** Tenant isolation and authorization answer different
questions. Isolation asks *"is this row in my tenant?"*; authorization asks *"is this principal
allowed to do this?"*. Priorities 1–2 made the first one solid. Where the second is absent, a
correctly-isolated tenant still lets **any** principal holding **any** valid envelope read or
mutate **anything within that tenant** — which is half a boundary, not a boundary.

Nothing here is a cross-tenant hole. Every service below is now tenant-scoped and verified.
These are intra-tenant privilege gaps.

**Measured, not guessed** — and the measurement needed two corrections, both worth recording
because the same traps keep recurring:

- A first pass grepped `CheckAllowed` across `internal/` and reported 10 services. It matched a
  **comment I had written myself** in evidence-manifest-svc explaining that the service has no
  authz client, excluding the very service the search was meant to find. Third time this class of
  false positive has appeared (after `default-tenant` in rows 4, 19, 8i).
- `workflow-svc` appeared as a gap but calls `h.authz.CheckApprovalAllowed(...)` — a
  differently-named method on its own client. Not a defect; a pattern that was too narrow.

### Legitimately authz-free — no fix, do not "fix" these

| Service | Why |
|---|---|
| `authorization-svc` | It *is* the authorization service. Its `/v1/admin/*` writes were hardened in Priority 1 row 5 (verified tenant + actor from `X-Principal-Id`); it cannot call itself. |
| `gateway-auth-svc` | The ForwardAuth verifier that produces the identity envelope every other service trusts. Sits upstream of authorization by design. |
| `audit-event-store-svc` | Zero HTTP routes — a Kafka consumer. Nothing to authorize. (Its missing query API is row 68a, a separate gap.) |
| `search-indexer-svc` | Background syncer, no HTTP API. (Its obligations sync is broken — row 65a.) |
| `workflow-svc` | False positive; uses `CheckApprovalAllowed`. |

### Real gaps — routes serving sensitive data with no authorization call

Ordered by payload severity, which is how Priority 2 taught us to order this rather than by tier.

| # | Service | Routes | What an unauthorized principal reaches |
|---|---|---|---|
| 87 | `evidence-manifest-svc` — **Done** `ba54163` | 6 | **Assembled evidence bundles** — `record_snapshot` holds verbatim governance decisions, access decisions and workflow instances, i.e. the artefact handed to an auditor or regulator. No `internal/authz` package at all. Worst payload in the estate. |
| 88 | `mtls-management-svc` — **Done** `d62a737` | 9 | Certificate lifecycle including **revoke** and rotate. Revoking a cert breaks the service-to-service auth depending on it. |
| 89 | `siem-integration-svc` — **Done** `9e54275` (3 of 5 routes; see notes) | 7 | Exporter `auth_token` (a live credential — see 8p-a) and the tenant's whole security event stream. |
| 90 | `key-management-svc` — **Done** `1d8623f` (5 of 5 routes) | 7 | Key metadata including **disable**, which is a denial of service on whatever the key protects. |
| 91 | `tenant-entity-registry-svc` | 28 | **Not applicable** — corrected. It DOES authorize: `internal/authz.HTTPAuthZClient.Authorize` makes a real `POST /v1/authorize`, reached through a shared helper in `internal/registry/service.go` that maps authz errors. My Priority 2b measurement grepped `CheckAllowed(` and missed it because this service names the method `Authorize`. Row 65b closes with it. |
| 92 | `schema-registry-svc` | 9 | **Not applicable** — corrected, 5th false positive from this measurement. It DOES authorize: `RegisterVersion` (its only write route, `POST /{eventName}/versions`) calls `h.authz.CheckSchemaPublishAllowed(...)`, a real HTTP client. My sweep grepped `CheckAllowed(` and this service names the method `CheckSchemaPublishAllowed`. Its four read routes are correctly open: event schema definitions are platform reference data every service must read to validate envelopes (Doc 04 §19). Same shape as jurisdiction-rules-svc — reads open, writes gated. |
| 93 | `carta-svc` — **Done** `57808c2` (2 of 3 routes; `/evaluate` guard would be circular — see 84d) | 5 | Access-decision telemetry: device trust, trusted IPs, allow/deny boundary, and `RiskFactors` naming why a score moved. |
| 94 | `workflow-history-svc` — **Done** `0f93455` | 2 | **Was worse than this row said.** Logged as "no authorization", but the real defect was a **cross-tenant read**: both routes took `tenant_id` from the **query string** and nothing in the service read `X-Tenant-Id` at all. The store feeds that into `set_config('app.tenant_id')` and the RLS policy reads it back — so the policy was **satisfied**, not bypassed, and Postgres returned whichever tenant the caller named in the URL. Strictly worse than the Priority 1b `default-tenant` cases: there a header-less caller landed in one shared bucket, here a caller picks its victim by editing a parameter. Exposed the workflow transition log — every state change, approval and payload for another tenant's governed work. Fixed both halves; also **had no handler tests at all**, which is part of how it survived. Two of the new tests assert on the tenant value the store *received*, since a 200 cannot distinguish "used the header" from "used the query param". |
| 95 | `identity-context-svc` | 7 | **Investigated, not yet fixed — the analysis is the deliverable here.** 7 routes in `internal/context/`, no authz package. `POST /v1/context/resolve` **must stay unguarded by a principal check**: it takes a token in the body and returns a JWT, returning 401 on `ErrTokenInvalid`/`ErrNoToken` — it IS the authentication endpoint, so requiring a verified principal is perfectly circular (you would need an envelope to get an envelope). That is a stronger case than carta `/evaluate` or siem `/stream`; belongs with row 84d. The other 6 need guards, most urgently `PUT /principals/{id}/status` (changes principal status — suspend/activate) and the `roles`/`delegations` reads. Also found: `InvalidateSession` reads `X-Actor-Principal-ID` and records it as the actor **without verifying or authorizing it**, so a caller can invalidate any session and attribute it to anyone — an attribution gap as well as an authorization one. No external HTTP callers, so guarding the 6 breaks nothing. |
| 96 | `jurisdiction-rules-svc` | 21 GET / 5 POST | **Not applicable** — corrected. Its 5 write routes each call a shared `checkAuthz` helper (5 call sites, matching 5 POSTs) which delegates to a real `Authorize`. The 21 GETs are platform reference data and correctly open. Missed for the same reason as row 91: the method is named `Authorize`, not `CheckAllowed`. |

### Notes for whoever picks this up

- Four of these (`mtls`, `siem`, `key-management`, `carta`) have **no Postgres at all** — in-memory
  stores. So there is no RLS dimension and no migration; the fix is entirely handler-side.
- `evidence-manifest-svc` needs an authz client built from scratch (no `internal/authz` package),
  action constants, and wiring through `main.go`. The other services with an existing client are
  cheaper.
- The pattern to copy is `commercial-account-svc`'s `h.authorize(w, r, principalID,
  organizationID, ACTION)` — note it passes the *resource's own* organization, resolved from the
  row, not a value the caller supplied. See row 84a for the identifier-namespace trap: that
  service passes `organization_id` on some routes and `commercial_account_id` on others into the
  same `legal_entity_id` parameter, and a grant written in one namespace can never match a check
  in the other.
- Adding an authz gate to a **read** path is a behaviour change with a real failure mode: if
  authorization-svc is unreachable the client fails closed (`ErrAuthzServiceUnavailable` → 503),
  so a read that works today starts failing during an authz outage. That is the correct posture
  for governed data, but it must be a deliberate decision per route, not a blanket sweep.
## Priority 3 — RLS enabled but not FORCEd (defense-in-depth only)

**Re-scoped 2026-08-25 after two empirical checks. This tier is genuinely low-value — read this before spending a day on 40 migrations.**

**Finding 1 — `WITH CHECK` is NOT missing, it is implicit.** 39 services write their policies as
`FOR ALL USING (...)` with no `WITH CHECK`. I hypothesised that left the write side ungoverned,
so the runtime role could INSERT rows attributed to other tenants. **Tested against real
Postgres 16 as a NOSUPERUSER NOBYPASSRLS role: wrong.** A foreign-tenant insert is refused with
`ERROR: new row violates row-level security policy (SQLSTATE 42501)`. Postgres reuses the
`USING` expression as the write check when `WITH CHECK` is omitted on a `FOR ALL` policy. The
own-tenant control insert succeeded, so that refusal is tenant-specific rather than a
refuse-everything policy. **Adding explicit `WITH CHECK` to those 39 services is cosmetic.**
Had I trusted the hypothesis I would have written 39 migrations and described them as closing a
write hole that never existed.

**Finding 2 — `FORCE` is close to a no-op under the current role setup.** `FORCE` makes RLS apply
to the table *owner*; it does **not** affect superusers, which bypass RLS unconditionally either
way. Verified from `deployments/init-db.sh`: the runtime role is `zoiko_app`, created
`NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS`, and 26 services get `DB_USER: zoiko_app` (6
more get `DATABASE_URL: postgres://zoiko_app` in the phase-6 compose). As a non-owner it is
already fully subject to RLS, so `FORCE` changes nothing for it. The tables are owned by whoever
ran the migrations — `postgres`, a superuser — for whom `FORCE` also changes nothing. So `FORCE`
only starts earning its keep if a **non-superuser owner** role ever connects. That is real
insurance against a future regression, but it is insurance, not a live gap.

**Recommendation: leave this tier where it is and do it opportunistically** — add `FORCE` when
touching a service for another reason, rather than as a 40-service sweep. The tier below was
correctly labelled "defense-in-depth only" and both checks confirm it.

Original note follows.

40 services (list below) have `ENABLE ROW LEVEL SECURITY` + a policy, but not `FORCE ROW LEVEL
SECURITY`. **Lower urgency than Priority 1/2** — the platform's `zoiko_app` runtime role is a
non-owner, so RLS already applies to normal traffic; FORCE only matters if something connects
as the table owner (a future regression, manual psql access, etc). Worth doing, but after 1–2.

| # | Service | Status |
|---|---|---|
| 21 | access-control-svc | Not Started |
| 22 | anomaly-detection-svc | Not Started |
| 23 | benefits-svc | Not Started |
| 24 | clause-template-svc | Not Started |
| 25 | compensation-svc | Not Started |
| 26 | compliance-risk-scoring-svc | Not Started |
| 27 | compliance-status-svc | Not Started |
| 28 | consolidation-svc | Not Started |
| 29 | contract-lifecycle-svc | Not Started |
| 30 | corporate-actions-svc | Not Started |
| 31 | corporate-tax-svc | Not Started |
| 32 | counterparty-management-svc | Not Started |
| 33 | decision-support-svc | Not Started |
| 34 | employee-master-svc | Not Started |
| 35 | employment-contracts-svc | Not Started |
| 36 | exception-escalation-svc | Not Started |
| 37 | filing-preparation-svc | Not Started |
| 38 | filing-tracker-svc | Not Started |
| 39 | forecasting-svc | Not Started |
| 40 | intercompany-accounting-svc | Not Started |
| 41 | invoice-approval-svc | Not Started |
| 42 | leave-absence-svc | Not Started |
| 43 | migration-integrity-svc | Not Started |
| 44 | obligation-tracking-svc | Not Started |
| 45 | offboarding-severance-svc | Not Started |
| 46 | org-structure-svc | Not Started |
| 47 | payroll-exceptions-svc | Not Started |
| 48 | payroll-run-svc | Not Started |
| 49 | payroll-tax-svc | Not Started |
| 50 | performance-review-svc | Not Started |
| 51 | procurement-workflow-svc | Not Started |
| 52 | reconciliation-intelligence-svc | Not Started |
| 53 | reporting-orchestration-svc | Not Started |
| 54 | tax-determination-svc | Not Started |
| 55 | tax-rules-svc | Not Started |
| 56 | tenant-entity-registry-svc | Not Started |
| 57 | treasury-svc | Not Started |
| 58 | vat-gst-svc | Not Started |
| 59 | withholding-tax-svc | Not Started |
| 60 | workflow-history-svc | Not Started |
| 61 | workforce-compliance-svc | Not Started |

## Priority 4 — Structural / cross-cutting

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 62 | Shared Go module enforcing the event envelope shape — today every service hand-copies its own struct; nothing stops the next new service from drifting | Doc 03 §19 | Not Started | |
| 63 | Transactional outbox rollout beyond the 1-service pilot (`commercial-account-svc`) — every other service can still silently drop an event on a crash between DB commit and Kafka publish | Doc 03 §17.3, Doc 07 §L1–L2 | Not Started | Verified: `grep -rl "internal/outbox" services/*/internal` returns only commercial-account-svc |
| 64 | General-purpose saga / compensating-transaction coordinator — only one flow (procurement-workflow-svc) got a one-off fix; no reusable pattern for the next multi-service flow | Doc 03 §17.8 | Not Started | |
| 65 | **`X-Tenant-Id` forwarding sweep across all service-to-service clients — DONE, and the result is that this is NOT a widespread class** | Doc 04 §2.2 | Done (audit) | Run because `evidence-manifest-svc` (row 14) was silently broken by a *correct* Priority 1 fix: we made `workflow-svc`/`governance-decision-log-svc` require a verified tenant, and its aggregator called them with no headers, surfacing as `ErrSourceUnavailable` — i.e. pointing at the wrong service. I expected that to be one of many. **It is not.** Of 194 files building outbound requests, 43 forward a tenant; of the rest, ~145 only call `/v1/authorize` (tenant travels in the request *body*, correct) or `/v1/mtls/certificates` (no tenant dimension, correct). That left 7 candidates, 6 of which are correct as-is: 3× jurisdiction-rules-svc (platform reference data — already established N/A, rows 19/8-adjacent), `access-control-svc` → `/v1/admin/roles` (role administration is platform scope), `document-vault-svc` → `/v1/authorize` (my grep matched a doc comment, not a call), and `document-vault-svc` → tenant-entity-registry residency (works — that service requires no tenant header, and a tenant *resolution* lookup cannot require the answer as input). **One real defect: row 65a.** Worth recording the negative result explicitly so nobody re-runs this sweep expecting a rich seam. |
| 65a | `search-indexer-svc`'s obligations sync is entirely broken — and the documented fix is a redesign, not a header | Doc 03 §37, Doc 04 §9.8/§556/§620 | Not Started | `fetchObligations` calls `GET /v1/obligations` on obligations-svc with **no headers at all**. That endpoint requires *both* `X-Principal-Id` and `X-Tenant-Id` (401 `tenant_missing`), so **every sync cycle fails and the obligations search index is never populated**. The tempting fix — forward a tenant — does not work: this syncer is *deliberately* cross-tenant (it polls all obligations, resolves each one's `tenant_id` via tenant-entity-registry-svc, and stamps it per record), and you cannot express "all tenants" in one tenant header. The other two options both mint new privileged cross-tenant surface: a platform-scope list endpoint on obligations-svc, or a `GET /v1/tenants` enumeration on tenant-entity-registry-svc (**which does not exist today** — only `/v1/tenants/{tenantID}`), so "just iterate tenants" is not implementable either. **The docs already answer this**: Doc 03 §37 and Doc 04 §556/§620 place search indexes as event-driven *derivative projections* ("Search is derivative, never authoritative", §9.8) — a consumer of domain events, which already carry `tenant_id` in the envelope and therefore needs no cross-tenant read privilege at all. HTTP polling is contrary to the documented architecture, which is *why* it hit the tenant wall. Deliberately not implemented unilaterally: it is a redesign of a service outside the isolation tiers, and inventing a privileged endpoint would undo two tiers of work. Needs a team decision. |
| 65b | `tenant-entity-registry-svc` reads have no authz gate (same class as 84b) | — | Not Started | Found during the row 65 sweep, and logged with a correction to my own first reading. `GET /v1/tenants/{tenantID}` and `/v1/tenants/{tenantID}/entities` take the tenant from the URL path with **no** `X-Tenant-Id` check (the service has zero references to that header). I initially read this as Priority 1c caller-declared identity; **it is not.** Its store filters explicitly by `tenant_id` in every WHERE clause, documents the superuser/RLS posture itself, and was audited method-by-method with integration tests that caught real leaks. For a *registry*, the `{tenantID}` path segment names the resource rather than asserting who the caller is — and a tenant-resolution endpoint cannot require the tenant as input without circularity. The actual gap is the same as 84b: no authorization check on the read paths, so within the platform any caller can read any tenant's registry record, entity structure and residency region by id. Whether that is exposed depends on gateway route restrictions, which is the thing to check first. |

## Priority 5 — Governance plane completeness

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 65 | No single enforced governance pipeline sequence — each service calls whichever governance engines it individually decided on | Doc 01 §07, Doc 03 | Not Started | |
| 66 | jurisdiction-rules-svc has no compliance calendar entity; `jurisdiction.calendar.changed` is declared but unemittable | Doc 03 §8.2 | Not Started | |
| 67 | authorization-svc has no platform-scoped, non-entity resource concept — services fake a synthetic `legal_entity_id` as a workaround | Doc 03 (spec silence, not a violation) | Not Started | |
| 68 | A DENIED governance decision doesn't auto-convert to an approval workflow except where explicitly wired per-service | Doc 02 Diagram 2 | Not Started | |
| 68a | **audit-event-store-svc has no query API at all.** Doc 03 §14.1 requires its records to be "immutable and **queryable** by actor, entity, action, workflow, or time range". The service has exactly one store method (`Store`) and only `/healthz` + `/readyz` routes — evidence goes in and cannot be got out. Found while doing row 7. The RLS policy added there is deliberately shaped so this API inherits tenant scoping by default when built. | Doc 03 §14.1 | Not Started | Real missing feature, not just missing RLS. Note the evidence is genuinely durable and hash-chained — it is only unreadable, which makes this a retrieval gap rather than a data-loss one. |

## Priority 6 — Data model (Doc 04)

| # | Item | Status | Notes |
|---|---|---|---|
| 69 | Missing entity: `UltimateBeneficialOwner` (no table anywhere) | Not Started | Verified: zero grep hits |
| 70 | Missing entity: `FiscalCalendar` (dangling FK column exists, no table) | Not Started | Verified: zero grep hits |
| 71 | Missing entity: `TaxLogicSnapshot` (dangling FK in 2 services) | Not Started | Verified: zero grep hits |
| 72 | Missing entity: `GrossToNetCalculationLog` | Not Started | Verified: zero grep hits |
| 73 | Missing entity: `NexusRecord` | Not Started | Verified: zero grep hits |
| 74 | Missing entity: chart-of-accounts (`Account`) in general-ledger-svc | Not Started | general-ledger-svc's own migration comment admits this |
| 75 | Missing entity: `SchemaDependencyMap` (+ `compatibility_mode`) | Not Started | Verified: zero grep hits |
| 76 | Missing entity: standalone `VendorProfile` (only scattered FK-shaped columns) | Not Started | Verified: zero grep hits |
| 77 | Document Vault missing `virus_scan_status` and `digital_signature_id` (Doc 04 §15.5 requires both) | Not Started | Verified: zero grep hits in document-vault-svc migrations |
| 78 | Obligation tracking duplicated across 3 services with non-identical schemas — violates §2.1 single-owner doctrine | Doc 04 §2.1 | Not Started | Verified: obligations-svc, obligation-tracking-svc, and filing-tracker-svc each have their OWN separate `obligations`/`filing_requirements` table |
| 79 | Identity/role assignment duplicated across authorization-svc and identity-context-svc | Not Started | |
| 80 | No field-level encryption/classification tagging on tax ID / bank reference / payroll columns anywhere outside document-vault-svc | Doc 04 §2.8, §20 | Not Started | |
| 81 | authorization-svc owns its own `delegated_authorities` table, duplicating delegated-authority-svc's ownership of the same concept (Doc 03 §9.3 names Delegated Authority Service as the authoritative owner — a separate service) | Doc 04 §2.1 | Not Started | Found while fixing row 5's auth gap (2026-08-21). Not addressed there — this is a cross-service consolidation decision, same class as item 78, not a quick fix |
| 82 | authorization-svc's `permission_bundles`, `principal_role_assignments`, `delegated_authorities`, and `access_decision_log` carry no `tenant_id` column at all — only `legal_entity_id` (and `delegated_authorities` not always that). RLS was only possible on `roles`/`sod_rules`, the 2 tables that actually have the column | Doc 04 §2.2 | Not Started | Found during row 5. Fabricating a `tenant_id` column on tables that were never given one is a data-model change, not an RLS migration — deliberately not done in that row |

## Priority 7 — Security (Doc 05) — capability exists, incomplete

| # | Item | Status | Notes |
|---|---|---|---|
| 81 | secret-vault-integration-svc's broker never returns real secret material — no service can bootstrap a runtime credential through it | Blocked | Needs a vault-side API design decision, not just more wiring — see `known-gaps.md` |
| 82 | key-management-svc is metadata CRUD only — never actually used to encrypt/decrypt anything | Not Started | |
| 83 | No confidential computing / TEE anywhere (spec calls for it on payroll/tax calculation) | Not Started | |
| 84 | No PAM / break-glass / just-in-time elevation anywhere | Not Started | |
| 84a | commercial-account-svc passes two different identifier namespaces into authorization-svc's single `legal_entity_id` scope parameter | Not Started | **Found while doing row 11a; NOT a tenant-isolation defect, logged separately so it is not lost.** The account/membership handlers pass `organization_id` as the authz scope; the subscription handlers pass `commercial_account_id` (`h.authorize(w, r, principalID, sub.CommercialAccountID, …)`). authorization-svc records grants against whatever string it is given and defaults to `DENIED` with basis `no_grant`, so a grant written in one namespace can never match a check in the other. That means the subscription routes are **fail-closed**, not fail-open — but it also means they only work if grants are seeded against commercial-account ids, which would be surprising. Needs a decision: either normalize the subscription handlers to resolve `commercial_account_id → organization_id` before authorizing, or document the account-id scope as intentional. Worth checking whether other services do the same thing — this is the kind of mismatch that looks fine in a passing test suite where the authz double grants everything. |
| 84b | commercial-account-svc's `GET /v1/commercial-accounts/{id}` and `GET /v1/memberships/{id}` have no authz check at all | Not Started | Also found during row 11a. Row 11a closed the tenant hole on both (they are now tenant-scoped and require a verified header), so cross-tenant reads are refused — but within a tenant, any principal with any grant can read them, because neither route calls `h.authorize`. Every mutating route in the service does. Deliberately left out of 11a rather than silently widening that commit's scope: adding an authz gate to a read path is a behaviour change that needs its own action constant and its own test. |
| 84c | An always-grant `Authorize` stub is defined in **7 services** | Not Started | Found while copying an authz client for row 87. Seven services' `internal/authz/client.go` define `func (c *Client) Authorize(ctx, tenantID, action, resource) (bool, error) { return true, nil }` — an authorization function that unconditionally grants. **It is dead code today**: nothing calls it. The two services that do call `.Authorize` (jurisdiction-rules-svc, tenant-entity-registry-svc) use a different 5-argument method on their own client, and those are real HTTP calls. So this is not a live bypass — it is one wiring mistake away from being a silent always-allow, in a file named `authz/client.go` where a reader would reasonably assume the opposite. Dropped from evidence-manifest-svc's copy rather than propagated. Fix is to delete it from all 7; the only reason not to is if something outside `services/` calls it, which should be checked first. |
| 84d | `siem-integration-svc`: 2 of 5 routes cannot use a user-principal guard — they need SERVICE identity (mTLS) | Not Started | Found while doing row 89, and the reason that row is "3 of 5". `POST /v1/siem/stream` and `GET /v1/siem/exporters` are called by **five** services (authorization-svc, gateway-auth-svc, identity-context-svc, key-management-svc, mtls-management-svc) whose `internal/siem` clients send `X-Tenant-ID` and nothing else. Adding `requirePrincipal` returns 401 to all five and **silently** stops security-event streaming — the clients log a warning and continue. It is also category-wrong: gateway-auth-svc streaming an authentication-FAILURE event has no authenticated principal by definition. **Residual exposure, named not dismissed:** a caller with a tenant header can inject fabricated security events (this is how a trail gets buried) and enumerate exporter names + endpoint URLs. The credential itself is no longer exposed (row 8p-a). Correct fix is mTLS service identity on those two routes, which `mtls-management-svc` already issues. `TestServiceToServiceRoutesStillWork` pins the current behaviour so a future "consistency" change fails loudly rather than breaking the pipeline. |
| 8p-a | siem-integration-svc returns a SIEM exporter's `auth_token` in full on read | **Partially done** `9e54275` — the leak is closed (`AuthToken` is now `json:"-"`, so no route can serialise it). TWO parts remain: (1) the token is still stored in **plaintext** — Doc 05 §13 wants a vault reference, blocked on row 81; (2) **nothing ever reads it** — `StreamEvent` persists events locally and never authenticates to the SIEM platform, so the service holds a live third-party credential with no functional use. Until egress exists, the safest version would not accept the token at all. | Found while fixing row 8p. `GetExporterByID` and `ListExporters` serialise `auth_token` as stored — it is `json:"auth_token,omitempty"` with no redaction anywhere in the handler. Row 8p closed the tenant hole, so it is no longer another tenant's credential; within a tenant it is still a stored secret handed back to any caller who can read the exporter, and this service has no authz on any route either (same class as 14b/84b). Not fixed in 8p deliberately: redacting a response field is an API-shape change that needs its own decision, and doing it silently inside an isolation commit would hide it. Options are to omit the field on read, return a masked prefix, or move the token to secret-vault-integration-svc and store a reference — the last is what Doc 05 §13 implies. |

## Priority 8 — Testing

| # | Item | Status | Notes |
|---|---|---|---|
| 85 | Most services' store layers are tested only against stubs, not real Postgres — only jurisdiction-rules-svc, identity-context-svc, tenant-entity-registry-svc, vendor-due-diligence-svc have real integration coverage | Not Started | |
| 86 | No contract tests, load/performance tests, or DR/restore tests anywhere | Not Started | |

---

## Explicitly NOT on this tracker (not backend-engineering work)

- Numeric SLOs, ZoikoSuite's own merchant/tax/processor billing setup, Doc 07 §27 sign-off by
  named function owners, per-service safe-degraded-mode definitions, source-authority-svc's
  real precedence data for actual connected systems — all blocked on a human/business decision,
  not code (see `full-architecture-gap-analysis.md`).
- CI security scanning, IaC coverage, staging/QA environments — devops, not backend service code.
- Frontend console wiring (identity-context-svc / workflow-svc have no console page) — a
  different teammate's lane per current team split.

## Change log

- 2026-08-21 — Tracker created. Priorities 1–3 (RLS) counts verified fresh against all 153
  migration files in `services/*/deployments/migrations/`, not copied from prior docs. Priority
  4 item 63's "1 service only" outbox claim re-verified by grep. Priority 6 items 69–77's "zero
  hits" re-verified by grep — all still genuinely absent as of this date.
