-- Migration: 000018_period_close_state_machine.up.sql
--
-- ACC-14 period close as the control state machine ZS-SVC-B-001 §17 and the
-- accounting kernel standard (§10.1) define:
--
--   OPEN → SOFT_CLOSE → CLOSE_REVIEW → HARD_CLOSED → AUTHORIZED_REOPEN → RECLOSED
--                                                  ↑______________________|
--
-- It used to be OPEN ⇄ LOCKED: one call closed a month, and one person with
-- PERIOD_REOPEN could reopen it, permanently, with no second pair of eyes —
-- the spec's own negative path #3, "Reopen without approval".
--
-- 1. close_status: LOCKED (and the never-written CLOSED) become HARD_CLOSED;
--    only the six states are admitted.
-- 2. reopened_at / reopen_expires_at: an AUTHORIZED_REOPEN is time-bounded
--    ("Named authority, time-bounded scope, reason and approval"). Present
--    exactly while the period is reopened.
-- 3. period_reopen_requests: reopening takes a request and an approval by a
--    DIFFERENT principal ("Requester cannot unilaterally reopen hard-closed
--    material book"). Enforced here as well as in the service. One pending
--    request per period; a decision is made once and never rewritten.
-- 4. period_state_transitions: every transition, who made it, when and why.
--    Append-only (GetCloseHistory; ACC-14 evidence "approvals, close/reopen
--    evidence").

UPDATE fiscal_periods SET close_status = 'HARD_CLOSED' WHERE close_status IN ('LOCKED', 'CLOSED');

ALTER TABLE fiscal_periods ADD CONSTRAINT fiscal_periods_close_status_known CHECK (close_status IN
    ('OPEN', 'SOFT_CLOSE', 'CLOSE_REVIEW', 'HARD_CLOSED', 'AUTHORIZED_REOPEN', 'RECLOSED'));

ALTER TABLE fiscal_periods ADD COLUMN reopened_at TIMESTAMPTZ;
ALTER TABLE fiscal_periods ADD COLUMN reopen_expires_at TIMESTAMPTZ;
ALTER TABLE fiscal_periods ADD CONSTRAINT fiscal_periods_reopen_window CHECK (
    (close_status = 'AUTHORIZED_REOPEN') = (reopened_at IS NOT NULL AND reopen_expires_at IS NOT NULL)
    AND (reopen_expires_at IS NULL OR reopen_expires_at > reopened_at)
);

CREATE TABLE period_reopen_requests (
    request_id               UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    fiscal_period_id         UUID         NOT NULL REFERENCES fiscal_periods(fiscal_period_id),
    requested_by_principal_id VARCHAR(255) NOT NULL,
    reason                   TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    reopen_until             TIMESTAMPTZ  NOT NULL,
    status                   VARCHAR(16)  NOT NULL DEFAULT 'PENDING'
                             CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED')),
    decided_by_principal_id  VARCHAR(255),
    decided_at               TIMESTAMPTZ,
    decision_reason          TEXT,
    created_at               TIMESTAMPTZ  NOT NULL,
    CONSTRAINT period_reopen_requests_decision CHECK (
        (status = 'PENDING' AND decided_by_principal_id IS NULL AND decided_at IS NULL)
        OR (status <> 'PENDING' AND decided_by_principal_id IS NOT NULL AND decided_at IS NOT NULL)
    ),
    -- The four-eyes rule, at the database: nobody decides their own request.
    CONSTRAINT period_reopen_requests_independent_decision CHECK (
        decided_by_principal_id IS NULL OR decided_by_principal_id <> requested_by_principal_id
    ),
    CONSTRAINT period_reopen_requests_until_after_request CHECK (reopen_until > created_at)
);
CREATE UNIQUE INDEX period_reopen_requests_one_pending
    ON period_reopen_requests (tenant_id, fiscal_period_id) WHERE status = 'PENDING';

CREATE OR REPLACE FUNCTION guard_reopen_request_mutation() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'period_reopen_requests rows are never deleted';
    END IF;
    IF OLD.status <> 'PENDING' THEN
        RAISE EXCEPTION 'reopen request % was already decided', OLD.request_id;
    END IF;
    IF (NEW.request_id, NEW.tenant_id, NEW.fiscal_period_id, NEW.requested_by_principal_id, NEW.reason,
        NEW.reopen_until, NEW.created_at)
       IS DISTINCT FROM
       (OLD.request_id, OLD.tenant_id, OLD.fiscal_period_id, OLD.requested_by_principal_id, OLD.reason,
        OLD.reopen_until, OLD.created_at) THEN
        RAISE EXCEPTION 'a reopen request is immutable except for its decision';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER period_reopen_requests_guard_update BEFORE UPDATE ON period_reopen_requests
    FOR EACH ROW EXECUTE FUNCTION guard_reopen_request_mutation();
CREATE TRIGGER period_reopen_requests_guard_delete BEFORE DELETE ON period_reopen_requests
    FOR EACH ROW EXECUTE FUNCTION guard_reopen_request_mutation();

CREATE TABLE period_state_transitions (
    transition_id      UUID PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    fiscal_period_id   UUID         NOT NULL REFERENCES fiscal_periods(fiscal_period_id),
    from_state         VARCHAR(32)  NOT NULL,
    to_state           VARCHAR(32)  NOT NULL,
    principal_id       VARCHAR(255) NOT NULL,
    reason             TEXT         NOT NULL DEFAULT '',
    reopen_request_id  UUID REFERENCES period_reopen_requests(request_id),
    occurred_at        TIMESTAMPTZ  NOT NULL
);
CREATE INDEX period_state_transitions_period ON period_state_transitions (tenant_id, fiscal_period_id, occurred_at);

CREATE OR REPLACE FUNCTION reject_period_transition_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'period_state_transitions is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER period_state_transitions_append_only BEFORE UPDATE OR DELETE ON period_state_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_period_transition_mutation();

ALTER TABLE period_reopen_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE period_reopen_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON period_reopen_requests FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
ALTER TABLE period_state_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE period_state_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON period_state_transitions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
