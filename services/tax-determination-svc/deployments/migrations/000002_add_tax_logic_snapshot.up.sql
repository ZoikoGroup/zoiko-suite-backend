-- tax_determinations pinned a determination to a rule via rule_id alone —
-- a plain pointer into tax-rules-svc's mutable, versioned tax_rules table.
-- If that rule is later edited, the determination silently loses its exact
-- historical basis (rate, deductions, code) even though rule_id still
-- resolves to *a* row. tax_logic_snapshot_id is a content-addressed
-- (SHA-256) reference over the actual rule fields applied at determination
-- time — computed by this service itself from the real tax-rules-svc
-- response, not a caller-supplied or fabricated value. Nullable: a
-- determination made via the zero-tax fallback (tax-rules-svc unreachable)
-- has no real rule content to snapshot.
ALTER TABLE tax_determinations
    ADD COLUMN tax_logic_snapshot_id TEXT;
