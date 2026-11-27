-- Migration 000060 down. Refused while any bundle or role template names one of the actions.
DO $$
DECLARE
    v_used TEXT;
    v_actions TEXT[] := ARRAY['DQC_CONTRACT_READ','DQC_CONTRACT_MANAGE','DQC_CONTRACT_PUBLISH'];
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (v_actions);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000060 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a = ANY (v_actions);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000060 down refused: still granted by a role template: %', v_used;
    END IF;
    DELETE FROM permission_definitions WHERE action_name = ANY (v_actions);
END
$$;
