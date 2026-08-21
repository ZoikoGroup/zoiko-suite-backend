-- The down migration for 000001 was missing entirely — this service was the
-- only one of the seventeen finished services with an up migration and no
-- matching down, so there was no way to roll its schema back.
--
-- Order matters: close_evidences references the period it seals, so it goes
-- first. Dropping the tables removes their policies and indexes with them; both
-- are named explicitly first so a partial application still cleans up.

DROP INDEX IF EXISTS idx_close_evidences_tenant;
DROP INDEX IF EXISTS idx_fiscal_periods_tenant_entity;

DROP POLICY IF EXISTS tenant_isolation_policy ON close_evidences;
DROP POLICY IF EXISTS tenant_isolation_policy ON fiscal_periods;

DROP TABLE IF EXISTS close_evidences;
DROP TABLE IF EXISTS fiscal_periods;
