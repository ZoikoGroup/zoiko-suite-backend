-- Removes the 000022 entry. Refused while any bundle still names it: deleting
-- a registered action under a live grant would leave the bundle holding a
-- name the registry no longer recognises.
--
-- permission_bundle_defs is FORCE ROW LEVEL SECURITY (000002), so a guard run
-- under row security sees only the bundles of whatever tenant app.tenant_id
-- names — with none set, no bundles at all — and would pass while another
-- tenant's bundle still grants the action. row_security is turned off first:
-- a role that may bypass row security then sees every tenant's bundles, and
-- one that may not fails the query instead. Either way the guard never passes
-- on a partial view.
SET LOCAL row_security = off;

DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT b.tenant_id, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = 'bank.transaction.read';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000022 down refused: bank.transaction.read is still granted by a bundle in tenant(s): %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions
 WHERE action_name = 'bank.transaction.read'
   AND naming = 'TAXONOMY'
   AND description LIKE 'Enforced by banking-connector-svc:%';
