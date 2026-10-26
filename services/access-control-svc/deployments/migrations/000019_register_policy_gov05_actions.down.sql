-- Removes the 000019 entries. Refused while any bundle still names one:
-- deleting a registered action under a live grant would leave a bundle
-- holding a name the registry no longer recognises.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (ARRAY['POLICY_READ', 'POLICY_EVALUATE', 'POLICY_CREATE_GLOBAL',
                          'POLICY_VERSION_APPROVE', 'POLICY_VERSION_APPROVE_GLOBAL',
                          'POLICY_VERSION_SUSPEND', 'POLICY_VERSION_SUSPEND_GLOBAL',
                          'POLICY_VERSION_RETIRE', 'POLICY_VERSION_RETIRE_GLOBAL']);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000019 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions
 WHERE naming = 'LEGACY'
   AND description LIKE 'Enforced by policy-svc:%'
   AND action_name = ANY (ARRAY['POLICY_READ', 'POLICY_EVALUATE', 'POLICY_CREATE_GLOBAL',
                                'POLICY_VERSION_APPROVE', 'POLICY_VERSION_APPROVE_GLOBAL',
                                'POLICY_VERSION_SUSPEND', 'POLICY_VERSION_SUSPEND_GLOBAL',
                                'POLICY_VERSION_RETIRE', 'POLICY_VERSION_RETIRE_GLOBAL']);
