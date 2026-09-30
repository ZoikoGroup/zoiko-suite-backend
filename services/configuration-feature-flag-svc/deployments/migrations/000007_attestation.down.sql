-- Migration: 000007_attestation.down.sql

DROP POLICY IF EXISTS drift_read_all_write_scoped ON drift_events;
DROP TABLE IF EXISTS drift_events;

DROP POLICY IF EXISTS attestations_read_all_write_scoped ON runtime_attestations;
DROP TABLE IF EXISTS runtime_attestations;