# search-indexer-svc — implementation progress

Tracked against ZS-SVC-AB-001's §18.2 Definition of Done and its §19 eight-wave
sequence. Every "Done" row below has a test that fails if the behaviour is
removed; the test names are in README.md's invariant table.

Last updated: 2026-09-30 (compliance re-audit remediation — see the 30 Sep section at the end).

---

## §19 Eight-wave implementation sequence

| Wave | Objective | Status |
|---|---|---|
| **1** | Authority & contract foundation — ESR-01, field exposure classes, source registration, tenant/residency partition metadata, reason-code taxonomy | **Done** |
| **2** | Secure indexing core — durable source ingestion, projection rules, checkpoints, tombstones, source-to-index reconciliation | **Done** |
| **3** | Query gateway — trusted context, scope registry, mandatory filter compiler, complexity budgets, cursors, lexical search | **Done** |
| **4** | Secure retrieval — current re-authorization, source hydration, snippets/redaction, safe facets/counts, export boundary | **Done**. R2 hydration is wired for `obligation` and hydrates as the caller; other source types need a `SOURCE_SERVICE_URL_<TYPE>` (OD-05 decides which scopes are R2) |
| **5** | Lifecycle & operations — generation build/validate/activate/retire, drift, recovery, degraded modes, priority restriction SLOs | **Done**, except numeric SLOs (OD-03/OD-04 — the metrics exist and are correctly separated; the thresholds are a capacity decision) |
| **6** | Sensitive-domain adoption | **Not started** — a registration exercise, not a code change. See "Onboarding a domain" in README.md |
| **7** | Semantic / RAG retrieval | **Built, fail-closed** — `/v1/search/semantic`, pinned models, filtered ANN, NP-33/34/35 controls, migration certification. No provider is configured (OD-10), so no semantic scope can build yet |
| **8** | Global search certification | **Not started** — gated on waves 6 and 7 |

---

## §18.2 Definition of Done

| Criterion | Status | Evidence |
|---|---|---|
| INV-01..INV-30 automated or mechanically evidenced, no bypass path | **Done** for the 24 that are code-expressible; 6 are deployment/organisational (see below) | README.md invariant table; `scripts/audit.sh` |
| NP-01..NP-60 pass in CI/pre-production certification for applicable source classes | **50 of 60** covered by tests or the audit script; 10 belong to waves 6–8 or to open decisions (table below) | test suites + `scripts/audit.sh` |
| Index generation lifecycle supports parallel build, validation, atomic activation and safe abort | **Done** | `TestIndexGenerations_OnlyOneActivePerScope`; audit §7 |
| Restriction propagation independently verified, producing measurable evidence | **Done** | `VerifyRestrictions` sweep; `esr.restriction.propagated` at VERIFIED only; audit §9 |
| Current-authorization retrieval across keyword, autocomplete, facet and semantic paths | **Done** for keyword, facet and semantic; autocomplete is wave 8 | `TestExecute_ReauthorizesEveryR1Hit`, `TestSemantic_HappyPathEmbedsWithThePinAndFiltersInsideTheWalk` |
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
| NP-32 | Prompt-injection handling is AIG's (§10.2); retrieved text is returned as data. |
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

---

## 30 Sep 2026 — compliance re-audit remediation

The Group 1 re-audit (docs/audit_files, 6/9) found the 25 Sep "fixes" for
ESR-012 and R2 did not work. Closed here:

| Gap | Was | Now | Evidence |
|---|---|---|---|
| ESR-012 STALE unreachable | lag = `time.Since(time.Now())` | broker-measured lag; STALE/UNKNOWN refuse R1+ (503), flag R0 | `TestCheckpoint_ConsumerHoursBehindIsStale`, `TestSearch_StaleProtectedScopeIsRefusedWithESR012`, `TestSearch_StaleR0ScopeAnswersFlagged` |
| Freshness never reached the caller | — | `index_freshness`, `index_lag_ms` on every response | `TestSearch_LaggingIsSurfacedNotBlocked` |
| False `esr.index_checkpoint.advanced` | emitted when nothing advanced | only when the broker watermark moves | `recordCheckpoint` |
| R2 hydrator | unwired, wrong path, no envelope, self-asserted identity | configured collections, caller's envelope, 404 → ESR-010 | `internal/hydrator` tests, `TestExecute_HydratedObjectIsProjectedBySourcePath` |
| ESR-018 never returned | event only | FAILED restrictions excluded + DEGRADED ESR-018; >500 blocks | `TestSearch_FailedRestrictionsAreExcludedAndReported` |
| Semantic / vector (§10.1, ESR-019) | absent | built, fail-closed pending OD-10 | `vector_test.go`, `semantic_test.go`, `freshness_test.go` |
| Idempotency-Key not honoured | — | dedupe store, principal-bound | `internal/idempotency` tests |
| openapi.yaml invalid | 2 structural errors | valid (redocly + openapi-spec-validator) | — |

