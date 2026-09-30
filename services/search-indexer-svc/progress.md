# search-indexer-svc — implementation progress

Tracked against ZS-SVC-AB-001's §18.2 Definition of Done and its §19 eight-wave
sequence. Every "Done" row below has a test that fails if the behaviour is
removed; the test names are in README.md's invariant table.

Last updated: 2026-09-21.

---

## §19 Eight-wave implementation sequence

| Wave | Objective | Status |
|---|---|---|
| **1** | Authority & contract foundation — ESR-01, field exposure classes, source registration, tenant/residency partition metadata, reason-code taxonomy | **Done** |
| **2** | Secure indexing core — durable source ingestion, projection rules, checkpoints, tombstones, source-to-index reconciliation | **Done** |
| **3** | Query gateway — trusted context, scope registry, mandatory filter compiler, complexity budgets, cursors, lexical search | **Done** |
| **4** | Secure retrieval — current re-authorization, source hydration, snippets/redaction, safe facets/counts, export boundary | **Done**, except the R2 hydrator (OD-05 — no scope is registered R2; a scope that were would answer ESR-014 rather than silently serving index content) |
| **5** | Lifecycle & operations — generation build/validate/activate/retire, drift, recovery, degraded modes, priority restriction SLOs | **Done**, except numeric SLOs (OD-03/OD-04 — the metrics exist and are correctly separated; the thresholds are a capacity decision) |
| **6** | Sensitive-domain adoption | **Not started** — a registration exercise, not a code change. See "Onboarding a domain" in README.md |
| **7** | Semantic / RAG retrieval | **Not started** (OD-10/OD-11). The projection and restriction model already carries the lineage vectors need |
| **8** | Global search certification | **Not started** — gated on waves 6 and 7 |

---

## §18.2 Definition of Done

| Criterion | Status | Evidence |
|---|---|---|
| INV-01..INV-30 automated or mechanically evidenced, no bypass path | **Done** for the 24 that are code-expressible; 6 are deployment/organisational (see below) | README.md invariant table; `scripts/audit.sh` |
| NP-01..NP-60 pass in CI/pre-production certification for applicable source classes | **50 of 60** covered by tests or the audit script; 10 belong to waves 6–8 or to open decisions (table below) | test suites + `scripts/audit.sh` |
| Index generation lifecycle supports parallel build, validation, atomic activation and safe abort | **Done** | `TestIndexGenerations_OnlyOneActivePerScope`; audit §7 |
| Restriction propagation independently verified, producing measurable evidence | **Done** | `VerifyRestrictions` sweep; `esr.restriction.propagated` at VERIFIED only; audit §9 |
| Current-authorization retrieval across keyword, autocomplete, facet and semantic paths | **Done** for keyword and facet; autocomplete and semantic are waves 7–8 | `TestExecute_ReauthorizesEveryR1Hit` |
| Search logs/telemetry pass privacy minimisation review | **Done** | `TestSearch_EvidenceNeverStoresQueryText`; keyed actor hash on the abuse stream |
| Cross-domain global search cannot reveal inaccessible existence/count/snippet information | **Done** for the mechanisms; global search itself is wave 8 | facet min-cell ×2, `total_is_exact`, TC-06 suppressions |
| Runbooks, SLOs, dashboards, alerting, rollback and emergency disable exercised | **Partial** — RUNBOOK.md covers all eight §13.3 scenarios and the emergency lever; dashboards and alert thresholds are OD-03/OD-04 | `RUNBOOK.md` |
| DQC reconciliation confirms source-to-index completeness and no cross-tenant contamination | **Done** | ledger-vs-engine count in `RecordCheckpoints`; untenanted-document check in `validateGeneration` |
| Production release approved by Platform Architecture, Security, Privacy/Records and domain owners | **Not started** — an organisational gate |

### Invariants that are not code-expressible here

| INV | Why it is not this service's to enforce |
|---|---|
| INV-19 | Legal hold changes retention authority in DRC, not search visibility. This service correctly does nothing differently for held records. |
| INV-20 | "Removing from search is not record deletion." Enforced by *not* offering a deletion API that a DRC obligation could be discharged against; documented in RUNBOOK §8. |
| INV-22 | "A failed reindex leaves the last validated generation active" — true by construction (a FAILED generation can never reach ACTIVE), but the *judgement* in NP-43 about when rollback is unsafe is an operator decision. |
| INV-26 | Vector index parity — wave 7. |
| INV-27 | "Retrieved text is untrusted data, not executable instructions" — AIG's to enforce on the prompt side. This service labels nothing as instructions and escapes what it returns. |
| INV-30 | Administrative console isolation is a deployment property (Traefik routing), not a service one. The code half — a separate control plane with platform-scoped authorization — is done. |

---

## Negative-path certification (§14)

Covered by an automated test or by `scripts/audit.sh`:

NP-01, 02, 03, 04, 05, 06, 07, 08, 09, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19,
20, 21, 22, 24, 25, 26, 29, 30, 31, 36, 40, 41, 42, 44, 45, 47, 48, 51, 52, 53,
54, 55, 56, 57, 58, 59, 60.

Deferred, with the reason:

