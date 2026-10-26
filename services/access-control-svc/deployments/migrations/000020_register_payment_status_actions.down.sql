-- Removes the 000020 entries. Refused while any bundle still names one:
-- deleting a registered action under a live grant would leave the bundle
-- holding a name the registry no longer recognises.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (ARRAY[
         'PAYMENT_STATUS_READ',
         'PAYMENT_STATUS_INGEST',
         'PAYMENT_STATUS_RESOLVE',
         'PAYMENT_FINALITY_CONFIRM'
     ]);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000020 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions
 WHERE naming = 'LEGACY'
   AND description LIKE 'Enforced by payment-status-svc:%'
   AND action_name = ANY (ARRAY[
       'PAYMENT_STATUS_READ',
       'PAYMENT_STATUS_INGEST',
       'PAYMENT_STATUS_RESOLVE',
       'PAYMENT_FINALITY_CONFIRM'
   ]);
