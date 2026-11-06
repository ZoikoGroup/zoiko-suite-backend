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

## Master data lives here, not in separate MDM services (R15, ORG-07 and MDM-01 / MDM-05)

ZS-SVC-U-001 describes MDM-01 to MDM-05 as services. They are built as modules of this service, with their own
tables, so a legal entity (ORG-03), its parties (ORG-07) and its ownership (ORG-04) commit in one database. A
consumer that follows the spec's `/v1/parties` surface finds it here.

Party kinds, the match ladder, the create flow and the non-destructive merge are as ZS-SVC-U-001 s9, s10 and s18.
Where the spec leaves a choice, the code chooses the safer reading:

- **No automatic link below tier A.** Tier A (a verified identifier in the right scheme and scope that names one
  active party) may return the existing party. An identifier the tenant only asserted is tier B, and a name match
  is tier B or C: all of them answer `REVIEW_REQUIRED` and open a case. The spec allows a governed threshold to link
  tier B for low-impact object types; none is configured, so none links.
- **An authority source verifies on arrival; nothing else does.** `AUTHORITY_REGISTRY` and `AUTHORITY_DOCUMENT`
  verify a fact or identifier when it is recorded. A tenant's own assertion is held `PROPOSED`. A fact that
  contradicts a verified one is held beside it and opens a FACT_CONFLICT case, even from an authority: replacing a
  verified fact is a steward's decision (s19), not a side effect of a write.
- **Scheme-proven needs a verified identifier.** The one exception is the source-system reference
  (`SOURCE_SYSTEM`, scope = the system), which exists to make an ingest replay-safe.
- **Sensitive identifiers** (`TAX_ID`, `NATIONAL_ID`) are masked in every read and answer; unmasking is
  `PARTY_IDENTIFIER_REVEAL` and is refused, not silently masked, when asked for without it. No event carries an
  identifier value, a name or evidence text.
- **Identifier verification through XIC** (`POST /v1/identifiers/{id}:verify`) is not built here; the verification
  state comes from the source class of the link until it is.
- **Search reads the authoritative tables.** The spec expects a search projection; a projection lags a write, and
  the answer to "does this party already exist" must not.

### Merge, split and stewardship (MDM-05, wave 2)

- **Merge is a case decision.** `POST /v1/resolution/{case}:merge` decides an open DUPLICATE_REVIEW or
  IDENTIFIER_CONFLICT case. Every party involved must be named by the case, and the steward sends the version of each
  (s18.2 step 2). The spec's "authorization/WFC approval where material" is built as separation of duties: the
  principal who opened the case cannot decide it, and the principal who merged cannot split. Every merge is treated as
  material because this service cannot see which parties are linked to financial records. A workflow approval step in
  front of the command is the place to relax that for low-impact objects; none is configured.
- **Absorbed ids stay.** A merged party keeps its row, status `MERGED`, resolving to the survivor. Merges do not chain:
  a survivor that was merged away, or a party that already absorbed others, is refused rather than re-pointed.
- **Identifiers and roles are consolidated here; references are not.** The survivor gains the identifiers and roles it
  lacked (new rows naming their source; the old rows end). `party.merged` carries `remediation_scope`, and each domain
  re-points what it owns prospectively through the alias. A historical snapshot is never rewritten.
- **A split restores, it does not guess.** Identifiers and a legal-entity link the merge moved go back; roles the merge
  consolidated are left for the domains to re-resolve from `party.split`. The merge stays in the lineage.
- **Matching skips merged parties.** A create resembling a merged-away name matches nothing; names are not copied to
  the survivor. If that matters, steward the survivor's names with `facts:propose`.
- **Decisions are not a merge.** `CREATE_NEW`, `LINK_EXISTING`, `DISMISS`, `ACCEPT_PROPOSED_FACT`, `KEEP_VERIFIED`,
  `KEEP_HOLDER` and `MOVE_TO_REQUESTER` settle the other cases. Accepting a proposed fact supersedes the verified one
  rather than editing it, and records the accepted fact as verified at decision time, so "what did we know then" still
  has its answer.

### Addresses (ORG-08 and MDM-03, wave 1)

- **An address is an identity with append-only versions.** `POST /v1/addresses` records one; `POST .../versions`
  amends it by closing the prior version and adding the next. Nothing is edited or deleted (database guards), and
  `GET /v1/addresses/{id}?as_of=` returns the version in force then. The structured fields are kept exactly as
  entered; the normalized form beside them only aids matching and search.
