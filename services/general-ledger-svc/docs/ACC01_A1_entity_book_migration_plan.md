# ACC-01 A1 — adding legal-entity/book to account identity

Status: **plan only, no code changes**. Written in response to SPEC_DEVIATIONS
A1: this service keys accounts by `tenant_id + account_code` only; the spec's
purpose line ("for each legal entity and accounting book"), required inputs
(`entity_id; book_id`) and §24 data model all make entity/book part of an
account's identity.

## 1. Why this isn't a single-file fix

Grepping every service's own migrations for `account_code` finds it in at
least these 8 services' own schemas, each storing it as a plain
`VARCHAR`, not a foreign key general-ledger-svc enforces:

| Service | Tables carrying `*account_code` |
|---|---|
| financial-close-svc | `accrual_schedules`, `prepayment_schedules`, `allocation_rules`/`allocation_runs`, `fx_revaluation_runs`, `migration_batches`, `subledger_control_runs` |
| consolidation-svc | `balance_snapshots`, consolidation eliminations |
| inventory-management-svc | valuation/landed-cost mapping tables |
| asset-management-svc | `depreciation_schedules`, `depreciation_runs`, asset event tables |
| financial-statements-svc | statement fact source-account tables |
| financial-control-svc | group match tables |
| project-accounting-svc | revenue recognition tables |
| bank-reconciliation-svc | `statement_lines.gl_cash_account_code` |

None of these are a foreign key into `chart_of_accounts` today (cross-service
FKs aren't possible across separate databases in this platform's deployment
model) — each service trusts the account code a human or an upstream event
gave it. Changing what identifies an account in general-ledger-svc changes
what every one of these columns means, even though none of them live in this
service's own database.

## 2. The one finding that makes this tractable

Every service checked already stores `legal_entity_id` as its own column,
directly alongside its `account_code` column(s) — confirmed in
financial-close-svc (`accrual_schedules.legal_entity_id`), asset-management-svc
(`depreciation_schedules.legal_entity_id`), and consolidation-svc
(`balance_snapshots.legal_entity_id`). This isn't a coincidence: every one of
these services already has to scope its own business logic by legal entity
for reasons that have nothing to do with the chart of accounts.

This means **A1 does not require adding a column to 8 other services'
databases.** The entity the account belongs to is already sitting next to
every account_code reference that matters; it's general-ledger-svc's own
account identity, and its own command/query surface, that doesn't use it yet.
bank-reconciliation-svc's `statement_lines` needs checking individually before
relying on this (not confirmed in the single migration grepped) — call this
out explicitly during implementation rather than assuming it holds universally.

## 3. What actually has to change, and where

### 3.1 general-ledger-svc (this service) — the real work

- **Schema:** add `legal_entity_id` (and optionally `book_id`) to
  `chart_of_accounts` and `account_versions`, **nullable**, not a hard
  NOT NULL migration. A NULL `legal_entity_id` means "this account is
  tenant-global" — the exact behavior every existing account already has
  today. No backfill decision is forced on day one.
- **Uniqueness:** the account-code uniqueness constraint changes from
  `UNIQUE(tenant_id, account_code)` to
  `UNIQUE(tenant_id, COALESCE(legal_entity_id, ''), account_code)` (a partial
  or expression index), so a tenant-global code and the same code scoped to
  one entity can coexist without ambiguity, and two different entities can
  hold genuinely different versions of the same code — the concrete
  capability A1 is missing today.
- **Commands:** `CreateAccount`, `AmendAccountVersion`, `ReparentAccount`,
  `ActivateAccount`, `DeactivateAccount` all gain an optional
  `legal_entity_id` input. Omitted = tenant-global, matching every existing
  caller exactly.
- **Queries:** `GetAccountByCode`/`GetAccountAsOf`/`ResolvePostingEligibility`
  need a resolution order when a caller supplies `legal_entity_id`: an
  entity-scoped version of the code, falling back to the tenant-global one if
  no entity-scoped version exists. This fallback is what keeps every
  caller that never adopts the entity dimension working unchanged.
- **ACC-02 mappings / ACC-04 posting:** `account_mappings` already has
  `legal_entity_id`/`book_id` columns (migration history shows scoped
  mappings exist) — confirm whether posting-eligibility resolution already
  threads a caller's `legal_entity_id` through to account resolution, or
  whether that's a second piece of this same gap.

### 3.2 Downstream services — much smaller than feared

For each of the 8 services above: no schema change. The only change is
**which calls into general-ledger-svc pass `legal_entity_id` down to account
resolution** (e.g. `checkAccountRestrictions`-equivalent lookups, or any
direct `GetAccountByCode` call) — and since every one of them already has
`legal_entity_id` sitting in the same row as the account code, this is a
"thread an existing value one hop further," not "find or capture a new
value." bank-reconciliation-svc is the one to verify, not assume, first.

## 4. Rollout order

1. general-ledger-svc: additive migration (nullable columns, relaxed unique
   index), commands/queries updated to accept but not require the entity
   dimension. Ship alone; zero behavior change for any existing caller.
2. Pick one downstream service (financial-close-svc, since it has the
   largest and most varied set of account_code columns) as the pilot: thread
   its already-available `legal_entity_id` through to general-ledger-svc on
   its account-resolution calls, verify entity-scoped accounts resolve
   correctly, non-scoped ones keep working.
3. Roll the same change to the remaining 7 services one at a time, verifying
   each independently — this is a sequence of small, independently
   shippable changes once step 1 lands, not one big-bang migration.
4. Decide, per tenant, whether any existing tenant-global accounts should be
   split into entity-scoped versions — a data/business decision belonging to
   whoever owns each tenant's chart of accounts, not an engineering default
   this plan should set.

## 5. What this plan deliberately does not decide

- Whether `book_id` is in scope for this pass or a second one — the spec
  names both, but entity is the dimension every downstream service already
  carries; book is not confirmed the same way and needs its own check before
  being bundled in.
- Whether any existing account should be migrated to an entity-scoped version
  — step 4 above is a per-tenant business decision, not something to default.
- Exact column types/index DDL — sketched above to show the shape is
  additive and backward-compatible; the literal migration file is the next
  step once this plan is approved, not part of it.
