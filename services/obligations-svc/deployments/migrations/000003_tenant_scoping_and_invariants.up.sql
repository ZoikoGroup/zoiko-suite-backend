-- 000002_tenant_scoping_and_invariants.up.sql
--
-- This service had NO tenant column at all. Not a missing filter — a missing
-- dimension. Every other governed register on this platform is tenant-isolated;
-- this one stored obligations keyed only by legal entity, so nothing in the
-- schema or the code could distinguish one tenant's statutory obligations from
-- another's, and every list returned all of them.
--
-- The sharpest edge was the dedup key. obligation_code carried a GLOBAL unique
-- index, and creation is idempotent on it: `ON CONFLICT (obligation_code) DO
-- NOTHING` followed by a lookup that returns the existing row. So a second
-- tenant registering a perfectly ordinary code — "VAT-Q1-2026" — did not create
-- their obligation. It silently returned the FIRST tenant's obligation, with
-- that tenant's legal entity, due date and source reference, as a 200. One
-- tenant's compliance register answering with another's record, through the
-- documented happy path.

BEGIN;

-- ── tenant_id ────────────────────────────────────────────────────────────────
--
-- Added nullable, backfilled, then constrained — the only safe order for a
-- table with existing rows.
--
-- The backfill value is the demo tenant. That is a judgement, and it is
-- recorded here rather than hidden: this service has only ever been written to
-- from the console in development, which uses that single tenant, so every
-- existing row does belong to it. A deployment with real multi-tenant history
-- must NOT run this as-is — it has to derive tenant_id from each row's
-- legal_entity_id via tenant-entity-registry-svc before applying the NOT NULL.
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS tenant_id UUID;
UPDATE obligations SET tenant_id = '11111111-1111-1111-1111-111111111111' WHERE tenant_id IS NULL;
ALTER TABLE obligations ALTER COLUMN tenant_id SET NOT NULL;

-- filing_requirements hangs off an obligation, but it carries its own tenant
-- so a query can be scoped without a join — and, more importantly, so
-- row-level security can apply to it directly. A policy that has to join to
-- find its tenant is a policy that does not run on a bare SELECT.
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS tenant_id UUID;
UPDATE filing_requirements fr
   SET tenant_id = o.tenant_id
  FROM obligations o
 WHERE fr.obligation_id = o.obligation_id AND fr.tenant_id IS NULL;
ALTER TABLE filing_requirements ALTER COLUMN tenant_id SET NOT NULL;

-- ── the dedup key, re-scoped ─────────────────────────────────────────────────
--
-- This is the fix for the cross-tenant replay described above. An obligation
-- code is unique WITHIN a tenant; two tenants may both have a "VAT-Q1-2026"
-- and they are different obligations.
DROP INDEX IF EXISTS idx_obligations_code_unique;
CREATE UNIQUE INDEX idx_obligations_tenant_code_unique
    ON obligations (tenant_id, obligation_code);

-- ── row-level security ───────────────────────────────────────────────────────
--
-- FORCE, not just ENABLE: Postgres exempts a table's OWNER from row-level
-- security unless FORCE is set, and these services connect as the owner. An
-- ENABLEd policy on an owner connection applies to nothing at all — the
-- control reads as present in the schema and does nothing at runtime. The
-- store also carries an explicit tenant predicate on every statement, so
-- isolation does not rest on this alone.
ALTER TABLE obligations ENABLE ROW LEVEL SECURITY;
ALTER TABLE obligations FORCE ROW LEVEL SECURITY;
ALTER TABLE filing_requirements ENABLE ROW LEVEL SECURITY;
ALTER TABLE filing_requirements FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON obligations;
CREATE POLICY tenant_isolation_policy ON obligations FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_policy ON filing_requirements;
CREATE POLICY tenant_isolation_policy ON filing_requirements FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ── query support ────────────────────────────────────────────────────────────
--
-- Every read is now tenant-first, so the existing single-column indexes are
-- the wrong shape for it.
CREATE INDEX IF NOT EXISTS idx_obligations_tenant_entity ON obligations (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_obligations_tenant_status ON obligations (tenant_id, obligation_status);
CREATE INDEX IF NOT EXISTS idx_obligations_tenant_due ON obligations (tenant_id, due_date);
-- The register is read newest-first and paged; due_date alone is not a total
-- order, so the primary key rides along as the tiebreaker for stable paging.
CREATE INDEX IF NOT EXISTS idx_obligations_tenant_due_id ON obligations (tenant_id, due_date DESC, obligation_id DESC);
CREATE INDEX IF NOT EXISTS idx_filing_requirements_tenant ON filing_requirements (tenant_id, obligation_id);

-- ── invariants the code relies on but never stated ───────────────────────────
--
-- NOT VALID: these constrain every future write while leaving existing rows
-- readable. An obligation register that quietly rewrites its own history to
-- satisfy a rule written afterwards is worth less than one with an odd row in
-- it. Run VALIDATE CONSTRAINT once the backlog has been looked at.
ALTER TABLE obligations
    ADD CONSTRAINT obligations_status_known
    CHECK (obligation_status IN ('OPEN', 'IN_PROGRESS', 'OVERDUE', 'CLOSED')) NOT VALID;

-- CLOSED is terminal and stamps closed_at; the two must agree, or the register
-- shows an obligation that is closed with no record of when.
ALTER TABLE obligations
    ADD CONSTRAINT obligations_closed_has_timestamp
    CHECK ((obligation_status = 'CLOSED') = (closed_at IS NOT NULL)) NOT VALID;

ALTER TABLE filing_requirements
    ADD CONSTRAINT filing_requirements_status_known
    CHECK (filing_status IN ('PENDING', 'SUBMITTED', 'ACCEPTED', 'REJECTED')) NOT VALID;

COMMIT;
