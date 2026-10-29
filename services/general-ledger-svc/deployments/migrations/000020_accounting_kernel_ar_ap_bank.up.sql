-- 000020_accounting_kernel_ar_ap_bank.up.sql
--
-- Global Accounting Kernel, Wave 2 (AR / AP / bank): control types and counterparties on the chart and the ledger (§12, §15,
-- §16), reconciliation runs and cases (§27) and reconciliation adjustments (§17).

ALTER TABLE chart_of_accounts
    ADD COLUMN control_type          VARCHAR(16),
    ADD COLUMN counterparty_required BOOLEAN NOT NULL DEFAULT FALSE,
    ADD CONSTRAINT chart_control_type CHECK (control_type IS NULL OR control_type IN
        ('AR', 'AP', 'PAYROLL', 'TAX', 'INVENTORY', 'ASSET', 'BANK', 'CLEARING', 'INTERCOMPANY'));

-- journal lines and ledger entries carry the counterparty; ledger entries also carry the control type the account had when
-- the entry was posted, so a later change to the chart cannot move history between reconciliations
ALTER TABLE journal_lines  ADD COLUMN counterparty_id VARCHAR(200);
ALTER TABLE ledger_entries ADD COLUMN counterparty_id VARCHAR(200), ADD COLUMN control_type VARCHAR(16);
CREATE INDEX idx_ledger_entries_control ON ledger_entries (tenant_id, legal_entity_id, book_id, control_type, entry_seq) WHERE control_type IS NOT NULL;
CREATE INDEX idx_ledger_entries_counterparty ON ledger_entries (tenant_id, legal_entity_id, control_type, counterparty_id) WHERE counterparty_id IS NOT NULL;

-- ── reconciliation (§27) ───────────────────────────────────────────────────────────────────────────────────────────

-- Tolerance rules are versioned and never changed: a new version supersedes the old one from its effective date.
CREATE TABLE reconciliation_tolerances (
    tolerance_rule_id    UUID PRIMARY KEY,
    tenant_id            UUID          NOT NULL,
    reconciliation_type  VARCHAR(24)   NOT NULL,
    legal_entity_id      UUID,
    match_amount         NUMERIC(24,6) NOT NULL DEFAULT 0,
    warning_amount       NUMERIC(24,6) NOT NULL,
    version              INTEGER       NOT NULL,
    effective_from       DATE          NOT NULL,
    created_by           VARCHAR(255)  NOT NULL,
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT reconciliation_tolerance_amounts CHECK (match_amount >= 0 AND warning_amount >= match_amount),
    CONSTRAINT reconciliation_tolerance_version CHECK (version >= 1)
);
CREATE UNIQUE INDEX uq_reconciliation_tolerance ON reconciliation_tolerances
    (tenant_id, reconciliation_type, COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::uuid), version);
ALTER TABLE reconciliation_tolerances ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_tolerances FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reconciliation_tolerances
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE TRIGGER reconciliation_tolerances_no_change BEFORE UPDATE OR DELETE ON reconciliation_tolerances
    FOR EACH ROW EXECUTE FUNCTION kernel_append_only();

