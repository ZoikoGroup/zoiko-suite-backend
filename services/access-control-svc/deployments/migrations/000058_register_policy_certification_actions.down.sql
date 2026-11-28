-- Migration 000058 down. Refused while any bundle or role template names one of the actions.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a IN ('POLICY_TEST_MANAGE', 'POLICY_TEST_RUN', 'POLICY_WAIVER_REQUEST', 'POLICY_WAIVER_APPROVE', 'POLICY_SIMULATE');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000058 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a IN ('POLICY_TEST_MANAGE', 'POLICY_TEST_RUN', 'POLICY_WAIVER_REQUEST', 'POLICY_WAIVER_APPROVE', 'POLICY_SIMULATE');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000058 down refused: still granted by a role template: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions
 WHERE action_name IN ('POLICY_TEST_MANAGE', 'POLICY_TEST_RUN', 'POLICY_WAIVER_REQUEST', 'POLICY_WAIVER_APPROVE', 'POLICY_SIMULATE');
