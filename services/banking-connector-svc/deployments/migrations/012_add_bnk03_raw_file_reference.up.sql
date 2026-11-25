-- BNK-03's authoritative ownership (table 19) includes "raw source
-- reference/hash", not just a hash. Before this migration, content_hash
-- (migration 004) proved integrity but there was nothing to compare it
-- AGAINST: no reference to the original file's bytes, so a disputed
-- statement had a hash and no way to pull the real original back up
-- (SPEC_DEVIATIONS.md A10, partial close — this does not fix content_hash
-- being caller-supplied, only adds the missing raw-file retrieval leg).
--
-- raw_file_document_id is the document_id document-vault-svc returned when
-- the original statement file was stored there (empty for a statement
-- ingested without a raw file attached, or ingested before this migration —
-- never backfilled with a guess).
--
-- raw_file_checksum_sha256 is read back from document-vault-svc's OWN
-- verify-digest query at ingest time, not computed locally — it is
-- independent evidence that what the vault stored is what was ingested,
-- kept beside content_hash (whatever hash the caller supplied) rather than
-- replacing it.
ALTER TABLE bank_statements
    ADD COLUMN raw_file_document_id     VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN raw_file_checksum_sha256 VARCHAR(64) NOT NULL DEFAULT '';
