-- search-indexer-svc control plane (ZS-SVC-AB-001 §2.1, §20 "Data model").
--
-- WHAT IS AND IS NOT HERE.
--
-- The searchable projections live in OpenSearch. This schema holds the
-- CONTROL PLANE: what may be indexed, which generation is serving, how far
-- ingestion has got, what has been restricted, and what each executed search
-- actually did. §8.3 calls indexes "disposable projections, not the sole
-- backup" — so every fact needed to rebuild one, and every fact needed to
-- audit one, has to survive the index being dropped. That is this database.
--
-- TENANCY IS DELIBERATELY SPLIT, and the split is the interesting part.
--
--   Platform-scoped (no tenant_id): search_sources, index_contracts,
--   search_field_definitions, index_generations. A source contract describes a
--   SHAPE, not a tenant's data — obligations look the same in every tenant —
--   and a generation is one physical index serving all of them. Giving these a
--   tenant_id would mean one generation per tenant, which is the index
--   proliferation search-client's README already rejected, and would make
--   OD-02's "shared shard vs tenant index" a schema decision rather than a
--   deployment one.
--
--   Tenant-scoped (tenant_id NOT NULL, RLS in 000002): restriction_tombstones,
--   projection_ledger, search_evidence. These are records ABOUT a tenant's
--   data — what was removed, what is indexed, who searched for what — and
--   every one of them would be a cross-tenant disclosure if read by the wrong
--   caller. A tenant's search evidence in particular names its query digests
--   and result counts.
--
-- The platform-scoped tables are NOT unprotected: every write to them is
-- gated on a platform-scope authorization check before the store is reached.
-- What they lack is row-level tenancy, because they have no tenant dimension
-- to be scoped by.

-- ── ESR-01: source and contract registry ─────────────────────────────────────

CREATE TABLE IF NOT EXISTS search_sources (
    source_id                UUID PRIMARY KEY,
    owner_service            TEXT        NOT NULL,
    -- source_type is the value that appears in every projection and in every
    -- source_ref. UNIQUE because two registrations of the same type would
    -- give one document two contracts and no way to say which applied.
    source_type              TEXT        NOT NULL UNIQUE,
    tenant_scope             TEXT        NOT NULL DEFAULT 'TENANT_SHARDED'
        CHECK (tenant_scope IN ('SINGLE_TENANT', 'TENANT_SHARDED', 'SHARED_REFERENCE')),
    residency_region         TEXT        NOT NULL DEFAULT 'GLOBAL',
    sensitivity_ceiling      TEXT        NOT NULL
        CHECK (sensitivity_ceiling IN (
            'PUBLIC', 'INTERNAL', 'PERSONAL', 'FINANCIAL', 'HR',
            'LEGAL_PRIVILEGED', 'RESTRICTED')),
    -- SECRET_PROHIBITED is absent from the CHECK above ON PURPOSE. INV-09 says
    -- those fields "never enter search indexes", so a source whose ceiling IS
    -- that class could have no legal contract at all — registering one would
    -- only create something for a later bug to misread as permission.
    event_topic              TEXT        NOT NULL,
    event_types              TEXT[]      NOT NULL DEFAULT '{}',
    restriction_event_types  TEXT[]      NOT NULL DEFAULT '{}',
    freshness_class          TEXT        NOT NULL DEFAULT 'S1'
        CHECK (freshness_class IN ('S0', 'S1', 'S2', 'S3')),
    max_lag_seconds          INTEGER     NOT NULL DEFAULT 300 CHECK (max_lag_seconds > 0),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_principal_id  UUID        NOT NULL
);

