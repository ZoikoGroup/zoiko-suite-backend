-- Migration: 000016_evidence_ledger.down.sql
--
-- Reverts the G-07 evidence ledger and the register fields. Dropping columns
-- rewrites no row, so the append-only triggers are not involved. Sealed
-- packages are evidence: this drops them, which is why it is a down migration
-- and never run against a ledger anyone relies on.

BEGIN;

DROP TABLE IF EXISTS evidence_packages;
DROP FUNCTION IF EXISTS reject_ledger_mutation();
DROP FUNCTION IF EXISTS reject_ledger_truncate();

DROP TRIGGER IF EXISTS gdl_20_chain ON governance_decisions;
DROP TRIGGER IF EXISTS gdl_10_classify ON governance_decisions;
DROP FUNCTION IF EXISTS gdl_chain_link();
DROP FUNCTION IF EXISTS gdl_classify_decision();
DROP FUNCTION IF EXISTS gdl_canonical(governance_decisions);
DROP FUNCTION IF EXISTS gdl_ts(TIMESTAMPTZ);

DROP INDEX IF EXISTS uq_governance_decisions_chain;
DROP INDEX IF EXISTS idx_governance_decisions_tenant_decided;
DROP INDEX IF EXISTS idx_governance_decisions_tenant_domain;
DROP INDEX IF EXISTS idx_governance_decisions_tenant_causation;

ALTER TABLE governance_decisions
    DROP CONSTRAINT IF EXISTS chk_chain_hashes,
    DROP CONSTRAINT IF EXISTS chk_priority,
    DROP CONSTRAINT IF EXISTS chk_tags,
    DROP COLUMN IF EXISTS chain_seq,
    DROP COLUMN IF EXISTS prev_hash,
    DROP COLUMN IF EXISTS curr_hash,
    DROP COLUMN IF EXISTS title,
    DROP COLUMN IF EXISTS summary,
    DROP COLUMN IF EXISTS domain,
    DROP COLUMN IF EXISTS priority,
    DROP COLUMN IF EXISTS tags,
    DROP COLUMN IF EXISTS related_obligation_id;

DROP FUNCTION IF EXISTS gdl_classify_domain(TEXT);
DROP TABLE IF EXISTS decision_outcome_categories;
DROP TABLE IF EXISTS decision_domain_rules;
DROP TABLE IF EXISTS decision_domains;

COMMIT;
