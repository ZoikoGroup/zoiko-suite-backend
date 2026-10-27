-- ZS-SVC-M-001 INT-08 (Tax authority adapter). Makes the authority transmission real and safe:
--
--   * a certified, versioned AUTHORITY PROFILE (platform reference data, maker-checker) says how one authority
--     is spoken to and carries the authority schema every filing is validated against before it is sent;
--   * a filing is ONE durable authority attempt per approved TAX subject (unique subject_ref per tenant). Its
--     document, fingerprint, schema version and profile version are pinned, so a retry or a poll always
--     concerns the original subject and a changed document is a different subject (a conflict), never an edit;
--   * SUBMITTING is the durable "the request is leaving" state, set by compare-and-set before the call; a
--     crash leaves it behind and a sweeper turns a stale one into PENDING_UNKNOWN, never into a blind resend;
--   * transport success is not acceptance: SUBMITTED means the authority has the filing; ACCEPTED and
--     REJECTED arrive only from the authority (poll or callback);
--   * every call and every authenticated callback leaves append-only evidence.
-- Re-runnable.

ALTER TABLE tax_interfaces
    ADD COLUMN IF NOT EXISTS provider_key           TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS connector_id           TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS certificate_ref        TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS certificate_not_after  TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS escalate_before_hours  INTEGER     NOT NULL DEFAULT 24 CHECK (escalate_before_hours BETWEEN 1 AND 720);

ALTER TABLE tax_filing_submissions
    ADD COLUMN IF NOT EXISTS subject_ref          TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS subject_fingerprint  TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS document             JSONB       NULL,
    ADD COLUMN IF NOT EXISTS payload_hash         TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS rail                 TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS profile_version      INTEGER     NULL,
    ADD COLUMN IF NOT EXISTS sent_payload_hash    TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS validated_at         TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS sent_at              TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS in_flight_since      TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS send_count           INTEGER     NOT NULL DEFAULT 0 CHECK (send_count >= 0),
    ADD COLUMN IF NOT EXISTS not_before           TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS next_poll_at         TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS deadline_at          TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS authority_ref        TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS authority_status     TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS receipt_hash         TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_failure         TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS escalated_at         TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS updated_at           TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE tax_filing_submissions DROP CONSTRAINT IF EXISTS tax_filing_status_check;
ALTER TABLE tax_filing_submissions ADD CONSTRAINT tax_filing_status_check CHECK (status IN
    ('PENDING', 'PREPARED', 'VALIDATED', 'BLOCKED', 'SUBMITTING', 'SUBMITTED', 'PENDING_UNKNOWN', 'ACCEPTED', 'REJECTED', 'SUPERSEDED'));
ALTER TABLE tax_filing_submissions DROP CONSTRAINT IF EXISTS tax_filing_in_flight_consistent;
ALTER TABLE tax_filing_submissions ADD CONSTRAINT tax_filing_in_flight_consistent
    CHECK ((status = 'SUBMITTING') = (in_flight_since IS NOT NULL));

-- one authority attempt identity per approved TAX subject
CREATE UNIQUE INDEX IF NOT EXISTS uq_tax_filing_subject ON tax_filing_submissions (tenant_id, subject_ref) WHERE subject_ref <> '';
CREATE INDEX IF NOT EXISTS idx_tax_filing_in_flight ON tax_filing_submissions (in_flight_since) WHERE status = 'SUBMITTING';
CREATE INDEX IF NOT EXISTS idx_tax_filing_poll ON tax_filing_submissions (next_poll_at) WHERE status IN ('SUBMITTED', 'PENDING_UNKNOWN');

