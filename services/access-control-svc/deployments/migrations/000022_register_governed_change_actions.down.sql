-- Removes the 000022 entries. Refused while any bundle still names one.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (ARRAY['iam.break_glass.approve', 'iam.authority_limit.manage', 'iam.authority_limit.approve',
                          'iam.sod_rule.publish', 'iam.policy.publish']);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000022 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions
 WHERE action_name = ANY (ARRAY['iam.break_glass.approve', 'iam.authority_limit.manage', 'iam.authority_limit.approve',
                                'iam.sod_rule.publish', 'iam.policy.publish'])
   AND description LIKE 'Enforced by %';
DELETE FROM protected_permissions
 WHERE action_name = ANY (ARRAY['iam.break_glass.approve', 'iam.authority_limit.manage', 'iam.authority_limit.approve',
                                'iam.sod_rule.publish', 'iam.policy.publish']);
