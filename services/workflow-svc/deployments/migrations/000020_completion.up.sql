-- 000020_completion.up.sql
-- workflow-svc — Group 2 completion pass (21 Oct 2026).

BEGIN;

-- 1 ── correlation_id is a trace, not an idempotency key ───────────────────
-- 000003 made (tenant_id, correlation_id) unique, so a second workflow
-- started in the same business flow under a new Idempotency-Key failed with
-- 23505 (503). Idempotency is (tenant_id, idempotency_key), 000013.
DROP INDEX IF EXISTS idx_workflow_instances_correlation_unique;

-- 2 ── transitions: idempotency, lineage, append-only ──────────────────────
ALTER TABLE workflow_transitions ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE workflow_transitions ADD COLUMN IF NOT EXISTS request_hash TEXT;
ALTER TABLE workflow_transitions ADD COLUMN IF NOT EXISTS stage_order INT;
ALTER TABLE workflow_transitions ADD COLUMN IF NOT EXISTS review_cycle INT;
-- Authority snapshot: the authorization decision (and delegation) the action
-- was taken under, and the route version it followed.
ALTER TABLE workflow_transitions ADD COLUMN IF NOT EXISTS access_decision_id TEXT;
ALTER TABLE workflow_transitions ADD COLUMN IF NOT EXISTS authority_basis TEXT;
ALTER TABLE workflow_transitions ADD COLUMN IF NOT EXISTS delegation_id TEXT;
ALTER TABLE workflow_transitions ADD COLUMN IF NOT EXISTS workflow_definition_version INT;
-- The governance-ledger record of this decision (decision-log-svc decision_id).
ALTER TABLE workflow_transitions ADD COLUMN IF NOT EXISTS decision_id UUID;
CREATE UNIQUE INDEX IF NOT EXISTS uq_workflow_transitions_idempotency
    ON workflow_transitions (workflow_instance_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND idempotency_key <> '';

CREATE OR REPLACE FUNCTION workflow_refuse_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'insufficient_privilege';
END;
$$;

DROP TRIGGER IF EXISTS trg_workflow_transitions_append_only ON workflow_transitions;
CREATE TRIGGER trg_workflow_transitions_append_only
    BEFORE UPDATE OR DELETE ON workflow_transitions
    FOR EACH ROW EXECUTE FUNCTION workflow_refuse_mutation();
DROP TRIGGER IF EXISTS trg_workflow_transitions_no_truncate ON workflow_transitions;
CREATE TRIGGER trg_workflow_transitions_no_truncate
    BEFORE TRUNCATE ON workflow_transitions
    FOR EACH STATEMENT EXECUTE FUNCTION workflow_refuse_mutation();

-- 3 ── review cycles: request-changes starts a new cycle ───────────────────
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS review_cycle INT NOT NULL DEFAULT 1;
ALTER TABLE workflow_stage_quorum_decisions ADD COLUMN IF NOT EXISTS review_cycle INT NOT NULL DEFAULT 1;
ALTER TABLE workflow_stage_quorum_decisions DROP CONSTRAINT IF EXISTS workflow_stage_quorum_decisions_one_per_approver;
ALTER TABLE workflow_stage_quorum_decisions ADD CONSTRAINT workflow_stage_quorum_decisions_one_per_approver
    UNIQUE (workflow_stage_id, approver_principal_id, review_cycle);
DROP TRIGGER IF EXISTS trg_quorum_decisions_append_only ON workflow_stage_quorum_decisions;
CREATE TRIGGER trg_quorum_decisions_append_only
    BEFORE UPDATE OR DELETE ON workflow_stage_quorum_decisions
    FOR EACH ROW EXECUTE FUNCTION workflow_refuse_mutation();

-- 4 ── ROLE stages: a definition routes by role, not by a person the
--      initiator chose (server-resolved route, WFC-02) ───────────────────
ALTER TABLE workflow_stages ADD COLUMN IF NOT EXISTS approver_role VARCHAR(128);
ALTER TABLE workflow_stages ADD COLUMN IF NOT EXISTS due_at TIMESTAMPTZ;
-- The stage's deadline length; due_at is set when the stage becomes current.
ALTER TABLE workflow_stages ADD COLUMN IF NOT EXISTS due_hours INT CHECK (due_hours IS NULL OR due_hours > 0);
ALTER TABLE workflow_stages ADD COLUMN IF NOT EXISTS acted_by TEXT;
ALTER TABLE workflow_stages DROP CONSTRAINT IF EXISTS workflow_stages_stage_type_check;
ALTER TABLE workflow_stages ADD CONSTRAINT workflow_stages_stage_type_check
    CHECK (stage_type IN ('SINGLE', 'QUORUM', 'ROLE'));
ALTER TABLE workflow_stages DROP CONSTRAINT IF EXISTS workflow_stages_shape_matches_type;
ALTER TABLE workflow_stages ADD CONSTRAINT workflow_stages_shape_matches_type CHECK (
    (stage_type = 'SINGLE' AND approver_principal_id IS NOT NULL AND required_approvals IS NULL)
    OR (stage_type = 'QUORUM' AND approver_principal_id IS NULL AND required_approvals IS NOT NULL AND required_approvals > 0)
    OR (stage_type = 'ROLE' AND approver_principal_id IS NULL AND approver_role IS NOT NULL AND required_approvals IS NULL)
);

-- 5 ── timers: expiry, stage deadlines, escalation, approver re-checks ─────
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS escalated_at TIMESTAMPTZ;
ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS approver_recheck_at TIMESTAMPTZ;
ALTER TABLE workflow_definitions ADD COLUMN IF NOT EXISTS expiry_hours INT CHECK (expiry_hours IS NULL OR expiry_hours > 0);
ALTER TABLE workflow_definitions ADD COLUMN IF NOT EXISTS escalation_target TEXT;
CREATE INDEX IF NOT EXISTS idx_workflow_instances_due
    ON workflow_instances (due_at) WHERE workflow_status IN ('PENDING', 'ESCALATED');
CREATE INDEX IF NOT EXISTS idx_workflow_instances_next_action
    ON workflow_instances (next_action_at) WHERE workflow_status = 'PENDING';
-- The timer sweep lists due instances across tenants (SELECT only); every
-- transition it then makes runs under the instance's own tenant.
DROP POLICY IF EXISTS workflow_timer_sweep_select ON workflow_instances;
CREATE POLICY workflow_timer_sweep_select ON workflow_instances
    FOR SELECT USING (current_setting('app.timer_sweep', true) = 'true');

-- 6 ── the governance ledger as an outbox destination ──────────────────────
-- Decisions are written to decision-log-svc through the same transactional
-- outbox as Kafka events, so an approval and its ledger record commit
-- together and a ledger outage delays the record instead of losing it.
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS destination VARCHAR(32) NOT NULL DEFAULT 'kafka';
ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
CREATE INDEX IF NOT EXISTS idx_outbox_events_destination_due
    ON outbox_events (destination, next_attempt_at) WHERE published_at IS NULL;

-- 7 ── consumed events (at-least-once delivery, applied once) ──────────────
CREATE TABLE IF NOT EXISTS workflow_consumed_events (
    event_id     TEXT        PRIMARY KEY,
    event_type   TEXT        NOT NULL,
    tenant_id    UUID,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMIT;
