-- Migration 000052 down. Refused while any bundle or role template names one of the actions.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a IN ('DIMENSION_TYPE_CREATE', 'DIMENSION_CREATE', 'DIMENSION_ACTIVATE', 'DIMENSION_DEACTIVATE', 'DIMENSION_REPARENT', 'DIMENSION_APPROVE_HIERARCHY', 'DIMENSION_MAP_SOURCE');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000052 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a IN ('DIMENSION_TYPE_CREATE', 'DIMENSION_CREATE', 'DIMENSION_ACTIVATE', 'DIMENSION_DEACTIVATE', 'DIMENSION_REPARENT', 'DIMENSION_APPROVE_HIERARCHY', 'DIMENSION_MAP_SOURCE');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000052 down refused: still granted by a role template: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions WHERE action_name IN ('DIMENSION_TYPE_CREATE', 'DIMENSION_CREATE', 'DIMENSION_ACTIVATE', 'DIMENSION_DEACTIVATE', 'DIMENSION_REPARENT', 'DIMENSION_APPROVE_HIERARCHY', 'DIMENSION_MAP_SOURCE');
