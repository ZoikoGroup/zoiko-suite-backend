-- Removes the 000020 entries. Refused while any bundle still names one, as
-- 000019's down is: a bundle would be left holding an unregistered action.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (ARRAY['IDENTITY_MFA_RESET', 'iam.workload.manage', 'MTLS_CERT_PROVISION', 'MTLS_CERT_READ',
                          'MTLS_CERT_ROTATE', 'MTLS_CERT_REVOKE', 'MTLS_POLICY_READ', 'MTLS_POLICY_WRITE']);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000020 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions
 WHERE action_name = ANY (ARRAY['IDENTITY_MFA_RESET', 'iam.workload.manage', 'MTLS_CERT_PROVISION', 'MTLS_CERT_READ',
                                'MTLS_CERT_ROTATE', 'MTLS_CERT_REVOKE', 'MTLS_POLICY_READ', 'MTLS_POLICY_WRITE'])
   AND description LIKE 'Enforced by %';

DELETE FROM protected_permissions WHERE action_name = 'iam.workload.manage';
