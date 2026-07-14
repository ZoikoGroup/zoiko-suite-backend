-- Migration: 000002_add_data_classification.up.sql
-- Adds data_classification column to the principals table per docs/architecture/04-data-model.md §20.

ALTER TABLE principals ADD COLUMN data_classification VARCHAR(32) NOT NULL DEFAULT 'RESTRICTED';