CREATE OR REPLACE FUNCTION tax_filing_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'tax filing submissions are never deleted';
    END IF;
    IF OLD.status IN ('ACCEPTED', 'REJECTED', 'SUPERSEDED', 'BLOCKED') AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'filing % is in terminal status % and cannot change', OLD.submission_id, OLD.status;
    END IF;
    IF OLD.status IN ('ACCEPTED', 'REJECTED', 'SUPERSEDED', 'BLOCKED') AND (
        NEW.authority_ref IS DISTINCT FROM OLD.authority_ref OR NEW.receipt_hash IS DISTINCT FROM OLD.receipt_hash) THEN
        RAISE EXCEPTION 'filing % is final; its authority receipt cannot change', OLD.submission_id;
    END IF;

    -- what was approved can never change: a changed document is a different subject
    IF NEW.interface_id IS DISTINCT FROM OLD.interface_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.tax_period IS DISTINCT FROM OLD.tax_period OR NEW.filing_type IS DISTINCT FROM OLD.filing_type
        OR NEW.tax_amount IS DISTINCT FROM OLD.tax_amount
        OR (OLD.payload_hash <> '' AND (NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
              OR NEW.document IS DISTINCT FROM OLD.document
              OR NEW.subject_ref IS DISTINCT FROM OLD.subject_ref
              OR NEW.subject_fingerprint IS DISTINCT FROM OLD.subject_fingerprint
              OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at))
    THEN
        RAISE EXCEPTION 'filing % was approved as submitted; its subject can never change', OLD.submission_id;
    END IF;

    -- the profile, schema rail and transformed payload are pinned once chosen
    IF (OLD.rail <> '' AND NEW.rail IS DISTINCT FROM OLD.rail)
        OR (OLD.profile_version IS NOT NULL AND NEW.profile_version IS DISTINCT FROM OLD.profile_version)
        OR (OLD.sent_payload_hash <> '' AND NEW.sent_payload_hash IS DISTINCT FROM OLD.sent_payload_hash)
    THEN
        RAISE EXCEPTION 'filing % profile context is pinned; a retry is sent exactly as the original', OLD.submission_id;
    END IF;
    IF NEW.send_count < OLD.send_count THEN
        RAISE EXCEPTION 'filing % send_count can only increase', OLD.submission_id;
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NEW.status = 'SUBMITTING' AND OLD.status NOT IN ('PREPARED', 'VALIDATED', 'PENDING_UNKNOWN') THEN
            RAISE EXCEPTION 'filing % cannot start sending from %', OLD.submission_id, OLD.status;
        END IF;
        IF NEW.status IN ('ACCEPTED', 'REJECTED') AND OLD.status NOT IN ('SUBMITTING', 'SUBMITTED', 'PENDING_UNKNOWN') THEN
            RAISE EXCEPTION 'filing % cannot be % without having been sent', OLD.submission_id, NEW.status;
        END IF;
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_tax_filing_guard ON tax_filing_submissions;
CREATE TRIGGER trg_tax_filing_guard BEFORE UPDATE OR DELETE ON tax_filing_submissions
    FOR EACH ROW EXECUTE FUNCTION tax_filing_guard();

-- ── authority profiles (platform reference data) ────────────────────────────
CREATE TABLE IF NOT EXISTS tax_authority_profiles (
    provider_key  VARCHAR(80)  NOT NULL CHECK (provider_key ~ '^[a-z0-9][a-z0-9._-]{2,79}$'),
    version       INTEGER      NOT NULL CHECK (version >= 1),
    status        VARCHAR(20)  NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT', 'CERTIFIED', 'DEPRECATED', 'WITHDRAWN')),
    spec          JSONB        NOT NULL,
    spec_hash     VARCHAR(64)  NOT NULL CHECK (spec_hash ~ '^[0-9a-f]{64}$'),
    created_by    VARCHAR(255) NOT NULL,
    certified_by  VARCHAR(255),
    certified_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_key, version),
    CONSTRAINT tax_profile_certified_by_other CHECK (
        status IN ('DRAFT', 'WITHDRAWN') OR (certified_by IS NOT NULL AND certified_at IS NOT NULL AND certified_by <> created_by))
);
CREATE OR REPLACE FUNCTION tax_profile_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'authority profiles are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.provider_key IS DISTINCT FROM OLD.provider_key OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.created_by IS DISTINCT FROM OLD.created_by THEN
        RAISE EXCEPTION 'authority profile identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'DRAFT' AND (NEW.spec IS DISTINCT FROM OLD.spec OR NEW.spec_hash IS DISTINCT FROM OLD.spec_hash) THEN
        RAISE EXCEPTION 'a certified authority profile is immutable; publish a new version' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status AND NOT (
        (OLD.status = 'DRAFT' AND NEW.status IN ('CERTIFIED', 'WITHDRAWN'))
        OR (OLD.status = 'CERTIFIED' AND NEW.status IN ('DEPRECATED', 'WITHDRAWN'))
        OR (OLD.status = 'DEPRECATED' AND NEW.status = 'WITHDRAWN')) THEN
        RAISE EXCEPTION 'authority profile status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_tax_profile_guard ON tax_authority_profiles;
CREATE TRIGGER trg_tax_profile_guard BEFORE UPDATE OR DELETE ON tax_authority_profiles
    FOR EACH ROW EXECUTE FUNCTION tax_profile_guard();

