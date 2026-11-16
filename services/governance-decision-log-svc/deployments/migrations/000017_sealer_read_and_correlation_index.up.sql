-- Migration: 000017_sealer_read_and_correlation_index.up.sql
--
-- 1. An index for the "audit" query dimension (decision D2-2.F11, 11 Nov
--    2026): GET /v1/decisions?correlation=<id> lists every decision made
--    under one audited business trace, within the caller's tenant.
--
-- 2. Lets the scheduled evidence sealer (EVIDENCE_SEAL_INTERVAL) find which
-- tenants have decisions not yet sealed into a GOV-07 evidence package.
--
-- governance_decisions and evidence_packages are under FORCE row-level
-- security keyed on app.tenant_id, so the runtime role (NOBYPASSRLS) sees one
-- tenant at a time. The sealer acts for no tenant when it looks for work: it
-- marks its own transaction with app.evidence_sealer = 'on', and these
-- policies admit READS under that marker. Nothing is admitted for writing: the
-- sealer seals each tenant through the ordinary tenant-scoped path, and the
-- request path never sets the marker. Same design as the outbox relay's
-- app.outbox_relay marker (000012).

BEGIN;

CREATE INDEX IF NOT EXISTS idx_governance_decisions_tenant_correlation
    ON governance_decisions (tenant_id, correlation_id);

DROP POLICY IF EXISTS evidence_sealer_read ON governance_decisions;
CREATE POLICY evidence_sealer_read ON governance_decisions
    FOR SELECT
    USING (current_setting('app.evidence_sealer', true) = 'on');

DROP POLICY IF EXISTS evidence_sealer_read ON evidence_packages;
CREATE POLICY evidence_sealer_read ON evidence_packages
    FOR SELECT
    USING (current_setting('app.evidence_sealer', true) = 'on');

COMMIT;
