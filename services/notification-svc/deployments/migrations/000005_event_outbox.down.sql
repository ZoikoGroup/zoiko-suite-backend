-- Reverse of 000005.
--
-- Dropping this table discards any event that has been committed alongside a
-- delivery conclusion but not yet handed to the broker. Those are notifications
-- that DID conclude — the register still says so — whose consumers will never
-- be told. Drain the outbox to empty before running this, or the loss is
-- exactly the one 000005 exists to prevent, performed deliberately:
--
--   SELECT count(*) FROM event_outbox WHERE published_at IS NULL;
--
-- Reverting the table alone is not enough to leave a working service: the
-- enqueue runs inside the delivery transaction, so every send would fail at the
-- INSERT and every notification would be refused. Roll the binary back with it.

DROP POLICY IF EXISTS outbox_tenant_isolation ON event_outbox;
DROP INDEX IF EXISTS idx_event_outbox_unpublished;
DROP TABLE IF EXISTS event_outbox;