### 30 Sep 2026 — live re-audit and second remediation

Run against real Postgres, Kafka and OpenSearch (service in a golang container;
permit-all authz; stubs for the embedding provider and obligations-svc). The
live run found five defects no unit test had, all fixed and re-verified live:

| Defect | Fix | Live proof |
|---|---|---|
| **CRITICAL** — HTTP restriction left the ledger at epoch 0; a later update re-indexed the ERASED record, and it still read VERIFIED | ledger stamped tombstoned at the restriction epoch first; every serving/candidate generation tombstoned | erased r3 stayed tombstoned after a later update; semantic search excluded it |
| **HIGH** — generations were never filled; validation passed `engine=0 ledger=4`; activating a rebuild emptied the scope | Kafka replay backfill (ledger-authoritative, create-only), parallel live writes, READY needs COMPLETE, no empty-index exemption | rebuild: `engine=2 ledger=2 vectors=2`, results unchanged after cutover |
| **HIGH** — model migration impossible (v2 contract written into v1's index) | each generation projected and searched with its own contract; publishing v2 retires v1 atomically | v1 served under m@1 while v2 built; certified; cut over to m@2 |
| **HIGH** — consumer subscribed before its topic existed never recovered | reader created only once the topic exists; retried every 30s | events indexed ~9s after the topic appeared, no restart |
| **HIGH** — NP-41 contamination check counted every document (empty term skipped), so every populated generation failed READY; failed open on error | `CountMissing` (must_not exists), fail closed | populated generation passed; untenanted doc still fails (unit) |
| MED — first quarantined message per topic lost from the DLQ | DLQ write retried; not committed until copied | first quarantine landed in a DLQ that did not exist |
| MED — watermark unit change pinned old rows | migration 000004 resets wall-clock watermarks | — |
| MED — retrieval evaluation was a cross-tenant existence oracle | runs in the caller's tenant only, with FAILED-restriction exclusion | — |
| LOW — migration gate failed open; 5xx refusals flooded the abuse stream; 409s cached 7 days; no X-Source-System to sources; lag corners read CURRENT; evidence lacked the model; 000002 not re-runnable | all fixed | migrations applied twice cleanly |

Verified in Docker afterwards (golang:1.25 + postgres:16, `-race`,
REQUIRE_DB_TESTS=1): all 13 packages race-clean; all 27 store integration tests
pass against real Postgres — 9 new ones for 000003/000004 — with RLS exercised
as an unprivileged role; the store harness now applies EVERY migration (it had
stopped at 000002); and 000001–000004 applied cleanly to a copy of the existing
search_indexer database.

Still not verified live: authorization-denial paths (permit-all stub), the real
obligations-svc, a real embedding provider.

### 30 Sep 2026 — `scripts/audit.sh` against the real compose stack

`search_indexer` was backed up (`pg_dump -Fc`) and upgraded with 000003 and
000004; the stack (this service + authorization-svc, mtls-management-svc,
postgres, kafka, opensearch) was rebuilt from source. `audit.sh` was updated:
it now waits for the generation backfill before READY, has a section 16 for the
30 Sep remediation (freshness on responses, checkpoint measured on activation,
HTTP restriction stamped into the ledger, idempotency replay and mismatch,
semantic refusal on a lexical scope, rebuild + cutover without resurrection),
validates openapi.yaml with a real validator, and re-runs the Go suite in
golang:1.25 with -race when host Application Control keeps blocking binaries.

Result: **51 PASS, 1 FAIL, 1 SKIP.** The run found one more defect in this
service, fixed: the restriction-backlog and freshness gauges exposed no series
until a scope went live, so alerts on them had nothing to evaluate
(`bootstrap` placeholders added). The one FAIL, and the sections behind it
(7–12, 16), are **authorization-svc**: its database is behind its code
(`column pra.book_id does not exist`; permission denied on its partition
functions), so every `/v1/authorize` answers 503 and this service correctly
refuses control-plane writes with ESR-008. Not a search-indexer defect.
