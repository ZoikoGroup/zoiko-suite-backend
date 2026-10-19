-- Migration: 000007_match_attempts.up.sql
--
-- SPEC_DEVIATIONS A3. ExplainMismatch was a free-text mismatch_reason on the
-- pair, overwritten by each match attempt, so earlier attempts were lost and
-- nothing said which rule failed, what was expected against what was
-- observed on each side, which journals were looked at, or which tolerance
-- applied. §14 evidence: "Both entity/source references,
-- amounts/currencies, matching rule, exceptions and resolution evidence."
--
-- intercompany_match_attempts records every applied MatchIntercompany
-- attempt, in the transaction that applies it: append-only (trigger), FORCE
-- RLS on tenant. An attempt that is refused because the pair moved on
-- (ErrInvalidPairTransition) changed nothing and is not recorded.
--
-- intercompany_entries.mismatch_reason is no longer written. The service
-- reads mismatch_reason from the latest attempt's message; the column keeps
-- its pre-000007 value only so a rollback has something to return to, and
-- can be dropped once no deployed binary reads it.

CREATE TABLE intercompany_match_attempts (
    attempt_id            BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id             VARCHAR(255) NOT NULL,
    intercompany_entry_id UUID         NOT NULL REFERENCES intercompany_entries (intercompany_entry_id),
    outcome               VARCHAR(16)  NOT NULL CHECK (outcome IN ('MATCHED', 'MISMATCH')),
    failed_rule           VARCHAR(40)  CHECK (failed_rule IN (
        'COUNTERPARTY_JOURNAL_MISSING', 'TARGET_ENTITY_MISMATCH', 'TARGET_JOURNAL_NOT_FINALIZED',
        'CURRENCY_UNVERIFIABLE', 'CURRENCY_MISMATCH', 'AMOUNT_MISMATCH', 'GROUP_RELATIONSHIP_LOST',
        'LEGACY_UNSTRUCTURED')),
    -- The sentence mismatch_reason used to hold. NULL on a MATCHED attempt.
    message               TEXT,

    -- Per side: what the pair expected, and what general-ledger-svc reported
    -- for the journal examined. Expected amounts are the pair's own
    -- NUMERIC(18,4) values. Observed amounts are unconstrained NUMERIC: they
    -- are general-ledger-svc's figures, recorded as reported, never rounded
    -- to fit. Observed columns are NULL when that side was not examined
    -- (the source side is not, SPEC_DEVIATIONS A6) or the journal was not
    -- found.
    source_journal_id               TEXT          NOT NULL,
    expected_source_amount          NUMERIC(18,4) NOT NULL,
    expected_source_currency        VARCHAR(10)   NOT NULL,
    observed_source_amount          NUMERIC,
    observed_source_currency        TEXT,
    observed_source_legal_entity_id TEXT,
    observed_source_status          TEXT,
    observed_source_fiscal_period   TEXT,

    target_journal_id               TEXT,
    expected_target_amount          NUMERIC(18,4) NOT NULL,
    expected_target_currency        VARCHAR(10)   NOT NULL,
    observed_target_amount          NUMERIC,
    observed_target_currency        TEXT,
    observed_target_legal_entity_id TEXT,
    observed_target_status          TEXT,
    observed_target_fiscal_period   TEXT,

    examined_journal_ids  TEXT[]       NOT NULL,
    tolerance             NUMERIC      NOT NULL CHECK (tolerance >= 0),
    -- The pair's period at the time of the attempt.
    fiscal_period         VARCHAR(20),
    attempted_by          VARCHAR(255) NOT NULL,
    correlation_id        VARCHAR(255),
    attempted_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CHECK ((outcome = 'MATCHED') = (failed_rule IS NULL)),
    CHECK ((outcome = 'MATCHED') = (message IS NULL))
);
CREATE INDEX idx_intercompany_match_attempts_pair
    ON intercompany_match_attempts (tenant_id, intercompany_entry_id, attempt_id);

-- Backfill: a pair whose mismatch_reason is set gets one LEGACY_UNSTRUCTURED
-- attempt carrying that sentence, so mismatch_reason reads the same after
-- this migration as before. Only the last attempt's text survived the old
-- overwrite, so only it is recorded. Its time is unknown: attempted_at is
-- the migration's, and attempted_by 'migration:000007' marks it. Tolerance
-- is the fixed 0.001 the old match applied.
INSERT INTO intercompany_match_attempts (
    tenant_id, intercompany_entry_id, outcome, failed_rule, message,
    source_journal_id, expected_source_amount, expected_source_currency,
    target_journal_id, expected_target_amount, expected_target_currency,
    examined_journal_ids, tolerance, fiscal_period, attempted_by)
SELECT tenant_id, intercompany_entry_id, 'MISMATCH', 'LEGACY_UNSTRUCTURED', mismatch_reason,
       source_journal_id::text, amount, currency_code,
       target_journal_id::text, counterparty_amount, counterparty_currency_code,
       CASE WHEN target_journal_id IS NULL THEN '{}'::text[] ELSE ARRAY[target_journal_id::text] END,
       0.001, fiscal_period, 'migration:000007'
FROM intercompany_entries
WHERE mismatch_reason IS NOT NULL;

ALTER TABLE intercompany_match_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE intercompany_match_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY intercompany_match_attempts_tenant_isolation ON intercompany_match_attempts
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE TRIGGER trigger_match_attempts_immutable
    BEFORE UPDATE OR DELETE ON intercompany_match_attempts
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();
