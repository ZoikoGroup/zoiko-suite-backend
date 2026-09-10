-- Migration: 000006_add_prepayments.up.sql
--
-- ACC-08 (Prepayments & Deferrals): "owns RecognitionSchedule, remaining
-- balance, period recognition instances and schedule versions. Must never
-- own: Direct ledger writes." Same shape as ACC-07's accrual schedule
-- (§ migration 000005) — a stateful schedule UPDATEd in place through its
-- own state model (Draft -> Approved -> Active ->
-- Completed/Terminated/Superseded), plus append-only recognition
-- instances unique per (schedule_id, fiscal_period).
--
-- Economically the mirror image of ACC-07: a prepayment schedule
-- recognizes an ALREADY-PAID prepaid asset into expense over time
-- (debit_account_code = expense, credit_account_code = the prepaid-asset
-- control account being drawn down), where ACC-07 recognizes a
-- NOT-YET-PAID liability. The schedule and recognition-instance shapes are
-- otherwise identical, which is why this migration and ACC-07's share a
-- structure rather than inventing a different one.

CREATE TABLE prepayment_schedules (
    schedule_id                UUID PRIMARY KEY,
    tenant_id                   VARCHAR(255) NOT NULL,
    legal_entity_id             VARCHAR(255) NOT NULL,
    description                  TEXT NOT NULL,
    total_amount                 NUMERIC(18,2) NOT NULL,
    start_fiscal_period           VARCHAR(20) NOT NULL, -- 'YYYY-MM'
    period_count                  INT NOT NULL,
    debit_account_code            VARCHAR(64) NOT NULL, -- expense side
    credit_account_code           VARCHAR(64) NOT NULL, -- prepaid-asset control side
    status                        VARCHAR(20) NOT NULL, -- DRAFT|APPROVED|ACTIVE|COMPLETED|TERMINATED
    created_at                    TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id       VARCHAR(255) NOT NULL,
    approved_at                   TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id      VARCHAR(255),
    terminated_at                 TIMESTAMP WITH TIME ZONE,
    terminated_by_principal_id    VARCHAR(255),
    termination_reason            TEXT,
    -- termination_final_treatment implements the spec's own negative path
    -- ("Terminate without final balance treatment" must be blocked): a
    -- terminate request with no explicit treatment is refused at the
    -- handler layer, so this column is only ever NULL before termination.
    termination_final_treatment   VARCHAR(20) -- WRITE_OFF | RECOGNIZE_REMAINING
);

CREATE TABLE prepayment_recognition_instances (
    recognition_instance_id UUID PRIMARY KEY,
    tenant_id                 VARCHAR(255) NOT NULL,
    schedule_id                UUID NOT NULL REFERENCES prepayment_schedules(schedule_id),
    fiscal_period               VARCHAR(20) NOT NULL, -- a real 'YYYY-MM' period, or the literal 'TERMINATION' for a final settlement entry
    recognized_amount           NUMERIC(18,2) NOT NULL,
    journal_id                   VARCHAR(255) NOT NULL,
    recognized_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    recognized_by_principal_id   VARCHAR(255) NOT NULL,
    UNIQUE (schedule_id, fiscal_period)
);

CREATE OR REPLACE FUNCTION reject_prepayment_recognition_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'prepayment_recognition_instances is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_prepayment_recognition_update
    BEFORE UPDATE ON prepayment_recognition_instances
    FOR EACH ROW EXECUTE FUNCTION reject_prepayment_recognition_mutation();
CREATE TRIGGER trg_reject_prepayment_recognition_delete
    BEFORE DELETE ON prepayment_recognition_instances
    FOR EACH ROW EXECUTE FUNCTION reject_prepayment_recognition_mutation();

ALTER TABLE prepayment_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE prepayment_schedules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON prepayment_schedules
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE prepayment_recognition_instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE prepayment_recognition_instances FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON prepayment_recognition_instances
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_prepayment_schedules_entity ON prepayment_schedules (tenant_id, legal_entity_id);
CREATE INDEX idx_prepayment_recognition_instances_schedule ON prepayment_recognition_instances (tenant_id, schedule_id);
