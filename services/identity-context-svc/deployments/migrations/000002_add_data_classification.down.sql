-- Migration: 000002_add_data_classification.down.sql
-- Removes data_classification column from the principals table.

ALTER TABLE principals DROP COLUMN data_classification;
