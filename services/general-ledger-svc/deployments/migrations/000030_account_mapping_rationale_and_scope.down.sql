-- Migration: 000030_account_mapping_rationale_and_scope.down.sql

DROP INDEX IF EXISTS idx_account_mappings_active_scoped;
CREATE UNIQUE INDEX idx_account_mappings_active
    ON account_mappings (tenant_id, mapping_key)
    WHERE status = 'ACTIVE';

ALTER TABLE account_mappings
    DROP CONSTRAINT IF EXISTS account_mappings_book_requires_entity,
    DROP COLUMN IF EXISTS reason,
    DROP COLUMN IF EXISTS legal_entity_id,
    DROP COLUMN IF EXISTS book_id;
