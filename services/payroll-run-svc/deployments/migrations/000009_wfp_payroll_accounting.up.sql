-- ZS-SVC-L-001 WFP-05 — Payroll Journal Interface.
--
-- Turns an ACCEPTED payroll result version into ONE governed accounting subject
-- per result version + book + purpose, and tracks it through submission to
-- general-ledger-svc's posting engine (ACC-04). This service never writes the
-- ledger: the only path to it is POST /v1/postings/events (and /reversals) on the
-- ledger service, performed by a dispatcher from rows in this table.
--
-- Decisions (2026-10-14): one mapping key per component kind; single default book
-- carried but not sent (the ledger has no book field yet); results flagged as
-- missing employer liabilities post what exists with the flags carried; an
-- explicit submit by a principal other than the one who received the result.

CREATE TABLE IF NOT EXISTS wfp_payroll_accounting_bridges (
    bridge_id             UUID PRIMARY KEY,
    tenant_id             TEXT NOT NULL,
    run_ref_id            UUID NOT NULL REFERENCES wfp_payroll_run_refs (run_ref_id),
    result_id             UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    legal_entity_id       TEXT NOT NULL,
    accounting_book_id    TEXT NOT NULL,
    posting_purpose       TEXT NOT NULL,
    -- A reversal subject points at the posted subject it reverses.
    original_bridge_id    UUID REFERENCES wfp_payroll_accounting_bridges (bridge_id),
    -- Deterministic: a retry of the same subject is the same posting, whatever
    -- replica or restart delivers it (the ledger is idempotent on this).
    source_event_id       TEXT NOT NULL,
    fiscal_period         TEXT NOT NULL,
    document_date         DATE NOT NULL,
    posting_date          DATE NOT NULL,
    transaction_currency  TEXT NOT NULL,
    description           TEXT NOT NULL,
    -- The posting proposal, frozen at creation: mapping_key + exact debit/credit.
    lines                 JSONB NOT NULL,
    line_count            INTEGER NOT NULL,
    total_debit           NUMERIC(18, 4) NOT NULL,
    total_credit          NUMERIC(18, 4) NOT NULL,
    evidence_flags        TEXT[] NOT NULL DEFAULT '{}',
    state                 TEXT NOT NULL,
    correction_kind       TEXT,
    rejection_reason      TEXT,
    execution_id          TEXT,
    journal_id            TEXT,
    attempts              INTEGER NOT NULL DEFAULT 0,
    last_error            TEXT,
    submitted_by          TEXT,
    submitted_at          TIMESTAMPTZ,
    posted_at             TIMESTAMPTZ,
    reconciliation_state  TEXT NOT NULL DEFAULT 'NOT_RECONCILED',
    reconciled_at         TIMESTAMPTZ,
    reconciliation_detail JSONB,
    version               INTEGER NOT NULL DEFAULT 1,
    created_by            TEXT NOT NULL,
    correlation_id        TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_acc_purpose_check CHECK (posting_purpose IN ('PAYROLL_RESULT', 'PAYROLL_REVERSAL')),
    CONSTRAINT wfp_acc_state_check CHECK (state IN ('EVENT_CREATED', 'SUBMITTED', 'POSTED', 'REJECTED', 'CORRECTED')),
    CONSTRAINT wfp_acc_correction_check CHECK ((state = 'CORRECTED') = (correction_kind IS NOT NULL)
                                               AND (correction_kind IS NULL OR correction_kind IN ('REVERSED', 'WITHDRAWN'))),
    CONSTRAINT wfp_acc_recon_check CHECK (reconciliation_state IN ('NOT_RECONCILED', 'MATCHED', 'MISMATCH')),
    CONSTRAINT wfp_acc_currency_check CHECK (transaction_currency ~ '^[A-Z]{3}$'),
    -- An unbalanced posting proposal is unrepresentable.
    CONSTRAINT wfp_acc_balanced_check CHECK (total_debit = total_credit AND total_debit >= 0),
    CONSTRAINT wfp_acc_posted_has_journal CHECK (state <> 'POSTED' OR (journal_id IS NOT NULL AND posted_at IS NOT NULL)),
    CONSTRAINT wfp_acc_submitted_has_actor CHECK (state NOT IN ('SUBMITTED', 'POSTED') OR submitted_by IS NOT NULL),
    CONSTRAINT wfp_acc_reversal_has_original CHECK ((posting_purpose = 'PAYROLL_REVERSAL') = (original_bridge_id IS NOT NULL)),
    CONSTRAINT wfp_acc_period_check CHECK (fiscal_period ~ '^[0-9]{4}-[0-9]{2}$'),
    -- One canonical accounting subject per result version + book + purpose.
    CONSTRAINT wfp_acc_unique_subject UNIQUE (tenant_id, result_id, accounting_book_id, posting_purpose),
    CONSTRAINT wfp_acc_unique_source UNIQUE (tenant_id, source_event_id)
);
CREATE INDEX IF NOT EXISTS idx_wfp_acc_run_ref ON wfp_payroll_accounting_bridges (tenant_id, run_ref_id);
CREATE INDEX IF NOT EXISTS idx_wfp_acc_dispatch ON wfp_payroll_accounting_bridges (created_at) WHERE state = 'SUBMITTED';

