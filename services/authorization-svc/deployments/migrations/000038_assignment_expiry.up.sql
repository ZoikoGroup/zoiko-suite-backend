-- 000038: iam.assignment.expired (ORG-06 s4.6 event AssignmentExpired).
--
-- authorization-svc stops granting through an assignment the instant its
-- effective_to passes, but until now said nothing: no event is emitted when a
-- window simply runs out, so a consumer holding its own projection (session
-- claims, an access review, a downstream cache) learned of it only when the
-- projection's own date logic caught up, if it had one.
--
-- A sweep finds assignments that were granting and whose window has closed, and
-- emits iam.assignment.expired exactly once for each. Nobody expires an
-- assignment, its window closes, so the event carries no actor.
--
-- WHAT COUNTS AS EXPIRY. An assignment that ends because someone ended it
-- already told the world: the effective_to change emits iam.assignment.revoked
-- (000026). A BEFORE UPDATE trigger marks an end that takes effect at once as
-- already announced, so the sweep does not announce it a second time as an
-- "expiry". A future-dated end (a scheduled revocation, or an end date set at
-- grant) is announced when it is set and again, as expired, when it arrives:
-- the second is the fact that the access stopped.
--
-- PENDING, REJECTED and EXPIRED-approval assignments never granted, and are
-- never swept.

ALTER TABLE principal_role_assignments
    ADD COLUMN IF NOT EXISTS expiry_notified_at TIMESTAMPTZ;

-- Everything that has already lapsed was lapsed before this existed; it is not
-- news, and emitting it all at once would flood every consumer.
UPDATE principal_role_assignments
   SET expiry_notified_at = NOW()
 WHERE effective_to IS NOT NULL AND effective_to <= NOW() AND expiry_notified_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_pra_expiry_due
    ON principal_role_assignments (effective_to)
    WHERE expiry_notified_at IS NULL AND effective_to IS NOT NULL;

CREATE OR REPLACE FUNCTION authz_assignment_end_announced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.effective_to IS NOT NULL AND NEW.effective_to <= NOW() AND NEW.expiry_notified_at IS NULL THEN
        NEW.expiry_notified_at := NOW();
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS pra_end_announced ON principal_role_assignments;
CREATE TRIGGER pra_end_announced
    BEFORE UPDATE OF effective_to ON principal_role_assignments
    FOR EACH ROW WHEN (OLD.effective_to IS DISTINCT FROM NEW.effective_to)
    EXECUTE FUNCTION authz_assignment_end_announced();

-- The sweep. SECURITY DEFINER because it reads and marks every tenant's
-- assignments from a background loop that belongs to no tenant, exactly as the
-- retention purges (000031) do; the function body is the whole of what that
-- privilege allows. FOR UPDATE SKIP LOCKED makes two replicas sweeping at once
-- step over each other's rows instead of announcing them twice.
CREATE OR REPLACE FUNCTION authz_sweep_expired_assignments(p_limit INTEGER)
RETURNS INTEGER
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    r RECORD;
    n INTEGER := 0;
BEGIN
    IF p_limit IS NULL OR p_limit < 1 OR p_limit > 10000 THEN
        RAISE EXCEPTION 'authz_sweep_expired_assignments: limit must be between 1 and 10000 (%)', p_limit;
    END IF;
    FOR r IN
        SELECT a.principal_role_assignment_id AS id, a.principal_id, a.role_id, a.legal_entity_id,
               a.book_id, a.org_unit_id, a.effective_from, a.effective_to, a.assigned_by,
               a.approved_by, a.approval_status, ro.tenant_id
          FROM principal_role_assignments a
          JOIN roles ro ON ro.role_id = a.role_id
         WHERE a.expiry_notified_at IS NULL
           AND a.effective_to IS NOT NULL
           AND a.effective_to <= NOW()
           AND a.approval_status IN ('APPROVED', 'SUSPENDED')
         ORDER BY a.effective_to
         LIMIT p_limit
           FOR UPDATE OF a SKIP LOCKED
    LOOP
        UPDATE principal_role_assignments SET expiry_notified_at = NOW()
         WHERE principal_role_assignment_id = r.id;
        -- No actor: nobody expires an assignment, its window closes. Cleared so
        -- a value left on this connection by an earlier request cannot be
        -- attributed to it.
        PERFORM set_config('app.actor_id', '', true);
        PERFORM set_config('app.reason', '', true);
        PERFORM authz_emit_event('iam.assignment.expired', r.id::text, r.tenant_id, r.legal_entity_id::text,
            jsonb_build_object(
                'principal_role_assignment_id', r.id, 'principal_id', r.principal_id, 'role_id', r.role_id,
                'legal_entity_id', r.legal_entity_id, 'book_id', r.book_id, 'org_unit_id', r.org_unit_id,
                'effective_from', r.effective_from, 'effective_to', r.effective_to,
                'effective_at', r.effective_to, 'assigned_by', r.assigned_by, 'approved_by', r.approved_by,
                'approval_status', r.approval_status));
        n := n + 1;
    END LOOP;
    RETURN n;
END;
$$;

REVOKE EXECUTE ON FUNCTION authz_sweep_expired_assignments(INTEGER) FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT EXECUTE ON FUNCTION authz_sweep_expired_assignments(INTEGER) TO app_authorization;
    END IF;
END
$$;
