-- Reverts 000036. Refused while any rule change is undecided or a rule was
-- rejected: without publication_status a rejected rule could be reactivated
-- directly, and a pending one would read as an ordinary retired rule.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM sod_rules WHERE pending_change IS NOT NULL OR publication_status = 'REJECTED')
       OR EXISTS (SELECT 1 FROM abac_rules WHERE pending_change IS NOT NULL OR publication_status = 'REJECTED') THEN
        RAISE EXCEPTION '000036 down refused: undecided or rejected rule changes exist';
    END IF;
END
$$;

DROP TRIGGER IF EXISTS abac_rules_change_events ON abac_rules;
DROP TRIGGER IF EXISTS sod_rules_change_events ON sod_rules;
DROP FUNCTION IF EXISTS authz_rule_change_events();
DROP TRIGGER IF EXISTS abac_rules_publication_independent ON abac_rules;
DROP TRIGGER IF EXISTS sod_rules_publication_independent ON sod_rules;
DROP FUNCTION IF EXISTS authz_rule_publication_independent();

-- The 000026 publication events.
CREATE OR REPLACE FUNCTION authz_config_events() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_version TEXT := 'cfg.' || NEW.history_id;
    v_payload JSONB := jsonb_build_object(
        'object_type', NEW.object_type, 'object_id', NEW.object_id, 'version', NEW.version,
        'snapshot', NEW.snapshot, 'changed_by', NEW.changed_by, 'reason', NEW.reason,
        'policy_set_version', v_version);
BEGIN
    IF NEW.object_type IN ('roles', 'permission_bundles') THEN
        PERFORM authz_emit_event('iam.role.published', NEW.object_id::text, NEW.tenant_id, NULL, v_payload);
    ELSIF NEW.object_type = 'sod_rules' THEN
        PERFORM authz_emit_event('iam.sod_policy.published', NEW.object_id::text, NEW.tenant_id, NULL, v_payload);
    END IF;
    PERFORM authz_emit_event('iam.policy_set.published', v_version, NEW.tenant_id, NULL,
        jsonb_build_object('policy_set_version', v_version, 'object_type', NEW.object_type,
                           'object_id', NEW.object_id, 'published_by', NEW.changed_by));
    RETURN NULL;
END;
$$;

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['sod_rules', 'abac_rules'] LOOP
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', t, t || '_rejected_inactive_check');
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', t, t || '_pending_change_check');
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', t, t || '_publication_status_check');
        EXECUTE format($f$
            ALTER TABLE %1$I
                DROP COLUMN IF EXISTS rejected_at,
                DROP COLUMN IF EXISTS rejected_by,
                DROP COLUMN IF EXISTS published_at,
                DROP COLUMN IF EXISTS published_by,
                DROP COLUMN IF EXISTS change_reason,
                DROP COLUMN IF EXISTS change_requested_at,
                DROP COLUMN IF EXISTS change_requested_by,
                DROP COLUMN IF EXISTS pending_change,
                DROP COLUMN IF EXISTS publication_status
        $f$, t);
    END LOOP;
END
$$;