CREATE TABLE IF NOT EXISTS index_contracts (
    contract_id              UUID PRIMARY KEY,
    source_id                UUID        NOT NULL REFERENCES search_sources(source_id),
    -- scope_name is what a caller names in a search request. It is the
    -- SEARCH SURFACE (§6.1 "records, people, invoices, matters"), not the
    -- index: the alias derived from it is server-owned (INV-03).
    scope_name               TEXT        NOT NULL,
    version                  INTEGER     NOT NULL CHECK (version > 0),
    -- schema_digest pins the exact field set this version published. TC-03:
    -- "every index generation traces to published IndexContract version" — the
    -- digest is what makes that traceability verifiable rather than asserted.
    schema_digest            TEXT        NOT NULL,
    publication_state        TEXT        NOT NULL DEFAULT 'DRAFT'
        CHECK (publication_state IN ('DRAFT', 'CERTIFIED', 'PUBLISHED', 'RETIRED')),
    freshness_class          TEXT        NOT NULL DEFAULT 'S1'
        CHECK (freshness_class IN ('S0', 'S1', 'S2', 'S3')),
    retrieval_class          TEXT        NOT NULL DEFAULT 'R1'
        CHECK (retrieval_class IN ('R0', 'R1', 'R2', 'R3')),
    analyzer_profile         TEXT        NOT NULL DEFAULT 'standard',
    -- authz_action is the action_type this scope's results are re-authorized
    -- against at retrieval time (§7.1 R1/R2). Held on the contract rather than
    -- hardcoded per scope so onboarding a domain is a registration, not a
    -- release.
    authz_action             TEXT        NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at             TIMESTAMPTZ,
    created_by_principal_id  UUID        NOT NULL,
    UNIQUE (scope_name, version)
);

-- Only one PUBLISHED version per scope at a time. Two would mean a generation
-- build could pick either, and §4.2's publication gate would have certified a
-- field set that is not the one being built.
CREATE UNIQUE INDEX IF NOT EXISTS index_contracts_one_published_per_scope
    ON index_contracts (scope_name)
    WHERE publication_state = 'PUBLISHED';

CREATE TABLE IF NOT EXISTS search_field_definitions (
    -- No surrogate key. A field definition has no identity apart from the
    -- contract it belongs to and the name it is known by: the projection
    -- writes it under field_name, the query planner looks it up by
    -- field_name, and a saved query references it by field_name. A UUID
    -- primary key would be a second identity nothing ever uses, and the one
    -- place it did get used — the insert — silently wanted the NAME.
    contract_id              UUID        NOT NULL REFERENCES index_contracts(contract_id) ON DELETE CASCADE,
    -- source_path is where the value comes from in the event payload; name is
    -- what it is called in the projection. Kept separate so a domain can
    -- rename a payload field without a breaking change to every saved query.
    source_path              TEXT        NOT NULL,
    field_name               TEXT        NOT NULL,
    field_type               TEXT        NOT NULL
        CHECK (field_type IN ('KEYWORD', 'TEXT', 'DATE', 'LONG', 'DOUBLE', 'BOOLEAN')),
    searchable               BOOLEAN     NOT NULL DEFAULT false,
    filterable               BOOLEAN     NOT NULL DEFAULT false,
    facetable                BOOLEAN     NOT NULL DEFAULT false,
    sortable                 BOOLEAN     NOT NULL DEFAULT false,
    snippet_allowed          BOOLEAN     NOT NULL DEFAULT false,
    returnable               BOOLEAN     NOT NULL DEFAULT false,
    exportable               BOOLEAN     NOT NULL DEFAULT false,
    sensitivity_class        TEXT        NOT NULL DEFAULT 'INTERNAL'
        CHECK (sensitivity_class IN (
            'PUBLIC', 'INTERNAL', 'PERSONAL', 'FINANCIAL', 'HR',
            'LEGAL_PRIVILEGED', 'RESTRICTED', 'SECRET_PROHIBITED')),
    analyzer_profile         TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (contract_id, field_name),
    -- INV-13: a snippet may only ever be produced from something the caller
    -- can already see, so snippet_allowed without returnable is incoherent.
    -- Enforced here as well as in the service: this is the constraint whose
    -- violation leaks text, and a database check survives a refactor of the
    -- validation code.
    CONSTRAINT snippet_requires_returnable CHECK (NOT snippet_allowed OR returnable),
    -- INV-09. SECRET_PROHIBITED is representable so a contract can DECLARE a
    -- field prohibited (which is how the projector knows to drop it), but it
    -- may never be exposed in any way.
    CONSTRAINT prohibited_is_never_exposed CHECK (
        sensitivity_class <> 'SECRET_PROHIBITED'
        OR (NOT searchable AND NOT filterable AND NOT facetable
            AND NOT sortable AND NOT snippet_allowed AND NOT returnable
            AND NOT exportable)
    )
);

