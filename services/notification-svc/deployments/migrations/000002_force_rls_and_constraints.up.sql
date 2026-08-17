-- FORCE, not just ENABLE.
--
-- 000001 enabled row-level security and wrote a tenant isolation policy, and
-- that policy never applied to a single query this service made: Postgres
-- exempts a table's OWNER from row-level security unless the table is declared
-- FORCE ROW LEVEL SECURITY, and these services connect as the owner.
--
-- The store carries an explicit `tenant_id = $n` predicate on every statement,
-- so isolation here does not rest on the policy — but a policy that silently
-- does nothing is worse than no policy, because it reads as a control that is
-- present.
ALTER TABLE notifications FORCE ROW LEVEL SECURITY;

-- The USING expression doubles as the WITH CHECK for INSERT/UPDATE when no
-- WITH CHECK is given, so under FORCE a row may only be written into the
-- tenant the connection has installed. Stated explicitly now that it is
-- load-bearing.
DROP POLICY IF EXISTS tenant_isolation_policy ON notifications;
CREATE POLICY tenant_isolation_policy ON notifications FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Every CHECK below is added NOT VALID, deliberately.
--
-- NOT VALID enforces the constraint on every INSERT and UPDATE from here on;
-- what it skips is the scan that would reject the table outright if any
-- EXISTING row violates it. Some do, and they are the defect this pass fixed:
-- the delivery adapter used to treat an unrecognised channel as a delivery
-- failure, so a caller's typo left rows like
-- (channel: 'PIGEON', status: 'FAILED', failure_reason: 'unsupported channel:
-- PIGEON') — a permanent record that a delivery was attempted and refused, for
-- a channel no provider ever saw. One such row is in the dev database today.
--
-- A migration must not silently delete or rewrite those. They are the audit
-- trail of what the service actually did, wrong as it was, and a notification
-- register that quietly edits its own history is worth less than one with an
-- embarrassing row in it. Correcting them is an operational decision with a
-- human attached; run ALTER TABLE ... VALIDATE CONSTRAINT afterwards to prove
-- the backlog is clean.
ALTER TABLE notifications
    ADD CONSTRAINT notifications_channel_known
    CHECK (channel IN ('EMAIL', 'SMS', 'IN_APP', 'WEBHOOK')) NOT VALID;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_status_known
    CHECK (status IN ('PENDING', 'SENT', 'FAILED')) NOT VALID;

-- A concluded delivery must say when it concluded, and a FAILED one must say
-- why. A FAILED row with no reason is a record that something did not go out
-- and no account of what happened — which is the only thing that row is for.
ALTER TABLE notifications
    ADD CONSTRAINT notifications_concluded_has_timestamp
    CHECK (status = 'PENDING' OR sent_at IS NOT NULL) NOT VALID;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_failed_has_reason
    CHECK (status <> 'FAILED' OR (failure_reason IS NOT NULL AND failure_reason <> '')) NOT VALID;

-- The register is read newest-first and paged; created_at alone is not a total
-- order, so the index carries the primary key as a tiebreaker for the same
-- reason the ORDER BY does.
CREATE INDEX IF NOT EXISTS idx_notifications_tenant_created
    ON notifications (tenant_id, created_at DESC, notification_id DESC);
