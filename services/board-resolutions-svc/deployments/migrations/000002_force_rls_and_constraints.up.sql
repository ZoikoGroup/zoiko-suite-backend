-- +migrate Up
BEGIN;

-- FORCE, not just ENABLE.
--
-- 000001 enabled row-level security and wrote a tenant isolation policy for
-- both tables, and that policy never applied to a single query the service
-- made: Postgres exempts a table's OWNER from row-level security unless the
-- table is declared FORCE ROW LEVEL SECURITY, and these services connect as
-- the owner. The isolation looked enforced in the schema and was not enforced
-- at runtime.
--
-- The store now also carries an explicit `tenant_id = $n` predicate on every
-- statement, so isolation does not rest on this alone — but a policy that
-- silently does nothing is worse than no policy, because it reads as a
-- control that is present.
ALTER TABLE board_meetings FORCE ROW LEVEL SECURITY;
ALTER TABLE board_resolutions FORCE ROW LEVEL SECURITY;

-- The USING expression doubles as the WITH CHECK for INSERT/UPDATE when no
-- WITH CHECK is given, so under FORCE a row may only be written into the
-- tenant the connection has installed. Stated explicitly rather than left
-- implicit, since it is now load-bearing.
DROP POLICY IF EXISTS meetings_tenant_isolation ON board_meetings;
CREATE POLICY meetings_tenant_isolation ON board_meetings
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS resolutions_tenant_isolation ON board_resolutions;
CREATE POLICY resolutions_tenant_isolation ON board_resolutions
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Every CHECK below is added NOT VALID, deliberately.
--
-- NOT VALID enforces the constraint on every INSERT and UPDATE from here on;
-- what it skips is the scan that would reject the table outright if any
-- EXISTING row violates it. Board minutes and resolutions are the record of
-- what a board decided — a migration that refuses to apply, or that rewrites
-- them to fit a rule written afterwards, is worse than one that constrains
-- everything from now on and leaves the history legible. Run
-- ALTER TABLE ... VALIDATE CONSTRAINT afterwards to prove the backlog is clean;
-- notification-svc's twin migration hit exactly this and had rows to show for
-- it.
--
-- Vote tallies are counts of people. Nothing stopped a negative one being
-- stored, so a resolution could carry -5 votes against and its arithmetic
-- would still "work".
ALTER TABLE board_resolutions
    ADD CONSTRAINT board_resolutions_votes_non_negative
    CHECK (votes_for >= 0 AND votes_against >= 0 AND abstentions >= 0) NOT VALID;

-- status and category are the two columns the service branches on: status
-- gates every transition, and category is sent to evidence-requirements-svc as
-- the domain_code. An unrecognised category asks the catalog about a domain
-- that does not exist and comes back with no requirements — an evidence gate
-- bypassed by a typo. Both are validated at the boundary now; this is the
-- backstop for anything that writes to the table without going through it.
ALTER TABLE board_meetings
    ADD CONSTRAINT board_meetings_status_known
    CHECK (status IN ('SCHEDULED', 'IN_PROGRESS', 'ADJOURNED', 'CANCELLED')) NOT VALID;

ALTER TABLE board_resolutions
    ADD CONSTRAINT board_resolutions_status_known
    CHECK (status IN ('PROPOSED', 'PASSED', 'REJECTED', 'RESCINDED')) NOT VALID;

ALTER TABLE board_resolutions
    ADD CONSTRAINT board_resolutions_category_known
    CHECK (category IN ('GOVERNANCE', 'FINANCIAL', 'OPERATIONAL', 'EXECUTIVE', 'STATUTORY')) NOT VALID;

-- A PASSED resolution is the record that the board put something into force.
-- Both halves of that record must be present: passed_at without passed_by is
-- an unattributed finalization, which is exactly what the segregation-of-
-- duties doctrine exists to prevent.
ALTER TABLE board_resolutions
    ADD CONSTRAINT board_resolutions_passed_is_attributed
    CHECK (status <> 'PASSED' OR (passed_by IS NOT NULL AND passed_at IS NOT NULL)) NOT VALID;

-- The resolution list is filtered by (tenant, legal entity, meeting, status)
-- and ordered by created_at; the existing indexes covered neither the tenant
-- prefix on meeting_id nor the ordering.
CREATE INDEX IF NOT EXISTS idx_resolutions_tenant_meeting ON board_resolutions (tenant_id, meeting_id);
CREATE INDEX IF NOT EXISTS idx_resolutions_tenant_created ON board_resolutions (tenant_id, created_at DESC, resolution_id DESC);
CREATE INDEX IF NOT EXISTS idx_meetings_tenant_scheduled ON board_meetings (tenant_id, scheduled_at DESC, meeting_id DESC);

COMMIT;
