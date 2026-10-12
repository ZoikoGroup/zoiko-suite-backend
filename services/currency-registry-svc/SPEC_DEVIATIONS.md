# SPEC_DEVIATIONS — currency-registry-svc (REF-02)

Every place this implementation interprets, narrows, extends or departs from
the spec (REF-02, spec lines 706-748; shared contract 192-218; API patterns
1219-1248; negative paths 24-25; DoD 1512-1531) or from the approved design.
Items are ordered roughly by how much a reviewer should care.

## A. Not implemented / weaker than the spec

1. **Import is synchronous, not an "async operation".** Spec 7 says bulk source
   import is an "async operation with manifest/hash, source version, validation
   and quarantine". `POST /v1/currency-imports` validates and applies inside the
   request (bounded to 5,000 rows / 4 MiB) and returns the finished import. The
   `RECEIVED` and `VALIDATED` statuses exist in the schema vocabulary but are
   never persisted. Large files or an external-fetch worker would need a real
   async design.
2. **No ISO 4217 source fetch / licensing / maintenance-agency integration.**
   The service accepts a caller-supplied manifest; it does not fetch or verify
   against the ISO maintenance source. "Source verification" is only a check
   that the supplied hash equals the SHA-256 recomputed from the supplied rows
   (integrity of the payload, not authenticity of its origin). There is no
   signature check. Spec DoR item "licensing/usage validated" is not addressed.
3. **SOURCE_UNVERIFIED leaves no stored evidence.** A manifest-hash mismatch is
   rejected with nothing persisted and no event, so spec DoD "all imports
   preserve manifest, source version, hash and quarantine evidence" is met for
   APPLIED and QUARANTINED imports but NOT for unverified ones (storing under
   the claimed hash would poison the idempotency key; this was judged worse).
   The refusal is only in application logs/metrics.
4. **Expected-version is not a "protected-field fingerprint".** Spec 3:
   "activation/approval binds protected-field fingerprint". Only the integer
   `version` is bound. There is no fingerprint/approval-binding.
5. **Approval chain is asserted, not verified.** Spec 4.12: "support activation
   requires finance/platform approval". The optional `approver_id` is stored in
   the status history as an asserted value. This service does not verify an
   approval workflow instance; the only enforced controls are the authz action
   `CURRENCY_ACTIVATE` (authorization-svc) and the SoD rule below.
6. **No backdated minor-unit correction.** A minor-unit change must take effect
   strictly after the current version's `valid_from`; otherwise that row (and so
   the whole import) is quarantined. Late-arriving corrections of an earlier
   period (spec 3 "late corrections ... as-of reconstruction") are therefore not
   supported; valid time is bitemporal only in the sense of `valid_from` +
   `recorded_at`, with no recorded-time query (`recorded_at` is stored and
   returned but there is no `?recorded_at=` read).
7. **`GET /v1/currencies/{code}?as_of=` returns the CURRENT lifecycle status**,
   not the status as of that instant (status history is stored but not
   reconstructed per query). The minor-unit version and the record's validity
   window are as-of correct.
8. **Tenant overlay has no history table.** `tenant_currency_support` keeps the
   latest row (version-bumped, never deleted); prior states survive only as
   outbox events (which are not retained forever) — unlike the global tables,
   which have append-only history.
9. **Postgres-level behaviour** was verified on 2026-10-07 against PostgreSQL 16 for the paths the 4 store tests exercise (see RELEASE_CERTIFICATE.md); concurrency, load and failure injection remain untested.
10. **`RULE_AMBIGUOUS` is never raised** (no REF-02 path produces it); it is in
    the code vocabulary and OpenAPI enum only because the shared contract lists
    it.
11. **No jurisdiction associations / rounding defaults.** Spec 4.12 lists
    "jurisdiction associations" and "rounding defaults" under server-resolved
    context. Not modelled (the doctrine forbids hardcoding them and no source
    for them was specified). No `RoundingScale` function (explicitly out of
    scope per the brief).

## B. Interpretations (choices where the spec is silent or ambiguous)

12. **KNOWN -> RETIRED is illegal.** Spec: "Known → Supported/Restricted →
    Retired". Read literally, a never-supported currency cannot be retired; it
    stays KNOWN (and `validate post` already refuses it). KNOWN -> RESTRICTED is
    allowed (design). Self-transitions are illegal. RETIRED is terminal.
13. **Re-introducing a retired code via import is quarantined.** If an import row
    names an alpha code that only exists as a RETIRED currency, the import is
    quarantined (reason says so). The partial unique indexes would allow a new
    row, but silently resurrecting a historical code under a new identity was
    judged unsafe. A deliberate re-introduction path would need its own command.
14. **Numeric / alpha code are immutable once registered.** An import row whose
    numeric code differs from the registered one for the same alpha code is a
    conflict (quarantine), and a DB trigger forbids changing codes.
15. **SoD scope.** Only *activation* (to SUPPORTED, including RESTRICTED ->
    SUPPORTED) is barred for `last_import_actor`, where that is the actor of the
    import that introduced **or last changed** the currency (name, flag or minor
    unit). Restrict/retire by the importer are allowed (they reduce, not widen,
    use). "Production only" (spec: "separated for production") is **not**
    environment-switched: the rule is always on.
