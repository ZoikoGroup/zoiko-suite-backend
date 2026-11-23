-- Migration 000016: suspension of a projected role assignment
-- (ORG-06 s4.6 SuspendAssignment; authorization-svc migration 000037).
--
-- authorization-svc withholds a SUSPENDED assignment at decision time, but this
-- projection is what the resolver frames a session's roles from. Without a way
-- to hold an assignment back here, a suspended role kept appearing in every
-- session envelope until the assignment ended, which is exactly the stale claim
-- the iam.assignment.* events exist to prevent.
--
-- Two timestamps and no flag, on purpose. iam.assignment.suspended and
-- iam.assignment.resumed are delivered at least once and can arrive out of
-- order, so the state is derived: the assignment is suspended when the latest
-- suspension is newer than the latest resumption. Whichever event is processed
-- last, the answer is the same. (Contrast a revocation, which is permanent and
-- tombstoned in revoked_role_assignments; a suspension must be reversible.)

ALTER TABLE principal_role_assignments
    ADD COLUMN IF NOT EXISTS suspended_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS resumed_at   TIMESTAMP WITH TIME ZONE;