CREATE TABLE reconciliation_runs (
    reconciliation_run_id  UUID PRIMARY KEY,
    tenant_id              UUID          NOT NULL,
    reconciliation_type    VARCHAR(24)   NOT NULL,
    legal_entity_id        UUID          NOT NULL,
    accounting_book_id     UUID          NOT NULL REFERENCES accounting_books(book_id),
    fiscal_period          VARCHAR(20)   NOT NULL,
    as_of                  DATE          NOT NULL,
    currency               CHAR(3),
    population_a           JSONB         NOT NULL,
    population_b           JSONB         NOT NULL,
    expected_total         NUMERIC(24,6) NOT NULL,
    actual_total           NUMERIC(24,6) NOT NULL,
    difference             NUMERIC(24,6) NOT NULL,
    tolerance_rule_id      UUID REFERENCES reconciliation_tolerances(tolerance_rule_id),
    tolerance_version      INTEGER,
    status                 VARCHAR(20)   NOT NULL,
    owner                  VARCHAR(255),
    sla_due_at             TIMESTAMPTZ,
    resolution_type        VARCHAR(24),
    resolution_note        TEXT,
    evidence               JSONB         NOT NULL DEFAULT '[]'::jsonb,
    resolved_by            VARCHAR(255),
    resolved_at            TIMESTAMPTZ,
    approved_by            VARCHAR(255),
    approved_at            TIMESTAMPTZ,
    created_by             VARCHAR(255)  NOT NULL,
    created_at             TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT reconciliation_runs_type CHECK (reconciliation_type IN ('AR_GL', 'AP_GL', 'BANK_GL', 'PAYROLL_GL', 'TAX_GL', 'ASSET_GL', 'INVENTORY_GL', 'INTERCOMPANY_GL', 'EVENT_POSTING')),
    CONSTRAINT reconciliation_runs_status CHECK (status IN ('MATCHED', 'WARNING', 'BREAK', 'ACCEPTED_EXCEPTION', 'RESOLVED')),
    CONSTRAINT reconciliation_runs_resolution CHECK (resolution_type IS NULL OR resolution_type IN
        ('TIMING', 'MAPPING', 'MISSING_EVENT', 'DUPLICATE', 'MANUAL_CORRECTION', 'EXTERNAL_ISSUE', 'OTHER')),
    CONSTRAINT reconciliation_runs_difference CHECK (difference = expected_total - actual_total)
);
CREATE INDEX idx_reconciliation_runs_scope ON reconciliation_runs (tenant_id, legal_entity_id, accounting_book_id, fiscal_period, created_at DESC);
CREATE INDEX idx_reconciliation_runs_open ON reconciliation_runs (tenant_id, status) WHERE status IN ('WARNING', 'BREAK');
ALTER TABLE reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reconciliation_runs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- The totals, the populations and the tolerance a run was judged by never change; only the handling of the case does, and only
-- along the allowed moves. Accepting an exception needs someone other than the person who ran the reconciliation.
CREATE OR REPLACE FUNCTION reconciliation_run_guard() RETURNS trigger AS $$
DECLARE
    handling CONSTANT text[] := ARRAY['status', 'owner', 'sla_due_at', 'resolution_type', 'resolution_note', 'evidence', 'resolved_by', 'resolved_at', 'approved_by', 'approved_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a reconciliation run is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (to_jsonb(NEW) - handling) IS DISTINCT FROM (to_jsonb(OLD) - handling) THEN
        RAISE EXCEPTION 'the populations, totals and tolerance of a reconciliation run are fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT ((OLD.status IN ('WARNING', 'BREAK') AND NEW.status IN ('ACCEPTED_EXCEPTION', 'RESOLVED')) OR (OLD.status = 'ACCEPTED_EXCEPTION' AND NEW.status = 'RESOLVED')) THEN
            RAISE EXCEPTION 'a reconciliation case cannot move from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.status = 'ACCEPTED_EXCEPTION' THEN
            IF NEW.approved_by IS NULL OR NEW.approved_by = OLD.created_by OR jsonb_array_length(NEW.evidence) = 0 OR COALESCE(btrim(NEW.resolution_note), '') = '' THEN
                RAISE EXCEPTION 'accepting an exception needs evidence, a reason and an approver other than the person who ran the reconciliation' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        END IF;
        IF NEW.status = 'RESOLVED' THEN
            IF NEW.resolution_type IS NULL OR NEW.resolved_by IS NULL OR jsonb_array_length(NEW.evidence) = 0 THEN
                RAISE EXCEPTION 'resolving a case needs a resolution type, who resolved it and evidence' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER reconciliation_runs_guard BEFORE UPDATE OR DELETE ON reconciliation_runs
    FOR EACH ROW EXECUTE FUNCTION reconciliation_run_guard();

CREATE TABLE reconciliation_case_events (
    event_id               UUID PRIMARY KEY,
    tenant_id              UUID          NOT NULL,
    reconciliation_run_id  UUID          NOT NULL REFERENCES reconciliation_runs(reconciliation_run_id),
    kind                   VARCHAR(24)   NOT NULL,
    from_status            VARCHAR(20),
    to_status              VARCHAR(20),
    actor                  VARCHAR(255)  NOT NULL,
    detail                 JSONB,
    at                     TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE INDEX idx_reconciliation_case_events_run ON reconciliation_case_events (tenant_id, reconciliation_run_id, at);
ALTER TABLE reconciliation_case_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_case_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reconciliation_case_events
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE TRIGGER reconciliation_case_events_no_change BEFORE UPDATE OR DELETE ON reconciliation_case_events
    FOR EACH ROW EXECUTE FUNCTION kernel_append_only();

-- ── reconciliation adjustments (§17): a controlled journal that names the case it settles ─────────────────────────────

ALTER TABLE journal_headers ADD COLUMN reconciliation_case_id UUID REFERENCES reconciliation_runs(reconciliation_run_id);
CREATE INDEX idx_journal_headers_recon ON journal_headers (tenant_id, reconciliation_case_id) WHERE reconciliation_case_id IS NOT NULL;
ALTER TABLE journal_headers DROP CONSTRAINT journal_headers_purpose;
ALTER TABLE journal_headers ADD CONSTRAINT journal_headers_purpose CHECK (journal_purpose IS NULL OR journal_purpose IN
    ('ACCRUAL', 'PREPAYMENT', 'RECLASSIFICATION', 'CORRECTION', 'ESTIMATE_CHANGE', 'POLICY_CHANGE', 'OPENING_BALANCE', 'MIGRATION', 'CLOSE_ADJUSTMENT',
     'ALLOCATION', 'REVALUATION', 'RECONCILIATION_ADJUSTMENT', 'OTHER'));
