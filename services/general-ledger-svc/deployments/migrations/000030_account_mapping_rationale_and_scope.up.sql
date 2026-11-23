-- Migration: 000030_account_mapping_rationale_and_scope.up.sql
--
-- ACC-02's "Required source inputs" names "rationale" and "target
-- entity/book/account" as part of what a mapping carries; migration 000029
-- built the Draft->Approved->Active->Superseded lifecycle but captured
-- neither. This adds both:
--
-- reason: the rationale for the mapping, set at whichever lifecycle step
-- supplies one (CreateMapping, ApproveMapping, ActivateMapping or
-- SupersedeMapping) and left unchanged by any step that doesn't.
--
-- legal_entity_id / book_id: the mapping's target scope. NULL/NULL is a
-- tenant-wide mapping (every existing mapping, and every mapping any
-- caller creates without naming a scope, is exactly this — nothing
-- changes for them). A mapping can instead be scoped to one legal entity,
-- or to one entity+book pair. This is ACC-02's own "mapping precedence" as
-- SERVER-DERIVED context, not a caller-supplied number: entity+book beats
-- entity-only beats tenant-wide, so a more specific mapping overrides a
-- more general one for the same mapping_key without the general one ever
-- needing to be touched.

ALTER TABLE account_mappings
    ADD COLUMN reason TEXT,
    ADD COLUMN legal_entity_id UUID,
    ADD COLUMN book_id VARCHAR(64);

-- A book-scoped mapping without a named entity is meaningless (which
-- entity's book?) — refused at the schema level, not just in application code.
ALTER TABLE account_mappings
    ADD CONSTRAINT account_mappings_book_requires_entity
    CHECK (book_id IS NULL OR legal_entity_id IS NOT NULL);

-- Replaces idx_account_mappings_active (migration 000029): at most one
-- ACTIVE row per (tenant, mapping_key, scope) — the scope tuple now
-- includes legal_entity_id/book_id, COALESCEd because plain UNIQUE treats
-- NULL as distinct from NULL, which would otherwise let multiple
-- tenant-wide (NULL, NULL) mappings for the same key coexist ACTIVE. This
-- is still exactly ACC-02's "zero or multiple equally applicable mappings
-- block automated posting; no fallback guessing" — now per scope tier
-- instead of only the single tenant-wide tier migration 000029 modeled.
DROP INDEX IF EXISTS idx_account_mappings_active;
CREATE UNIQUE INDEX idx_account_mappings_active_scoped
    ON account_mappings (
        tenant_id, mapping_key,
        COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::uuid),
        COALESCE(book_id, '')
    )
    WHERE status = 'ACTIVE';