-- ── Guards ───────────────────────────────────────────────────────────────────
-- Creation: only an ACCEPTED result can be posted; a reversal only reverses a
-- POSTED subject whose result has been SUPERSEDED. Nothing is ever deleted.
CREATE OR REPLACE FUNCTION wfp_acc_insert_guard() RETURNS trigger AS $$
DECLARE r_state TEXT; res_outcome TEXT; o_state TEXT; o_result UUID;
BEGIN
    SELECT acceptance_state INTO r_state FROM wfp_payroll_run_refs WHERE run_ref_id = NEW.run_ref_id;
    SELECT outcome INTO res_outcome FROM wfp_payroll_results WHERE result_id = NEW.result_id AND run_ref_id = NEW.run_ref_id;
    IF res_outcome IS DISTINCT FROM 'ACCEPTED' THEN
        RAISE EXCEPTION 'only an ACCEPTED payroll result has an accounting consequence'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.state <> 'EVENT_CREATED' THEN
        RAISE EXCEPTION 'an accounting subject starts as EVENT_CREATED' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.posting_purpose = 'PAYROLL_RESULT' THEN
        IF r_state IS DISTINCT FROM 'ACCEPTED' THEN
            RAISE EXCEPTION 'a payroll accounting subject is created only for an ACCEPTED result (run is %)', r_state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSE
        SELECT state, result_id INTO o_state, o_result FROM wfp_payroll_accounting_bridges WHERE bridge_id = NEW.original_bridge_id;
        IF o_state IS DISTINCT FROM 'POSTED' OR o_result IS DISTINCT FROM NEW.result_id OR r_state IS DISTINCT FROM 'SUPERSEDED' THEN
            RAISE EXCEPTION 'a reversal needs a POSTED original whose result was SUPERSEDED'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wfp_acc_insert_guard_trg ON wfp_payroll_accounting_bridges;
CREATE TRIGGER wfp_acc_insert_guard_trg BEFORE INSERT ON wfp_payroll_accounting_bridges
    FOR EACH ROW EXECUTE FUNCTION wfp_acc_insert_guard();

