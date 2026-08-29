-- 000001_initial_schema.up.sql
-- privacy-decision-svc — initial schema (PRV-03, ZS-SVC-W-001 §12/§13).
--
-- Third of five ZS-SVC-W-001 services. A single append-only table: this
-- service does not own any registry of its own (activities/purposes
-- belong to PRV-01, consent to PRV-02, holds to retention-registry-svc)
-- — it only records the decisions it computes from calling them, per
-- §13.2 "decision durability."
--
-- No foreign keys to those other services' tables because none of them
-- live in this database — activity_version_id/purpose_version_id/
-- consent_receipt_id/legal_hold_id are opaque references to rows in
-- OTHER services' databases, validated at decision time via a live HTTP
-- call (see internal/purposeregistry, internal/consentregistry,
-- internal/retentionregistry), not enforceable as a local FK.
CREATE TABLE privacy_decisions (
    decision_id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID,

    subject_ref              TEXT        NOT NULL,
    processing_activity_id   TEXT        NOT NULL,
    -- The EXACT resolved version at decision time, not just the caller's
    -- activity_id — this is what makes a past decision reproducible
    -- regardless of what PRV-01 reports about that activity later.
    activity_version_id      TEXT,

    purpose_id               TEXT        NOT NULL,
    purpose_version_id       TEXT,

    -- Data only — see domain.ProposedOperation.
    proposed_operation       VARCHAR(32) NOT NULL,

    -- PERMIT | RESTRICT | BLOCK | REVIEW_REQUIRED | INDETERMINATE.
    -- RESTRICT/REVIEW_REQUIRED are reserved in the column's domain but
    -- never written by this version of the service — see the domain
    -- package's doc comment for why.
    result                   VARCHAR(32) NOT NULL,
    -- Data-only reason codes explaining the result — see domain.Reason*.
    reason_codes             JSONB       NOT NULL DEFAULT '[]',

    -- Evidence references, populated only when the corresponding
    -- opt-in check was actually requested and performed.
    consent_receipt_id       TEXT,
    legal_hold_id            TEXT,

    actor_principal_id       TEXT        NOT NULL,
    correlation_id           TEXT,

    decided_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_privacy_decisions_subject
    ON privacy_decisions (subject_ref, purpose_id, decided_at DESC);

CREATE INDEX idx_privacy_decisions_activity
    ON privacy_decisions (processing_activity_id, decided_at DESC);
