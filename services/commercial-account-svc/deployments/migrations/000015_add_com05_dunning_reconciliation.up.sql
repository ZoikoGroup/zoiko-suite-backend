-- 000015_add_com05_dunning_reconciliation.up.sql
-- COM-05 Platform Commercial Billing, part 5d (ZS-SVC-Q-001 §4.5; COM-CTRL-012,
-- -030, -031, -032; negative paths #30, #33, #34, #42, #43).
--
-- Scope of this part: DunningCase (Start/AdvanceDunning/StopDunning,
-- GetDunningState) and CommercialReconciliation (ReconcileCommercialAccount,
-- GetCommercialReconciliation) — the last two entities of COM-05's Owns list.
--
-- Dunning-entitlement boundary (COM-CTRL-030): this migration adds no
-- column or trigger anywhere that lets a dunning case mutate an
-- entitlement decision. A DunningCase is billing-health evidence COM-03
-- may read and weigh through its own restriction/policy mechanism (COM-03,
-- migration 000010) — it is never itself a capability grant or denial.
--
-- CommercialReconciliation re-verifies, per organization and on demand,
-- invariants this service already enforces at write time (invoice
-- header/line totals, collected-vs-invoiced, credit+write-off caps, refund
-- caps) — defense in depth against anything that bypassed the application
-- layer, not a second copy of the same arithmetic GetBalance already does
-- from the same source rows. A run is an immutable, append-only snapshot;
-- like COM-01/COM-02/COM-04's other RunStatus-carries-the-answer precedents
-- (see also the BNK-05 gap-remediation plan's identical reasoning), a
-- discrepancy is recorded as an exception, never auto-corrected or hidden,
-- and there is deliberately no second "resolution workflow" table this
-- doc does not itself describe.
--
-- Custom SQLSTATE reused: CP001 immutable/lifecycle violation.

-- ── Dunning policy (seller plane, versioned; COM-CTRL-012, negative path #34) ─
--
-- Day-count thresholds for the four escalation stages. A case binds to the
-- version effective when it opened and keeps using it for its whole life —
-- a later policy version never retroactively changes an open case's
-- thresholds (negative path #34).
CREATE TABLE dunning_policy_versions (
    policy_version           INT          PRIMARY KEY CHECK (policy_version >= 1),
    notice1_after_days        INT          NOT NULL CHECK (notice1_after_days >= 0),
    notice2_after_days         INT          NOT NULL CHECK (notice2_after_days > notice1_after_days),
    restrict_after_days         INT          NOT NULL CHECK (restrict_after_days > notice2_after_days),
    suspend_after_days           INT          NOT NULL CHECK (suspend_after_days > restrict_after_days),
    effective_from                 TIMESTAMPTZ  NOT NULL,
    reason                           TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    created_at                       TIMESTAMPTZ  NOT NULL,
    created_by_principal_id           VARCHAR(255) NOT NULL
);

CREATE TRIGGER trg_dunning_policy_versions_immutable
    BEFORE UPDATE OR DELETE ON dunning_policy_versions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE dunning_policy_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE dunning_policy_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY dunning_policy_read ON dunning_policy_versions FOR SELECT USING (true);
CREATE POLICY dunning_policy_seller_insert ON dunning_policy_versions FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- ── Dunning case ────────────────────────────────────────────────────────────
--
-- One case per invoice while open; a new case can only open once the prior
-- one for that invoice is CLOSED (e.g. paid, credited/written off, or
-- otherwise resolved outside COM-05's own view). COM-CTRL-031: closing a
-- case never deletes it — CLOSED is a status, the row and its full history
-- of advances (dunning_case_events) remain.
CREATE TABLE dunning_cases (
    case_id                    TEXT         PRIMARY KEY
        CHECK (case_id ~ '^cdun_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id              UUID         NOT NULL,
    invoice_id                     TEXT         NOT NULL REFERENCES platform_commercial_invoices (invoice_id),
    policy_version                   INT          NOT NULL REFERENCES dunning_policy_versions (policy_version),
    status                             VARCHAR(16)  NOT NULL DEFAULT 'NOTICE_1'
        CHECK (status IN ('NOTICE_1', 'NOTICE_2', 'RESTRICTED', 'SUSPENDED', 'CLOSED')),
    opened_at                           TIMESTAMPTZ  NOT NULL,
    opened_by_principal_id               VARCHAR(255) NOT NULL,
    last_advanced_at                       TIMESTAMPTZ,
    closed_at                               TIMESTAMPTZ,
    closed_by_principal_id                   VARCHAR(255),
    close_reason                              TEXT,
    CONSTRAINT dunning_cases_closed_all_or_nothing CHECK (
        (closed_at IS NULL) = (status <> 'CLOSED')
        AND (closed_at IS NULL) = (closed_by_principal_id IS NULL)
        AND (closed_at IS NULL) = (close_reason IS NULL))
);

CREATE UNIQUE INDEX idx_dunning_cases_one_open_per_invoice ON dunning_cases (invoice_id) WHERE status <> 'CLOSED';
CREATE INDEX idx_dunning_cases_org ON dunning_cases (organization_id);

CREATE FUNCTION enforce_dunning_case_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    stage_rank CONSTANT jsonb := '{"NOTICE_1": 1, "NOTICE_2": 2, "RESTRICTED": 3, "SUSPENDED": 4}'::jsonb;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dunning case % cannot be deleted' , OLD.case_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status = 'CLOSED' THEN
        RAISE EXCEPTION 'dunning case % is closed and immutable', OLD.case_id USING ERRCODE = 'CP001';
    END IF;
    IF NEW.status = 'CLOSED' THEN
        RETURN NEW; -- StopDunning: closeable from any open stage.
    END IF;
    IF (stage_rank->>NEW.status)::int IS DISTINCT FROM (stage_rank->>OLD.status)::int + 1 THEN
        RAISE EXCEPTION 'dunning case % may only advance one stage at a time (% -> %)', OLD.case_id, OLD.status, NEW.status
            USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_dunning_cases_lifecycle
    BEFORE UPDATE OR DELETE ON dunning_cases
    FOR EACH ROW EXECUTE FUNCTION enforce_dunning_case_lifecycle();

ALTER TABLE dunning_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE dunning_cases FORCE ROW LEVEL SECURITY;
CREATE POLICY dunning_cases_read ON dunning_cases FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY dunning_cases_seller_insert ON dunning_cases FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY dunning_cases_seller_update ON dunning_cases FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY dunning_cases_seller_delete ON dunning_cases FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');

-- ── Commercial reconciliation (immutable, append-only snapshot) ───────────
CREATE TABLE commercial_reconciliations (
    reconciliation_id          TEXT         PRIMARY KEY
        CHECK (reconciliation_id ~ '^crec_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id               UUID         NOT NULL,
    status                          VARCHAR(16)  NOT NULL CHECK (status IN ('CLEAN', 'EXCEPTIONS_OPEN')),
    invoice_count                    INT          NOT NULL CHECK (invoice_count >= 0),
    exception_count                    INT          NOT NULL CHECK (exception_count >= 0),
    exceptions                          JSONB        NOT NULL DEFAULT '[]'::jsonb,
    run_at                                TIMESTAMPTZ  NOT NULL,
    run_by_principal_id                    VARCHAR(255) NOT NULL,
    CONSTRAINT commercial_reconciliations_status_matches_count CHECK (
        (status = 'CLEAN') = (exception_count = 0))
);

CREATE INDEX idx_commercial_reconciliations_org ON commercial_reconciliations (organization_id, run_at DESC);

CREATE TRIGGER trg_commercial_reconciliations_immutable
    BEFORE UPDATE OR DELETE ON commercial_reconciliations
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE commercial_reconciliations ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_reconciliations FORCE ROW LEVEL SECURITY;
CREATE POLICY commercial_reconciliations_read ON commercial_reconciliations FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY commercial_reconciliations_seller_insert ON commercial_reconciliations FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
