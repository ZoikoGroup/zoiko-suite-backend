-- Migration 000060 down. Refused while any bundle or role template names one of the actions.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a LIKE 'SIGNATORY\_SIGN\_AS\_%';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000060 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a LIKE 'SIGNATORY\_SIGN\_AS\_%';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000060 down refused: still granted by a role template: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions
 WHERE action_name IN ('SIGNATORY_SIGN_AS_DIRECTOR', 'SIGNATORY_SIGN_AS_CFO', 'SIGNATORY_SIGN_AS_CEO',
                       'SIGNATORY_SIGN_AS_FINANCE_SIGNER', 'SIGNATORY_SIGN_AS_COMPANY_SECRETARY',
                       'SIGNATORY_SIGN_AS_GENERAL_COUNSEL', 'SIGNATORY_SIGN_AS_TREASURER');