- **Verification is an observation, not a certificate.** `:verify` runs a deterministic local rule check (country is
  an assigned ISO 3166-1 code, required fields present, postal code fits the published format where one is fixed) and
  appends the result. Only a confident PASSED moves a version to VERIFIED; a country with no rule is surfaced as
  LOW_CONFIDENCE, a violation is FAILED, and neither changes the address. It never reports full confidence, because it
  does not geocode or confirm delivery. The external provider verifier is BLOCKED (503) until the external data feed
  service exists; it never falls back silently to the local rules.
- **A postal address is not a registered office (MDM s14.2).** A REGISTERED address is recorded, or amended, only with
  source_class AUTHORITY_REGISTRY or AUTHORITY_DOCUMENT; verifying never changes an address's type, and an address is
  linked to a party only in its own capacity. A party has one live registered-office link; a new one ends the old on its
  start date and the old stays queryable (`GET /v1/parties/{id}/addresses?as_of=&include_ended=`).
- **Country and subdivision are resolved against a built-in ISO 3166 list, not MDM-04 reference ids.** The reference
  module (ORG-8) is not built yet; when it is, the verifier should resolve through it.

### Establishments and the registered office on the address model (ORG-08, wave 2)

- **An establishment is not its address.** It has its own id, stands at an address and is recorded from evidence
  (`evidence_ref` and `source_class` required). Nothing is inferred from an address: no establishment, nexus or
  registration follows from one existing. Events carry no name or address, and state no tax conclusion.
- **Named status commands, recorded as transitions.** `:activate`, `:deactivate` and `:close` move PROPOSED, ACTIVE,
  INACTIVE and CLOSED (final) by the lifecycle, each appending a transition with its effective date, evidence, actor and
  versions. `GET /v1/entities/{id}/establishments?as_of=` reconstructs what was active on a date from the transitions,
  not from the current status. The spec's `ChangeEstablishmentStatus` is one service operation behind the three routes
  (ST-3: no generic status write).
- **Segregation of duties.** A material establishment (BRANCH, PERMANENT_ESTABLISHMENT) cannot be activated by the
  principal who proposed it (403 SOD_DENIED); a table constraint holds it even if the service is bypassed. The spec says
  "where policy requires independent evidence"; no policy source exists yet, so materiality is by type. The other types
  may be activated by their proposer.
- **The registered-office route is kept and mirrored, not replaced.** `POST /v1/entities/{id}/registered-office` still
  writes the ORG-03 profile version, which remains the legal record. When the office is a structured address (`lines`
  or `line1`, `locality` or `city`, a two-letter `country`; key names matched leniently) and the command carries evidence,
  the same transaction also records a REGISTERED address (source AUTHORITY_DOCUMENT, because the command is evidenced and
  approved under ORG-03) and links it to the entity, ending the previous link on the new start date.
  `GET /v1/entities/{id}/addresses?as_of=&include_ended=` reads it. Two cases stay in the profile history only: an
  unstructured office (always accepted, unchanged) and a change that does not start after the current office began (a
  backdated correction would have to rewrite link intervals). The mirror emits no extra event: `entity.registered_office.changed`
  already attests the change.

### Registrations and licences (ORG-09 and MDM-03, wave 1)

- **Evidence, not conclusions.** A registration records what an authority was shown to say and the lifecycle of that
  evidence. It decides no tax semantics (a `TAX_REFERENCE` is a reference; TAX-01 stays authoritative), no legal
  sufficiency and no nexus. Events state the type, jurisdiction, status move and dates, never the number.
- **Unverified cannot be active.** A registration is created DRAFT. `ACTIVE`, `SUSPENDED`, `EXPIRED` and `SUPERSEDED`
  all require a recorded verification, and the table refuses otherwise. `in_force` (ACTIVE, begun, not past its expiry
  date) and `lapsed` are computed from the dates on every read, so a registration past its date never reads as active
  even when the sweep has not yet recorded it. A DRAFT is never in force.
