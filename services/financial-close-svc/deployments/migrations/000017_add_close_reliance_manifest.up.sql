-- Migration: 000017_add_close_reliance_manifest.up.sql
--
-- ZS-CONTROL-001 §22: "Control run IDs and evidence packages pinned to close
-- instance." A close's evidence recorded the trial-balance hash and its
-- signature, but not WHICH control runs and bank reconciliations it relied
-- on — so the question an auditor asks first ("what proved cash and the
-- subledgers when October was closed?") had no recorded answer, and a later
-- re-run could not be told apart from the run the close actually used.
--
-- reliance_manifest is the close's own record of what it relied on: each
-- required control and the run that matched it, each bank account and the
-- certified reconciliation that proved it, the exclusions and checklist that
-- applied, and whether any gate was switched off. TEXT, not JSONB: the exact
-- bytes are hashed and signed, and JSONB re-orders keys.
--
-- Nullable: closes recorded before this migration have none. The three
-- columns are written together or not at all.

ALTER TABLE close_evidences ADD COLUMN reliance_manifest TEXT;
ALTER TABLE close_evidences ADD COLUMN reliance_hash VARCHAR(64);
ALTER TABLE close_evidences ADD COLUMN reliance_signature VARCHAR(255);
ALTER TABLE close_evidences ADD CONSTRAINT close_evidences_reliance_complete CHECK (
    (reliance_manifest IS NULL AND reliance_hash IS NULL AND reliance_signature IS NULL)
    OR (reliance_manifest IS NOT NULL AND reliance_hash IS NOT NULL AND reliance_signature IS NOT NULL)
);
