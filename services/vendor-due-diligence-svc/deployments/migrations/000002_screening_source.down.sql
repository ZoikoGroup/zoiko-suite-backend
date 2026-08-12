ALTER TABLE vendor_dd_checks
    DROP CONSTRAINT IF EXISTS vendor_dd_checks_outcome_requires_conclusion;

ALTER TABLE vendor_dd_checks
    DROP COLUMN IF EXISTS screening_source;

-- The document_reference '' -> NULL normalisation is deliberately not reversed.
-- Restoring '' would put back the state where "no document" and "a document with
-- a blank reference" are the same value, and nothing reads the column expecting
-- an empty string.
