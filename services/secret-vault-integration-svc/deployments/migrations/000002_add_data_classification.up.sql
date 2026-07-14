-- Migration: 000002_add_data_classification.up.sql
-- Adds data_classification column to the secret_policies table per docs/architecture/04-data-model.md §20.

ALTER TABLE secret_policies ADD COLUMN data_classification VARCHAR(32) NOT NULL DEFAULT 'RESTRICTED';
