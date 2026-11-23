-- Migration 000044 down. Refused while any bundle or role template names one of the actions.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a IN ('PARTY_CREATE', 'PARTY_AMEND', 'PARTY_ROLE_MANAGE', 'PARTY_IDENTIFIER_LINK', 'PARTY_IDENTIFIER_REVEAL');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000044 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a IN ('PARTY_CREATE', 'PARTY_AMEND', 'PARTY_ROLE_MANAGE', 'PARTY_IDENTIFIER_LINK', 'PARTY_IDENTIFIER_REVEAL');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000044 down refused: still granted by a role template: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions WHERE action_name IN ('PARTY_CREATE', 'PARTY_AMEND', 'PARTY_ROLE_MANAGE', 'PARTY_IDENTIFIER_LINK', 'PARTY_IDENTIFIER_REVEAL');