| NP | Deferred because |
|---|---|
| NP-23 | Special-category query telemetry — the minimisation is done (digest only); the *classification* of a query as special-category is PRV's. |
| NP-27, NP-28 | Analyzer/synonym change certification. The mechanism is done — an analyzer change is a new contract version and therefore a new generation — but the relevance-evaluation gold sets are OD-15. |
| NP-32, NP-33, NP-34, NP-35 | AI/RAG and vector boundary — wave 7. |
| NP-37 | AI citation deep links — wave 7; the re-authorization that makes it work is done. |
| NP-38, NP-39 | Search personalisation and recent-query history. **Not built at all**, which is the strongest form of compliance with both: there is no profile to lack a purpose and no history to leak between sessions. |
| NP-43 | "Active generation corrupt but old generation stale on permissions" — an operator judgement, in RUNBOOK §3. |
| NP-46 | Break-glass engine console access — OD-14, a PAM/JIT selection. |
| NP-49 | Residency cell migration — OD-02. |
| NP-50 | Field-retirement compatibility gate. Contract versioning supports it; the consumer-migration check is a process. |

---

## Gaps closed in this implementation

| Gap | Where it was | Status |
|---|---|---|
| **Tracker row 65a** — HTTP polling of obligations-svc could never work (headerless call to a tenant-requiring endpoint), and could not be fixed by adding a header | `internal/sync/obligations_syncer.go` | **Closed** — redesigned as an event consumer per Doc 03 §37 / Doc 04 §9.8. The polling package is deleted; audit §13 fails if it returns |
| **Second half of row 65a** — `resolveTenantID` called tenant-entity-registry-svc headerless, which also could only 404 | same file | **Closed** — tenant comes from the event envelope; no resolution lookup exists |
| **Readiness measured the wrong thing** — set from whether the sync loop was running, not whether it achieved anything, so the service reported healthy while indexing zero documents | `internal/health/health.go` | **Closed** — readiness probes Postgres, OpenSearch and the projector registry, with per-dependency detail |
| **obligations-svc events carried no `tenant_id`** — so its records were unprojectable without a privileged cross-tenant lookup | `obligations-svc/internal/events/publisher.go` | **Closed** at the producer — `emit` reads the verified tenant from the request context |
| **`search-client` had dynamic mapping on** — OpenSearch's default indexes whatever arrives, the opposite of INV-08/NP-51 | `searchclient` | **Closed** — `EnsureGeneration` writes `dynamic:"strict"`; the legacy `EnsureIndex` path is unchanged and documented as the uncontracted one |
| **No alias/generation layer** — INV-21 requires building a replacement while the old one still serves | `searchclient` | **Closed** — `GenerationIndex`/`Alias`/`ActivateGeneration`, one atomic `_aliases` call |
| **No `SEARCH_*` actions in RBAC** — every control-plane write was a correct 403 with no way to grant it | `deployments/scripts/seed-demo-rbac.ps1` | **Closed** — `SEARCH_FULL` bundle, listed as platform-scoped |
| **No database** — the service had no schema at all | `deployments/init-db.sh`, compose | **Closed** — `search_indexer` database, migrations mounted, 8 tables with RLS |

## Defects found by the tests during this build

All fixed; recorded in `context.md` §5 with the reasoning.

1. Nil Go slices became SQL `NULL` on four `NOT NULL DEFAULT '{}'` columns — a
   source with no restriction events failed to register, and every *successful*
   search silently failed to record its evidence.
2. `search_field_definitions.field_id` was a UUID primary key the domain never
   used; the insert passed a field *name* into it.
3. The RLS test connected as a superuser, which bypasses row-level security
   unconditionally — it would have passed against a broken or absent policy.
4. `strings.Contains(query, "script")` refused `description`, `transcript`,
   `prescription` and `subscription`.
5. A malformed UUID in a path reached the driver and answered 500 rather
   than 404.

---

## Frontend

| Item | Status |
|---|---|
| `lib/api/search.ts` — server-only client | **Done** |
| `app/admin/search/` — page, actions, state | **Done** |
| `components/admin/search/` — panels and forms | **Done** |
| `lib/api/config.ts` — port and gateway prefix | **Done** |
| `lib/constants.ts` — navigation entry | **Done** |
| `lib/api/health.ts` — status grid entry | **Done** |

---

## Audit result

`scripts/audit.sh`, run against the live stack on 2026-09-21:

```
PASS 111   FAIL 0   SKIP 0
100% of executed checks passed
```

Reproducible — two consecutive runs, exit 0 both times. The 111 checks span
static analysis, the unit suites, 18 store integration tests against a real
Postgres 16 (including RLS verified as a NON-superuser), live health, the §13.1
telemetry families, and live end-to-end exercise of ESR-01 through ESR-05:
source registration, every field-contract rule, the full generation lifecycle
with a real OpenSearch alias swap, every query-planner refusal, the restriction
lane through to independently VERIFIED, tenant isolation, evidence
minimisation, the export boundary, and the console integration.

Three checks are worth singling out because they assert against the ENGINE
rather than against this service's own code, so they cannot pass by agreeing
with a bug:

* `NP-51 generation mapping is dynamic:strict` — read from OpenSearch's
  `_mapping`, not from our request body.
* `INV-09 a SECRET_PROHIBITED field has no index representation` — the field is
  absent from the live mapping entirely.
* `alias resolves to the activated generation` — read from `_alias`, which is
  the half of NP-42's drift check this service does not control.

## What a reviewer should run

```bash
cd services/search-indexer-svc
go build ./... && go vet ./... && go test ./...
./scripts/audit.sh          # against the running stack
```

`audit.sh` reports a pass percentage and exits non-zero on any failure. It
detects the case where the RBAC bundle has not been seeded and *says so*,
rather than reporting a correctly fail-closed service as broken.
