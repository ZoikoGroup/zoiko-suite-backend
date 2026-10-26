-- Removes the 000019 entry. Refused while any bundle still names it: deleting
-- a registered action under a live grant would leave the bundle holding a name
-- the registry no longer recognises.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = 'GL_CLOSE_REVIEW_POSTING';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000019 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions
 WHERE naming = 'LEGACY'
   AND description LIKE 'Enforced by %'
   AND action_name = 'GL_CLOSE_REVIEW_POSTING';
