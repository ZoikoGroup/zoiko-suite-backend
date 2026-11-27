-- ZS-SVC-Z-001 RB-05 / DQC-017 (docs/architecture/DQC_GAP_MATRIX.md, "GL acts on withdrawal").
--
-- A journal or kernel posting that cites a certification records its reliance with the issuing service (steps K, K2). When that
-- certification is revoked, the ledger must not stay silent about what rests on it. The general ledger therefore
--
--   * WATCHES every certification it has relied on (certification_watch), and
--   * FLAGS every posted journal and kernel posting that cites a certification once it is found revoked (certification_flags),
--     until a person acknowledges the flag with a reason.
--
-- It does NOT reverse anything: whether a posted journal is reversed is a financial decision, so a flag is evidence for a person,
-- and an acknowledgement is that person's recorded decision (optionally naming the remediating journal).
--
-- The database holds the rules: a watch row never goes back from withdrawn; a flag is never deleted, and only ever moves OPEN to
-- ACKNOWLEDGED, once, with who/when/why.

CREATE TABLE certification_watch (
    tenant_id             UUID         NOT NULL,
    legal_entity_id       UUID         NOT NULL,
    certification_service VARCHAR(40)  NOT NULL CHECK (certification_service IN ('data-quality-svc', 'reconciliation-engine-svc')),
    run_id                VARCHAR(128) NOT NULL CHECK (run_id ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$'),
    first_relied_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- The issuing service's last answer: ACTIVE, STALE, SUPERSEDED or REVOKED, and when it was asked.
    last_status           VARCHAR(16),
    last_checked_at       TIMESTAMPTZ,
    withdrawn_at          TIMESTAMPTZ,
    revocation_id         VARCHAR(128),
    withdrawal_reason     TEXT,
    PRIMARY KEY (tenant_id, legal_entity_id, certification_service, run_id),
    CONSTRAINT certification_watch_withdrawn_complete CHECK (
        (withdrawn_at IS NULL AND revocation_id IS NULL AND withdrawal_reason IS NULL)
        OR (withdrawn_at IS NOT NULL AND revocation_id IS NOT NULL AND withdrawal_reason IS NOT NULL AND length(btrim(withdrawal_reason)) > 0
            AND COALESCE(last_status, '') = 'REVOKED'))
);

CREATE OR REPLACE FUNCTION enforce_certification_watch_rules()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a certification watch row cannot be deleted';
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.certification_service IS DISTINCT FROM OLD.certification_service OR NEW.run_id IS DISTINCT FROM OLD.run_id
       OR NEW.first_relied_at IS DISTINCT FROM OLD.first_relied_at THEN
        RAISE EXCEPTION 'a certification watch row''s identity cannot change';
    END IF;
    IF OLD.withdrawn_at IS NOT NULL AND (NEW.withdrawn_at IS DISTINCT FROM OLD.withdrawn_at OR NEW.revocation_id IS DISTINCT FROM OLD.revocation_id
       OR NEW.withdrawal_reason IS DISTINCT FROM OLD.withdrawal_reason OR NEW.last_status IS DISTINCT FROM 'REVOKED') THEN
        RAISE EXCEPTION 'a withdrawn certification stays withdrawn';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER certification_watch_rules BEFORE UPDATE OR DELETE ON certification_watch
    FOR EACH ROW EXECUTE FUNCTION enforce_certification_watch_rules();

ALTER TABLE certification_watch ENABLE ROW LEVEL SECURITY;
ALTER TABLE certification_watch FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON certification_watch
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

CREATE TABLE certification_flags (
    flag_id               UUID         PRIMARY KEY,
    tenant_id             UUID         NOT NULL,
    legal_entity_id       UUID         NOT NULL,
    certification_service VARCHAR(40)  NOT NULL,
    run_id                VARCHAR(128) NOT NULL,
    -- What rests on the withdrawn certification: a manual journal, or a kernel posting (event:book:basis). journal_id is the
    -- journal either way, so a person can open it.
    subject_type          VARCHAR(24)  NOT NULL CHECK (subject_type IN ('journal', 'accounting-posting')),
    subject_id            VARCHAR(160) NOT NULL,
    journal_id            UUID         NOT NULL REFERENCES journal_headers (journal_id),
    revocation_id         VARCHAR(128) NOT NULL CHECK (length(btrim(revocation_id)) > 0),
    reason                TEXT         NOT NULL CHECK (length(btrim(reason)) > 0),
    status                VARCHAR(14)  NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'ACKNOWLEDGED')),
    raised_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    acknowledged_by       VARCHAR(128),
    acknowledged_at       TIMESTAMPTZ,
    acknowledgement       TEXT,
    -- The journal that remediated it (a reversal or correction), when the person's decision was to act.
    remediation_journal_id UUID        REFERENCES journal_headers (journal_id),
    FOREIGN KEY (tenant_id, legal_entity_id, certification_service, run_id)
        REFERENCES certification_watch (tenant_id, legal_entity_id, certification_service, run_id),
    -- One flag per thing and certification: finding it again is the same fact.
    UNIQUE (tenant_id, certification_service, run_id, subject_type, subject_id),
    CONSTRAINT certification_flags_ack_complete CHECK (
        (status = 'OPEN' AND acknowledged_by IS NULL AND acknowledged_at IS NULL AND acknowledgement IS NULL AND remediation_journal_id IS NULL)
        -- Written with explicit NOT NULLs: a CHECK that evaluates to NULL passes, so length(btrim(NULL)) > 0 would admit a missing reason.
        OR (status = 'ACKNOWLEDGED' AND acknowledged_by IS NOT NULL AND length(btrim(acknowledged_by)) > 0 AND acknowledged_at IS NOT NULL
            AND acknowledgement IS NOT NULL AND length(btrim(acknowledgement)) > 0))
);

CREATE INDEX idx_certification_flags_open ON certification_flags (tenant_id, legal_entity_id, status, raised_at);

CREATE OR REPLACE FUNCTION enforce_certification_flag_rules()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a certification flag cannot be deleted';
    END IF;
    IF OLD.status = 'ACKNOWLEDGED' THEN
        RAISE EXCEPTION 'an acknowledged certification flag is immutable';
    END IF;
    -- The only legal move is OPEN -> ACKNOWLEDGED; nothing about what was flagged changes.
    IF NEW.flag_id IS DISTINCT FROM OLD.flag_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id OR NEW.certification_service IS DISTINCT FROM OLD.certification_service
       OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.subject_type IS DISTINCT FROM OLD.subject_type
       OR NEW.subject_id IS DISTINCT FROM OLD.subject_id OR NEW.journal_id IS DISTINCT FROM OLD.journal_id
       OR NEW.revocation_id IS DISTINCT FROM OLD.revocation_id OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.raised_at IS DISTINCT FROM OLD.raised_at THEN
        RAISE EXCEPTION 'a certification flag records what was flagged and cannot be rewritten';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER certification_flag_rules BEFORE UPDATE OR DELETE ON certification_flags
    FOR EACH ROW EXECUTE FUNCTION enforce_certification_flag_rules();

ALTER TABLE certification_flags ENABLE ROW LEVEL SECURITY;
ALTER TABLE certification_flags FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON certification_flags
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- Finding what rests on one certification is a containment lookup on the evidence the journal or event carries.
CREATE INDEX idx_journal_headers_evidence_refs ON journal_headers USING gin (evidence_refs);
CREATE INDEX idx_accounting_events_evidence_refs ON accounting_events USING gin (evidence_refs jsonb_path_ops);
