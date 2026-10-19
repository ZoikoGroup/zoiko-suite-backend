-- Migration: 000004_statement_disclosures.up.sql
--
-- DisclosureInstance (SPEC_DEVIATIONS A5): freeform narrative notes attached
-- to a statement set. A child of financial_statement_sets exactly as
-- statement_facts is: no lifecycle of its own. Rows are only ever inserted
-- while the parent set is DRAFT (enforced in the store under the parent
-- row's lock) and never updated or deleted, so certification freezes them
-- with the set.
--
-- Additive: one new table, nothing existing is altered.

CREATE TABLE statement_disclosures (
    disclosure_id          UUID PRIMARY KEY,
    tenant_id              TEXT NOT NULL,
    statement_set_id       UUID NOT NULL REFERENCES financial_statement_sets(statement_set_id) ON DELETE CASCADE,
    sequence               INTEGER NOT NULL,
    title                  TEXT NOT NULL,
    body                   TEXT NOT NULL,
    added_by_principal_id  TEXT NOT NULL,
    added_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Replay protection for a retried add (the request's Idempotency-Key).
    -- NULL when none was sent; UNIQUE ignores NULLs.
    idempotency_key        TEXT,

    CONSTRAINT statement_disclosures_sequence_positive CHECK (sequence >= 1),
    CONSTRAINT statement_disclosures_title_present CHECK (length(btrim(title)) BETWEEN 1 AND 200),
    CONSTRAINT statement_disclosures_body_present CHECK (length(btrim(body)) BETWEEN 1 AND 20000),
    CONSTRAINT statement_disclosures_unique_sequence UNIQUE (statement_set_id, sequence),
    CONSTRAINT statement_disclosures_unique_idempotency UNIQUE (statement_set_id, idempotency_key)
);

-- Serves the SoD check: did this principal author a disclosure on this set.
CREATE INDEX statement_disclosures_author_idx ON statement_disclosures (statement_set_id, added_by_principal_id);

ALTER TABLE statement_disclosures ENABLE ROW LEVEL SECURITY;
ALTER TABLE statement_disclosures FORCE ROW LEVEL SECURITY;

CREATE POLICY statement_disclosures_tenant_isolation ON statement_disclosures
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
