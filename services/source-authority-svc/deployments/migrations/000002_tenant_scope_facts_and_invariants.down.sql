-- 000002_tenant_scope_facts_and_invariants.down.sql
--
-- Reverses 000002. Note that dropping normalized_facts.tenant_id discards the
-- tenant attribution of every fact recorded while it existed; re-applying the up
-- migration cannot reconstruct it, and every surviving row would come back as
-- unattributable.

DROP INDEX IF EXISTS idx_source_authority_maps_effective;
ALTER TABLE source_authority_maps DROP CONSTRAINT IF EXISTS source_authority_maps_supersession_has_evidence;
ALTER TABLE source_authority_maps DROP COLUMN IF EXISTS superseded_by_principal_id;
ALTER TABLE source_authority_maps DROP COLUMN IF EXISTS superseded_at;
DROP INDEX IF EXISTS idx_source_authority_maps_idempotency;
ALTER TABLE source_authority_maps DROP COLUMN IF EXISTS correlation_id;
ALTER TABLE source_authority_maps DROP CONSTRAINT IF EXISTS source_authority_maps_rank_positive;
ALTER TABLE source_authority_maps DROP CONSTRAINT IF EXISTS source_authority_maps_window_ordered;

ALTER TABLE normalized_facts DROP CONSTRAINT IF EXISTS normalized_facts_authority_class_known;
DROP INDEX IF EXISTS idx_normalized_facts_tenant_lookup;
CREATE INDEX IF NOT EXISTS idx_normalized_facts_lookup
    ON normalized_facts (field_family, entity_ref, source_system, effective_at DESC);
DROP INDEX IF EXISTS idx_normalized_facts_idempotency;
ALTER TABLE normalized_facts DROP COLUMN IF EXISTS correlation_id;

DROP POLICY IF EXISTS tenant_isolation_policy ON normalized_facts;
ALTER TABLE normalized_facts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE normalized_facts DISABLE ROW LEVEL SECURITY;
ALTER TABLE normalized_facts DROP CONSTRAINT IF EXISTS normalized_facts_tenant_present;
ALTER TABLE normalized_facts DROP COLUMN IF EXISTS tenant_id;
