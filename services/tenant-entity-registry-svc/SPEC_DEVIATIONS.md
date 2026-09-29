# tenant-entity-registry-svc — spec interpretations and proposed doc corrections

Spec: `ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications.docx`,
§4.2 (ORG-02 Tenant) and §4.3 (ORG-03 Legal Entity). Audit:
`group1-docx-compliance-audit-2026-09-23.md`, section 2/9.

This file records where the service deliberately reads the spec in a particular
way, and the doc corrections proposed where the code is right and the text is
not. Each item was decided by the service owner on 24 Sep 2026.

## Proposed doc corrections (code unchanged)

### Home-region change: the command §4.2 does not name (implemented 28 Sep 2026)
§4.2's SoD row requires maker-checker for "home-region changes" but names no
command. Implemented as `ChangeHomeRegion` (`POST /v1/tenants/{id}/home-region`,
migration 000012): platform-scope authority (`TENANT_HOME_REGION_CHANGE`), always
filed for independent approval (`..._APPROVE`), `home_region_decision_ref`
required, the default residency policy's region re-pointed, a lineage row
(`tlh_home_region_evidenced` refuses one without evidence or approver), and
`tenant.home_region.changed`.

**Proposed wording:** add `ChangeHomeRegion` to §4.2 "Named commands" with that
authority, so the next revision of the spec names what the code does.

### expected_version is required (reversed 28 Sep 2026)
The earlier reading — optional on the wire, substituted with the version just
read — is withdrawn. Substitution guards a race inside one request but not a
caller acting on a stale view, which is what §3 "All material master updates
require optimistic concurrency" is for. Every named command now refuses a
missing `expected_version` (400 `VALIDATION_FAILED`) outside local development
(`EXPECTED_VERSION_OPTIONAL`, dev-only).

### Routes outside ORG-02 / ORG-03
The service also serves workspaces, entity hierarchies, entity–jurisdiction
assignments, residency policies and regions, tax identity bundles and tenant
host bindings. They are **out of scope for the ORG-02/03 audit** and need their
own audit against the sections that own them:

| Surface | Likely owning spec |
|---|---|
| Entity hierarchies | ORG-04 (ownership/control relationships) or ORG-05 |
| Entity–jurisdiction assignments | ORG-09 (registration) / REF-01 |
| Residency policies & regions | GOV-02 GTRM |
| Tax identity bundles | TAX-01 (tax registration semantics) |
| Workspaces | doc7 §A5/§T (commercial workspace) |
| Host bindings | ORG-02 (ResolveTenantByHost) — in scope, already covered |

Only the isolation-identifier and sensitive-identifier guards below touch them.

## Interpretations

| Spec text | Reading implemented |
|---|---|
| §4.2 "maker-checker" (who owns the approval decision) | In-service two-step: commands file an `approval_requests` row (202); a different verified caller releases it via `/v1/approval-requests/{id}/approve`. |
| §4.2 "for controlled environments" | Always, in every environment. |
| §4.2 "tenant admin cannot change hard isolation identifiers" | tenant_id, tenant_code, host bindings, default residency-policy pointer. No write path changes the first two or the pointer; host binding is authorized in the **platform** scope only. |
| §4.2 "Provisioning/FailedProvisioning with compensating cleanup" | Core tenant + policy insert is atomic. The follow-on step (creation approval + `tenant.created`) failing marks `FAILED_PROVISIONING`. `RetryProvisioning` re-runs it; `AbandonProvisioning` (maker-checker) deactivates host bindings and residency policies and terminates, deleting nothing. |
| §4.2 "Create by approved onboarding correlation / external customer key" | `external_customer_key` required; same key + same request → original tenant (200); same key + different request → 409. |
| §4.3 "Draft → Verified → Active" | New entities are DRAFT. `VerifyLegalEntity` is independently approved (approver ≠ requester ≠ creator). `ActivateLegalEntity` moves VERIFIED → ACTIVE. "Inactive" is the existing DORMANT. DRAFT/VERIFIED entities cannot be transacted against. |
| §4.3 `MergeDuplicateCandidate` vs §1 "destructive merge is prohibited" | Non-destructive: the duplicate becomes DORMANT with `merged_into_legal_entity_id` and an `entity_merge_records` row; nothing deleted or re-pointed; reversible via `UnmergeEntity`. Both directions maker-checker. |
| §4.3 "sensitive identifier access scoped" | Tax identity bundles classified RESTRICTED/CONFIDENTIAL need `ENTITY_SENSITIVE_IDENTIFIER_READ` **and** `X-Purpose-Context`. Registry numbers and LEIs are public-registry data and stay unmasked. Note: this service stores no tax *numbers* (TAX-01 owns them), so scoping gates the bundle headers. |
| ORG-03 control "store LEI as an external organizational identifier with source/status" | LEI on the effective-dated profile version with `lei_source` and GLEIF `lei_status`; ISO 17442 MOD 97-10 validated; changing it is an independently approved identity change. LEI de-duplication is not implemented (registry number + jurisdiction remains the dedup signal, per §4.3). |

### Added 28 Sep 2026 (re-audit)

| Spec text | Reading implemented |
|---|---|
| §4.3 "profile versions effective-dated" — an amendment effective **before the entity's first version** | It becomes history, not the present: it ends exactly where version 1 begins, and the current name stays version 1's. Business time is the axis; the first version is a later fact. Before 28 Sep such an amendment stayed open-ended alongside version 1, and as-of "now" disagreed with `GetEntity`. |
| §4.3 — two amendments effective at the **same instant** | The second corrects the first: the first is superseded in record time (`superseded_at`) and keeps its business interval; the as-of tie-break (higher `version_number`) gives the interval to the correction. Nothing is deleted. Before 28 Sep this answered 500. |
| §3 "Idempotency: replays return the original material result" | Per (tenant, method + concrete path, key), fingerprinted on principal + body. A retry is answered from the stored response with `X-Idempotent-Replay: true`. 4xx answers are recorded (a refusal is a terminal result); 5xx are not. 7-day retention. Provisioning is keyed in the caller's scope; `external_customer_key` remains the business-level dedupe for tenant creation. |

