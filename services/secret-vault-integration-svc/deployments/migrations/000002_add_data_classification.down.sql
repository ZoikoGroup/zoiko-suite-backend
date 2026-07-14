-- Migration: 000002_add_data_classification.down.sql
-- Removes data_classification column from the secret_policies table.

ALTER TABLE secret_policies DROP COLUMN data_classification;
