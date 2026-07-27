-- Partial unique index enabling idempotent journal creation: a retried
-- CreateJournal call with the same (tenant_id, correlation_id) returns the
-- original journal instead of creating a duplicate. Partial (WHERE
-- correlation_id != '') because correlation_id was previously optional —
-- an empty string must never collide across genuinely different journals.
CREATE UNIQUE INDEX idx_journal_headers_tenant_correlation
    ON journal_headers (tenant_id, correlation_id)
    WHERE correlation_id != '';