### Added 28 Sep 2026 (gap closure)

| Spec text | Reading implemented |
|---|---|
| §3 "stable typed errors: CONTEXT_INVALID, …" | Every error body has `error_code`. The nine §3 codes are used where they fit; the list names no code for not-found, malformed input, authorization, idempotency reuse, a policy refusal at provisioning or a server fault, so these extend it: `NOT_FOUND`, `VALIDATION_FAILED`, `AUTHORIZATION_DENIED`, `IDEMPOTENCY_MISMATCH`, `IDEMPOTENCY_IN_FLIGHT`, `JURISDICTION_RESTRICTED`, `NOT_ENTITLED`, `INTERNAL_ERROR`. `RULE_AMBIGUOUS` is used for the one resolution this service performs (a tenant's residency region). |
| §4.2 "Required source inputs … primary jurisdiction … residency preference; onboarding evidence" | `primary_jurisdiction_id`, `residency_region_id`, `onboarding_request_ref` required at CreateTenant (000013). The residency preference becomes the default residency policy's region — the home region from birth. |
| §4.2 "Server-resolved context: available regions; plan entitlement; … restricted-jurisdiction checks" | Region must exist and be active; `subscription_id` checked with commercial-account-svc (ACTIVE or EVALUATION may provision); the primary jurisdiction's code is refused if on `RESTRICTED_JURISDICTION_CODES`, a compliance-owned list (config, required in staging/production; `NONE` states no restrictions). All fail closed. |
| §4.3 "Legal-form mapping may use ISO 20275/ELF code … preserving local legal-form text/source" | An ELF code must be four alphanumeric characters and needs `legal_form_source` and `legal_form_local_text`. Validation against the ELF list itself waits on §11's pre-production decision on authoritative code-list sources. |
| §4.3 "permitted calendar/currency references" | `fiscal_calendar_id` must be a UUID. REF-04 Fiscal Calendar does not exist in the estate, so the reference cannot be resolved against its owner — a dependency gap, not a service choice. |
| §3 "purpose limitation" | Registry-conflict quarantine rows (which hold a rejected claimant's payload) need `ENTITY_REGISTRY_CONFLICT_READ`; approval requests need `APPROVAL_REQUEST_READ` to list or read (deciding implies reading). |

### Added 29 Sep 2026 (re-audit)

| Spec text | Reading implemented |
|---|---|
| §4.3 "Required source inputs: legal name; entity type/legal form; incorporation jurisdiction; registry number/date; registered address; functional-currency preference; fiscal-calendar reference; supporting evidence" | `registered_office` and `source_evidence_ref` required at CreateEntity (relaxed only by `LEGACY_PROVISIONING_INPUTS`). The legal form is optional — `entity_type` answers "entity type/legal form" and not every entity has an ELF code — but when sent is held to the ELF control. All are recorded on profile version 1. Before 29 Sep none of the legal-form, registry-authority, registered-office or evidence fields existed on the create request, so a client that sent them had them silently discarded (and an invalid ELF code was accepted with a 201). |
| §3 "stable typed errors" — a malformed identifier | `VALIDATION_FAILED` (400). Every route taking an id answered 500 `INTERNAL_ERROR` to a non-UUID or empty id before 29 Sep; mapped once, at the store's RLS boundary (SQLSTATE 22P02). |
| §8 NP3 — which hostname | The guard compares the request's `Host`. The ingress must preserve the client's `Host` (the GCP load balancer does by default); a proxy that rewrites it to the service name silently disables NP3. `X-Forwarded-Host` is deliberately not trusted: any client can set it. |

### Contract changes a client must adopt (oasdiff, `scripts/contract_gate.sh`)
16 deliberate breaking changes against the 24 Sep contract — 14 on 28 Sep, and
on 29 Sep `registered_office` and `source_evidence_ref` required on
`POST /v1/entities`. The 28 Sep fourteen:
`expected_version` required on every command body; `primary_jurisdiction_id`,
`residency_region_id`, `subscription_id` new required and
`onboarding_request_ref` now required on `POST /v1/tenants`; `ChangeHomeRegion`
added to `command_name`. The Next.js console sends none of the provisioning
fields and no `external_customer_key` (it has been unable to create tenants
since 24 Sep); locally, `LEGACY_PROVISIONING_INPUTS=true` and
`EXPECTED_VERSION_OPTIONAL=true` keep it working until it is migrated.

## Not addressed
Nothing set aside. Outside this service: REF-04 Fiscal Calendar (above);
dependent services' own integration controls (§9.2 gate 6); production
certification of recovery (§9.2 gate 7 — see SLO.md).

## Dev-only compatibility flags
All refused at boot in staging and production.

| Flag | Restores |
|---|---|
| `MAKER_CHECKER_LEGACY_BODY_APPROVER` | body-supplied approver (pre-000007) |
| `LEGACY_ENTITY_CREATE_ACTIVE` | entities created straight into ACTIVE |
| `ONBOARDING_KEY_OPTIONAL` | provisioning without `external_customer_key` |
| `LEGACY_PROVISIONING_INPUTS` | provisioning without the §4.2 inputs (000013), and entity creation without `registered_office` / `source_evidence_ref` (§4.3) |
| `EXPECTED_VERSION_OPTIONAL` | substitution of the read version for a missing `expected_version` |
