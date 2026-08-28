-- 000001_initial_schema.up.sql
-- privacy-purpose-registry-svc — initial schema (PRV-01, ZS-SVC-W-001).
--
-- This is the first of five services ZS-SVC-W-001 (Privacy/Consent/
-- Purpose/Data Rights Control) specifies, and the ONLY one implemented so
-- far — see internal/domain's package doc comment for the full scope
-- statement. §35's eight-wave sequence names this registry as the first
-- buildable service; PRV-02 (consent), PRV-03 (runtime decisions), PRV-04
-- (rights requests), and PRV-05 (transfers) all depend on this one
-- existing first.
--
-- Owns two independently-versioned registries, same "stable identity +
-- immutable-once-published version" shape used throughout this platform
-- (retention_policies, sod_rules, policy_versions):
--
--   purposes / purpose_versions — PRV-I05/I06: every purpose has a
--     stable ID and an effective-dated version; once PUBLISHED it is
--     immutable (migration 000002's trigger), amended only by creating a
--     new version.
--
--   processing_activities / processing_activity_versions — the canonical
--     ProcessingActivityVersion (§7): what is processed, why (via
--     purpose_ids), under what privacy role, for whom, and where.
--     Content is structurally immutable once a version leaves DRAFT
--     (migration 000002's trigger) — only version_status and the fields
--     the lifecycle actions themselves set (validation_findings,
--     rejection_reason, effective_from) may change after that.
--
-- tenant_id is nullable on both stable-identity tables — NULL means a
-- platform-wide (Zoiko-as-independent-controller, §23.1) purpose or
-- activity rather than a tenant-instructed one. Same nullable-scope
-- doctrine as retention-registry-svc's retention_policies/legal_holds.

-- ── purposes ─────────────────────────────────────────────────────────────────

CREATE TABLE purposes (
    purpose_id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL
);

CREATE TABLE purpose_versions (
    purpose_version_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    purpose_id               UUID        NOT NULL REFERENCES purposes(purpose_id),

    statement                TEXT        NOT NULL,
    -- DATA ONLY, e.g. PRIMARY, SECONDARY_COMPATIBLE, SECONDARY_INCOMPATIBLE
    -- (§4.1's compatibility_class concept) — no registry backs it yet,
    -- same doctrine as retention_policies.record_class.
    compatibility_class      VARCHAR(64) NOT NULL,
    -- Opaque references to PDC lawful-basis packages — this service does
    -- not author or validate legal-basis content (§0's own status line:
    -- "jurisdiction-specific production rules require approved PDC
    -- packages"), it only records which ones this purpose cites.
    lawful_basis_refs        JSONB       NOT NULL DEFAULT '[]',

    -- DRAFT | PUBLISHED
    version_status           VARCHAR(32) NOT NULL DEFAULT 'DRAFT',

    effective_from           TIMESTAMPTZ NOT NULL,
    supersedes_version_id    UUID        REFERENCES purpose_versions(purpose_version_id),

    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL
);

CREATE INDEX idx_purpose_versions_purpose
    ON purpose_versions (purpose_id, version_status, effective_from DESC);

-- ── processing activities ────────────────────────────────────────────────────

CREATE TABLE processing_activities (
    activity_id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL
);

CREATE TABLE processing_activity_versions (
    activity_version_id      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    activity_id              UUID        NOT NULL REFERENCES processing_activities(activity_id),

    -- CONTROLLER | PROCESSOR | JOINT_CONTROLLER (§4.1's PrivacyRole)
    privacy_role             VARCHAR(32) NOT NULL,
    owner                    TEXT        NOT NULL,

    -- purpose_ids references purposes.purpose_id — not a foreign key
    -- array (Postgres has none), enforced at the application layer by
    -- Validate (PRV-001 PURPOSE_NOT_REGISTERED if any entry doesn't
    -- resolve to a currently PUBLISHED purpose).
    purpose_ids              JSONB       NOT NULL DEFAULT '[]',
    subject_classes          JSONB       NOT NULL DEFAULT '[]',
    data_categories          JSONB       NOT NULL DEFAULT '[]',
    sources                  JSONB       NOT NULL DEFAULT '[]',
    recipients               JSONB       NOT NULL DEFAULT '[]',
    jurisdictions            JSONB       NOT NULL DEFAULT '[]',
    -- Opaque references into retention-registry-svc / a future transfer
    -- registry — same doctrine as lawful_basis_refs above: recorded, not
    -- validated by this service.
    retention_rule_refs      JSONB       NOT NULL DEFAULT '[]',
    transfer_refs            JSONB       NOT NULL DEFAULT '[]',

    -- DRAFT | VALIDATED | SUBMITTED | APPROVED | ACTIVE | SUSPENDED |
    -- REJECTED | RETIRED — see domain.activityTransitions for the
    -- complete, exhaustive legal-transition table this column obeys.
    version_status           VARCHAR(32) NOT NULL DEFAULT 'DRAFT',
    -- Set by Validate; empty/absent means either not yet validated or
    -- validated clean. PRV-I13: any finding at all keeps version_status
    -- at DRAFT, never a partial PERMIT.
    validation_findings      JSONB,
    -- Set by Reject; required whenever version_status = REJECTED.
    rejection_reason         TEXT,
    -- Set by Activate; the version's effective date once ACTIVE. Also
    -- doubles as the valid-time axis ResolveActivityAsOf queries.
    effective_from           TIMESTAMPTZ,

    supersedes_version_id    UUID        REFERENCES processing_activity_versions(activity_version_id),

    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL
);

CREATE INDEX idx_activity_versions_activity
    ON processing_activity_versions (activity_id, version_status, effective_from DESC);

CREATE INDEX idx_activity_versions_ropa
    ON processing_activity_versions (privacy_role, effective_from DESC)
    WHERE version_status = 'ACTIVE';