CREATE OR REPLACE FUNCTION wfp_acc_update_guard() RETURNS trigger AS $$
DECLARE r_state TEXT; received TEXT; rev_posted BOOLEAN;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'accounting subjects are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- The proposal is evidence: it never changes after creation.
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.run_ref_id IS DISTINCT FROM OLD.run_ref_id
       OR NEW.result_id IS DISTINCT FROM OLD.result_id OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.accounting_book_id IS DISTINCT FROM OLD.accounting_book_id OR NEW.posting_purpose IS DISTINCT FROM OLD.posting_purpose
       OR NEW.original_bridge_id IS DISTINCT FROM OLD.original_bridge_id OR NEW.source_event_id IS DISTINCT FROM OLD.source_event_id
       OR NEW.fiscal_period IS DISTINCT FROM OLD.fiscal_period OR NEW.document_date IS DISTINCT FROM OLD.document_date
       OR NEW.posting_date IS DISTINCT FROM OLD.posting_date OR NEW.transaction_currency IS DISTINCT FROM OLD.transaction_currency
       OR NEW.description IS DISTINCT FROM OLD.description OR NEW.lines IS DISTINCT FROM OLD.lines
       OR NEW.line_count IS DISTINCT FROM OLD.line_count OR NEW.total_debit IS DISTINCT FROM OLD.total_debit
       OR NEW.total_credit IS DISTINCT FROM OLD.total_credit OR NEW.evidence_flags IS DISTINCT FROM OLD.evidence_flags
       OR NEW.created_by IS DISTINCT FROM OLD.created_by THEN
        RAISE EXCEPTION 'an accounting subject is immutable; a changed result is a new result version'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
           (OLD.state = 'EVENT_CREATED' AND NEW.state IN ('SUBMITTED', 'CORRECTED'))
        OR (OLD.state = 'SUBMITTED'     AND NEW.state IN ('POSTED', 'REJECTED', 'CORRECTED'))
        OR (OLD.state = 'REJECTED'      AND NEW.state IN ('SUBMITTED', 'CORRECTED'))
        OR (OLD.state = 'POSTED'        AND NEW.state = 'CORRECTED')) THEN
        RAISE EXCEPTION 'illegal accounting transition % -> %', OLD.state, NEW.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- Submitting: the result must still be ACCEPTED (a reversal: SUPERSEDED), and the
    -- submitter must not be the principal who received the result (separation of duties).
    IF NEW.state = 'SUBMITTED' AND OLD.state IS DISTINCT FROM 'SUBMITTED' THEN
        SELECT r.acceptance_state INTO r_state FROM wfp_payroll_run_refs r WHERE r.run_ref_id = NEW.run_ref_id;
        SELECT res.received_by INTO received FROM wfp_payroll_results res WHERE res.result_id = NEW.result_id;
        IF NEW.posting_purpose = 'PAYROLL_RESULT' AND r_state IS DISTINCT FROM 'ACCEPTED' THEN
            RAISE EXCEPTION 'a superseded or rejected payroll result cannot be submitted for posting'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.submitted_by IS NULL OR NEW.submitted_by = received THEN
            RAISE EXCEPTION 'separation of duties: the principal who received the payroll result cannot submit its posting'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;

    IF NEW.state = 'CORRECTED' THEN
        SELECT r.acceptance_state INTO r_state FROM wfp_payroll_run_refs r WHERE r.run_ref_id = NEW.run_ref_id;
        IF NEW.correction_kind = 'REVERSED' THEN
            SELECT EXISTS (SELECT 1 FROM wfp_payroll_accounting_bridges rb
                           WHERE rb.original_bridge_id = NEW.bridge_id AND rb.posting_purpose = 'PAYROLL_REVERSAL' AND rb.state = 'POSTED')
              INTO rev_posted;
            IF OLD.state IS DISTINCT FROM 'POSTED' OR NOT rev_posted THEN
                RAISE EXCEPTION 'a subject is REVERSED only after its reversal has POSTED'
                    USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        ELSIF OLD.state = 'POSTED' THEN
            RAISE EXCEPTION 'a POSTED subject is corrected only by a reversal' USING ERRCODE = 'integrity_constraint_violation';
        ELSIF r_state IS DISTINCT FROM 'SUPERSEDED' AND NEW.posting_purpose = 'PAYROLL_RESULT' THEN
            RAISE EXCEPTION 'an unposted subject is withdrawn only when its result has been SUPERSEDED'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wfp_acc_update_guard_trg ON wfp_payroll_accounting_bridges;
CREATE TRIGGER wfp_acc_update_guard_trg BEFORE UPDATE OR DELETE ON wfp_payroll_accounting_bridges
    FOR EACH ROW EXECUTE FUNCTION wfp_acc_update_guard();

-- ── Row-level security ───────────────────────────────────────────────────────
ALTER TABLE wfp_payroll_accounting_bridges ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_accounting_bridges FORCE ROW LEVEL SECURITY;
-- Request-path access is confined to the actor's tenant. The dispatcher is not a
-- request: it installs app.outbox_relay (like the event relay) to drain every
-- tenant's SUBMITTED subjects.
CREATE POLICY tenant_isolation_policy ON wfp_payroll_accounting_bridges FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
