-- 000011_outbox_relay_policy.up.sql
-- The outbox relay is a system worker: it must read and mark records across
-- every tenant, but 000010 forces RLS on event_outbox. A second policy,
-- gated on the transaction-local app.outbox_relay flag, grants exactly the
-- relay's two operations without widening tenant-facing access.

CREATE POLICY outbox_relay_select ON event_outbox
    FOR SELECT
    USING (current_setting('app.outbox_relay', true) = 'true');

CREATE POLICY outbox_relay_update ON event_outbox
    FOR UPDATE
    USING (current_setting('app.outbox_relay', true) = 'true')
    WITH CHECK (current_setting('app.outbox_relay', true) = 'true');