- **Lifecycle.** `:verify` takes DRAFT or SUSPENDED to ACTIVE on evidence (a registration already past its expiry is
  renewed, not verified); `:renew` moves the expiry later on evidence and returns an EXPIRED one to ACTIVE; `:suspend`,
  `:expire` and `:revoke` need a reason and evidence; REVOKED and SUPERSEDED are final. Every step appends a history row
  with the dates, evidence, evidence hash, actor and versions (`GET /v1/registrations/{id}/history`). Creating with
  `supersedes_id` ends the older registration as SUPERSEDED when the new one is verified.
- **Segregation of duties, by type.** PERMIT, LICENSE and REGULATED are material: they are not verified by the principal
  who created them, and not renewed by the principal who last verified or renewed them (403 SOD_DENIED; the first is also
  a table constraint). The spec says "where policy says"; no policy source exists yet, so materiality is by type.
- **Candidate uniqueness without exceptions.** One live registration per authority, type, jurisdiction and normalized
  number (409 DUPLICATE_CANDIDATE); an ended registration frees its number. The spec allows jurisdiction-specific
  exceptions; none are modelled until a jurisdiction pack says where a number is legitimately shared.
- **Sensitive numbers are masked.** TAX_REFERENCE and EMPLOYER_PAYROLL numbers show their last four characters in every
  read, search and expiry list unless `reveal=true` and the caller holds `REGISTRATION_NUMBER_REVEAL` (refused, not
  silently masked, without it).
- **The expiry sweep is a background loop.** `main.go` runs it hourly: it records the expiry of registrations past their
  date (actor `system:expiry-sweep`, history row and `registration.expired` in one transaction) and publishes
  `registration.expiring` once per expiry date, 30 days ahead; a renewal to a new date warns again. Which tenants have
  anything due is found by one read-only, due-rows-only policy (`app.registration_sweep`); everything it then changes is
  done inside that tenant's own policy.
- **FindByNumber is new, not an extension.** `GET /v1/registrations:find` matches the normalized number; the ORG-03
  registry-number lookup for legal entities is unchanged. Identifier verification and LEI alignment (`POST
  /v1/identifiers/{id}:verify`) are wave 2.

### Identifier verification and LEI alignment (ORG-09 and MDM-03, wave 2)

- **A verification is an observation, applied by rules.** `POST /v1/identifiers/{id}:verify` records what a source said
  (MATCH, MISMATCH or NOT_FOUND) with its class, reference, version, exchange reference, time and evidence, always kept
  (`GET /v1/identifiers/{id}/verifications`). Whether it changes the identifier is `domain.DecideObservation`: a provider
  or authority MATCH verifies; a tenant assertion or system import never does; an observation older than the freshness
  window (90 days for an LEI, 365 for other schemes) or older than the one already applied is recorded and not applied; a
  higher authority takes over a verification (SOURCE_UPGRADED); a lower or equal one that disagrees changes nothing. It is
  not last-write-wins. The response's `effect` and `effect_note` say which happened, and `party.identifier.verified` is
  published only for VERIFIED and SOURCE_UPGRADED.
- **The service calls no authority.** s14.1 step 3 puts external exchange in XIC, not in MDM-03. No XIC client exists
  yet, so the observation is supplied by whoever called an authority through the exchange layer, or by a steward holding
  the evidence; `exchange_ref` carries the exchange's own reference. Wiring a caller is a later integration, not a change
  to these rules.
- **A higher-authority disagreement opens a case instead of overwriting (step 6).** A MISMATCH or NOT_FOUND from a source
  that outranks the one that verified the identifier opens a `VERIFICATION_CONFLICT` stewardship case and leaves the
  identifier CONFLICTED, with its verifying source intact. A CONFLICTED identifier is not one a create links to
  automatically, and no new observation moves it. The case is decided by someone other than whoever opened it:
  `KEEP_VERIFIED` restores VERIFIED, `ACCEPT_OBSERVATION` makes it FAILED. "Registry conflicts" (the ORG-03 entity
  registry-number quarantine) are not reused: those are about two entities claiming a number, and this is the party
  stewardship case the rest of MDM-05 already uses.
- **Source precedence is by class.** AUTHORITY_REGISTRY, then AUTHORITY_DOCUMENT, then EXTERNAL_PROVIDER, then
  SYSTEM_IMPORT, then TENANT_ASSERTED. The spec says precedence is "by fact type" and per policy; no policy source exists,
  so one order applies to every scheme.