CREATE INDEX IF NOT EXISTS search_field_definitions_contract_idx
    ON search_field_definitions (contract_id);

-- ── ESR-05: generations ──────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS index_generations (
    generation_id            UUID PRIMARY KEY,
    contract_id              UUID        NOT NULL REFERENCES index_contracts(contract_id),
    contract_version         INTEGER     NOT NULL,
    scope_name               TEXT        NOT NULL,
    physical_index           TEXT        NOT NULL UNIQUE,
    state                    TEXT        NOT NULL DEFAULT 'PLANNED'
        CHECK (state IN ('PLANNED', 'BUILDING', 'VALIDATING', 'READY', 'ACTIVE', 'FAILED', 'RETIRED')),
    build_from               TIMESTAMPTZ,
    validation_digest        TEXT        NOT NULL DEFAULT '',
    validation_note          TEXT        NOT NULL DEFAULT '',
    activated_at             TIMESTAMPTZ,
    retired_at               TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_principal_id  UUID        NOT NULL
);

-- At most one ACTIVE generation per scope, enforced by the database rather
-- than by the code that swaps aliases.
--
-- This is the NP-42 / alias-drift guard. The OpenSearch alias and this row are
-- two representations of the same fact, and the failure mode that matters is
-- them disagreeing — an alias pointing at a generation this table does not
-- call ACTIVE. A partial unique index makes the control-plane half of that
-- disagreement impossible to write, so drift can only ever come from outside,
-- which is exactly the case §8.3 says to open an incident for.
CREATE UNIQUE INDEX IF NOT EXISTS index_generations_one_active_per_scope
    ON index_generations (scope_name)
    WHERE state = 'ACTIVE';

CREATE INDEX IF NOT EXISTS index_generations_scope_state_idx
    ON index_generations (scope_name, state);

-- ── ESR-02: checkpoints ──────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS index_checkpoints (
    scope_name               TEXT        NOT NULL,
    source_partition         TEXT        NOT NULL,
    watermark                BIGINT      NOT NULL DEFAULT -1,
    committed_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    observed_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    lag_ms                   BIGINT      NOT NULL DEFAULT 0,
    -- UNKNOWN, not CURRENT, as the default. §2.2: "UNKNOWN is never
    -- represented as CURRENT" — a checkpoint row that has never been observed
    -- must not claim freshness it has no evidence for.
    freshness                TEXT        NOT NULL DEFAULT 'UNKNOWN'
        CHECK (freshness IN ('UNKNOWN', 'CURRENT', 'LAGGING', 'STALE')),
    indexed_live             BIGINT      NOT NULL DEFAULT 0,
    indexed_tombstoned       BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (scope_name, source_partition)
);

-- ── ESR-02: tenant-scoped projection ledger ──────────────────────────────────

CREATE TABLE IF NOT EXISTS projection_ledger (
    tenant_id                UUID        NOT NULL,
    scope_name               TEXT        NOT NULL,
    source_type              TEXT        NOT NULL,
    source_id                TEXT        NOT NULL,
    -- source_version and restriction_epoch are the two monotonic counters the
    -- whole idempotency story rests on (§5.2: "index writes are idempotent by
    -- tenant + source_ref + source_version/projection_version").
    source_version           BIGINT      NOT NULL DEFAULT 0,
    restriction_epoch        BIGINT      NOT NULL DEFAULT 0,
    content_hash             TEXT        NOT NULL DEFAULT '',
    tombstoned               BOOLEAN     NOT NULL DEFAULT false,
    last_event_id            TEXT        NOT NULL DEFAULT '',
    indexed_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, scope_name, source_type, source_id)
);

