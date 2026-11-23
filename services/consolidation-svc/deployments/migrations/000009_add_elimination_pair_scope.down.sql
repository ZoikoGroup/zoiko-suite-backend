-- Migration: 000009_add_elimination_pair_scope.down.sql

ALTER TABLE consolidation_adjustments
    DROP COLUMN IF EXISTS source_legal_entity_id,
    DROP COLUMN IF EXISTS target_legal_entity_id;