- **LEI (s13.3).** An LEI must be a well-formed ISO 17442 identifier to be verified (400 otherwise; linking one is still
  unchecked, as before). A MATCH verifies only when the LOU reports `authority_status` ISSUED; LAPSED, RETIRED and the
  rest are recorded and verify nothing. A verified LEI is compared with the legal entity profile's: `lei_alignment` is
  ALIGNED, PROFILE_ABSENT, DIFFERS, NOT_LINKED or NOT_APPLICABLE. ORG-03 owns the profile, so a difference is reported and
  never written over. GLEIF relationship observations (parent and ultimate parent) are not ingested.
- **Existing identifiers.** Identifiers already VERIFIED when the migration ran are backfilled with the source that linked
  them, and a trigger records it for every later insert, so no linking code changed.

### Exchange rates (REF-03, ORG-8 wave 1)

- **Where it lives.** REF-03 is built as a module of this service, like MDM-01 to MDM-05, because the plan puts the
  reference sets here. Currency identity is not owned here: currency-registry-svc (REF-02) is.
- **Exact decimals.** A rate is a string at every boundary (`^(0|[1-9][0-9]{0,19})(\.[0-9]{1,18})?$`), `NUMERIC(38,18)` in
  the table and `math/big` in code. No float touches a rate. A derived inverse or cross rate is exact arithmetic on its
  components, rounded half up to 18 decimals with the exact fraction returned beside it (`1.1 x 0.8` is `0.88`, `22/25`).
- **Currencies are confirmed read-only and fail closed.** Before an observation is accepted both codes are checked with
  currency-registry-svc (`GET /v1/currencies:validate?operation=read`: a rate is a reference fact, not a posting, so any
  currency that ever existed is acceptable for the record). An unknown code is 400 REFERENCE_RETIRED; a registry that
  cannot answer is 503 and the rate is not accepted. `CURRENCY_REGISTRY_URL` selects the client; unset, development and test
  use a shape-only stub and staging and production refuse ingestion rather than accept an unconfirmed currency. Config
  validation does not yet require it, so an unset URL in production is a refused command, not a refused start.
- **Rate type is mandatory and never conflated.** Outlier history, rate sets and reads are all per type. There is no
  default type.
- **Lifecycle.** OBSERVED, VALIDATED, APPROVED, SUPERSEDED. The spec's REJECTED has no command and no state here: an
  observation that fails validation is refused (4xx), not stored as rejected, and one that is wrong is superseded.
  Ingestion checks the source is approved, then duplicates (a provider observation id, or the same source, pair, type and
  instant: a replay returns the first, a different rate under the same key is 409), then the outlier check against the
  last accepted rate of the pair and type, and keeps every result as `checks`. A manual (MANUAL_EXCEPTION source) or
  outlying rate stays OBSERVED and needs independent approval; everything else is VALIDATED.
- **Outlier threshold is one constant.** A move of more than 10% from the last accepted rate of the pair and type. The spec
  says outlier policy is server resolved; no policy source exists yet, so one threshold applies to every pair.
- **Segregation of duties.** The principal who ingested a manual or outlying rate cannot approve it into a set (403
  SOD_DENIED, and a table trigger holds it if the service is bypassed). Including such a rate needs an `override_reason`.
  Registering and withdrawing a source is a single HIGH permission step, with no second approver: the spec names none.
