-- 20260818000800_governance_decision_log_svc.sql
-- governance-decision-log-svc → schema `governance_decision_log`
--
-- Squashed end state of 000001_initial_schema, 000002_add_rls,
-- 000003_enforce_immutability, 000004_add_event_linkage_keys and
-- 000005_add_replay_manifests. Two tables: governance_decisions,
-- replay_manifests.
--
-- ── Why the immutability TRIGGERS are kept, not replaced by grants ───────────
-- Both tables are append-only evidence. Elsewhere in this migration set that is
-- enforced by withholding the UPDATE/DELETE grant, which is sufficient because
-- zoiko_backend is an ordinary role. It is NOT sufficient here, and 000003 says
-- why: a privileged role bypasses privilege checks and row-level security
-- entirely, but it does NOT bypass a BEFORE trigger.
--
-- On Supabase that argument gets stronger rather than weaker. The `service_role`
-- key carries BYPASSRLS, so anything holding it is exempt from every policy in
-- this file — and these triggers are the only control in the whole schema that
-- still binds it. On the platform's canonical governance evidence log, that is
-- worth the cost of a trigger.
--
-- ── One gap this migration does NOT close ────────────────────────────────────
-- The service's CreateDecision handler takes tenant_id, actor_id AND decided_at
-- from the REQUEST BODY, authorising against X-Principal-Id but never
-- reconciling any of the three with it. So an authorised principal can append a
-- decision to any tenant, attributed to anyone, dated anything. The DEFAULT on
-- actor_id below is a backstop for other writers only — it cannot fire while
-- the handler passes an explicit value. Fixing it is a handler change.

CREATE SCHEMA IF NOT EXISTS governance_decision_log;

COMMENT ON SCHEMA governance_decision_log IS
    'governance-decision-log-svc. Append-only governance evidence: decisions and the replay manifests that reproduce them.';

GRANT USAGE ON SCHEMA governance_decision_log TO zoiko_backend, authenticated;

-- ── governance_decisions ─────────────────────────────────────────────────────
-- Append-only. No UPDATE, no DELETE — ever. A correction is a NEW decision
-- record referencing the original through evaluation_context, never a mutation
-- of this row.

