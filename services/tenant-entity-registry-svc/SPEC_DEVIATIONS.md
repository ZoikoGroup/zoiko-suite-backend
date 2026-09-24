# tenant-entity-registry-svc — spec interpretations and proposed doc corrections

Spec: `ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications.docx`,
§4.2 (ORG-02 Tenant) and §4.3 (ORG-03 Legal Entity). Audit:
`group1-docx-compliance-audit-2026-09-23.md`, section 2/9.

This file records where the service deliberately reads the spec in a particular
way, and the doc corrections proposed where the code is right and the text is
not. Each item was decided by the service owner on 24 Sep 2026.

## Proposed doc corrections (code unchanged)

### Home-region change maker-checker is unimplementable as written
§4.2's SoD row requires maker-checker for "home-region changes", but the command is named only
there — not in §4.2 "Named commands" — and the tenant's home region is anchored by the default
residency-policy pointer, a hard isolation identifier the same section says a tenant admin cannot
change. There is no write a tenant admin could make to a home region, and no command name this
service could evidence, so the control as written cannot be implemented without another decision.

**Proposed wording:** name the command (e.g. `ChangeHomeRegion`) in §4.2 "Named commands" and
state the authority that may invoke it (platform scope, like a host-binding change). Until the doc
does, the control stays set aside (see "Not addressed"); the audit ❌ stands as a doc gap, not a
code defect.

### expected_version is optional on the wire, never optional in effect
§4.2: "lifecycle commands use expected_version". §4.3: "commands use UUID and
expected_version".

**Proposed wording:** "Lifecycle and profile commands are compare-and-swap on
the record's version. A caller MAY supply `expected_version`; when it does, a
mismatch is refused (409). When it does not, the service substitutes the version
it read, so the write is still guarded against a concurrent change."

**Why:** making it mandatory would break every current caller for no change in
safety — the guard runs either way.

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

## Not addressed (set aside by the owner)
- **Maker-checker on home-region changes.** No home-region change command exists;
  whether to add `ChangeHomeRegion` is pending a decision.

## Dev-only compatibility flags
All refused at boot in staging and production.

| Flag | Restores |
|---|---|
| `MAKER_CHECKER_LEGACY_BODY_APPROVER` | body-supplied approver (pre-000007) |
| `LEGACY_ENTITY_CREATE_ACTIVE` | entities created straight into ACTIVE |
| `ONBOARDING_KEY_OPTIONAL` | provisioning without `external_customer_key` |
