-- Migration: 000012_eventing_outbox_rls.up.sql
--
-- 000011 created eventing_outbox without row-level security, inconsistent
-- with this service's own doctrine (every other tenant table in this
-- service has RLS). Adding it directly with the relay-aware policy, not a
-- tenant-only policy that would have to be walked back later (the mistake
-- made, and fixed, in consolidation-svc's 000005 and financial-close-svc's
-- 000020 earlier this session): the outbox relay drains every tenant's
-- backlog from one background loop and authenticates via app.outbox_relay,
-- not a tenant, so a tenant-only policy would block it from ever publishing
-- under a real NOBYPASSRLS role.
--
-- NULLIF because a transaction-local custom GUC reads as '' (not NULL) on a
-- pooled connection that has already served a request.

ALTER TABLE eventing_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY eventing_outbox_tenant_isolation ON eventing_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
