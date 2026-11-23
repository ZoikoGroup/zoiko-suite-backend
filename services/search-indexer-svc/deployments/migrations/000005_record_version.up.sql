-- 000005_record_version.up.sql
--
-- Add optimistic-concurrency version (record_version) to mutable aggregates
-- in search-indexer-svc. API-3 requires ETag/If-Match on every aggregate
-- mutation; GET must return ETag, and mutations require If-Match.

ALTER TABLE index_contracts
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE index_generations
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE restriction_tombstones
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE index_contracts
    ADD CONSTRAINT ic_version_positive CHECK (record_version > 0);

ALTER TABLE index_generations
    ADD CONSTRAINT ig_version_positive CHECK (record_version > 0);

ALTER TABLE restriction_tombstones
    ADD CONSTRAINT rt_version_positive CHECK (record_version > 0);