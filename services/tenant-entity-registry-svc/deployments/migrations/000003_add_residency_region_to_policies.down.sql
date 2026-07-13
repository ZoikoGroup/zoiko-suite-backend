-- Migration: 000003_add_residency_region_to_policies.down.sql

ALTER TABLE data_residency_policies
    DROP COLUMN IF EXISTS residency_region_id;
