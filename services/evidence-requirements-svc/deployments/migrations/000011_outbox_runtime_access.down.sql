-- Migration 000011 down: restore 000008's outbox policy and drop the backlog
-- count. Note this restores the state in which the runtime role cannot write
-- events (see the up migration).

DROP FUNCTION IF EXISTS outbox_unpublished_count(INTERVAL);
DROP POLICY IF EXISTS outbox_append_only ON outbox;
CREATE POLICY tenant_isolation_policy ON outbox
    FOR ALL USING (false);
