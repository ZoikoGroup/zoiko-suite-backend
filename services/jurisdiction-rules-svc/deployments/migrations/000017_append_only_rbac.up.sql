-- 000017_append_only_rbac.up.sql
-- Close the remaining append-only gap at the privilege layer (G5).
--
-- The 000009 trigger already raises on any UPDATE/DELETE of
-- jurisdiction_rule_drift_events, but deployments/scripts/create-app-roles.sh
-- grants the runtime role blanket UPDATE/DELETE on ALL TABLES IN SCHEMA public,
-- so a written UPDATE still gets as far as the trigger. Strictly the trigger is
-- load-bearing; this removes the privilege so the write fails at the permission
-- layer instead, and (ordering caveat) re-applies whenever this migration runs
-- after the role exists.
--
-- rule_status_history is only DELETE-proof. Its bitemporal close-out — writing
-- known_to on the row being superseded — is a legitimate UPDATE performed by the
-- record_rule_status_change trigger on every transition, so UPDATE stays granted;
-- a direct DELETE, however, is tampering with "what the rule status was" and is
-- blocked by a trigger of its own (independent of role, so superuser sessions
-- are covered too).
--
-- Mirrors the guarded pattern of 000009: on a fresh volume the role is created
-- after migrations, in which case both guarded blocks are no-ops.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        REVOKE UPDATE, DELETE ON jurisdiction_rule_drift_events FROM app_jurisdiction_rules;
        REVOKE DELETE ON rule_status_history FROM app_jurisdiction_rules;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION prevent_rule_status_history_delete()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rule_status_history is append-only: deletions are not permitted';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_rule_status_history_no_delete ON rule_status_history;
CREATE TRIGGER trg_rule_status_history_no_delete
BEFORE DELETE ON rule_status_history
FOR EACH ROW EXECUTE FUNCTION prevent_rule_status_history_delete();