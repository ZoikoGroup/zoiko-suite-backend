-- Migration: 000004_add_data_classification.down.sql
-- Removes data_classification column from the tax_identity_bundles table.

ALTER TABLE tax_identity_bundles DROP COLUMN data_classification;
