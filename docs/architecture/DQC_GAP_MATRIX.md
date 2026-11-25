# ZS-SVC-Z-001 (DQC-01..05) — Gap Matrix against the existing code

Spec: `ZS-SVC-Z-001_Data_Quality_Reconciliation_Lineage_Control_Totals` v1.0 (Domain 3, Data / Evidence / Reliability).
Date: 2026-11-23. Status: **steps A-E done (2026-11-23); D and E are partial; F started (2026-11-25): F1 (contract registry), F2 (runs judged against a contract) F3a (certification revocation) F3b (the quarantine-release and lineage routes) F4 (the remaining /dqc/* routes) and G (observation link, tolerance rounding, deployment wiring) are done - see Progress.** See "Progress" below.

## How this was produced, and how far to trust it

Three read-only audits, one per slice of the spec, each reading migrations, domain types, handlers and stores.
Nothing was executed: "tested" below means *a test exists in the source*, not that it was run for this document.
Two headline claims were re-read by hand and confirmed: `RunDQRequest.Validate` only requires **at least one** outcome
(`data-quality-svc/internal/domain/types.go:142`), and lineage has no `run_id` / config column
(`data-lineage-svc/deployments/migrations/000001_initial_schema.up.sql`). Everything else is the auditors' reading
(file:line cited) and should be spot-checked before it is relied on. `UNVERIFIED` is used where an auditor could not tell.

Verdicts: **IMPL** enforced by code or a DB constraint/trigger · **PART** partly · **MISS** absent · **N/A** not owned by these services.

## Progress

| Step | State | What changed |
|---|---|---|
| A - DQ completeness + INDETERMINATE | **DONE** | `data-quality-svc`: every rule in the pinned version must have an outcome (unreported -> `Indeterminate` result, never dropped); a rule that evaluated 0 records is `Indeterminate`; unknown or duplicate rule keys refused (400); `CertifyDQ` re-derives completeness from the pinned version (409 `ErrEvidenceIncomplete`); issues can be raised on Indeterminate results; migration `000002` (CHECK admits `Indeterminate`, which must carry no counts). New event `DATA.DQIndeterminate`. 12 new tests incl. negative controls; verified in Docker. |
| B - lineage run/config/version pins | **DONE** | `data-lineage-svc`: activities now carry `run_id`, `build_version`, `config_digest` (sha256 hex), `environment`, `region`; edges pin `source_version` and `derived_version` (mutable pointers such as `latest`/`HEAD` refused in the app and by a DB CHECK). `SealManifest` now refuses an empty graph, and any activity or edge missing that evidence. Same `run_id` + different build/config is a 409 conflict; a re-run is a new edge. `lineage_activities` was not immutable at the database - now it is. `SupersedeLineage` accepts replacement provenance (writes a NEW activity, inherits unspecified fields), which is the only way to repair a legacy edge. Migration `000002`. 14 new tests, 10 negative controls, verified in Docker. |
| C - freshness window + certification basis | **DONE** | **data-quality-svc**: a run may carry `evidence_as_of`; certification is refused (409, `ErrEvidenceStale`) when the evidence is older than the service window (`DQ_EVIDENCE_MAX_AGE_SECONDS`, default 24h; non-positive values fall back to the default, an unlimited window is not expressible). **reconciliation-engine-svc**: the window is `max_evidence_age_seconds` in the pinned certification profile (optional; absent = no expiry, so existing profiles and rule-set digests are unchanged); evidence age is the oldest frozen population. **Both**: certifications seal a `basis_sha256` of WHAT was certified (excludes run, certifier, timestamps), unique per tenant - a second certification of the identical basis is a 409 that names the existing one; an optional lineage manifest reference (id + sha256) is bound into the basis; the freshness state is sealed with the certification. Migrations DQ `000003`, REC `000013`. 27 new tests, 17 negative controls, DQ verified in Docker. |
| D - reconciliation hardening | **PARTIAL** | **Done (reconciliation-engine-svc):** (1) *A defect the audit only suspected, now proven and fixed:* after a source was corrected and the run reperformed over populations that now reconcile, the old exceptions stayed OPEN forever and Certify refused - the only exits were accepting risk on variances that no longer exist, or abandoning the run. Both item and balance exceptions were trapped. Reperform now moves every still-working exception raised on an earlier snapshot generation to a new terminal status `SUPERSEDED` (who, when, which generation, why; nothing deleted; ACCEPTED_RISK and CLOSED untouched; only a re-test can write it). A balance exception is now unique per generation, so a re-test that finds the same bucket with a *different* variance records the new numbers instead of silently keeping the old. (2) Hashes are now re-derivable (INV-24): snapshots record `hash_algorithm`+`hash_serialization`, certifications record `manifest_serialization`, `VerifyPopulationHashes` + `GET /runs/{id}/population-hashes` re-derive every snapshot hash and report corruption or an unknown format (never guessed), and a golden vector pins the v1 serialization. Migrations `000014`, `000015`. 29 new tests, 25 negative controls. **NOT done, and why:** sequence totals and cross-period roll-forward need new fields from the *source services* (a per-record sequence number; opening/closing balances) - a change to `control-population-contract.md`, which is the interface to other services and has uncommitted edits of yours; PARTIAL result needs a subset model (a design decision); source-declared hash needs the contract to carry one; conversion contract/rate provenance is a product decision; rounding mode is moot while the engine refuses over-scale amounts rather than rounding them. |
| E - governed quarantine release | **PARTIAL** | **Done (data-ingestion-svc, controlled imports / INT-06):** quarantining and releasing an import item is now governed. A durable, append-only **quarantine ledger** (QUARANTINED, RELEASE_REQUESTED, RELEASE_REFUSED, RELEASED, REVALIDATED per item) replaces evidence that used to be destroyed: a retry nulled the item's quarantine reason, owner and quarantiner and nothing durable recorded who had quarantined it, why, or who put it back. A plain retry of a QUARANTINED item is refused (`INT-QUAR-008`); a release needs a request that names the correction made in the owning domain, then a decision under its own permission `DATA_IMPORT_QUARANTINE_RELEASE` by someone other than the requester AND other than whoever quarantined it (enforced by a database trigger as well as in code), and how the owning domain's re-validation ended is recorded. Quarantine state NONE/PARTIAL/FULL/RELEASE_PENDING is exposed at `GET /imports/{id}/quarantine`. *A defect found by testing, not by the audit:* a released item was re-sent under its unchanged, immutable idempotency key, so a domain that honours Idempotency-Key would replay its cached refusal and **no release could ever take effect**; each release now delivers under a key of its own (a transient-failure retry keeps the original key, because its first attempt may have committed). Migration `000003`. 28 new tests, 14 negative controls; **verified end to end in Docker** through the real container. **NOT done:** (1) the INT-04 run-level `quarantine_items` (batches that failed validation and never landed) still have no release or disposition path; (2) the control-population contract is unchanged - it does not carry `quarantine_state`, because that contract is the interface to other services (and has your uncommitted edits); (3) batch-level QUARANTINED (a whole file) remains terminal: the route back is a corrected re-upload, which is re-validated from scratch, but nothing links the new batch to the quarantined one; (4) `data-ingestion-svc` is still not in docker-compose. |
| F1 - DQC-01 contract registry | **PARTIAL (F1 of F)** | **Done (data-quality-svc):** a real contract registry, which did not exist (rule sets were free-text names with two thresholds). Stable asset identity (`dca_`, tenant-unique key); immutable contract versions (`dct_`) carrying schema (field/type/nullability/format), semantic predicates, rules with quality dimension and severity INFO/WARNING/ERROR/BLOCKER, explicit tolerance (kind, value, scale, rounding, population) and a certification profile; lifecycle DRAFT/REVIEW/PUBLISHED/DEPRECATED/RETIRED, forward only, enforced by a database trigger as well as the service; content frozen by trigger (NP-02); maker-checker (the publisher is neither the author nor the submitter, by service and by CHECK); one open draft per asset; a deterministic compatibility check (BACKWARD/FORWARD/FULL/NONE) whose breaking-change evidence blocks publication with DQC-003 (NP-03) and is stored with the version, NONE needing an explicit acknowledgement; exact-version reads only, no `latest` (NP-01, DQC-001/002); routes under `/v1/data-quality/dqc/contracts`; events `dqc.contract.created|submitted|published|deprecated|retired` in the outbox in the same transaction. Migration `000004`. Tests against real Postgres, 13 negative controls, **verified end to end in Docker**. **Defaults taken, to be confirmed (not decided by the spec):** OD-01 asset kind is a free lowercase token, not an enum; OD-06 tolerance is frozen with the version; OD-07 predicates are stored as opaque strings whose field references are validated, not executed; the default certification profile (every ERROR and BLOCKER rule) is computed and stored. **NOT done:** (1) nothing yet *consumes* a contract: `RunDQ` still pins a rule-set version, not a contract version, and DQ results do not carry severity; (2) *(closed 2026-11-25)* the permissions `DQC_CONTRACT_READ/MANAGE/PUBLISH` and all twelve `DATA_IMPORT_*` actions (none had ever been registered, so a real authorization service denied every import call) are now registered by access-control-svc migrations `000060` and `000061`; who holds them is still a role-design decision, no role template was created; (3) the rest of the `dqc.*` surface (observations, validate, certifications, exceptions, quarantine release, lineage) and the DQC-wide events; (4) WFC approval for classified material; (5) OD-08/OD-10 (OpenLineage, certification seal). |
| F2 - runs bound to a contract | **PARTIAL (F2 of F)** | **Done (data-quality-svc):** `RunDQ` can now be judged against an exact PUBLISHED contract version (`contract_asset_id` + `contract_version`, both required, never "latest") instead of a rule-set version; exactly one rule source per run (service and CHECK). Each result carries the rule's **severity, dimension and whether the certification profile requires it**, frozen on the append-only result. Severity now decides what blocks: only a MANDATORY rule's Fail or missing evidence opens exceptions and blocks certification (a BLOCKER is always mandatory, by contract validation and by CHECK); a failing non-mandatory rule is recorded and stays visible in the sealed certification, it just does not block. Tolerance is applied (ABSOLUTE on the failure magnitude sum, PERCENT on failures over evaluated records). A DRAFT, REVIEW, DEPRECATED or RETIRED contract is refused for new runs (409) but a run already judged against one still certifies; a reperform must stay on the same contract version. The certification summary and basis name the contract and its content hash. New event `dqc.validation.completed` (per-rule status, severity, mandatory, count of BLOCKERs not passed) for contract runs. Rule-set runs behave exactly as before (existing tests unchanged and green). Migration `000005`. 13 negative controls, **verified end to end in Docker**. **NOT done:** (1) *(closed in step G)* a tolerance's scale and rounding mode are now applied; (2) predicates are still not executed (the caller computes outcomes); (3) governed risk acceptance for an unresolved BLOCKER (`allow_risk_acceptance` is stored, nothing consumes it); (4) lineage, reconciliation and ingestion do not yet reference a contract. |
| F3a - certification revocation | **PARTIAL (F3 of F)** | **Done (data-quality-svc, RB-05 / DQC-017):** a certification can now be revoked, which was impossible (a suspected false PASS could be noticed and not acted on). Revocation is a separate append-only record (`dq_certification_revocations`, database trigger blocks update and delete), one per certification, with a reason code (FALSE_PASS, SOURCE_DEFECT, CONTROL_DEFECT, EVIDENCE_INVALID, OTHER), a mandatory reason and mandatory evidence references (1-20), and **the revoker cannot be the certifier** (service and CHECK), nor can the revocation pre-date the certification (CHECK) - the same standard as reconciliation-engine-svc's existing revocation (migration 000011), which this was aligned to. It is **prospective** and **preserves prior evidence**: the sealed certification, its summary and hash, and the run's Certified status are byte for byte unchanged (tested). The certification's basis stays claimed, so the same evidence cannot be certified again, the remedy is new evidence. `GET .../certification` now reports `status` ACTIVE or REVOKED and the revocation. Route `POST /runs/{id}/certification:revoke` under its own permission `DQC_CERTIFICATION_REVOKE` (HIGH, registered by access-control-svc migration `000062`). Events `dqc.certification.revoked` and, new, `dqc.certification.issued` (the legacy `DATA.DQCertified` is unchanged). Migration `000006` (its down refuses while any certification is revoked). 12 negative controls, **verified end to end in Docker**. **NOT done:** (1) no blast-radius query (which downstream things relied on the certification): nothing downstream records that reliance yet; (2) *(correction, 2026-11-25: an earlier version of this row said reconciliation-engine-svc certifications have no revocation. That was wrong: they have it, with reason, evidence, independence, authority snapshot and prospective effect - migration 000011, `POST /runs/{id}:revoke-certification`, `RECONCILIATION_REVOKE`. Nothing was built there; the data-quality revocation was aligned to it instead.)* (3) stale-certification at read time (DQC-016) is not modelled, the spec does not say what window applies after issue; (4) the other `dqc.*` routes (observations, validate, exceptions, quarantine release, lineage) and governed risk acceptance. |
| F3b - quarantine release and lineage routes | **PARTIAL (F3 of F)** | **Done:** (1) **data-ingestion-svc** `POST /dqc/quarantine/release` (the spec's route, section 11: "optimistic concurrency + authority check + reason required"). One release decision (RELEASE or REFUSE) that must NAME the release request it reviewed (`request_ledger_id`, read from the quarantine view); if the item's open request is any other it is refused with 409 and nothing changes, checked under the item's row lock. Authority and independence are exactly those of the existing `:approve-release` / `:refuse-release` (permission `DATA_IMPORT_QUARANTINE_RELEASE`, decider not the requester or quarantiner); the existing routes are unchanged and still unchecked for staleness. (2) **data-lineage-svc** `GET /dqc/lineage` (read-only, `DATA_LINEAGE_READ`): upstream (default), downstream, or upstream as-of; returns the graph plus `complete` and the full list of `gaps` (missing run id, build version, config digest, transformation version, source/derived version pin, or no upstream at all), each naming its activity or edge (RB-03). `domain.LineageGaps` is now the single definition: `SealManifest`'s refusal is derived from it, so what a reader is told is missing and what sealing refuses for cannot diverge (tested both ways). (3) New event **`dqc.lineage.recorded`** (run id, input and output node ids and versions, build version, config digest, transformation version), written with the edge in the same transaction; a duplicate recording emits none. The edge-only `DATA.LineageRecorded` is unchanged. No new migrations, no new permissions. 15 negative controls, **verified end to end in Docker** (both services). **NOT done:** (a) "authorization filtered" lineage: authorization here is tenant and legal-entity level, so the projection is tenant-isolated (RLS) but not filtered per entity; (b) the run-level `quarantine_items` (INT-04) still have no release path; (c) `dqc.observation.completed`, `dqc.exception.opened` and the `/dqc/observations`, `/dqc/validate`, `/dqc/exceptions` routes; (d) `dqc.reconciliation.completed`. |
| F4 - the /dqc/* routes | **PARTIAL (F4 of F)** | **Done:** the rest of the spec's section 11 surface, each with the idempotency rule the spec names. **data-quality-svc**: `POST /dqc/observations` (new: an immutable record that an asset was observed over a window at a source watermark, keyed by asset + window + watermark; the same key with the same evidence returns the existing observation (200), the same key with different evidence is a 409 because late evidence is a new observation at a later watermark; the asset must be a registered DQC-01 asset; event `dqc.observation.completed`) and `GET /dqc/observations/{id}`; `POST /dqc/validate` (RunDQ judged against an exact contract only; **the same contract and evidence is the same run**, by an evidence fingerprint unique per tenant, returning the existing run with 200; a reperform of identical evidence is refused, it would supersede a run with itself); `POST /dqc/certifications` (CertifyDQ with the run named in the body; a basis conflict now returns 409 **naming the existing certification**, on both routes, `DQC-015`); `POST /dqc/exceptions` (RaiseIssue; **one exception per failing result**, a second report returns the first with 200; new event `dqc.exception.opened` with exception id, severity (the rule's, null for a rule-set run), control id, evidence references). The existing `/runs` and `/issues` routes got the same deduplication, so the two surfaces cannot disagree. **reconciliation-engine-svc**: `POST /dqc/reconciliations/{definitionID}/runs` is StartRun with the definition named in the path (a body naming another definition is a 400). Migration `000007` (down refuses while observations exist; up refuses, rather than skips, over existing duplicate issues, tested). 30 negative controls, **verified end to end in Docker** (data-quality-svc). **Deliberately NOT done:** `dqc.reconciliation.completed` and a `dqc.*` exception event for reconciliation: reconciliation-engine-svc already emits the canonical events the Enterprise Event Catalogue standard requires (`...reconciliation-run.completed`, `...reconciliation-break.detected`), and a second, differently named copy would put two vocabularies on one fact; mapping the spec's names to the catalogue's is a catalogue decision. **NOT done:** (1) *(closed in step G)* a validation run can now name its observation; (2) *(closed in step G)* reconciliation-engine-svc now has a Dockerfile and a compose entry, and the route was exercised in the real container. |
| G - observation link, tolerance rounding, deployment wiring | **DONE** | **data-quality-svc:** (1) a validation run can name the observation its population was taken from (`observation_id`, contract runs only). The run must BE what was observed: same asset as the contract, same row count, the observation's evidence hash as the population content hash, and not observed after the observation was recorded; with no `evidence_as_of` of its own the evidence is as old as the observation. The link is part of the evidence fingerprint, so a linked and an unlinked run of the same evidence are different records (service and CHECK; the observation must be in the same tenant). Migration `000008` (down refuses while any run is linked). (2) A tolerance's **scale and rounding mode are applied**: the measured value (failure magnitude sum, or failures as a percentage of records evaluated) is rounded to the tolerance's scale by HALF_UP, HALF_EVEN or DOWN before it is compared with the tolerance value, in exact rational arithmetic, as `domain.EvaluateTolerance`. 17 negative controls. **Deployment:** `data-ingestion-svc` and `reconciliation-engine-svc` are now in docker-compose (ports 8181 and 8182, because the services' own defaults collide with capability-registry and another service), with their databases, migrations and `zoiko_app` grants in init-db.sh, and reconciliation-engine-svc has a Dockerfile (build context `services/`, as data-ingestion's, because of the `zoiko.io/eventing` replace). `docker compose config` validates; both images build; each was run against Postgres with its real migrations, passed its compose healthcheck, and reconciliation's `/dqc` route answered 404/400/404 as designed. Controlled imports are OFF by default in compose (`DATA_IMPORT_ENABLED`), so the ingestion `/dqc/quarantine/release` route is not mounted until a deployment enables imports and supplies targets and a scanner; reconciliation refuses every run until `RECONCILIATION_SOURCE_ENDPOINTS` registers sources. |
| H | not started | |

Rows marked **(A)** or **(B)** were changed by that step: the verdict is current, but the evidence/gap text in those rows is the pre-change audit wording and describes what was missing at the time.

## 0. The structural finding

The spec describes one "DQC" plane. The code implements the same ideas in **different services with different vocabularies**,
built to a different document (ZS-SVC-N-001 DATA-02 etc.):

| Spec control | Where it actually lives | Notes |
|---|---|---|
| DQC-01 contract registry | `data-quality-svc` rule sets (+ `schema-registry-svc` for *event* schemas, `data-ingestion-svc` import profiles) | Three unrelated "contract" artifacts; none is a DQC contract. |
| DQC-02 validation / quarantine | `data-ingestion-svc` (quarantine), `data-quality-svc` (Pass/Fail evidence) | DQ never observes data; it records caller-computed outcomes. |
| DQC-03 reconciliation | `reconciliation-engine-svc` | Furthest along. Not deployed in Docker. |
| DQC-04 lineage | `data-lineage-svc` | Entity→activity→entity edges only. |
| DQC-05 certification / exceptions | `reconciliation-engine-svc` (strong) and `data-quality-svc` (weak) | Split. Revocation exists in both (DQ since F3a). |

None of `/dqc/*` routes or `dqc.*` event names exist. Events are `DATA.*` (DQ, lineage), `CONTROL.*` and
`com.zoikosuite.accounting.*` (reconciliation). A consumer written to the spec would receive nothing.

## 1. DQC-01 — contract registry (`data-quality-svc`, `schema-registry-svc`, `data-ingestion-svc`)

| Requirement | Verdict | Evidence / gap |
|---|---|---|
| Stable DataAsset ID | **IMPL (F1)** | Rule sets are identified by tenant + free-text name (DQ migration 000001:5-11). |
| Schema (field/type/nullability) and semantic predicates | MISS | `RuleDefinition` = rule_key, description, two thresholds. DQ stores no rule logic. |
| Quality dimensions | MISS | Rules untyped. |
| Severity INFO/WARNING/ERROR/BLOCKER | MISS | DQ results are Pass/Fail only. |
| Tolerance (numeric/percent, scale, rounding, population) | PART | Aggregate materiality decimal only (`pg_store.go:220-243`); `PerRecordMaterialityThreshold` stored but unused. |
| Certification profile (mandatory rules/evidence) | PART **(A)** | Every rule in the pinned version is now mandatory for certification. A configurable profile (subset, severity) is still absent. |
| Lifecycle DRAFT→REVIEW→PUBLISHED→DEPRECATED/RETIRED | **IMPL (F1)** | DQ publishes straight to an immutable version. DI import profiles have DRAFT/CERTIFIED/DEPRECATED/WITHDRAWN with a guard trigger (DI 000002:44-62) but no REVIEW. |
| Published versions immutable | IMPL | DQ trigger + `TestDQ_RuleSetChangeCreatesNewVersion`; SR triggers; DI profile guard. |
| Compatibility mode declared and tested | PART | SR only, for event schemas: BACKWARD/NONE (no FORWARD/FULL). No DQC compatibility check. |
| Explicit breaking-change evidence / dry-run endpoint | PART | SR returns 409 with violations on register; no `compatibility-check` route. |
| Maker-checker on publish | **IMPL (F1) for contracts** | Only DI profile certify (DB CHECK `certified_by <> created_by`). DQ publish and SR register have none. |
| Never resolve via "latest" | PART | DQ exact-version only. SR still exposes `/versions/latest`. |
| API surface (`/dqc/contracts`, publish, versions, compatibility-check) | **IMPL (F1)** | Different routes (`/rulesets/{name}:publish` …). |

## 2. DQC-02 — validation, profiling, quarantine

| Requirement | Verdict | Evidence / gap |
|---|---|---|
| Observation run bound to exact contract | PART | `RunDQ` binds ruleset version + frozen population ref/count/hash, but the run is caller-submitted evidence. |
| Watermark | PART | DI `source_checkpoints` is an ingestion offset, not a freshness window; DQ has none. |
| Idempotency | IMPL | DQ `idempotency_keys` + replay test; DI replay tests. |
| Validation PASS/FAIL/**INDETERMINATE** | **IMPL (A)** | DQ now records `Indeterminate` for unreported or zero-record rules; certification is refused until a Reperform supplies the evidence. |
| Quarantine NONE/PARTIAL/FULL/RELEASE_PENDING | **PART (E)** | DI has `PartiallyQuarantined` and terminal batch QUARANTINED; no state model. |
| Release needs authority + reason + re-validation | **IMPL (E) for import items** | DI `RetryItem` needs reason + a generic MANAGE action and re-queues to the owning domain; run-level `quarantine_items` are write-once with **no release path**. |
| Stale data flagged | MISS | No timeliness rule anywhere. |
| Duplicates never deleted | IMPL (DI) | `ON CONFLICT DO NOTHING`, no deletes. Duplicate is skipped, not kept as evidence. |
| Unknown schema never guessed | PART | DI quarantines on version mismatch against a string it recorded, not a registry contract; first version accepted unchecked. |

## 3. DQC-03 — reconciliation (`reconciliation-engine-svc`)

| Requirement | Verdict | Evidence / gap |
|---|---|---|
| Record count | PART | Source declared count checked; no A-vs-B count control; dedup basis not pinned. |
| Monetary sum | PART | Per-currency totals, scale CHECK 0..12, exact minor units. **No rounding mode** (SPEC_DEVIATIONS A5). |
| Hash total | PART (D) | sha256 over fixed format (`population.go:291`); not compared to a source-declared hash. |
| Sequence total | MISS | No continuity/gap/wrap control. |
| Dimensional balance | IMPL | `CheckBalances` per declared dimension (`matching.go:374`); tests exist. Only for declared dimensions. |
| Cross-period roll-forward | MISS | Not built, not in SPEC_DEVIATIONS. |
| MATCH / VARIANCE / INDETERMINATE | IMPL | `state.go`; DB CHECK + triggers; INDETERMINATE only → SUPERSEDED. |
| PARTIAL | MISS | Enum exists but unreachable (SPEC_DEVIATIONS A16). |
| SUPERSEDED | IMPL | Replacement uniqueness + lifecycle trigger. |
| Tolerance versioned and in basis | IMPL | Immutable approved rule versions with `rules_sha256`; run pins it. |
| Re-run never overwrites | PART (D) | Evidence rows immutable, but **Reperform mutates the same run row**; balance-exception uniqueness may swallow a changed variance (UNVERIFIED). |
| No cross-currency sum without conversion contract | PART | Enforced by refusal only; no conversion/rate-provenance path; several M5 CHECKs are `NOT VALID` (new rows only). |
| Missing source never coerced to zero | IMPL | Refuses page without declared totals/watermark; failures → INDETERMINATE. |
| INV-24 hash pins serialization + algorithm version | **IMPL (D)** | No version field on `content_hash` or manifest. |

## 4. DQC-04 — lineage (`data-lineage-svc`)

| Requirement | Verdict | Evidence / gap |
|---|---|---|
| Edge pins run identity | **IMPL (B)** | `lineage_activities` / `lineage_edges` have no run_id, times, environment, region. |
| Job/build version | **IMPL (B)** | `transformation_version` nullable free text; **only sealing refuses NULL**, recording allows it. |
| Config digest | **IMPL (B)** | None. NP-34 unenforced. |
| No lineage to mutable "latest" | **IMPL (B)** | Entities are `(type, external_ref)`, no version/watermark/snapshot. |
| No cross-tenant edge | PART | RLS FORCEd; edge FKs not tenant-composite; no direct test of "create cross-tenant edge". |
| Backfill never rewrites history | IMPL | Edge UPDATE/DELETE trigger; supersede inserts new edge with reason; tested. |
| Manual intervention actor + evidence | PART | agent/created_by NOT NULL; nothing links evidence to an edge, no before/after refs. |
| Authorization-filtered graph | PART | Coarse action check per route (tested); no per-node/classification filtering (NP-37). |
| Lineage is evidence, not authorization | IMPL (by absence) | No test asserts it. |
| **NP-31 output with no input lineage** | **IMPL (B)** | `SealManifest` on an entity with **zero edges succeeds** (the version check iterates existing activities only, `pg_store.go:631`). Fails open. |

## 5. DQC-05 — certification, exceptions, events

| Requirement | Verdict | Evidence / gap |
|---|---|---|
| Pins exact versions + basis hash | **PART (C)** | REC manifest pins definition, rules hash, tolerance, populations. No observation or lineage-manifest reference; **no basis-hash uniqueness** (only per-run). DQ pins ruleset + population hash only. |
| Freshness window / expiry | **IMPL (C)** | Certifications never go stale (NP-43, DQC-016). |
| Open blockers block certification | IMPL | REC `Assess` + store refusal; DQ blocks on open issues. DQ has no severity. |
| Accepted risk never becomes PASS | IMPL | VARIANCE retained; CHECK allows it only if profile permits. |
| WFC closure does not auto-certify | PART | Safe by absence of any WFC hook; no WFC case link (TC-12). |
| Re-test required after remediation | PART | DQ: needs clean Reperform. REC: REMEDIATED→CLOSED without an enforced new evidence run. |
| Revocation reason + evidence, prospective, history kept, revoker ≠ certifier | IMPL (REC) | Migration 000011 CHECKs + lifecycle trigger. DQ revocation added in F3a, to the same standard (reason, evidence, independent revoker, prospective). |
| Unauthorized certifier refused | PART | REC strong; DQ stores only `certified_by`, no authority snapshot, no independence check. |
| Events durable + idempotent | PART | Outbox in all three. Names are not `dqc.*`; payloads lack lineage ref / scope. Keyed ordering UNVERIFIED. |

## 6. Invariants INV-01..30 (summary)

| IMPL | PART | MISS / N/A |
|---|---|---|
| 01, 04, 05, 07, 08, 11, 13, 16*, 17, 19, 22, 25, 26 | 02, 06, 09, 10, 12, 15, 18, 20, 21, 23, 24, 28, 29, 30 | 04, 05 (DQ), 14, 27 (N/A) |

\* by absence of any lineage-based access path; untested.

## 7. Negative paths NP-01..60 (summary)

| Range | Mostly | Notes |
|---|---|---|
| NP-01..10 contracts | PART | 02 IMPL. 09 (deprecated after cutoff) MISS. 10 (maker-checker) only DI profiles. |
| NP-11..20 validation/quarantine | PART | 14, 17 IMPL; **16 IMPL for import items (step E)**. **13 (stale watermark) MISS.** 15, 19, 20 PART. 18 N/A. |
| NP-21..30 reconciliation | IMPL/PART | 21, 23, 26, 27, 30 IMPL. 22, 24, 25, 28, 29 PART. |
| NP-31..40 lineage | IMPL/PART | 31, 32, 33, 34 IMPL (step B); 39 IMPL, 40 IMPL by absence; **37 MISS** (no privilege/classification on entities); 36, 38 PART; 35 N/A. |
| NP-41..50 certification | IMPL/PART | 41 (REC), 42, 43 (step C), 44, 47, 48, 50 IMPL; 45, 46, 49 PART. |
| NP-51..60 cross-domain | N/A/PART | 51-54 only if a population definition exists (none seeded for payroll/bank/tax/billing). 55-60 not owned by these services. |

## 8. Highest-value gaps, merged and ranked

1. ~~**Certification can pass on incomplete evidence (INV-04, NP-20).**~~ **DONE (A).** Original finding: `RunDQ` requires only ≥1 outcome; `CertifyDQ` does not require an outcome for every rule in the pinned version, so a 1-of-N run certifies. *Small, local, testable.*
2. ~~**No INDETERMINATE in `data-quality-svc` (INV-05, NP-41 for DQ).**~~ **DONE (A).** Original finding: Missing evidence has no representation.
3. ~~**Lineage carries no run identity, config digest or version pin (DQC-04, TC-09, NP-32/34)** and **sealing an input-less entity succeeds (NP-31)**.~~ **DONE (B).** Original finding: Needs migration + contract change.
4. ~~**No freshness/expiry on certification or evidence (NP-43, NP-13, INV-14).**~~ **PARTLY DONE (C):** certification now expires (NP-43). NP-13/INV-14 (stale watermark presented as current at observation time) are still open - they need an expected-arrival window on the data source, which is step F (DQC-01 contract model).
5. ~~**Certification does not bind lineage/observation evidence and has no basis-hash uniqueness (INV-21, §11).**~~ **PARTLY DONE (C):** basis hash + uniqueness + a lineage reference are in. The reference is caller-asserted and NOT verified against data-lineage-svc, and it is optional (requiring it is a certification-profile decision, step F). There is still no observation-run reference.
6. **No DQC-01 contract model** (asset id, severity, profile, lifecycle, maker-checker on publish, compatibility-check). The largest piece of work.
7. ~~**No governed quarantine release (NONE/PARTIAL/FULL/RELEASE_PENDING).**~~ **DONE for import items (E);** the INT-04 run-level quarantine and the control-population contract are still open.
8. **Reconciliation:** ~~hash/serialization version (INV-24)~~ **DONE (D)**; ~~Reperform leaves findings trapped~~ **DONE (D)** (found by testing the audit's "UNVERIFIED" remark). **Still open:** sequence + roll-forward totals (blocked on a source-contract change), reachable PARTIAL, a source-declared hash, rounding mode (moot today), and the run row itself still advances in place on Reperform (its history is in run_transitions, and no evidence row is overwritten).
9. **No `dqc.*` events or `/dqc/*` API surface.** Decide first whether to adopt the names (an adapter/alias layer is cheaper than renaming).
10. **Deployment:** `reconciliation-engine-svc`, `data-ingestion-svc`, `document-extraction-svc`, `analytical-data-platform-svc` still have no compose entry.

## 9. Suggested build order (each item: change → migration if any → test that fails without it → Docker check)

| Step | Gaps | Why this order |
|---|---|---|
| A | #1, #2 — DQ completeness + INDETERMINATE | Smallest, no cross-service impact, closes a real false-certify path. |
| B | #3 — lineage run/config/version pin; seal refuses empty graph | Unblocks #5 and the `dqc.lineage.recorded` payload. |
| C | #4, #5 — freshness window and basis hash (+ lineage ref) | Depends on B. |
| D | #8 — reconciliation hardening | Independent; can run in parallel with B/C. |
| E | #7 — quarantine release model | Touches data-ingestion. |
| F | #6, #9 — contract model and `dqc.*` surface | Largest; needs product decisions (spec OD-01..OD-15 are still open). |

## 10. Open decisions that block step F (from spec §19)

OD-01 asset taxonomy and default certification profiles · OD-03 canonical serialization for hashes · OD-06 tolerance/materiality authorities ·
OD-07 rule-authoring DSL · OD-08 OpenLineage adapter vs native model · OD-10 certification signature/seal mechanism.
Steps A–E do not depend on these, except OD-03 for the hash-version work in step D.
