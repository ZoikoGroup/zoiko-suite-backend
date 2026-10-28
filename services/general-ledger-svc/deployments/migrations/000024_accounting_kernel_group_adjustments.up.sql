-- Global Accounting Kernel, Wave 7: group policy adjustments and capital consolidation (standard §26: group adjustments, non-controlling
-- interest, intercompany elimination). Completes the consolidation interface of Wave 5.

-- ── capital consolidation: which member holds the investment in which member's equity ─────────────────────────────────

ALTER TABLE consolidation_members
    ADD COLUMN investment_account  VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN investor_entity_id  UUID,
    ADD COLUMN equity_accounts     JSONB       NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT consolidation_members_capital CHECK (
        (investment_account = '' AND investor_entity_id IS NULL AND equity_accounts = '[]'::jsonb)
        OR (investment_account <> '' AND investor_entity_id IS NOT NULL AND equity_accounts <> '[]'::jsonb AND investor_entity_id <> legal_entity_id));

CREATE OR REPLACE FUNCTION consolidation_member_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a consolidation membership is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.effective_to IS NOT NULL OR NEW.effective_to IS NULL
       OR (NEW.member_id, NEW.tenant_id, NEW.group_id, NEW.legal_entity_id, NEW.source_book_id, NEW.method, NEW.ownership_pct, NEW.effective_from, NEW.created_by, NEW.created_at,
           NEW.investment_account, NEW.investor_entity_id, NEW.equity_accounts::text)
          IS DISTINCT FROM (OLD.member_id, OLD.tenant_id, OLD.group_id, OLD.legal_entity_id, OLD.source_book_id, OLD.method, OLD.ownership_pct, OLD.effective_from, OLD.created_by, OLD.created_at,
           OLD.investment_account, OLD.investor_entity_id, OLD.equity_accounts::text) THEN
        RAISE EXCEPTION 'a consolidation membership can only be ended, once' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── group policy adjustments: balanced, evidenced, approved by someone else, applied by a run ─────────────────────────

CREATE TABLE consolidation_adjustments (
    adjustment_id     UUID PRIMARY KEY,
    tenant_id         UUID          NOT NULL,
    group_id          UUID          NOT NULL REFERENCES consolidation_groups(group_id),
    fiscal_period     VARCHAR(20)   NOT NULL,
    description       TEXT          NOT NULL,
    policy_ref        VARCHAR(120)  NOT NULL,
    evidence_ref      VARCHAR(255)  NOT NULL,
    lines             JSONB         NOT NULL,
    status            VARCHAR(10)   NOT NULL,
    created_by        VARCHAR(255)  NOT NULL,
    created_at        TIMESTAMPTZ   NOT NULL,
    approved_by       VARCHAR(255),
    approved_at       TIMESTAMPTZ,
    withdrawn_by      VARCHAR(255),
    withdrawn_at      TIMESTAMPTZ,
    withdrawal_reason TEXT,
    CONSTRAINT consolidation_adjustments_status CHECK (status IN ('PROPOSED', 'APPROVED', 'WITHDRAWN')),
    CONSTRAINT consolidation_adjustments_text CHECK (btrim(description) <> '' AND btrim(policy_ref) <> '' AND btrim(evidence_ref) <> ''),
    CONSTRAINT consolidation_adjustments_approved CHECK (status <> 'APPROVED' OR (approved_by IS NOT NULL AND approved_by <> created_by)),
    CONSTRAINT consolidation_adjustments_withdrawn CHECK (status <> 'WITHDRAWN' OR (withdrawn_by IS NOT NULL AND COALESCE(btrim(withdrawal_reason), '') <> ''))
);
CREATE INDEX idx_consolidation_adjustments_group ON consolidation_adjustments (tenant_id, group_id, fiscal_period, status);
ALTER TABLE consolidation_adjustments ENABLE ROW LEVEL SECURITY;
ALTER TABLE consolidation_adjustments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consolidation_adjustments
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE OR REPLACE FUNCTION consolidation_adjustment_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a group adjustment is never deleted: withdraw it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.adjustment_id, NEW.tenant_id, NEW.group_id, NEW.fiscal_period, NEW.description, NEW.policy_ref, NEW.evidence_ref, NEW.lines::text, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.adjustment_id, OLD.tenant_id, OLD.group_id, OLD.fiscal_period, OLD.description, OLD.policy_ref, OLD.evidence_ref, OLD.lines::text, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'the content of a group adjustment is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT ((OLD.status = 'PROPOSED' AND NEW.status IN ('APPROVED', 'WITHDRAWN')) OR (OLD.status = 'APPROVED' AND NEW.status = 'WITHDRAWN') OR OLD.status = NEW.status) THEN
        RAISE EXCEPTION 'group adjustment status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'WITHDRAWN' THEN
        RAISE EXCEPTION 'a withdrawn adjustment is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER consolidation_adjustment_guard_trg BEFORE UPDATE OR DELETE ON consolidation_adjustments FOR EACH ROW EXECUTE FUNCTION consolidation_adjustment_guard();

-- ── runs carry a third journal: the group adjustments ──────────────────────────────────────────────────────────────

ALTER TABLE consolidation_runs ADD COLUMN adjustment_journal_id UUID;

CREATE OR REPLACE FUNCTION consolidation_run_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a consolidation run is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.run_id, NEW.tenant_id, NEW.group_id, NEW.fiscal_period, NEW.as_of, NEW.mapping_version, NEW.prior_run_id, NEW.population, NEW.blocking_count,
        NEW.translation_journal_id, NEW.elimination_journal_id, NEW.adjustment_journal_id, NEW.run_hash, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.run_id, OLD.tenant_id, OLD.group_id, OLD.fiscal_period, OLD.as_of, OLD.mapping_version, OLD.prior_run_id, OLD.population, OLD.blocking_count,
        OLD.translation_journal_id, OLD.elimination_journal_id, OLD.adjustment_journal_id, OLD.run_hash, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'what a consolidation run consumed and produced is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'a superseded run is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'CERTIFIED' AND NEW.status = 'PROCESSED' THEN
        RAISE EXCEPTION 'a certified run cannot go back to review' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE journal_headers DROP CONSTRAINT journal_headers_purpose;
ALTER TABLE journal_headers ADD CONSTRAINT journal_headers_purpose CHECK (journal_purpose IS NULL OR journal_purpose IN
    ('ACCRUAL', 'PREPAYMENT', 'RECLASSIFICATION', 'CORRECTION', 'ESTIMATE_CHANGE', 'POLICY_CHANGE', 'OPENING_BALANCE', 'MIGRATION', 'CLOSE_ADJUSTMENT',
     'ALLOCATION', 'REVALUATION', 'RECONCILIATION_ADJUSTMENT', 'CONSOLIDATION', 'ELIMINATION', 'GROUP_ADJUSTMENT', 'OTHER'));
