-- Migration: 000004_add_data_classification.up.sql
-- Adds data_classification column to the tax_identity_bundles table per docs/architecture/04-data-model.md §20.

ALTER TABLE tax_identity_bundles ADD COLUMN data_classification VARCHAR(32) NOT NULL DEFAULT 'RESTRICTED';