CREATE TABLE governance_decision_log.governance_decisions (
    decision_id          VARCHAR(64)  PRIMARY KEY,
    tenant_id            VARCHAR(64)  NOT NULL,
    legal_entity_id      VARCHAR(64)  NOT NULL,

    -- See the header: the handler supplies this from the body today, so the
    -- default is a backstop for direct writers rather than the live behaviour.
    actor_id             VARCHAR(64)  NOT NULL DEFAULT app.current_principal_id(),

    action_type          VARCHAR(128) NOT NULL,
    outcome              VARCHAR(32)  NOT NULL,
    rule_basis           VARCHAR(256) NOT NULL,
    evaluation_context   JSONB,
    correlation_id       VARCHAR(64)  NOT NULL,

    -- Event Linkage Keys. Previously buried in the evaluation_context JSONB
    -- catch-all; promoted to columns because "find every decision made during
    -- workflow instance X" and "find the decision this event caused" are real
    -- queries on the platform's canonical evidence log and nothing could answer
    -- them without a JSONB scan. Both nullable — a decision may not be
    -- workflow-triggered, and causation is not always known.
    workflow_instance_id TEXT,
    causation_id         TEXT,

    decided_at           TIMESTAMPTZ  NOT NULL,
    stored_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Serves the five-filter query contract: actor, entity, action, rule basis,
-- time range.
CREATE INDEX idx_governance_decisions_tenant_entity
    ON governance_decision_log.governance_decisions (tenant_id, legal_entity_id);
CREATE INDEX idx_governance_decisions_actor
    ON governance_decision_log.governance_decisions (actor_id);
CREATE INDEX idx_governance_decisions_action_type
    ON governance_decision_log.governance_decisions (action_type);
CREATE INDEX idx_governance_decisions_rule_basis
    ON governance_decision_log.governance_decisions (rule_basis);
CREATE INDEX idx_governance_decisions_decided_at
    ON governance_decision_log.governance_decisions (decided_at);
CREATE INDEX idx_governance_decisions_workflow_instance
    ON governance_decision_log.governance_decisions (workflow_instance_id)
    WHERE workflow_instance_id IS NOT NULL;

-- ── replay_manifests ─────────────────────────────────────────────────────────
-- A governance decision's basis must be REPRODUCIBLE: replayable against the
-- exact policy version and facts that produced it, not against whatever policy
-- is active now. rule_basis already encodes "policy_code:policy_version_id" and
-- evaluation_context already stores the facts; this table records that a replay
-- happened and whether it reproduced the original outcome.

CREATE TABLE governance_decision_log.replay_manifests (
    replay_manifest_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    decision_id              VARCHAR(64) NOT NULL
        REFERENCES governance_decision_log.governance_decisions(decision_id),

    -- The exact policy version replayed against — parsed from the decision's
    -- rule_basis at replay time and stored explicitly, so a manifest is
    -- self-describing without re-parsing rule_basis later.
    policy_version_id        VARCHAR(64) NOT NULL,

    -- Same vocabulary as governance_decisions.outcome. Data only.
    replayed_outcome         VARCHAR(64) NOT NULL,
    original_outcome         VARCHAR(64) NOT NULL,

    -- Denormalised from (replayed_outcome = original_outcome) at write time. A
    -- manifest is a permanent record of what was FOUND, not a live comparison
    -- that could drift if either column were later reinterpreted.
    outcomes_match           BOOLEAN     NOT NULL,

    replay_notes             TEXT,
    replayed_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    replayed_by_principal_id TEXT        NOT NULL DEFAULT app.current_principal_id()
);

CREATE INDEX idx_replay_manifests_decision
    ON governance_decision_log.replay_manifests (decision_id, replayed_at DESC);

-- ── Immutability ─────────────────────────────────────────────────────────────
-- app.reject_mutation() is defined once in the platform foundation rather than
-- copied per service, which is what the compose migrations grew into — two
-- near-identical functions in this service alone. TG_TABLE_SCHEMA and
-- TG_TABLE_NAME keep the message specific without a per-table copy.

CREATE TRIGGER governance_decisions_immutable
    BEFORE UPDATE OR DELETE ON governance_decision_log.governance_decisions
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

CREATE TRIGGER replay_manifests_immutable
    BEFORE UPDATE OR DELETE ON governance_decision_log.replay_manifests
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── Row-level security ───────────────────────────────────────────────────────
-- tenant_id is NOT NULL on every decision — no global or shared decisions
-- exist — so the policy is a plain equality with no NULL carve-out.
--
-- replay_manifests carries no tenant_id of its own. It is reachable only
-- through a decision_id, so its policy is an EXISTS against the parent, which
-- is itself subject to the policy above: a manifest whose decision is invisible
-- to this tenant is invisible too.

ALTER TABLE governance_decision_log.governance_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_decision_log.governance_decisions FORCE  ROW LEVEL SECURITY;
ALTER TABLE governance_decision_log.replay_manifests     ENABLE ROW LEVEL SECURITY;
ALTER TABLE governance_decision_log.replay_manifests     FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON governance_decision_log.governance_decisions
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON governance_decision_log.governance_decisions
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON governance_decision_log.replay_manifests
    FOR ALL
    TO zoiko_backend
    USING (EXISTS (
        SELECT 1 FROM governance_decision_log.governance_decisions d
        WHERE d.decision_id = replay_manifests.decision_id))
    WITH CHECK (EXISTS (
        SELECT 1 FROM governance_decision_log.governance_decisions d
        WHERE d.decision_id = replay_manifests.decision_id));

CREATE POLICY tenant_read ON governance_decision_log.replay_manifests
    FOR SELECT
    TO authenticated
    USING (EXISTS (
        SELECT 1 FROM governance_decision_log.governance_decisions d
        WHERE d.decision_id = replay_manifests.decision_id));

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON governance_decision_log.governance_decisions TO authenticated;
GRANT SELECT ON governance_decision_log.replay_manifests     TO authenticated;

-- SELECT and INSERT only. The triggers above would refuse a mutation anyway;
-- withholding the grant means an ordinary role is refused before the trigger is
-- even reached, and the trigger remains as the backstop for privileged ones.
GRANT SELECT, INSERT ON governance_decision_log.governance_decisions TO zoiko_backend;
GRANT SELECT, INSERT ON governance_decision_log.replay_manifests     TO zoiko_backend;
