-- Migration: 000021_acc_controls_hardening.up.sql
--
-- ZS-SVC-B-001 first survey (SPEC_DEVIATIONS.md B1-B4). Four database-level
-- controls the application already claimed but nothing below it enforced.
--
-- 1. Maker-checker (B1). ACC-07/08/09/10/17 separated approve from create
--    only by permission name, so one principal holding both grants approved
--    their own schedule, rule, run or batch. These CHECKs make the row itself
--    refuse it. Spec: ACC-07 "Requester/preparer separation"; ACC-08
--    "Material manual schedule changes require independent approval"; ACC-09
--    "Rule author cannot approve material allocation"; ACC-17 "Migration
--    preparer cannot certify own material cutover". ACC-10 and ACC-17
--    approval are B-interpretations (see SPEC_DEVIATIONS B1).
--    Names start with sod_ so the store can map a violation to
--    ErrSelfApproval instead of "store unavailable".
--
--    DEPLOYMENT PREREQUISITE: these are validating constraints. A database
--    holding a historical self-approved row fails this migration and must be
--    remediated first (find them with the predicates below, negated).
--
-- 2. One reversal per FX revaluation run (B2). ReversePriorRevaluation never
--    looked for an existing reversal, so two calls produced two reversal runs
--    that could both be approved and posted: the original revaluation undone
--    twice. A historical duplicate fails this index the same way.
--
-- 3. command_idempotency (B3). The platform's table and rule (consolidation-
--    svc 000008, financial-control-svc BeginExecution): unique per tenant,
--    names the command and the one resource it created, recorded in the same
--    transaction as that write. request_hash is the shared contract's
--    "request fingerprint retained": the same key with a different body is a
--    conflict, never a replay of something else.
--
-- 4. fiscal_periods transition guard (B4). ACC-14 states are the period's
--    whole posting policy, yet any UPDATE could set any close_status: a
--    recovery script, a bad deploy or a direct SQL fix could reopen a
--    hard-closed book with no request and no approval. The BEFORE trigger
--    admits only the spec's graph; the deferred constraint trigger checks at
--    commit that entering AUTHORIZED_REOPEN is accompanied by an APPROVED
--    reopen request whose decision time and window match the period's
--    (ApproveReopenRequest moves the period before it marks the request, in
--    one transaction, so an immediate check would see PENDING).

-- 1. Maker-checker ------------------------------------------------------------

ALTER TABLE accrual_schedules ADD CONSTRAINT sod_accrual_approver_independent CHECK (
    approved_by_principal_id IS NULL
    OR (approved_by_principal_id <> created_by_principal_id
        AND approved_by_principal_id IS DISTINCT FROM submitted_by_principal_id)
);

ALTER TABLE prepayment_schedules ADD CONSTRAINT sod_prepayment_approver_independent CHECK (
    approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id
);

ALTER TABLE allocation_rules ADD CONSTRAINT sod_allocation_rule_approver_independent CHECK (
    approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id
);

ALTER TABLE fx_revaluation_runs ADD CONSTRAINT sod_fx_revaluation_approver_independent CHECK (
    approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id
);

ALTER TABLE migration_batches ADD CONSTRAINT sod_migration_approver_independent CHECK (
    approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id
);

ALTER TABLE migration_batches ADD CONSTRAINT sod_migration_certifier_independent CHECK (
    certified_by_principal_id IS NULL OR certified_by_principal_id <> created_by_principal_id
);

-- 2. One reversal per FX revaluation run ----------------------------------------

CREATE UNIQUE INDEX fx_revaluation_runs_one_reversal
    ON fx_revaluation_runs (tenant_id, reversal_of_run_id)
    WHERE reversal_of_run_id IS NOT NULL;

-- 3. command_idempotency ----------------------------------------------------------

