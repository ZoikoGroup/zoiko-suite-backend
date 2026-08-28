-- 000001_initial_schema.up.sql
-- privacy-consent-svc — initial schema (PRV-02, ZS-SVC-W-001).
--
-- Second of five ZS-SVC-W-001 services. Depends on PRV-01
-- (privacy-purpose-registry-svc) for purpose validity — this service does
-- NOT store a copy of the purpose registry; every consent_receipts row's
-- purpose_id was checked at write time against a live call to that
-- service (see internal/purposeregistry), so this schema deliberately has
-- no foreign key to a local purposes table because none exists here.
--
-- Two shapes, same distinction the domain package doc comment makes:
--
--   notices / notice_versions — a stable-identity + versioned-lifecycle
--     registry, same shape as PRV-01's purposes/processing_activities.
--     DRAFT -> APPROVED -> PUBLISHED -> WITHDRAWN, with SUPERSEDED as a
--     side effect of publishing a successor (see migration 000002 and
--     PgStore.PublishNoticeVersion), not a directly-taken transition.
--
--   presentation_receipts / consent_receipts / withdrawal_receipts /
--     preference_assertions — pure append-only evidence logs. None of
--     these has a "status" or "current" column: every read that needs a
--     current answer (ResolveConsentStatus, ResolvePreference) derives it
--     from the latest row(s) at query time. This is PRV-I09/I10/I11 taken
--     literally: denial and withdrawal must be representable
--     independently of anything else, withdrawal must never delete
--     evidence, and it must affect only future resolution — a mutable
--     status column would make at least one of those three impossible to
--     guarantee.

CREATE TABLE notices (
    notice_id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL
);

CREATE TABLE notice_versions (
    notice_version_id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    notice_id                 UUID        NOT NULL REFERENCES notices(notice_id),

    locale                    VARCHAR(16) NOT NULL,
    audience                  VARCHAR(64) NOT NULL,
    -- Pointer to the actual rendered notice content, owned by DRC
    -- (document-vault-svc / a future document store) — this service does
    -- not store notice document bytes itself.
    content_hash              TEXT        NOT NULL,

    -- DRAFT | APPROVED | PUBLISHED | SUPERSEDED | WITHDRAWN
    version_status            VARCHAR(32) NOT NULL DEFAULT 'DRAFT',
    effective_from            TIMESTAMPTZ,
    supersedes_version_id     UUID        REFERENCES notice_versions(notice_version_id),
    approved_by_principal_id  TEXT,
    -- Tiebreaker only, never returned to callers — two versions can share
    -- an identical wall-clock effective_from (timestamp collisions are
    -- rare but real under concurrent writes, and routine under fast
    -- automated test clocks). sequence_no is a Postgres-generated,
    -- strictly monotonic ordinal; see ResolveNoticeAsOf's ORDER BY.
    sequence_no               BIGSERIAL,

    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id   TEXT        NOT NULL
);

CREATE INDEX idx_notice_versions_notice
    ON notice_versions (notice_id, version_status, effective_from DESC);

CREATE TABLE presentation_receipts (
    presentation_receipt_id  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID,
    notice_version_id        UUID        NOT NULL REFERENCES notice_versions(notice_version_id),
    subject_ref              TEXT        NOT NULL,
    channel                  VARCHAR(64) NOT NULL,
    locale                   VARCHAR(16) NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_presentation_receipts_subject
    ON presentation_receipts (subject_ref, notice_version_id);

CREATE TABLE consent_receipts (
    consent_receipt_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID,
    subject_ref              TEXT        NOT NULL,
    -- References privacy-purpose-registry-svc's purpose_id — NOT a local
    -- foreign key (no purposes table exists in this database); validated
    -- at write time via a live HTTP call instead. See PgStore.RecordConsent.
    purpose_id                TEXT        NOT NULL,
    notice_version_id         UUID        REFERENCES notice_versions(notice_version_id),
    -- GRANTED | DENIED
    action                    VARCHAR(16) NOT NULL,
    capture_channel           VARCHAR(64) NOT NULL,
    actor_principal_id        TEXT        NOT NULL,
    correlation_id            TEXT,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_consent_receipts_subject_purpose
    ON consent_receipts (subject_ref, purpose_id, created_at DESC);

CREATE TABLE withdrawal_receipts (
    withdrawal_receipt_id     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID,
    consent_receipt_id        UUID        NOT NULL REFERENCES consent_receipts(consent_receipt_id),
    withdrawn_by_principal_id TEXT        NOT NULL,
    channel                   VARCHAR(64) NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one withdrawal per consent receipt — a second withdrawal of an
-- already-withdrawn receipt is meaningless (there is nothing further to
-- withdraw), and PgStore.WithdrawConsent's own existence check already
-- refuses it at the application layer; this is the same guarantee backed
-- at the schema layer too.
CREATE UNIQUE INDEX idx_withdrawal_receipts_one_per_consent
    ON withdrawal_receipts (consent_receipt_id);

CREATE TABLE preference_assertions (
    preference_assertion_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID,
    subject_ref               TEXT        NOT NULL,
    channel_or_purpose        VARCHAR(128) NOT NULL,
    -- ENABLED | DISABLED | UNSET | NOT_APPLICABLE
    value                     VARCHAR(32) NOT NULL,
    source                    VARCHAR(64) NOT NULL DEFAULT '',
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_preference_assertions_subject
    ON preference_assertions (subject_ref, channel_or_purpose, created_at DESC);
