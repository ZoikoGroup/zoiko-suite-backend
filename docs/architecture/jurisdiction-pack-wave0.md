# ZS-JUR-001 Wave 0: regime, source, interpretation and pack registries

Implemented inside the existing `jurisdiction-rules-svc` (no new service). Scope is Wave 0 of ZS-JUR-001 s37
("Registries: jurisdiction, regime, source and pack registries; identifiers; schemas"). Later waves are documented separately (the wave1, wave2, wave3, wave4 and wave7 documents); Waves 5, 6 and 8 are not built (Wave 4 is built in both halves).

## What exists now

| Registry (ZS-JUR-001) | Tables | Notes |
|---|---|---|
| Regime (s4) | `regulatory_regimes` | unique `regime_code` |
| Source Register (s8) | `regulatory_sources` | `snapshot_hash` (sha256) mandatory; independent review; supersede link; content immutable |
| Interpretation (s8, s20) | `interpretation_records`, `interpretation_sources` | PENDING to APPROVED; approver is not the author; cited sources must be reviewed |
| Pack (s5, s27) | `jurisdiction_packs`, `jurisdiction_pack_versions`, `pack_version_jurisdictions`, `pack_version_regimes`, `pack_dependencies` | DRAFT versions from a validated manifest, canonical sha256 digest, exact dependency pins |
| Rule temporal/provenance (s7) | new nullable columns on `jurisdiction_rules`, `rule_sources` | `regime_id`, `interpretation_id`, `supersedes_rule_id`, `precedence`, `published_on`, sources |

Migration `000005_jurisdiction_pack_registries` is additive (up and down tested as a round trip). The existing
tables, endpoints, constructor and events are unchanged; the registry routes are mounted only when
`Handler.WithRegistry` is called (done in `main.go`).

## Invariants and where each is enforced

Enforced in the database (triggers and CHECKs), so a writer that bypasses the Go code is also stopped:

* A captured source is evidence: content never changes; only the review and supersede link can be added, each once;
  never deleted. (s8, JUR-NEG-06)
* A source reviewer is not its author. An interpretation approver is not its author. (s3, s20, JUR-NEG-18)
* An approved interpretation, and its source links, are frozen.
* A pack version's manifest, identity, dates and digests freeze when it leaves DRAFT; an artifact digest or
  signature, once set, is write-once; versions are never deleted. (s3, JUR-NEG-20)
* Pack version status moves only along the s23 edges (for example REVIEW cannot jump to RELEASED).
* Rule provenance (and `rule_sources`) can only change while the rule is DRAFT. (s3: released rules are immutable)

Enforced in the service:

* Manifest is parsed strictly. Status, digests and signature are server-owned, so a manifest that asserts them is
  rejected. Dependencies must be exact pins (`latest`, ranges are refused). Jurisdiction and regime codes must
  resolve to exactly one active registry entry. `rule_modules` must be existing rule ids.
* Versions must strictly increase (numeric order: 2026.08.10 is later than 2026.08.9). The same version with
  different content is a 409. Identical content replays as 200.
* An interpretation backing a rule must be APPROVED and belong to the rule's jurisdiction or an ancestor.
  `supersedes_rule_id` must be an earlier version of the same rule code.
* Authoring, review, approval, supersede and submit are separate authorization actions (s28), see below.

## API (all admin routes need `X-Principal-Id` and an authorization decision)

Reads (public, like the existing registry): `GET /v1/regimes[/{regime}]`, `/v1/sources[/{id}]`,
`/v1/interpretations/{id}`, `/v1/packs[/{ref}]`, `/v1/packs/{ref}/versions[/{version}]`,
`/v1/rules/{id}/provenance`.

Commands: `POST /v1/admin/regimes`, `/sources`, `/sources/{id}/review`, `/sources/{id}/supersede`,
`/interpretations`, `/interpretations/{id}/approve`, `/packs`, `/packs/{ref}/versions` (body is the manifest),
`/packs/{ref}/versions/{version}/submit-review`, and `PUT /v1/admin/rules/{id}/provenance`.
Documented in `services/jurisdiction-rules-svc/openapi.yaml`.