CREATE TABLE command_idempotency (
    tenant_id                  VARCHAR(255) NOT NULL,
    idempotency_key            VARCHAR(255) NOT NULL,
    command                    VARCHAR(64)  NOT NULL,
    request_hash               CHAR(64)     NOT NULL,
    accrual_schedule_id        UUID REFERENCES accrual_schedules(schedule_id),
    prepayment_schedule_id     UUID REFERENCES prepayment_schedules(schedule_id),
    allocation_rule_version_id UUID REFERENCES allocation_rules(rule_version_id),
    fx_revaluation_run_id      UUID REFERENCES fx_revaluation_runs(run_id),
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, idempotency_key),
    CONSTRAINT chk_command_idempotency_one_target CHECK (
        num_nonnulls(accrual_schedule_id, prepayment_schedule_id,
                     allocation_rule_version_id, fx_revaluation_run_id) = 1
    )
);

-- The PK answers the command path's only lookup; these keep the foreign keys
-- cheap to check and answer "which key created this".
CREATE INDEX idx_command_idempotency_accrual ON command_idempotency (tenant_id, accrual_schedule_id);
CREATE INDEX idx_command_idempotency_prepayment ON command_idempotency (tenant_id, prepayment_schedule_id);
CREATE INDEX idx_command_idempotency_allocation ON command_idempotency (tenant_id, allocation_rule_version_id);
CREATE INDEX idx_command_idempotency_fx ON command_idempotency (tenant_id, fx_revaluation_run_id);

-- A claim is a record of what a key already did; editing one would let a key
-- replay something it never created.
CREATE OR REPLACE FUNCTION reject_command_idempotency_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'command_idempotency is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER command_idempotency_append_only BEFORE UPDATE OR DELETE ON command_idempotency
    FOR EACH ROW EXECUTE FUNCTION reject_command_idempotency_mutation();

ALTER TABLE command_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE command_idempotency FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON command_idempotency FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- 4. fiscal_periods transition guard ------------------------------------------------

CREATE OR REPLACE FUNCTION guard_fiscal_period_transition() RETURNS trigger AS $$
BEGIN
    IF (NEW.fiscal_period_id, NEW.tenant_id, NEW.legal_entity_id, NEW.period_name,
        NEW.period_start, NEW.period_end)
       IS DISTINCT FROM
       (OLD.fiscal_period_id, OLD.tenant_id, OLD.legal_entity_id, OLD.period_name,
        OLD.period_start, OLD.period_end) THEN
        RAISE EXCEPTION 'a fiscal period''s identity and dates are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.close_status = OLD.close_status THEN
        RETURN NEW;
    END IF;
    IF (OLD.close_status, NEW.close_status) NOT IN (
        ('OPEN', 'SOFT_CLOSE'),
        ('SOFT_CLOSE', 'CLOSE_REVIEW'),
        ('CLOSE_REVIEW', 'HARD_CLOSED'),
        ('HARD_CLOSED', 'AUTHORIZED_REOPEN'),
        ('RECLOSED', 'AUTHORIZED_REOPEN'),
        ('AUTHORIZED_REOPEN', 'RECLOSED')
    ) THEN
        RAISE EXCEPTION 'fiscal period % cannot move from % to %',
            OLD.fiscal_period_id, OLD.close_status, NEW.close_status
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER fiscal_periods_guard_transition BEFORE UPDATE ON fiscal_periods
    FOR EACH ROW EXECUTE FUNCTION guard_fiscal_period_transition();

CREATE OR REPLACE FUNCTION require_approved_reopen() RETURNS trigger AS $$
BEGIN
    IF NEW.close_status = 'AUTHORIZED_REOPEN' AND OLD.close_status <> 'AUTHORIZED_REOPEN'
       AND NOT EXISTS (
           SELECT 1 FROM period_reopen_requests r
            WHERE r.tenant_id = NEW.tenant_id
              AND r.fiscal_period_id = NEW.fiscal_period_id
              AND r.status = 'APPROVED'
              AND r.decided_at = NEW.reopened_at
              AND r.reopen_until = NEW.reopen_expires_at) THEN
        RAISE EXCEPTION 'fiscal period % entered AUTHORIZED_REOPEN without an approved reopen request',
            NEW.fiscal_period_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER fiscal_periods_reopen_requires_approval AFTER UPDATE ON fiscal_periods
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION require_approved_reopen();
