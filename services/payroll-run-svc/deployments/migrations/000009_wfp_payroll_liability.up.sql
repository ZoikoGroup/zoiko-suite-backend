-- ZS-SVC-L-001 WFP-06 — Payroll Liability Interface.
--
-- Tracks payroll-derived liabilities (tax, social insurance, pension, benefit,
-- garnishment, other) from an ACCEPTED result through due, scheduled, settled and
-- reconciled states, separately from filing and payment. It does not decide what is
-- owed (the payroll engine did), does not move money, and never marks a liability
-- paid from payment INTENT: settlement needs Banking evidence.
--
-- Decisions (2026-10-14): the registry lives here (not in payroll-tax-svc); a
-- governed, effective-dated liability-class registry supplies authority, jurisdiction
-- and due-date rule; settlement is verified Banking evidence only; Zoiko results that
-- lack expected classes get a visible completeness exception.

-- ── Liability class registry (WFP-D-06) ──────────────────────────────────────
-- component kind + code -> class, authority (a counterparty), jurisdiction, due-date
-- rule. Append-only: a change is a new row with a later effective_from. component_code
-- '*' matches any code of the kind; an exact code wins over '*'.
CREATE TABLE IF NOT EXISTS wfp_liability_classes (
    class_row_id               UUID PRIMARY KEY,
    tenant_id                  TEXT NOT NULL,
    legal_entity_id            TEXT NOT NULL,
    component_kind             TEXT NOT NULL,
    component_code             TEXT NOT NULL DEFAULT '*',
    class_version              INTEGER NOT NULL,
    class_code                 TEXT NOT NULL,
    liability_type             TEXT NOT NULL,
    authority_counterparty_id  TEXT NOT NULL,
    jurisdiction_id            TEXT NOT NULL,
    due_rule_kind              TEXT NOT NULL,
    due_rule_value             INTEGER NOT NULL,
    -- true: every result of this entity is expected to carry this class; a result
    -- that does not gets a completeness exception (WFP-CTRL-014).
    required                   BOOLEAN NOT NULL DEFAULT false,
    active                     BOOLEAN NOT NULL DEFAULT true,
    rule_ref                   TEXT NOT NULL,
    effective_from             DATE NOT NULL,
    registered_by              TEXT NOT NULL,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_lc_kind_check CHECK (component_kind IN
        ('EMPLOYEE_DEDUCTION', 'EMPLOYEE_TAX', 'EMPLOYEE_SOCIAL', 'EMPLOYEE_PENSION', 'EMPLOYEE_BENEFIT',
         'EMPLOYER_TAX', 'EMPLOYER_SOCIAL', 'EMPLOYER_PENSION', 'EMPLOYER_BENEFIT', 'EMPLOYER_OTHER')),
    CONSTRAINT wfp_lc_type_check CHECK (liability_type IN ('TAX', 'SOCIAL', 'PENSION', 'BENEFIT', 'GARNISHMENT', 'OTHER')),
    CONSTRAINT wfp_lc_rule_check CHECK (
        (due_rule_kind = 'DAY_OF_FOLLOWING_MONTH' AND due_rule_value BETWEEN 1 AND 28)
        OR (due_rule_kind = 'DAYS_AFTER_PAY_DATE' AND due_rule_value BETWEEN 0 AND 120)),
    CONSTRAINT wfp_lc_ref_check CHECK (length(btrim(rule_ref)) > 0),
    CONSTRAINT wfp_lc_version_check CHECK (class_version >= 1),
    CONSTRAINT wfp_lc_unique_version UNIQUE (tenant_id, legal_entity_id, component_kind, component_code, class_version),
    CONSTRAINT wfp_lc_unique_effective UNIQUE (tenant_id, legal_entity_id, component_kind, component_code, effective_from)
);

