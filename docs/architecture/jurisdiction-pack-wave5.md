# ZS-JUR-001 Wave 5: e-invoice profiles, statutory filing profiles and the submission lifecycle

Builds on Waves [0](jurisdiction-pack-wave0.md) to [4](jurisdiction-pack-wave4.md), [6](jurisdiction-pack-wave6.md) and [7](jurisdiction-pack-wave7.md). Implemented in
`jurisdiction-rules-svc` (migration `000014`). Follows ZS-JUR-001 s13, s14, s33 and JUR-NEG-08, 09, 10, 11, 27.

## What the document asks for (s37 Wave 5)
"Profile registry, schema/code-list dependencies, adapters, receipts/rejections." Three of the four are built. **Adapters are not.**

## Decision: profiles are rule modules with two more closed families
As for Wave 6, a profile is a governed rule module with typed `parameters` (families `EINVOICE_PROFILE` in the `EINVOICE_PROFILE` domain, `FILING_PROFILE` in the
`STATUTORY_FILING` domain). It inherits provenance, independent publication, signing, certification, rollout and rollback. The compiler refuses a profile filed
under any other domain and refuses a rule in those domains that carries no profile (`JUR-C092`, `JUR-C093`).

## The profile (s13, s14)
* **Identity:** `channel` (network or authority), `document_type`, `profile_version` (a decimal string), the rule's effective window.
* **Syntax and transmission:** `UBL_XML`, `JSON` or `AUTHORITY_SPECIFIC`; e-invoice modes `EXCHANGE`, `CLEARANCE`, `REPORTING_REALTIME`, `REPORTING_POST_ISSUANCE`; filing modes `FILING_DIRECT`, `FILING_VIA_PROVIDER`.
* **Timing:** optional issue deadline, retry window, correction window. **Corrections:** the permitted workflows (`CREDIT_NOTE`, `CANCEL`, `AMEND`, `AMENDED_FILING`).
* **Dependencies:** pinned external schema, code-list and business-rule versions, each with `valid_from` and `valid_to`. At least one is required: no schema is assumed.
* **E-invoice only:** `semantic_mapping` (canonical field to target element). **Filing only:** `obligation_code`, `approval_roles`, `retention_class`.
* **Lifecycle:** initial state, terminal states, legal transitions and the states that need a receipt. The validator guarantees: terminal states cannot be left, nothing returns to the
  initial state, every state is reachable, and **`ACCEPTED`, `CLEARED` or `FILED` must require a receipt** (no false accepted state, JUR-NEG-09).
* **Never in a profile:** endpoints, credentials, certificates (s39). The schema is closed; an unknown field such as `api_key` is refused.

## Runtime
* `POST /v1/submission-profiles:resolve`: the verified profile in force. When the caller states the schema, code-list and rule versions it built the payload with, each is checked against the profile:
  `DEPENDENCY_UNKNOWN` (not declared, JUR-NEG-27) or `DEPENDENCY_EXPIRED` (outside its window, JUR-NEG-08). The failure names the exact reference, version and date. Evidence kind `PROFILE`.
* `POST /v1/regulatory-submissions`: registers a submission under the profile in force, refusing (and registering nothing) on a dependency failure. **A filing also needs an approval for every
  required role from a principal other than the preparer, and one principal cannot hold two roles** (`APPROVAL_MISSING`). The submission snapshots the profile and records pack release, artifact digest
  and rule digest, so a later pack release cannot change an in-flight lifecycle. Idempotent per caller.
* `POST /v1/regulatory-submissions/{id}/events`: records an authority or provider status report under a row lock.
  * duplicate `provider_event_id` is stored once and returns the original outcome (`replayed`, JUR-NEG-10);
  * `AFTER_TERMINAL`: a late or contradictory report cannot change a final outcome (JUR-NEG-11);
  * `STALE`: a report older than the last applied one; `ILLEGAL_TRANSITION`; `MISSING_RECEIPT`; `UNKNOWN_STATUS`.
  Always HTTP 200 once stored, with `applied` and `disposition` in the body, so a webhook caller is not driven into retries.
* `GET /v1/regulatory-submissions/{id}`: the submission and every report, applied or not.
* **Database guards (000014):** a terminal submission cannot change, only `status` and its bookkeeping can change, submissions are never deleted, status reports are append-only.

## Certification
Profiles are parameter-only rules: a GOLDEN case must resolve the rule and pin `expect.parameter_amount` to the profile version (`JUR-T030`). No new harness case kind.

## Authorization (demo seed bundle `JURISDICTION_RESOLVER_CALLER`)
`SUBMISSION_PROFILE_RESOLVE`, `REGULATORY_SUBMISSION_CREATE`, `REGULATORY_SUBMISSION_VIEW`, `REGULATORY_SUBMISSION_RECORD_EVENT`.

## Not built
* **Adapters and transmission.** Nothing is sent to an authority, network or provider. Endpoints, credentials and certificates stay in the provider and secret-store layer. The
  country-by-country provider strategy is a pre-production decision (s38) owned by Product, Partnerships and Tax Engineering.
* **Payload generation and validation:** no UBL or authority XML is built, and no schema validation or semantic validation is run. The profile states the mapping and dependencies; the producer
  of the payload is a provider or adapter.
* **Dependency content:** the schema and code-list contents are not stored or fetched here. A profile pins `ref` and `version`; the artifact registry for the files themselves is a separate platform
  decision (s38).
* **Timing enforcement:** issue deadlines, retry windows and correction windows are carried and returned but not enforced; nothing retries or expires a submission.
* **Corrections:** the permitted workflows are data on the profile; no credit, cancel or amend workflow is executed.
* **Events:** no `submission.*` domain events are published yet.
* **Provider reconciliation:** the document's "provider/authority state reconciliation" is limited to the lifecycle rules above; there is no polling and no reconciliation job.
* Dependency availability per region (JUR-NEG-15) is not checked for profile dependencies; the pack-level dependency gate from Wave 7 is unchanged.

## Deploy order
Apply migration `000014` before the new binary. The down script drops the submission tables and re-narrows the evidence-kind check as `NOT VALID` (evidence is append-only).

## Verification
Unit tests: profile validation (including credential smuggling, unreachable and terminal-leaving lifecycles, false accepted), dependency validity windows (inclusive end, early start, unknown
version), approval segregation, and the event decision matrix. Integration (embedded Postgres 16): profile resolve with dependency outcomes and evidence, registration and refusals (nothing registered on
an expired dependency), idempotency, the full lifecycle with missing receipt, stale, duplicate, late and contradictory reports, database guards attacked in SQL, filing approvals, reserved-domain compile
refusal, and the 000005-000014 down/up round trip.
