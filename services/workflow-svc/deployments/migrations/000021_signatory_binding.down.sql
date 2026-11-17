-- Migration: 000021_signatory_binding.down.sql
ALTER TABLE workflow_stage_quorum_decisions
    DROP COLUMN IF EXISTS document_hash,
    DROP COLUMN IF EXISTS signer_offices;
DROP TABLE IF EXISTS workflow_signatory_bindings;
DROP FUNCTION IF EXISTS workflow_signatory_bindings_immutable();