16. **Event `tenant_id` for global changes = the actor's tenant context.** The
    envelope middleware makes tenant_id mandatory on every request, so there is
    always one; the outbox RLS is keyed on it. `payload.scope` (`GLOBAL`/`TENANT`)
    tells consumers whether the fact is platform-wide. No literal `"platform"`
    tenant is used. See asyncapi.yaml note 3.
17. **Overlay events reuse `CurrencySupportChanged`** with `scope: TENANT`,
    `object_id` = the currency id and `object_version` = the overlay row's
    version (not the currency's). The currency's version is in `currency_version`.
18. **Event names are the spec's PascalCase** (`CurrencyUpdated`, ...), not the
    dotted lower-case names (`authority.delegated`) the template service uses.
    `CurrencyImportQuarantined` is an addition requested in the brief (not in
    the spec's three).
19. **Extra authz actions.** Besides the four named (`CURRENCY_IMPORT`,
    `CURRENCY_ACTIVATE`, `CURRENCY_RESTRICT`, `CURRENCY_RETIRE`), tenant overlay
    commands ask `CURRENCY_TENANT_ENABLE` / `CURRENCY_TENANT_DISABLE`. These
    actions must be seeded in authorization-svc's role catalogue; this change
    did not (and must not, per scope) touch that service. Until seeded, every
    command is denied (fail closed).
20. **Queries/reads do not call authorization-svc.** Currency data is C0
    reference data and high-cacheability; reads need no permission and (global
    ones) not even a tenant header. The tenant-overlay read requires the path
    tenant to equal the trusted tenant context.
21. **Error code additions and HTTP mapping.** `NOT_FOUND` and `FORBIDDEN` are
    additions to the nine shared codes. CONTEXT_INVALID is 400 when the request
    is malformed / a mandatory header or field is missing, 401 when tenant or
    actor is missing, and 422 when raised by the service for semantic reasons
    (e.g. enabling a non-SUPPORTED currency, idempotency-key reuse with a
    different body). `SOURCE_UNVERIFIED` is 422. An authz denial is 403
    `FORBIDDEN` (distinct from `SOD_DENIED`, also 403).
22. **Idempotency-Key reuse with a different request is `CONTEXT_INVALID` (422).**
    The shared contract has no dedicated code. Idempotency records are scoped to
    (actor's tenant, key) and store the full original response; failed commands
    store nothing (so a retry after a failure re-executes).
23. **`minor_unit_versions.valid_to` is derived, not stored.** The approved
    design listed a `valid_to` column; storing it would require an UPDATE to
    "close" the previous version, contradicting append-only. It is computed on
    read (next version's `valid_from`, or the currency's `valid_to`).
24. **`fund_or_metal_flag` is a boolean**, not a classification string. Spec:
    "fund/metal classification where applicable". A richer classification would
    need a schema change.
25. **`validate`** adds `operation` (`post`|`read`, default `post`), optional
    `tenant_scoped=true`, and extra pin fields (`currency_id`,
    `currency_version`, `status`, `minor_unit_valid_from`) beyond the requested
    `{supported, minor_unit, reason}`. `minor_unit` is reported (not null) for
    existing-but-unsupported currencies because it is real registry data;
    consumers must still key on `supported`. Codes are matched exactly and
    case-sensitively.
26. **GET unknown code = 404 `NOT_FOUND`; retired = 200.** `REFERENCE_RETIRED`
    is used only where a *write* targets a retired reference (tenant enable of a
    retired currency, 409).
27. **No `X-Legal-Entity-Id` requirement.** The vendored envelope policy sets
    `LegalEntityID: NotRequired` (currencies are global). authorization-svc is
    asked with the entity header if supplied, else the platform scope id
    `00000000-0000-0000-0000-00000000f001` (the same constant the siblings use).
28. **`envelope/contract.go` is hand-written.** The header says "generated by
    services/_contract/rollout.sh", but the rollout script's service list was not
    extended (out of scope: "do not touch other code"). The vendored envelope
    sources are otherwise copies of the delegated-authority-svc ones with only
    the module path changed. A real rollout run will regenerate `contract.go`.
29. **Readiness checks the database only**, not authorization-svc (the template
    includes authz). Reads must keep serving during an authz outage; commands
    fail closed with 503 on their own.
30. **Registered with the shared `zoiko_app` DB role** (like 31 other
    services) rather than a per-service `app_currency_registry` role, because
    adding the latter would require editing `create-app-roles.sh`, which the
    brief did not list. Port 8172 chosen as next unused after 8171.
31. **Template extras not carried over:** no expiry sweeper (nothing expires),
    no SoD-engine client (the SoD rule here is record-based), no
    `scripts/audit.sh`/`progress.md`.
32. **Vendored templates' test files**: the outbox/health tests were copied
    from the template (with event names changed); the template's envelope tests
    live in `_contract` and were not copied.