-- ── evidence (append-only) ──────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION tax_evidence_immutable() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE IF NOT EXISTS tax_transport_log (
    log_id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       VARCHAR(64)  NOT NULL,
    submission_id   VARCHAR(64)  NOT NULL REFERENCES tax_filing_submissions (submission_id),
    provider_key    TEXT         NOT NULL,
    profile_version INTEGER      NOT NULL,
    operation       VARCHAR(10)  NOT NULL CHECK (operation IN ('SUBMIT', 'QUERY')),
    disposition     VARCHAR(10)  NOT NULL CHECK (disposition IN ('NOT_SENT', 'ANSWERED', 'UNKNOWN')),
    class           VARCHAR(20)  NOT NULL DEFAULT '',
    http_status     INTEGER      NOT NULL DEFAULT 0,
    request_hash    TEXT         NOT NULL DEFAULT '',
    response_hash   TEXT         NOT NULL DEFAULT '',
    provider_ref    TEXT         NOT NULL DEFAULT '',
    error_code      TEXT         NOT NULL DEFAULT '',
    duration_ms     INTEGER      NOT NULL DEFAULT 0,
    retry_after_s   INTEGER      NOT NULL DEFAULT 0,
    actor           TEXT         NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tax_transport_submission ON tax_transport_log (submission_id, created_at);
CREATE INDEX IF NOT EXISTS idx_tax_transport_provider ON tax_transport_log (provider_key, created_at DESC);
DROP TRIGGER IF EXISTS trg_tax_transport_immutable ON tax_transport_log;
CREATE TRIGGER trg_tax_transport_immutable BEFORE UPDATE OR DELETE ON tax_transport_log
    FOR EACH ROW EXECUTE FUNCTION tax_evidence_immutable();

CREATE TABLE IF NOT EXISTS tax_authority_callbacks (
    receipt_id      UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       VARCHAR(64)  NOT NULL,
    provider_key    VARCHAR(80)  NOT NULL,
    event_ref       VARCHAR(128) NOT NULL,
    submission_ref  VARCHAR(128) NOT NULL,
    submission_id   VARCHAR(64)  NULL REFERENCES tax_filing_submissions (submission_id),
    scheme          VARCHAR(20)  NOT NULL,
    reported_status VARCHAR(40)  NOT NULL,
    mapped_status   VARCHAR(20)  NOT NULL DEFAULT '',
    event_time      TIMESTAMPTZ  NULL,
    body_hash       VARCHAR(64)  NOT NULL CHECK (body_hash ~ '^[0-9a-f]{64}$'),
    outcome         VARCHAR(24)  NOT NULL CHECK (outcome IN
        ('APPLIED', 'DUPLICATE', 'STALE_IGNORED', 'REGRESSION_BLOCKED', 'UNKNOWN_SUBMISSION', 'AMBIGUOUS_SUBMISSION')),
    detail          TEXT         NOT NULL DEFAULT '',
    received_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_tax_callback_first ON tax_authority_callbacks (tenant_id, provider_key, event_ref)
    WHERE outcome NOT IN ('DUPLICATE', 'UNKNOWN_SUBMISSION', 'AMBIGUOUS_SUBMISSION');
CREATE INDEX IF NOT EXISTS idx_tax_callbacks_submission ON tax_authority_callbacks (submission_id, received_at);
DROP TRIGGER IF EXISTS trg_tax_callbacks_immutable ON tax_authority_callbacks;
CREATE TRIGGER trg_tax_callbacks_immutable BEFORE UPDATE OR DELETE ON tax_authority_callbacks
    FOR EACH ROW EXECUTE FUNCTION tax_evidence_immutable();

-- ── row-level security ──────────────────────────────────────────────────────
ALTER TABLE tax_authority_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_authority_profiles FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tax_profiles_read ON tax_authority_profiles;
CREATE POLICY tax_profiles_read ON tax_authority_profiles FOR SELECT USING (true);
DROP POLICY IF EXISTS tax_profiles_platform_write ON tax_authority_profiles;
CREATE POLICY tax_profiles_platform_write ON tax_authority_profiles FOR INSERT
    WITH CHECK (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-00000000f001');
DROP POLICY IF EXISTS tax_profiles_platform_update ON tax_authority_profiles;
CREATE POLICY tax_profiles_platform_update ON tax_authority_profiles FOR UPDATE
    USING (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-00000000f001')
    WITH CHECK (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-00000000f001');

ALTER TABLE tax_transport_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_transport_log FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON tax_transport_log;
CREATE POLICY tenant_isolation_policy ON tax_transport_log FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE tax_authority_callbacks ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_authority_callbacks FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON tax_authority_callbacks;
CREATE POLICY tenant_isolation_policy ON tax_authority_callbacks FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- The in-flight sweeper and the status poller discover due work across tenants. They name themselves
-- (app.tax_worker) instead of running unscoped: the flag admits them to SELECT filings and to nothing else;
-- each filing is then resolved in a tenant-scoped transaction.
DROP POLICY IF EXISTS tax_filing_worker_read ON tax_filing_submissions;
CREATE POLICY tax_filing_worker_read ON tax_filing_submissions FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.tax_worker', true), ''), 'false') = 'true');
