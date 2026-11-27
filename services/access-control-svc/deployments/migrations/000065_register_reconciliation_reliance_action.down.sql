-- Migration 000065 down. Refused while any bundle or role template names the action.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = 'RECONCILIATION_RELIANCE_RECORD';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000065 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a = 'RECONCILIATION_RELIANCE_RECORD';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000065 down refused: still granted by a role template: %', v_used;
    END IF;
    DELETE FROM permission_definitions WHERE action_name = 'RECONCILIATION_RELIANCE_RECORD';
END
$$;
