-- Migration: 000002_add_idempotency_index.up.sql
--
-- Adds a unique index on (tenant_id, source_journal_id) so a retried
-- CreateEntry call (e.g. after a client-side timeout on a POST that
-- actually succeeded server-side) resolves to the ORIGINAL intercompany
-- entry instead of creating a duplicate. One source journal can only
-- ever be tracked by one intercompany entry — there is no legitimate
-- business case for two entries against the same source_journal_id — so
-- this collision is always safe to treat as a replay, same reasoning as
-- financial-close-svc's period_name uniqueness.
--
-- Not partial (unlike the correlation_id indexes in other Phase 3
-- services): source_journal_id is NOT NULL with no historical
-- empty-string rows to guard against.
CREATE UNIQUE INDEX idx_intercompany_entries_tenant_source_journal
    ON intercompany_entries (tenant_id, source_journal_id);
