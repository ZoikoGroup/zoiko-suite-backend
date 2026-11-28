-- 000017: decision-log outbox (tracker "Assignment 25 Nov 2026: automatic
-- decision capture", step DL-5; Doc 01 s7).
--
-- An obligation raised by POST /v1/obligations/evaluate and a clock waiver are queued here in the transaction of the
-- decision, and the decision-log relay (identity-envelope/decisionledger)
-- delivers them to governance-decision-log-svc as svc-obligations (authorization-svc
-- 000046). A ledger outage delays the record; it never fails the decision.
--
-- Row-level security: a row is visible to its own tenant (app.tenant_id, set by
-- the store's tenant transaction) and to the relay, whose transactions set
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
    'Governance decisions awaiting delivery to governance-decision-log-svc (000017, DL-5).';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_obligations') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON decision_log_outbox TO app_obligations;
    END IF;
END $$;

COMMIT;
