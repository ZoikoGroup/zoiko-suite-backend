-- Migration: 000012_add_accrual_reversal.up.sql
--
-- ACC-07 (Accruals) gap closure. §3.7's Minimum Negative-Path Acceptance
-- table names a scenario this codebase never built a mechanism for at
-- all: "Auto-reversal duplicates -> Block/deny/quarantine or preserve
-- safe state; stable error/evidence; no unauthorized or duplicate
-- accounting consequence." Failure/degradation semantics: "automatic
-- reversal uses same schedule identity and is idempotent." Until this
-- migration, no code path could reverse a RecognitionInstance at all —
-- CancelFutureAccrual only stops FUTURE recognition, it never touches
-- what has already posted (that is a deliberate, separate negative path,
-- "Accrual changed after some periods recognized").
--
-- accrual_recognition_instances (migration 000005) is deliberately
-- append-only — a trigger rejects UPDATE/DELETE outright — so a
-- reversal record cannot live as a mutation of that row. It lives here,
-- in its own table, exactly mirroring the append-only pattern:
-- UNIQUE(recognition_instance_id) is the database-enforced "at most one
-- reversal per recognized instance" invariant the negative path itself
-- names ("no ... duplicate accounting consequence").
CREATE TABLE accrual_recognition_reversals (
    recognition_reversal_id    UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    schedule_id                UUID NOT NULL REFERENCES accrual_schedules(schedule_id),
    recognition_instance_id    UUID NOT NULL REFERENCES accrual_recognition_instances(recognition_instance_id),
    reversing_journal_id       VARCHAR(255) NOT NULL, -- general-ledger-svc's own reversing FINALIZED journal
    reason                     TEXT NOT NULL,
    reversed_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    reversed_by_principal_id   VARCHAR(255) NOT NULL,
    UNIQUE (recognition_instance_id)
);

CREATE OR REPLACE FUNCTION reject_recognition_reversal_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'accrual_recognition_reversals is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_recognition_reversal_update
    BEFORE UPDATE ON accrual_recognition_reversals
    FOR EACH ROW EXECUTE FUNCTION reject_recognition_reversal_mutation();
CREATE TRIGGER trg_reject_recognition_reversal_delete
    BEFORE DELETE ON accrual_recognition_reversals
    FOR EACH ROW EXECUTE FUNCTION reject_recognition_reversal_mutation();

ALTER TABLE accrual_recognition_reversals ENABLE ROW LEVEL SECURITY;
ALTER TABLE accrual_recognition_reversals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON accrual_recognition_reversals
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_recognition_reversals_schedule ON accrual_recognition_reversals (tenant_id, schedule_id);
