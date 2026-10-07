-- 000023: the decision record carries the evidence the specs require, and is
-- append-only in the database rather than by convention.
--
-- GOV-03 Evidence: "decision_id, subject, action, resource, scope, attributes
-- digest, result, obligations, policy versions, timestamp"; ZS-IAM-001 §20 adds
-- on_behalf_of / delegation_id, session assurance, assignment references,
-- reason codes and the resource version; §8.2 / §7 stage 9 the four-valued
-- decision. The log held principal, entity, action, outcome, basis,
-- correlation and tenant only, so a decision could not be reconstructed
-- against the policy version it was made under (GOV invariant #11, A28).
--
-- policy_set_version is the configuration watermark — the latest
-- authz_config_history id (000022) at the instant of the decision — so the
-- exact role, bundle, SoD and ABAC state it was evaluated against can be
-- re-read from history.
--
-- Raw attribute values are NOT stored (§25 data minimisation): only a SHA-256
-- digest of the canonicalised attribute map, which proves what was evaluated
-- without retaining it.

ALTER TABLE access_decision_log
    ADD COLUMN IF NOT EXISTS decision           TEXT,
    ADD COLUMN IF NOT EXISTS policy_set_version TEXT,
    ADD COLUMN IF NOT EXISTS obligations        JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS reason_codes       JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS matched_grants     JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS resource_type      TEXT,
    ADD COLUMN IF NOT EXISTS resource_id        TEXT,
    ADD COLUMN IF NOT EXISTS resource_version   TEXT,
    ADD COLUMN IF NOT EXISTS attributes_digest  TEXT,
    ADD COLUMN IF NOT EXISTS session_assurance  TEXT,
    ADD COLUMN IF NOT EXISTS on_behalf_of       TEXT,
    ADD COLUMN IF NOT EXISTS delegation_id      TEXT,
    ADD COLUMN IF NOT EXISTS expires_at         TIMESTAMPTZ;

-- Append-only, enforced. ZS-IAM-001 §32 prohibits "deleting authorization
-- history" and GOV-03 calls the decision "an immutable fact"; until now only
-- a comment said so. Retention is unaffected: it DETACHes whole partitions
-- (000009) and never updates or deletes a row.
CREATE OR REPLACE FUNCTION access_decision_log_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'access_decision_log is append-only (% refused)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

DROP TRIGGER IF EXISTS access_decision_log_no_update ON access_decision_log;
CREATE TRIGGER access_decision_log_no_update
    BEFORE UPDATE OR DELETE ON access_decision_log
    FOR EACH ROW EXECUTE FUNCTION access_decision_log_immutable();
