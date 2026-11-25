-- 000038: decision-log outbox (tracker "Assignment 25 Nov 2026: automatic
-- decision capture", step DL-4; Doc 01 s7).
--
-- The Jurisdiction engine's governed decisions (pack certify, release, block,
-- unblock, withdraw, rollback, supersede; interpretation and import approval;
-- calendar publication; payment-term approval) are queued here in the request's
-- unit of work, with the change and its registry event, and the decision-log
-- relay (identity-envelope/decisionledger) delivers them to
-- governance-decision-log-svc as svc-jurisdiction-rules (authorization-svc
-- 000046). A ledger outage delays the record; it never fails the change.
--
-- The ledger is tenant-scoped, so each record carries the tenant of the request
-- that made the decision (the packs themselves are platform reference data).
-- Row-level security as in 000032: a row is visible to its own tenant
-- (app.tenant_id, set by store.Unit) and to the relay, whose transactions set
-- app.decision_log_relay = 'on'. Delivered rows are purged after the relay's
-- retention; pending and parked rows never are.

BEGIN;

CREATE TABLE IF NOT EXISTS decision_log_outbox (
    decision_id     VARCHAR(64) PRIMARY KEY,
    tenant_id       VARCHAR(64) NOT NULL,
    record          JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,
    parked_at       TIMESTAMPTZ,
    last_error      TEXT
);

CREATE INDEX IF NOT EXISTS idx_decision_log_outbox_due
    ON decision_log_outbox (next_attempt_at)
    WHERE delivered_at IS NULL AND parked_at IS NULL;

ALTER TABLE decision_log_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE decision_log_outbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS decision_log_outbox_scope ON decision_log_outbox;
CREATE POLICY decision_log_outbox_scope ON decision_log_outbox
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.decision_log_relay', true) = 'on')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.decision_log_relay', true) = 'on');

COMMENT ON TABLE decision_log_outbox IS
    'Governed jurisdiction decisions awaiting delivery to governance-decision-log-svc (000038, DL-4).';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON decision_log_outbox TO app_jurisdiction_rules;
    END IF;
END $$;

COMMIT;