CREATE INDEX IF NOT EXISTS projection_ledger_scope_idx
    ON projection_ledger (scope_name, tenant_id)
    WHERE NOT tombstoned;

-- ── ESR-05: restriction tombstones ───────────────────────────────────────────

CREATE TABLE IF NOT EXISTS restriction_tombstones (
    tombstone_id             UUID PRIMARY KEY,
    tenant_id                UUID        NOT NULL,
    scope_name               TEXT        NOT NULL,
    source_type              TEXT        NOT NULL,
    source_id                TEXT        NOT NULL,
    reason                   TEXT        NOT NULL,
    -- epoch is monotonic per source_ref. NP-48: an out-of-order restriction
    -- event must not resurrect visibility, so a lower epoch is refused rather
    -- than applied.
    epoch                    BIGINT      NOT NULL CHECK (epoch >= 0),
    source_event_id          TEXT        NOT NULL,
    effective_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    propagated_at            TIMESTAMPTZ,
    -- verified_at is separate from propagated_at because §2.2 says APPLIED is
    -- not VERIFIED until search visibility has been tested. TC-08: "every
    -- restriction verification records when invisibility was proven."
    verified_at              TIMESTAMPTZ,
    state                    TEXT        NOT NULL DEFAULT 'PENDING'
        CHECK (state IN ('PENDING', 'APPLIED', 'VERIFIED', 'FAILED')),
    failure_reason           TEXT        NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NP-47: "restriction event duplicate/replayed → idempotent by source
    -- event/tombstone version." The unique key is what makes the replay a
    -- no-op rather than a second tombstone.
    UNIQUE (tenant_id, source_type, source_id, source_event_id)
);

CREATE INDEX IF NOT EXISTS restriction_tombstones_unverified_idx
    ON restriction_tombstones (scope_name, state, effective_at)
    WHERE state <> 'VERIFIED';

-- ── ESR-03/04: search evidence ───────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS search_evidence (
    evidence_id              UUID PRIMARY KEY,
    tenant_id                UUID        NOT NULL,
    request_id               TEXT        NOT NULL,
    correlation_id           TEXT        NOT NULL DEFAULT '',
    actor_id                 TEXT        NOT NULL DEFAULT '',
    workload_id              TEXT        NOT NULL DEFAULT '',
    on_behalf_of_principal   TEXT        NOT NULL DEFAULT '',
    purpose_context          TEXT        NOT NULL DEFAULT '',
    scope_name               TEXT        NOT NULL,
    -- A DIGEST, never the query text. INV-17 and §9.2: search text is personal
    -- data when it can reveal a person, condition, employment matter, legal
    -- issue or financial fact, and a table of everyone's queries is one of the
    -- most sensitive things a platform can accumulate. The digest still
    -- supports replay correlation and repeat-query detection, which is what
    -- the evidence is actually for.
    query_digest             TEXT        NOT NULL,
    mandatory_filters_digest TEXT        NOT NULL,
    plan_digest              TEXT        NOT NULL,
    index_generation         TEXT        NOT NULL DEFAULT '',
    partition_set            TEXT[]      NOT NULL DEFAULT '{}',
    complexity_score         INTEGER     NOT NULL DEFAULT 0,
    result_count             INTEGER     NOT NULL DEFAULT 0,
    suppressed_count         INTEGER     NOT NULL DEFAULT 0,
    completeness_state       TEXT        NOT NULL DEFAULT 'COMPLETE'
        CHECK (completeness_state IN ('COMPLETE', 'PARTIAL', 'DEGRADED', 'UNKNOWN')),
    reason_codes             TEXT[]      NOT NULL DEFAULT '{}',
    duration_ms              BIGINT      NOT NULL DEFAULT 0,
    trace_id                 TEXT        NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS search_evidence_tenant_created_idx
    ON search_evidence (tenant_id, created_at DESC);

CREATE INDEX IF NOT EXISTS search_evidence_scope_idx
    ON search_evidence (scope_name, created_at DESC);
