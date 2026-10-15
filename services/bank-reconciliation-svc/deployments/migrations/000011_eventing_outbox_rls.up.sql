-- Migration: 000011_eventing_outbox_rls.up.sql
--
-- 000010 created eventing_outbox without row-level security, inconsistent
-- with this service's own doctrine (000001/000004: every tenant table gets
-- RLS, FORCE'd so the table owner is bound too). Adding it directly with the
-- relay-aware policy, not the tenant-only one financial-close-svc's 000019
-- shipped and had to fix in 000020: the outbox relay drains every tenant's
-- backlog from one background loop and authenticates as app.outbox_relay,
-- not a tenant (eventing/outbox's inRelayTx) -- a tenant-only policy would
-- block the relay from ever publishing a row under a NOBYPASSRLS role.
--
-- NULLIF because a transaction-local custom GUC reads as '' (not NULL) on a
-- pooled connection that has already served a request (see 000005).

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
