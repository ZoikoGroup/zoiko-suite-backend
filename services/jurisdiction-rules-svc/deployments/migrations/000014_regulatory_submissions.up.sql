-- 000014_regulatory_submissions.up.sql
-- ZS-JUR-001 Wave 5 (e-invoice s13, filing s14): submission profiles share the
-- decision evidence ledger, and submissions with their authority/provider status
-- reports get their own tables.
--
-- A submission snapshots the profile it was registered under, so a later pack
-- release cannot change an in-flight lifecycle. Status reports are append-only
-- evidence; the submission row itself may only advance its status, and never out
-- of a terminal state (no illegal regression, JUR-NEG-11), and a repeated report
-- from the provider is stored once (JUR-NEG-10).

ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET', 'RETENTION', 'MAPPING', 'PROFILE')),
    DROP CONSTRAINT ck_decision_records_have_basis,
    ADD CONSTRAINT ck_decision_records_have_basis CHECK (
        outcome NOT IN ('RETENTION_RESOLVED', 'MAPPING_RESOLVED', 'PROFILE_RESOLVED')
        OR (pack_version_id IS NOT NULL AND artifact_digest IS NOT NULL AND rule_id IS NOT NULL AND rule_content_digest IS NOT NULL));

CREATE TABLE regulatory_submissions (
    submission_id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    submission_kind        VARCHAR(16)  NOT NULL CHECK (submission_kind IN ('EINVOICE', 'FILING')),
    jurisdiction_code      VARCHAR(32)  NOT NULL,
    profile_code           VARCHAR(128) NOT NULL,
    subject_ref            VARCHAR(256) NOT NULL,
    payload_hash           VARCHAR(71)  NOT NULL CHECK (payload_hash ~ '^sha256:[0-9a-f]{64}$'),
    status                 VARCHAR(40)  NOT NULL,
    is_terminal            BOOLEAN      NOT NULL DEFAULT FALSE,
    last_event_occurred_at TIMESTAMPTZ,
    effective_at           TIMESTAMPTZ  NOT NULL,
    pack_version_id        UUID         NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    artifact_digest        VARCHAR(71)  NOT NULL,
    rule_id                UUID         NOT NULL,
    rule_content_digest    VARCHAR(71)  NOT NULL,
    -- The profile (lifecycle, dependencies, timing) as it was when the submission was registered.
    profile_snapshot       JSONB        NOT NULL,
    dependencies_used      JSONB        NOT NULL DEFAULT '[]'::jsonb,
    approvals              JSONB        NOT NULL DEFAULT '[]'::jsonb,
    submitted_by           TEXT         NOT NULL,
    idempotency_key        TEXT,
    request_digest         VARCHAR(71)  NOT NULL,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX uq_submission_idempotency ON regulatory_submissions (submitted_by, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_submission_subject ON regulatory_submissions (submission_kind, subject_ref);

CREATE TABLE regulatory_submission_events (
    event_row_id      UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    submission_id     UUID         NOT NULL REFERENCES regulatory_submissions(submission_id),
    provider_event_id VARCHAR(128) NOT NULL,
    reported_status   VARCHAR(40)  NOT NULL,
    receipt_id        VARCHAR(256),
    occurred_at       TIMESTAMPTZ  NOT NULL,
    received_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    disposition       VARCHAR(24)  NOT NULL CHECK (disposition IN
        ('APPLIED', 'STALE', 'AFTER_TERMINAL', 'ILLEGAL_TRANSITION', 'MISSING_RECEIPT', 'UNKNOWN_STATUS')),
    status_after      VARCHAR(40)  NOT NULL,
    detail            JSONB        NOT NULL DEFAULT '{}'::jsonb,
    recorded_by       TEXT         NOT NULL,
    CONSTRAINT uq_submission_provider_event UNIQUE (submission_id, provider_event_id),
    -- An applied acceptance-style status without a receipt is impossible to store.
    CONSTRAINT ck_event_applied_status CHECK (disposition <> 'APPLIED' OR status_after = reported_status)
);

CREATE OR REPLACE FUNCTION jur_submission_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'submissions are never deleted' USING ERRCODE = '23514';
    END IF;
    IF OLD.is_terminal THEN
        RAISE EXCEPTION 'a submission in a terminal state cannot change (no illegal regression)' USING ERRCODE = '23514';
    END IF;
    IF NEW.submission_id <> OLD.submission_id OR NEW.submission_kind <> OLD.submission_kind OR NEW.jurisdiction_code <> OLD.jurisdiction_code
       OR NEW.profile_code <> OLD.profile_code OR NEW.subject_ref <> OLD.subject_ref OR NEW.payload_hash <> OLD.payload_hash
       OR NEW.effective_at <> OLD.effective_at OR NEW.pack_version_id <> OLD.pack_version_id OR NEW.artifact_digest <> OLD.artifact_digest
       OR NEW.rule_id <> OLD.rule_id OR NEW.rule_content_digest <> OLD.rule_content_digest OR NEW.profile_snapshot <> OLD.profile_snapshot
       OR NEW.dependencies_used <> OLD.dependencies_used OR NEW.approvals <> OLD.approvals OR NEW.submitted_by <> OLD.submitted_by
       OR NEW.request_digest <> OLD.request_digest OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'only the status of a submission can change' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_submission_guard BEFORE UPDATE OR DELETE ON regulatory_submissions
    FOR EACH ROW EXECUTE FUNCTION jur_submission_guard();

CREATE OR REPLACE FUNCTION jur_submission_event_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'submission status reports are append-only evidence' USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_submission_event_append_only BEFORE UPDATE OR DELETE ON regulatory_submission_events
    FOR EACH ROW EXECUTE FUNCTION jur_submission_event_append_only();
