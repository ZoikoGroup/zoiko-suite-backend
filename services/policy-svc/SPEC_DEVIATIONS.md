# policy-svc: spec deviations

Each entry names the spec clause, what is built instead, why, and what would close it.
Specs: Doc 03 §8.1, GOV-05, ZS-SVC-V-001 (PDC-01…05), REF-07, ZS-API-001 (API-2…API-5),
GCP §16.

## P-01: closed 17 Nov 2026 (ZS-GOV-POL-001, ZS-GOV-INT-002)

SIGNATORY_MATRIX is evaluated here (`POST /internal/v1/policy/signatory-matrix/resolve`).
SPEND_CONTROL, SOD_RULE and LEGAL_BASIS are decided by spend-controls-svc, authorization-svc and
privacy-decision-svc; their versions here are references, and evaluate answers 501 NOT_IMPLEMENTED.

## Evaluate keeps 404 / 409 / 501 (Doc 02 §9 wants 200)

Its callers fail closed on these codes. The bodies carry `outcome` and `reason_codes`.
**Closes when:** the callers read `outcome`.

## Signer offices

Only the offices in access-control 000060 can sign (`SIGNATORY_SIGN_AS_<OFFICE>`). Any other
office in a matrix cannot be signed until it is registered.

## Decision signing key

`POLICY_DECISION_SIGNING_KEY_HEX` is required outside local/dev. Locally a random key is used.

## GET /v1/policies and GET /v1/accounting-policies are not paged (API-4)

These two routes are applicability sets, not registers. They return the versions that apply to
one scope, ordered by precedence, and a resolver needs the whole set. They stay plain arrays.
The paged registers are `/v1/policy-families`, `/v1/policy-versions`,
`/v1/policy-evaluations`, `/v1/control-test-definitions` and `/v1/attestations`. A policy's
version history, a version's transitions and a definition's executions are paged in the
handler, because the store returns them whole; each list is bounded by its parent.

## API-5: simulation is synchronous and bounded

V-001 lists simulation states QUEUED and RUNNING. `POST /v1/simulations` instead evaluates at
most 5000 recorded decisions in one request, using an in-memory evaluator (milliseconds per
thousand), and answers COMPLETE or PARTIAL. PARTIAL means the corpus was cut off at the limit,
or the candidate could not evaluate an input. The FAILED state is in the schema; QUEUED and
RUNNING are never used. **Closes when:** a corpus larger than 5000 is needed. The run then
moves onto the shared `apicontract` async operations (202 + `/v1/operations/{id}`).

## PDC-05: the certification gate applies to evaluable policy types only

PDC-I-24 requires certified tests before activation. A type with no evaluator has no test that
could run, so the gate reports `EXEMPT_NO_EVALUATOR` for it:
- REF-07 accounting policies, which are resolved rather than evaluated; draft-time validation
  covers them (impact analysis, pinned rule mappings);
- SPEND_CONTROL, SOD_RULE and LEGAL_BASIS (their owners evaluate them).

Versions that were ACTIVE before migration 000012 are grandfathered. The gate applies at their
successors' activation. `POLICY_CERTIFICATION_GATE=warn|off` exists only for migration windows.

## V-001 PDC-01 authority sources, decision models and package/release split (P-10)

V-001 models immutable policy packages, decision-model versions, authority sources and release
bindings as separate aggregates. policy-svc keeps the GOV-05 model instead:
- a version is the package;
- its `artifact_digest` is the content hash;
- its `source` is free text, not a governed authority-source record;
- activation is the release binding.

Rollback is a new draft (`rollback-draft`), never a re-pointed binding. **Closes when:** a
service owns authority sources (V-001 PDC-01). This is roadmap.

## APPROVAL_THRESHOLD currency is optional on a version

PDC-I-11 wants currency explicit. A version may name `currency` (ISO 4217). When it does,
evaluation requires the same `action_context.currency`. A version without one still answers,
because today's callers send no currency (expense-claim-svc and payment-authorization-svc also
send float amounts, from their float domain models). payroll-run-svc sends the exact decimal
and the currency. **Closes when:** the two float callers move to decimals and currency; then
make `currency` mandatory on new APPROVAL_THRESHOLD versions.

## Duplicate migration numbers 000003 and 000009

Two files each share the numbers 000003 and 000009. `deployments/migrate.py` orders and tracks
migrations by file name, so they apply deterministically, and renaming them would orphan the
applied-migration records of every existing database. `TestMigrations_NumberedOnceWithDownFiles`
allows exactly these two numbers and refuses any new duplicate.

## P-07: `X-Purpose-Context: policy_evaluation_evidence` on ledger appends

This header is sent because governance-decision-log-svc requires it on appends (its decision
D-06). **Closes when:** D-06 moves purpose to reads only; then drop the header.

## Attestation revocation has no If-Match

Attestations carry no `row_version`. Revocation is a one-way, status-guarded transition
(ACTIVE/CHALLENGED → REVOKED), and a second revocation is `409`. A concurrent revocation
therefore cannot overwrite anything.
