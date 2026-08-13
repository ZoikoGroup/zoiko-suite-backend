-- 000005_add_replay_manifests.up.sql
-- Governance Decision Log Service — replay_manifests
--
-- docs/original_doc/zoiko_suite_doc7.txt §29's own evidence-quality
-- doctrine and §I5's replay requirement (doc7-implementation-backlog.md
-- item 34): a governance decision's basis must be REPRODUCIBLE — replaying
-- the exact evaluation against the exact policy version and facts that
-- produced it, not against "whatever policy is active now."
--
-- governance_decisions.rule_basis already encodes "policy_code:
-- policy_version_id" (see policy-svc's evaluateApprovalThreshold), and
-- evaluation_context already stores the facts used. What was missing was
-- (a) a way to fetch that EXACT policy version by ID regardless of
-- whether it's still ACTIVE today (added to policy-svc as
-- GET /v1/policy-versions/{version_id}), and (b) somewhere to record that
-- a replay happened and whether it reproduced the original outcome — this
-- table.
--
-- replay_manifests is append-only, same doctrine and same enforcement
-- mechanism as governance_decisions itself (000003_enforce_immutability):
-- a BEFORE trigger, since GRANT/REVOKE and RLS both bypass for the
-- Postgres superuser this platform connects as, but a trigger does not.

CREATE TABLE replay_manifests (
    replay_manifest_id       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    decision_id                VARCHAR(64)  NOT NULL REFERENCES governance_decisions(decision_id),

    -- The exact policy version replayed against — parsed from the
    -- decision's rule_basis at replay time, stored here explicitly so a
    -- manifest is self-describing without re-parsing rule_basis later.
    policy_version_id             VARCHAR(64)  NOT NULL,

    -- GRANTED | DENIED | ESCALATED | ... — DATA ONLY, same vocabulary as
    -- governance_decisions.outcome.
    replayed_outcome                 VARCHAR(64)  NOT NULL,
    original_outcome                    VARCHAR(64)  NOT NULL,

    -- Denormalized from (replayed_outcome = original_outcome) at write
    -- time — a manifest is a permanent record of what was found, not a
    -- live comparison that could drift if either column were ever
    -- reinterpreted.
    outcomes_match                         BOOLEAN      NOT NULL,

    replay_notes                              TEXT,

    replayed_at                                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    replayed_by_principal_id                        TEXT         NOT NULL
);

CREATE INDEX idx_replay_manifests_decision
    ON replay_manifests (decision_id, replayed_at DESC);

CREATE OR REPLACE FUNCTION reject_replay_manifest_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted on row %',
        TG_TABLE_NAME, TG_OP, OLD.replay_manifest_id;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER replay_manifests_immutable
    BEFORE UPDATE OR DELETE ON replay_manifests
    FOR EACH ROW EXECUTE FUNCTION reject_replay_manifest_mutation();