- **Rate sets and certification.** `POST /v1/fx/rate-sets` creates a set already approved (the spec's ApproveFXRateSet):
  one rate per pair, all of the set's rate type and market date. `:close` certifies it. A closed set is immutable at the
  table. Several named sets can exist for one type and date (`set_name`, default `DEFAULT`); a read names the set.
- **Corrections are superseding sets.** A rate in a set is never edited or replaced in place. A correction is a new set
  with `supersedes_set_id`: the old set becomes SUPERSEDED and stays readable, rates the correction drops stop being in
  force, and `GetHistoricalRate` with `known_at` returns what was in force then. `SupersedeRateObservation` replaces only an
  observation not yet in a set.
- **No fallback, ever.** `GetRate` returns the approved direct rate or 404 `FX_RATE_MISSING`: never an older date, another
  type, another set or an unapproved observation. `ExplainDerivedCrossRate` tries direct, then inverse, then a cross
  through a pivot (`via`, default USD) and nothing else, and lists every component with its direction and set. The spec's
  "calendar cutoff" and "source timezone" resolution is not built: the source's timezone is recorded, and the market date is
  as the source states it; REF-10 calendars are not consulted.
- **`GET /v1/reference/{set}/{code}` is not built yet.** It is the last wave of ORG-8, over books, dimensions and units.

### Accounting books (REF-06, ORG-8 wave 2)

- **A book is an identity, and books stay separate.** Statutory, management, tax, consolidation and regulatory books are
  never conflated, even where their charts overlap, and there is no default book and no default type. A book states a
  basis. It holds no ledger lines or balances (ACC-05) and no accounting policy content (REF-07): `chart_ref` and
  `policy_ref` are references.
- **Owner and code.** A book belongs to a legal entity, or to a group (a `group_code`, not an entity id: the plan has no
  group object to point at) for consolidation and management books only. A statutory, tax or regulatory book is an
  entity's. The code is unique within its owner for ever, whatever the book's status: a retired code is not reused. There
  is no `create_request_id`; a retried create is a 409 on the code.
- **The basis is versioned and prospective.** Framework, functional and presentation currencies, chart and policy
  references are a version, effective-dated, with one open version at a time. For a book in use a new version starts
  today or later and after the current one began, so a basis change cannot rewrite how earlier periods were booked
  (s4.16). A DRAFT book, never in use, may be corrected on or after the day its basis starts: the replaced version is then
  zero-length and never in force. A retired book keeps its basis and takes no new one.
- **Allowed frameworks are by book type.** Statutory: IFRS, IFRS_SME, US_GAAP, LOCAL_GAAP. Management: MANAGEMENT_BASIS,
  IFRS, US_GAAP, LOCAL_GAAP. Tax: TAX_BASIS only. Consolidation: as statutory. Regulatory: REGULATORY_BASIS, LOCAL_GAAP.
  LOCAL_GAAP and REGULATORY_BASIS name the standard (`framework_detail`). The spec says allowed frameworks are server
  resolved; no policy source exists, so this table is the policy.
- **Currencies are confirmed, not matched.** Functional and presentation currencies are checked with currency-registry-svc
  like exchange rates (read-only, fail-closed). A functional currency different from the entity's default is not refused:
  a parallel book may legitimately differ. `ExplainBookBasis` states whether it matches.
- **Lifecycle and "could it post on that date".** DRAFT, ACTIVE, POSTING_SUSPENDED, RETIRED, each move a recorded
  transition with its effective date, evidence and actor. A suspended book is activated again with `:activate`. Whether a
  book could post on a date is reconstructed from the transitions and the basis versions, never from the current status:
  `GET /v1/books/{id}/posting-eligibility?as_of=` answers eligible only for a book that was ACTIVE then with a basis in
  force, and otherwise names the reason (NOT_ACTIVE, POSTING_SUSPENDED, RETIRED, NOT_YET_EFFECTIVE, NO_BASIS_IN_FORCE).
  This service answers the question; the posting service must ask it for the book it names. It cannot stop a posting
  itself.
- **Segregation of duties, by type.** A statutory, tax or regulatory book is not activated by the principal who created it
  (403 SOD_DENIED, and a table constraint). The spec says "controller-approved" for statutory and tax book activation;
  there is no controller role to name here, so independence from the creator is what is enforced, and who may hold
  `ACCOUNTING_BOOK_ACTIVATE` is a role-design decision.
- **Parallel books differ in framework.** An entity cannot have two live (ACTIVE or POSTING_SUSPENDED) books of the same
  type and framework for the three material types: a second would make "the" statutory book ambiguous. A parallel
  statutory book on another framework, a tax book and any number of management books can be live beside it. Activations
  of two such books are serialised so the check cannot be raced past. The consolidation-role rules the spec mentions are
  not modelled.
- **`GetBookAsOf` is `GetAccountingBook` with the date required**, at its own path; both return the book's status then,
  the basis then and the eligibility then.

### Financial dimensions (REF-08, ORG-8 wave 3)

- **Identity is separate from hierarchy and label.** A value's id and code never change (table guards). What it is called
  and where it sits are effective-dated versions of its own, so renaming or moving a node leaves its identity alone and a
  historical roll-up is reconstructable on either side of a change (`GET /v1/dimension-types/{type}/hierarchy?as_of=`,
  and the `path` on `GET /v1/dimension-values/{id}?as_of=`). The spec has no rename command; renaming is carried on
  `ReparentDimensionValue` (`new_label`), because the label is part of the same versioned placement.
- **Hierarchy changes are prospective and loop-free.** A change starts today or later and after the current version began,
  so it never rewrites a roll-up already reported. A loop is refused on the date the change takes effect and on every later
  date a version of the type's hierarchy begins (a loop needs only one such date); approval checks again.
- **Independent review is a type property.** `controlled` types (a hierarchy feeding reporting or control aggregation) hold a
  change as PENDING until someone other than its proposer approves it with `POST .../hierarchy/{n}:approve` (403 SOD_DENIED,
  and a table trigger holds it). Other types take effect directly. The approval command is not in the spec's list; it is how
  "independently reviewed" is realised. The first version of a new node is its placement, not a change, and needs no review.
  Which types are controlled is the steward's choice at type creation: no policy source says.
- **Codes are stable within their scope.** Unique per type for tenant-wide values and per entity for an entity's own; an
  entity's code never shadows a tenant-wide one, and a tenant-wide code never covers an entity's, so resolving by code is
  never ambiguous. The spec's "effective scope" is the value's owner; codes are not reused over time.
- **Inactive rejects new postings; a controlled exception is explicit and bounded.** DRAFT, ACTIVE, INACTIVE, each move a
  recorded transition, and INACTIVE can be activated again (a dimension is reference data, not a legal record). Whether a
  value could take a posting on a date is reconstructed from the transitions: ACTIVE only, or INACTIVE inside a recorded
  `exception_until` (after the deactivation, within 90 days of it, with a reason). This registry answers the question
  (`ListPostingEligibleValues`, and the eligibility on every read); the posting service must ask, because it cannot stop a
  posting itself.
- **Source mappings are versioned.** An operational source object maps to one value of a type at a time; mapping it again ends
  the previous mapping where the new one begins and keeps both. The value must be ACTIVE, and when the type names a
  `source_domain` the mapping must be in it. The spec says source mappings "may require joint approval": no approval step is
  built, as no policy source says which. `ResolveDimension` finds a value by code or by source object as of a date, and says
  not found rather than guessing.
- **Not built.** Roll-up rules beyond the hierarchy itself (the spec's "roll-up rules" server-resolved context), and the
  "impacted reporting nodes" lineage on a hierarchy change: the change event carries the old and new parent and label, and a
  consumer computes the impact from the hierarchy as of either date.

### Units of measure (REF-09, ORG-8 wave 4)

- **One base unit per dimension; conversions are exact fractions.** A quantity dimension (length, mass, count) is created with
  its base unit, ACTIVE at once. Every other unit converts to that base by a factor held as two whole numbers (NUMERIC, reduced,
  positive), parsed from a decimal or a fraction with `math/big`. No float touches a quantity. A conversion between two units
  is quantity x from / to, exact, and only the final figure is rounded (half away from zero, to the target unit's decimals).
  The unrounded result is returned beside it as a fraction.
- **Dimensionality is checked before arithmetic.** A unit has a factor to its own dimension's base and to nothing else, so
  a cross-dimension conversion cannot be stored; asking for one is 422 DIMENSION_MISMATCH naming both dimensions. A unit with
  no approved conversion in force on the date is 404 CONVERSION_MISSING, and a unit retired on the date is REFERENCE_RETIRED:
  never a guess and never a fallback to another date.
- **The applied version is named.** `ConvertQuantity` returns, for each unit, the conversion version number, exact factor,
  source and validity start it applied. The domain holding the quantity retains that pin with the business fact; this
  registry owns no quantities (the spec's "applied conversion version retained" is the caller's to store).
- **Conversions are versioned, effective-dated and prospective.** A replacement (`conversions:supersede`) starts today or
  later and after the active one began, so a quantity already converted is never rewritten; the old version ends where the new
  one begins and stays readable (`GET .../conversions/{n}`, `GET /v1/units/{code}?as_of=&history=true`).
- **Non-standard conversions are independently approved.** STANDARD (a defined constant such as ISO 80000) takes effect at
  once. ORGANISATION and MANUAL_OVERRIDE wait PENDING until someone other than the proposer approves
  (`conversions/{n}:approve`; 403 SOD_DENIED, and a table CHECK holds it). A manual override states why. Which source class a
  given conversion is belongs to the proposer: no policy source says which are standard. The approve command is not in the
  spec's list; it is how "manual override is not self-approved" is realised.
- **Retirement.** `RetireUnit` takes effect today (no future-dated retirement: a status that is not true yet would be
  misleading). History is kept; a past date still converts. A base unit retires only once every other unit of its dimension has.
- **Not built.** Nonlinear conversions (offsets such as temperature, or context-dependent ones such as density), which the spec
  says need specialised rules; the factor here is a pure proportion. No unit catalogue is pre-loaded: units are tenant master data.

### Reference releases (MDM-04, ORG-8 wave 5)

- **What `GET /v1/reference/{set}/{code}?as_of=` answers.** A code as it stood on the date, from published releases only: its
  status (ACTIVE, RETIRED or SUPERSEDED), label, origin (OFFICIAL or CUSTOM, kept distinct as the spec requires), metadata, the
  release that governs it (version, effective date, publication time, authority, compatibility, checksum, evidence), when it
  was added (`valid_from`) and when it stopped (`valid_to`), and the supersession from both sides (`superseded_by`,
  `supersedes`). A retired or superseded code is still returned, so a consumer reading old data can interpret it. A code not yet
  added on the date, or never added, is 404: nothing is guessed. `history=true` lists every published change.
- **Pinning a release.** `release=` answers under one exact published release and those before it, whatever the date, for
  processing that must interpret codes under the release it was built on ("exact reference set release controlling code
  interpretation"). The date is ignored when a release is pinned, and a staged release cannot be pinned.
- **A release is a set of changes, not a full snapshot.** Each entry is ADD, MODIFY_METADATA, SUPERSEDE, RETIRE or CORRECT, one per
  code per release. Entries are checked against the set as published: a duplicate ADD, a change to a code that is not active,
  a replacement that is not live once the release is applied, a custom code in a set that takes none, and a change of origin are
  refused naming the code. A retired code is not reused. The checks run at staging and again at publication, so two releases
  staged together cannot both be published if the first changes what the second assumed.
- **Releases are published in effective-date order.** A release takes effect after every release already published (service
  checks, and a trigger holds it), so published history is never rewritten; corrections are a later CORRECT release. A published
  release is final (trigger). The spec's publication timestamp and business effective date are separate columns.
- **Releasing is maker-checker.** The principal who staged a release cannot publish it (403 SOD_DENIED, and a table CHECK holds it).
  The spec has no stage/publish commands; this is how the "release gate" is realised.
- **Reference sets are per tenant.** Every other table of the service is tenant-scoped under row-level security, so a set (and its
  releases) belongs to a tenant and is read under that tenant. A platform-wide ISO baseline shared by all tenants would need a
  separate, cross-tenant read path that no existing spec section describes; it is not built. **No baseline is pre-loaded**: the
  ISO 3166/4217 and IANA releases are data a steward stages and a releaser publishes, with the retrieval evidence the spec asks for.
- **Not built.** Calendar profiles and time-zone resolution (MDM-04 s16.2), which belong to the deadline evaluation in WFC; the
  `checksum` is stored as provenance but not verified against a content digest, since no canonical form of a source file is defined.

### Master imports (ORG-9)

- **Rows arrive as JSON, not a file upload.** `POST /v1/master-imports` takes a `rows` array; there is no multipart or file-store path. The content SHA-256 is
  computed over the rows received. Raw rows are not returned by any read: only the per-row outcome and its errors.
- **Always asynchronous.** Upload answers 202 with `Location: /v1/operations/{id}`; approval is also an operation. The same `Idempotency-Key` returns the same
  operation, and the same key with a different request is a conflict.
- **Party is the only kind.** Other master-data kinds are refused.
- **Approval is maker-checker.** The uploader cannot approve (service and table CHECK), and any INVALID row blocks approval.
- **Import goes through the ordinary party create path** as the uploader, so matching and review apply. A row that matches an existing party or opens a review
  is recorded as CONFLICT and never overwrites. The request id `imp:{import}:{row}` makes a rerun idempotent.
- **Reconciliation counts the rows themselves**, not the counters kept during the run. An import is COMPLETED only if every row is accounted for. Per-row
  errors are kept.
- **Cancel.** Allowed until the import starts. `StartImport` is the point of no return: it marks the operation not cancelable, and a later cancel is 409.
  A cancel requested before start cancels an APPROVED import.
- **No startup resume sweeper.** An import interrupted by a process restart is not resumed automatically; it needs an operator to re-run it (the row request
  ids make that safe).
