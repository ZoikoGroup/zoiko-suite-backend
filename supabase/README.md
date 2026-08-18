# Zoiko Suite on Supabase

Migration of the 20 finished services from 63 per-service Postgres databases to
one Supabase database, **one service at a time**.

Apply `migrations/` in filename order. `./verify.sh` applies the whole set to a
throwaway container and reports the RLS posture — run it after each service.

## The three decisions everything else follows from

**One schema per service.** The compose estate has 63 databases; Supabase is one.
Schema-per-service preserves the ownership boundary and keeps the five colliding
table names apart — `obligations`, `filing_requirements`, `access_decision_log`,
`delegated_authorities` and `principal_role_assignments` are each defined by two
different services and would overwrite each other in a flat `public`.

**Services connect as `zoiko_backend`, never as `postgres` or the service-role key.**
This is the whole reason the move is worth doing. On compose, every service
connects as the Postgres superuser, and a superuser bypasses RLS
unconditionally — so the estate's 119 policies are present, correctly written,
and have never once executed. `zoiko_backend` is `NOSUPERUSER NOBYPASSRLS`, so
with `FORCE ROW LEVEL SECURITY` the policies finally run.

> The same trap exists on Supabase wearing different clothes: **the
> `service_role` key has `BYPASSRLS`**. Anything connecting with it is exactly as
> unprotected as the old superuser. `verify.sh` creates that role deliberately so
> the exemption is visible rather than assumed.

**Every tenant-scoped table gets `ENABLE` *and* `FORCE`, and every policy carries
`WITH CHECK` as well as `USING`.** `USING` governs what is visible; `WITH CHECK`
governs what may be written. With only `USING`, a caller can insert a row into
another tenant that it then cannot see — the write-side gap still open in
obligations-svc.

Policies fail closed on a missing tenant by SQL semantics, not by a guard:
`tenant_id = app.current_tenant_id()` is `NULL` when no tenant is installed, and
`NULL` is not `true`, so the connection sees nothing. **Never** rewrite it as
`app.current_tenant_id() IS NULL OR tenant_id = ...` — that is a filter which
switches itself off exactly when identity is absent, which is the document-vault
defect.

## Identity

`app.current_tenant_id()` and `app.current_principal_id()` resolve from the
PostgREST JWT claims first, then fall back to `set_config('app.tenant_id', ...)`
— which is what the Go services already do in `withRLS` / `withTenantTx`. That
fallback is a deliberate bridge: a service can be pointed at Supabase **without
rewriting its store layer** and still be constrained by the policies. Drop it
once every service authenticates with a real JWT.

`created_by_principal_id` and friends default to `app.current_principal_id()`,
so attribution comes from the verified caller rather than the request body. It is
fail-closed — with no principal on the connection the default is `NULL` and the
`NOT NULL` rejects the write.

## Progress — 20 of 20 complete

All applied and verified against a clean Postgres 16 container. **42 tables, 21
schemas** (`app` plus one per service).

| # | service | schema | tables |
|---|---|---|---|
| 1 | jurisdiction-rules-svc | `jurisdiction_rules` | 3 |
| 2 | delegated-authority-svc | `delegated_authority` | 1 |
| 3 | accounts-payable-svc | `accounts_payable` | 1 |
| 4 | purchase-request-svc | `purchase_request` | 1 |
| 5 | bank-reconciliation-svc | `bank_reconciliation` | 1 |
| 6 | notification-svc | `notification` | 1 |
| 7 | schema-registry-svc | `schema_registry` | 1 |
| 8 | governance-decision-log-svc | `governance_decision_log` | 2 |
| 9 | configuration-feature-flag-svc | `configuration_feature_flag` | 2 |
| 10 | purchase-order-svc | `purchase_order` | 2 |
| 11 | spend-controls-svc | `spend_controls` | 2 |
| 12 | vendor-due-diligence-svc | `vendor_due_diligence` | 2 |
| 13 | evidence-requirements-svc | `evidence_requirements` | 2 |
| 14 | general-ledger-svc | `general_ledger` | 2 |
| 15 | financial-close-svc | `financial_close` | 2 |
| 16 | board-resolutions-svc | `board_resolutions` | 2 |
| 17 | obligations-svc | `obligations` | 3 |
| 18 | document-vault-svc | `document_vault` | 3 |
| 19 | secret-vault-integration-svc | `secret_vault_integration` | 4 |
| 20 | policy-svc | `policy` | 5 |

## To apply

`./build-combined.sh` writes **`zoiko-suite-all.sql`** — every migration in
order, ready to paste into the Supabase SQL editor in one go. Regenerate it
rather than hand-editing; the per-service files are the source of truth.

Three things to do before any service connects:

1. **Rotate the `zoiko_backend` password.** The foundation creates the role with
   a placeholder.
2. **Point the Go services at `zoiko_backend`** — not at `postgres`, not at the
   `service_role` key. Both bypass RLS, which reproduces the exact gap this
   migration exists to close.
3. **Do not expose `secret_vault_integration` through PostgREST.** It grants the
   `authenticated` role nothing on purpose: a secret *path* is a map of the
   platform's credentials, even though no secret value is stored.

## What the migration fixes on the way over

- **RLS becomes load-bearing.** All 42 tables are `ENABLE` *and* `FORCE`, and
  `zoiko_backend` is `NOSUPERUSER NOBYPASSRLS`, so the policies execute. On
  compose they never have.
- **`applicability_decisions` gets a tenant dimension** — the one open gap in
  `known-gaps.md` this closes rather than carries. It arrived from `origin/main`
  with no `tenant_id`, uncovered by RLS, and its queries never joined back to
  the parent obligation, so another tenant's `obligation_id` returned that
  tenant's decisions including `facts_used` and who decided.
- **Cross-tenant children are unrepresentable.** Composite foreign keys tie
  `journal_lines`, `purchase_order_amendments`, `filing_requirements`,
  `vendor_dd_evidence`, `close_evidences` and `spend_consumptions` to a parent
  *in the same tenant*. Several previously carried their own `tenant_id` with
  nothing making it agree with the parent's.
- **Cross-currency spend checks are unrepresentable.** A `spend_consumption` may
  only reference a policy in its own currency — comparing 5,000 JPY against a
  5,000 GBP threshold is a coincidence of digits, not a threshold check.
- **Append-only is enforced, not asserted.** Withheld `UPDATE`/`DELETE` grants
  on every evidence table, plus `app.reject_mutation()` triggers on the ones
  where mutation destroys evidence. The trigger is the only control that binds a
  `BYPASSRLS` role — verified: `UPDATE` as the superuser still fails.
- **`NOT VALID` constraints become `VALID`.** They were added `NOT VALID` to
  avoid rewriting real history to make a rule pass; an empty database has no
  history to protect, so the "run `VALIDATE CONSTRAINT` later" items close.
- **Attribution defaults to the verified principal** via
  `app.current_principal_id()`, fail-closed against the `NOT NULL`.

## Still open — these live in the service code, not the schema

- No service reconciles a body-supplied `legal_entity_id` against the caller's
  tenant. Nothing anywhere calls tenant-entity-registry-svc.
- `governance-decision-log-svc` takes `tenant_id`, `actor_id` and `decided_at`
  from the request body. The column defaults are backstops for other writers;
  they cannot fire while the handler passes explicit values.
- `spend-controls`, `vendor-due-diligence`, `notification` and
  `delegated-authority` are absent from the CI matrix entirely.
- notification-svc still delivers through a stub adapter — `SENT` means accepted
  by the stub, not delivered.
- document-vault's register LIST route records nothing in the access log, and
  `retention_policy` is a label no engine enforces.
