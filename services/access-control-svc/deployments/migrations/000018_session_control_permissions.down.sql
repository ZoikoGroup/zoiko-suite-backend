-- Removes the 000018 permissions. Refused while any bundle still grants one.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a IN ('iam.pam.eligible', 'iam.pam.approve', 'iam.break_glass.eligible', 'iam.session.review');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000018 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;
DELETE FROM permission_definitions WHERE action_name IN ('iam.pam.eligible', 'iam.pam.approve', 'iam.break_glass.eligible', 'iam.session.review');
DELETE FROM protected_permissions WHERE action_name IN ('iam.pam.eligible', 'iam.pam.approve', 'iam.break_glass.eligible', 'iam.session.review');