-- ── Liabilities ──────────────────────────────────────────────────────────────
-- Identity: result + class + authority + liability type (+ period via the result).
CREATE TABLE IF NOT EXISTS wfp_payroll_liabilities (
    liability_id              UUID PRIMARY KEY,
    tenant_id                 TEXT NOT NULL,
    legal_entity_id           TEXT NOT NULL,
    run_ref_id                UUID NOT NULL REFERENCES wfp_payroll_run_refs (run_ref_id),
    result_id                 UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    payroll_period_id         TEXT NOT NULL,
    class_code                TEXT NOT NULL,
    class_row_id              UUID NOT NULL REFERENCES wfp_liability_classes (class_row_id),
    liability_type            TEXT NOT NULL,
    party_side                TEXT NOT NULL,
    authority_counterparty_id TEXT NOT NULL,
    jurisdiction_id           TEXT NOT NULL,
    rule_ref                  TEXT NOT NULL,
    component_kinds           TEXT[] NOT NULL,
    source_component_count    INTEGER NOT NULL,
    authority_labels          TEXT[] NOT NULL DEFAULT '{}',
    -- Per component kind, so reconciliation can compare each payable key with the posting.
    kind_amounts              JSONB NOT NULL DEFAULT '{}'::jsonb,
    amount                    NUMERIC(18, 4) NOT NULL,
    currency                  TEXT NOT NULL,
    due_date                  DATE NOT NULL,
    due_rule_kind             TEXT NOT NULL,
    due_rule_value            INTEGER NOT NULL,
    state                     TEXT NOT NULL,
    exception_state           TEXT NOT NULL DEFAULT 'NONE',
    settled_amount            NUMERIC(18, 4) NOT NULL DEFAULT 0,
    adjustment_total          NUMERIC(18, 4) NOT NULL DEFAULT 0,
    scheduled_for             DATE,
    scheduled_by              TEXT,
    dispute_reason            TEXT,
    reconciliation_state      TEXT NOT NULL DEFAULT 'NOT_RECONCILED',
    reconciled_at             TIMESTAMPTZ,
    reconciliation_detail     JSONB,
    version                   INTEGER NOT NULL DEFAULT 1,
    correlation_id            TEXT NOT NULL,
    created_by                TEXT NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_liab_state_check CHECK (state IN
        ('RECOGNIZED', 'DUE', 'SCHEDULED', 'PARTIALLY_SETTLED', 'SETTLED', 'RECONCILED', 'SUPERSEDED')),
    CONSTRAINT wfp_liab_exc_check CHECK (exception_state IN ('NONE', 'REVIEW_REQUIRED', 'DISPUTED')),
    CONSTRAINT wfp_liab_side_check CHECK (party_side IN ('EMPLOYEE_WITHHELD', 'EMPLOYER')),
    CONSTRAINT wfp_liab_recon_check CHECK (reconciliation_state IN ('NOT_RECONCILED', 'MATCHED', 'MISMATCH')),
    CONSTRAINT wfp_liab_amount_check CHECK (amount > 0),
    CONSTRAINT wfp_liab_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    -- Never settled beyond what is owed (original amount plus explicit adjustments).
    CONSTRAINT wfp_liab_settled_bound CHECK (settled_amount >= 0 AND settled_amount <= amount + adjustment_total),
    CONSTRAINT wfp_liab_adjust_bound CHECK (amount + adjustment_total >= 0),
    CONSTRAINT wfp_liab_state_vs_settled CHECK (
        (state IN ('SETTLED', 'RECONCILED')) = (settled_amount = amount + adjustment_total AND settled_amount > 0)
        OR state = 'SUPERSEDED' OR state NOT IN ('SETTLED', 'RECONCILED')),
    CONSTRAINT wfp_liab_dispute_reason CHECK ((exception_state = 'DISPUTED') = (dispute_reason IS NOT NULL)),
    CONSTRAINT wfp_liab_identity UNIQUE (tenant_id, result_id, class_code, authority_counterparty_id, liability_type)
);
CREATE INDEX IF NOT EXISTS idx_wfp_liab_result ON wfp_payroll_liabilities (tenant_id, result_id);
CREATE INDEX IF NOT EXISTS idx_wfp_liab_due ON wfp_payroll_liabilities (tenant_id, due_date) WHERE state IN ('RECOGNIZED', 'DUE', 'SCHEDULED', 'PARTIALLY_SETTLED');
CREATE INDEX IF NOT EXISTS idx_wfp_liab_authority ON wfp_payroll_liabilities (tenant_id, authority_counterparty_id, currency);

-- ── Settlements (verified Banking evidence) ──────────────────────────────────
CREATE TABLE IF NOT EXISTS wfp_liability_settlements (
    settlement_id     UUID PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    liability_id      UUID NOT NULL REFERENCES wfp_payroll_liabilities (liability_id),
    payment_id        TEXT NOT NULL,
    amount            NUMERIC(18, 4) NOT NULL,
    currency          TEXT NOT NULL,
    declared_by       TEXT NOT NULL,
    banking_status    TEXT NOT NULL,
    finality_source   TEXT,
    -- The Banking status record carries status and entity but no amount or beneficiary,
    -- so the amount is the caller's declaration until payment instructions (WFP-07)
    -- carry fingerprints. The evidence says so.
    amount_evidence   TEXT NOT NULL DEFAULT 'DECLARED_NOT_BANK_VERIFIED',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_settle_amount CHECK (amount > 0),
    CONSTRAINT wfp_settle_status CHECK (banking_status = 'SETTLED'),
    -- One payment applied to one liability once: a replay is the same settlement.
    CONSTRAINT wfp_settle_unique UNIQUE (tenant_id, liability_id, payment_id)
);

