-- Migration 000041 down. Refused while any bundle or role template names one of the actions.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a IN ('PAYMENT_TERM_CREATE', 'PAYMENT_TERM_APPROVE', 'PAYMENT_TERM_SUPERSEDE', 'PAYMENT_TERM_RETIRE');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000041 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a IN ('PAYMENT_TERM_CREATE', 'PAYMENT_TERM_APPROVE', 'PAYMENT_TERM_SUPERSEDE', 'PAYMENT_TERM_RETIRE');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000041 down refused: still granted by a role template: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions WHERE action_name IN ('PAYMENT_TERM_CREATE', 'PAYMENT_TERM_APPROVE', 'PAYMENT_TERM_SUPERSEDE', 'PAYMENT_TERM_RETIRE');
