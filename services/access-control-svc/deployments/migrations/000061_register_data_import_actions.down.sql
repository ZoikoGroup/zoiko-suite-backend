-- Migration 000061 down. Refused while any bundle or role template names one of the actions.
DO $$
DECLARE
    v_used TEXT;
    v_actions TEXT[] := ARRAY['DATA_IMPORT_PROFILE_PUBLISH','DATA_IMPORT_PROFILE_CERTIFY','DATA_IMPORT_CREATE','DATA_IMPORT_UPLOAD','DATA_IMPORT_VALIDATE','DATA_IMPORT_APPROVE','DATA_IMPORT_EXECUTE','DATA_IMPORT_CANCEL','DATA_IMPORT_ITEM_MANAGE','DATA_IMPORT_QUARANTINE_RELEASE','DATA_IMPORT_RECONCILE','DATA_IMPORT_READ'];
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (v_actions);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000061 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a = ANY (v_actions);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000061 down refused: still granted by a role template: %', v_used;
    END IF;
    DELETE FROM permission_definitions WHERE action_name = ANY (v_actions);
END
$$;