-- ── Adjustments ──────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS wfp_liability_adjustments (
    adjustment_id  UUID PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    liability_id   UUID NOT NULL REFERENCES wfp_payroll_liabilities (liability_id),
    delta          NUMERIC(18, 4) NOT NULL,
    reason         TEXT NOT NULL,
    evidence_ref   TEXT NOT NULL,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_adj_delta CHECK (delta <> 0),
    CONSTRAINT wfp_adj_text CHECK (length(btrim(reason)) > 0 AND length(btrim(evidence_ref)) > 0)
);

-- ── Exceptions (non-dismissible: they close only when their cause is fixed) ──
CREATE TABLE IF NOT EXISTS wfp_liability_exceptions (
    exception_id    UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    legal_entity_id TEXT NOT NULL,
    result_id       UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    liability_id    UUID REFERENCES wfp_payroll_liabilities (liability_id),
    code            TEXT NOT NULL,
    subject_key     TEXT NOT NULL,
    amount          NUMERIC(18, 4),
    detail          TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'OPEN',
    resolution      TEXT,
    resolved_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_exc_state CHECK (state IN ('OPEN', 'RESOLVED')),
    CONSTRAINT wfp_exc_resolved CHECK ((state = 'RESOLVED') = (resolved_at IS NOT NULL)),
    CONSTRAINT wfp_exc_unique UNIQUE (tenant_id, result_id, code, subject_key)
);
CREATE INDEX IF NOT EXISTS idx_wfp_exc_open ON wfp_liability_exceptions (tenant_id, legal_entity_id) WHERE state = 'OPEN';

-- ── Guards ───────────────────────────────────────────────────────────────────
-- Registry, settlements and adjustments are evidence: append-only.
CREATE OR REPLACE FUNCTION wfp_liab_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only (% blocked)', TG_TABLE_NAME, TG_OP USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_lc_append_only_trg ON wfp_liability_classes;
CREATE TRIGGER wfp_lc_append_only_trg BEFORE UPDATE OR DELETE ON wfp_liability_classes FOR EACH ROW EXECUTE FUNCTION wfp_liab_append_only();
DROP TRIGGER IF EXISTS wfp_settle_append_only_trg ON wfp_liability_settlements;
CREATE TRIGGER wfp_settle_append_only_trg BEFORE UPDATE OR DELETE ON wfp_liability_settlements FOR EACH ROW EXECUTE FUNCTION wfp_liab_append_only();
DROP TRIGGER IF EXISTS wfp_adj_append_only_trg ON wfp_liability_adjustments;
CREATE TRIGGER wfp_adj_append_only_trg BEFORE UPDATE OR DELETE ON wfp_liability_adjustments FOR EACH ROW EXECUTE FUNCTION wfp_liab_append_only();

-- A liability exists only for an ACCEPTED result; nothing is deleted.
CREATE OR REPLACE FUNCTION wfp_liab_insert_guard() RETURNS trigger AS $$
DECLARE r_state TEXT; res_outcome TEXT;
BEGIN
    SELECT acceptance_state INTO r_state FROM wfp_payroll_run_refs WHERE run_ref_id = NEW.run_ref_id;
    SELECT outcome INTO res_outcome FROM wfp_payroll_results WHERE result_id = NEW.result_id AND run_ref_id = NEW.run_ref_id;
    IF res_outcome IS DISTINCT FROM 'ACCEPTED' OR r_state IS DISTINCT FROM 'ACCEPTED' THEN
        RAISE EXCEPTION 'a payroll liability is recognized only from an ACCEPTED result (run is %)', r_state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.state <> 'RECOGNIZED' OR NEW.settled_amount <> 0 OR NEW.adjustment_total <> 0 THEN
        RAISE EXCEPTION 'a liability starts RECOGNIZED with nothing settled or adjusted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_liab_insert_guard_trg ON wfp_payroll_liabilities;
CREATE TRIGGER wfp_liab_insert_guard_trg BEFORE INSERT ON wfp_payroll_liabilities FOR EACH ROW EXECUTE FUNCTION wfp_liab_insert_guard();