Authorization action codes (flat UPPER_SNAKE, like every service): `REGULATORY_REGIME_CREATE`,
`REGULATORY_SOURCE_CREATE|REVIEW|SUPERSEDE`, `INTERPRETATION_RECORD_CREATE|APPROVE`, `JURISDICTION_PACK_CREATE`,
`JURISDICTION_PACK_VERSION_CREATE|SUBMIT`, `JURISDICTION_RULE_SET_PROVENANCE`. They are in the demo seed as bundle
`JURISDICTION_PACK_REGISTRY_FULL` (platform scope). **Operational step:** real environments must grant them through
separate roles; the demo bundle gives one role everything (the service still refuses self-review and self-approval).

Events (same envelope as the service's existing events; `aggregate_id` keys the partition):
`regulatory-regime.created`, `regulatory-source.captured|reviewed|superseded`,
`regulatory-interpretation.recorded|approved`, `jurisdiction-pack.created`, `jurisdiction-pack.version-drafted|
version-submitted`, `regulatory-rule.provenance-set`. Emitted only on a real write, never on an idempotent replay.
Like the service's existing events they are published after commit, best effort (no outbox yet).

## Not built (by wave)

* **Wave 1** (compiler, signed artifacts, verification): built, see [jurisdiction-pack-wave1.md](jurisdiction-pack-wave1.md).
* **Wave 4** is split: the calendar and obligation half is built ([jurisdiction-pack-wave4.md](jurisdiction-pack-wave4.md)); the tax half (typed rule parameters and exact calculation) is built too, after the rule-payload doctrine decision below was taken. **Waves 5, 6 and 8** (e-invoice and filing, payroll/retention/reporting mappings, country packs) are not built.
* `RulePrecedence` conflict handling beyond the declared `precedence` column; `PackCertification`, `PackDeployment`,
  `RuleDecisionEvidence`, `RegulatoryCalendar`, `RegulatoryObligation` entities.

## Decisions the document leaves open (s38) and what Wave 0 assumed

None of these is decided by this change; Wave 0 deliberately does not depend on them.

* **Rule DSL / decision tables, pack artifact format, signing key hierarchy** (Waves 1-2): not chosen. `rule_modules`
  currently only references existing `jurisdiction_rules` ids.
* **Rule payload doctrine conflict: RESOLVED in Wave 4.** This service's earlier rule (OQ-1, "Model B") kept rates out of `rule_payload`; ZS-JUR-001 s11-s12 puts them in
  released rule modules. Decision: ZS-JUR-001 governs, and rates live in a new typed `parameters` field while `rule_payload` keeps its meaning. See
  [jurisdiction-pack-decision-rule-parameters.md](jurisdiction-pack-decision-rule-parameters.md); `tax-rules-svc` (editable rate rows) remains a known non-conformance with a documented migration path.
* **Source snapshot retention** (where the captured bytes live): only the hash and an optional `snapshot_ref` are stored.
* **Manifest `jurisdiction_ids` are registry `jurisdiction_code` values** (the document's example uses `[GB]`).
  Codes are unique only per (code, type, parent); a code matching more than one active jurisdiction is refused as
  ambiguous rather than guessed.
* **Version scheme** is any dotted numbers ordered numerically (the example `2026.08.1` fits). Semantic-version
  ranges are intentionally not supported.

## Verification

`go vet` (also with `-tags=integration`) and the unit tests pass. The integration suite
`internal/registryit` (embedded Postgres 16, real store, real handlers) covers the flows above, every database
trigger by attempting the forbidden write in raw SQL, the original rule lifecycle with the new triggers installed,
the original constructor exposing only the original routes, and the migration down/up round trip. It is behind the
`integration` build tag: `go test -tags=integration -count=1 -timeout=240s ./internal/registryit/`.
