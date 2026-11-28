-- 000047: decision-log outbox (tracker "Assignment 25 Nov 2026: automatic
-- decision capture", step DL-3; decision D-DL-1).
--
-- Doc 03 s8.3: "every decision logged with actor, action, basis, and outcome;
-- denials must be evidentially retrievable". Every decision stays in
-- access_decision_log. A DENY or REQUIRE_APPROVAL is also written here, in the
-- transaction of its access_decision_log row, and the decision-log relay
-- (identity-envelope/decisionledger) delivers it to governance-decision-log-svc
-- as svc-authorization (000046). PERMIT and STEP_UP stay only in the
-- access-decision log (D-DL-1).
--
-- A ledger outage delays the record (it waits here, retried with backoff); it
-- never fails or slows the decision. Delivered rows are purged after the
-- relay's retention (the ledger is the durable record); pending and parked
-- rows never are.
--
-- Row-level security: a row is visible to its own tenant (app.tenant_id, set by
-- the store's withRLS) and to the relay, whose transactions set
-- app.decision_log_relay = 'on'.

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
    'DENY / REQUIRE_APPROVAL decisions awaiting delivery to governance-decision-log-svc (000047, D-DL-1).';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON decision_log_outbox TO app_authorization;
    END IF;
END $$;

COMMIT;