CREATE OR REPLACE FUNCTION wfp_liab_update_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payroll liabilities are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- What is owed, to whom, and by when never changes in place.
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.run_ref_id IS DISTINCT FROM OLD.run_ref_id OR NEW.result_id IS DISTINCT FROM OLD.result_id
       OR NEW.payroll_period_id IS DISTINCT FROM OLD.payroll_period_id OR NEW.class_code IS DISTINCT FROM OLD.class_code
       OR NEW.class_row_id IS DISTINCT FROM OLD.class_row_id OR NEW.liability_type IS DISTINCT FROM OLD.liability_type
       OR NEW.party_side IS DISTINCT FROM OLD.party_side OR NEW.authority_counterparty_id IS DISTINCT FROM OLD.authority_counterparty_id
       OR NEW.jurisdiction_id IS DISTINCT FROM OLD.jurisdiction_id OR NEW.rule_ref IS DISTINCT FROM OLD.rule_ref
       OR NEW.component_kinds IS DISTINCT FROM OLD.component_kinds OR NEW.source_component_count IS DISTINCT FROM OLD.source_component_count
       OR NEW.authority_labels IS DISTINCT FROM OLD.authority_labels OR NEW.kind_amounts IS DISTINCT FROM OLD.kind_amounts OR NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.currency IS DISTINCT FROM OLD.currency OR NEW.due_date IS DISTINCT FROM OLD.due_date
       OR NEW.due_rule_kind IS DISTINCT FROM OLD.due_rule_kind OR NEW.due_rule_value IS DISTINCT FROM OLD.due_rule_value
       OR NEW.created_by IS DISTINCT FROM OLD.created_by THEN
        RAISE EXCEPTION 'a recognized liability is immutable; correct it with an explicit adjustment'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
           (OLD.state = 'RECOGNIZED'        AND NEW.state IN ('DUE', 'SCHEDULED', 'PARTIALLY_SETTLED', 'SETTLED', 'SUPERSEDED'))
        OR (OLD.state = 'DUE'               AND NEW.state IN ('SCHEDULED', 'PARTIALLY_SETTLED', 'SETTLED', 'SUPERSEDED'))
        OR (OLD.state = 'SCHEDULED'         AND NEW.state IN ('PARTIALLY_SETTLED', 'SETTLED', 'SUPERSEDED'))
        OR (OLD.state = 'PARTIALLY_SETTLED' AND NEW.state IN ('PARTIALLY_SETTLED', 'SETTLED'))
        OR (OLD.state = 'SETTLED'           AND NEW.state IN ('RECONCILED', 'PARTIALLY_SETTLED'))) THEN
        RAISE EXCEPTION 'illegal liability transition % -> %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- A liability under review or in dispute cannot be scheduled.
    IF NEW.state = 'SCHEDULED' AND OLD.state IS DISTINCT FROM 'SCHEDULED' AND NEW.exception_state <> 'NONE' THEN
        RAISE EXCEPTION 'a liability under review or in dispute cannot be scheduled' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- A SUPERSEDED liability had nothing settled.
    IF NEW.state = 'SUPERSEDED' AND NEW.settled_amount <> 0 THEN
        RAISE EXCEPTION 'a liability with settlements is not superseded; it is adjusted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_liab_update_guard_trg ON wfp_payroll_liabilities;
CREATE TRIGGER wfp_liab_update_guard_trg BEFORE UPDATE OR DELETE ON wfp_payroll_liabilities FOR EACH ROW EXECUTE FUNCTION wfp_liab_update_guard();

-- Exceptions are never deleted; identity never changes; OPEN -> RESOLVED only.
CREATE OR REPLACE FUNCTION wfp_exc_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'liability exceptions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.result_id IS DISTINCT FROM OLD.result_id OR NEW.code IS DISTINCT FROM OLD.code
       OR NEW.subject_key IS DISTINCT FROM OLD.subject_key OR NEW.detail IS DISTINCT FROM OLD.detail THEN
        RAISE EXCEPTION 'an exception''s identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_exc_guard_trg ON wfp_liability_exceptions;
CREATE TRIGGER wfp_exc_guard_trg BEFORE UPDATE OR DELETE ON wfp_liability_exceptions FOR EACH ROW EXECUTE FUNCTION wfp_exc_guard();

-- ── Row-level security ───────────────────────────────────────────────────────
ALTER TABLE wfp_liability_classes ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_liability_classes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_liability_classes FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payroll_liabilities ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_liabilities FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_liabilities FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_liability_settlements ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_liability_settlements FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_liability_settlements FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_liability_adjustments ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_liability_adjustments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_liability_adjustments FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_liability_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_liability_exceptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_liability_exceptions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
