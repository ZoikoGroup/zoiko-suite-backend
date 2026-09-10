-- Migration: 000005_add_accruals.up.sql
--
-- ACC-07 (Accruals): "owns AccrualSchedule, basis/evidence, recognition
-- instances and reversal plan. Must never own: Direct ledger writes." A
-- schedule is a real, stateful resource (Draft -> PendingApproval ->
-- Approved -> Active -> Completed/Cancelled/Superseded), so it is UPDATEd
-- in place like this service's own fiscal_periods — not effective-dated
-- versioning, which is for facts superseded by a new fact, not a lifecycle
-- with real forward-only states.
--
-- Recognition instances are the opposite: permanent evidence that a
-- specific period's recognition posting happened, for exactly one purpose
-- (deduping a replayed recognition run per the spec's own negative-path
-- table: "Recognition run replay -> ... no unauthorized or duplicate
-- accounting consequence"). Append-only, same doctrine as this platform's
-- other evidence tables, and UNIQUE(schedule_id, fiscal_period) makes "at
-- most one recognition per schedule per period" a database-enforced
-- invariant, not just application discipline.

CREATE TABLE accrual_schedules (
    schedule_id           UUID PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    legal_entity_id        VARCHAR(255) NOT NULL,
    description             TEXT NOT NULL,
    policy_version          VARCHAR(64) NOT NULL,
    total_amount            NUMERIC(18,2) NOT NULL,
    start_fiscal_period     VARCHAR(20) NOT NULL, -- 'YYYY-MM'
    period_count            INT NOT NULL,
    debit_account_code      VARCHAR(64) NOT NULL, -- expense side
    credit_account_code     VARCHAR(64) NOT NULL, -- accrued-liability control side
    status                  VARCHAR(20) NOT NULL, -- DRAFT|PENDING_APPROVAL|APPROVED|ACTIVE|COMPLETED|CANCELLED|SUPERSEDED
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id VARCHAR(255) NOT NULL,
    submitted_at            TIMESTAMP WITH TIME ZONE,
    submitted_by_principal_id VARCHAR(255),
    approved_at             TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id VARCHAR(255),
    cancelled_at            TIMESTAMP WITH TIME ZONE,
    cancelled_by_principal_id VARCHAR(255)
);

CREATE TABLE accrual_recognition_instances (
    recognition_instance_id UUID PRIMARY KEY,
    tenant_id                 VARCHAR(255) NOT NULL,
    schedule_id                UUID NOT NULL REFERENCES accrual_schedules(schedule_id),
    fiscal_period               VARCHAR(20) NOT NULL,
    recognized_amount           NUMERIC(18,2) NOT NULL,
    journal_id                   VARCHAR(255) NOT NULL, -- general-ledger-svc's FINALIZED journal — ACC-07 never writes the ledger itself
    recognized_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    recognized_by_principal_id   VARCHAR(255) NOT NULL,
    UNIQUE (schedule_id, fiscal_period)
);

CREATE OR REPLACE FUNCTION reject_recognition_instance_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'accrual_recognition_instances is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_recognition_update
    BEFORE UPDATE ON accrual_recognition_instances
    FOR EACH ROW EXECUTE FUNCTION reject_recognition_instance_mutation();
CREATE TRIGGER trg_reject_recognition_delete
    BEFORE DELETE ON accrual_recognition_instances
    FOR EACH ROW EXECUTE FUNCTION reject_recognition_instance_mutation();

ALTER TABLE accrual_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE accrual_schedules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON accrual_schedules
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE accrual_recognition_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE accrual_recognition_instances FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON accrual_recognition_instances
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_accrual_schedules_entity ON accrual_schedules (tenant_id, legal_entity_id);
CREATE INDEX idx_recognition_instances_schedule ON accrual_recognition_instances (tenant_id, schedule_id);
