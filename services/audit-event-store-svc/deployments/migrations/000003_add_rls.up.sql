-- Migration: 000003_add_rls.up.sql
--
-- Row-level security on audit_events, with a deliberate, load-bearing
-- platform-scope escape hatch for this service's own writer.
--
-- READ THIS BEFORE CHANGING THE POLICY. A plain tenant-only policy here
-- does not merely over-restrict — it silently destroys the platform's
-- append-only evidence store. Three facts combine:
--
--   1. This service has NO tenant-context plumbing. It is a Kafka
--      consumer, not an HTTP request path: nothing sets app.tenant_id,
--      because there is no X-Tenant-Id header to read it from.
--   2. PgStore.Store's hash chain is GLOBAL and cross-tenant by design
--      (Doc 04 §15.4 tamper-evidence chain). It reads the chain tip with
--      `SELECT payload_hash, sequence_number FROM audit_events
--      ORDER BY sequence_number DESC LIMIT 1` — across every tenant —
--      and links each new row to it.
--   3. sequence_number carries a UNIQUE index (migration 000002).
--
-- So under a tenant-only FORCE policy: the chain-tip SELECT matches zero
-- rows, every event believes it is genesis, nextSeq is permanently 1, and
-- the INSERT's WITH CHECK rejects the row anyway. The result is that the
-- audit event store stops accepting events entirely — and it fails
-- quietly, because a Kafka consumer's insert error is absorbed by the DLQ
-- rather than surfacing to a caller. Doc 03 §22 requires evidence
-- services to "fail safe and durable"; refusing all evidence is the
-- worst available outcome.
--
-- Hence app.platform_scope, set only by PgStore.Store (see its doc
-- comment). The writer legitimately writes on behalf of every tenant and
-- must read the global chain tip, so it is exempt.
--
-- WHY THE POLICY IS STILL WORTH ADDING, given the only current caller
-- bypasses it: Doc 03 §14.1 REQUIRES this service's records to be
-- "queryable by actor, entity, action, workflow, or time range" — and no
-- such query API exists yet (tracked separately). When it is built, it
-- inherits tenant scoping by default and has to opt out explicitly and
-- visibly, rather than needing this retrofitted onto a live evidence
-- store later. That is the whole value here: the not-yet-written reader
-- is born safe.
--
-- tenant_id is TEXT in this service (not UUID), so no ::uuid cast — and
-- NULLIF guards against app.tenant_id being set to the empty string,
-- which must never match a real tenant_id.

ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON audit_events
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR current_setting('app.platform_scope', true) = 'true'
    );
