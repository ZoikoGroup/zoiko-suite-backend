-- Vendor Due Diligence Service — record WHICH screening produced an outcome.
--
-- 000001 stored `risk_outcome` and a free-text `screening_basis` and nothing
-- else, which left CLEAR indistinguishable from a real sanctions clearance. The
-- only screening this service performs is an exact, case-insensitive match
-- against a hardcoded list of two names: there is no sanctions feed on this
-- platform to call (external-data-feed-svc carries MARKET_DATA, CREDIT_SCORE,
-- COMPANY_INFO, FX_RATE, and ESG_DATA only).
--
-- A consumer cannot avoid over-reading CLEAR unless the record says what ran, and
-- parsing it out of `screening_basis` prose is not a contract. This column is
-- that contract. When a real feed is integrated it becomes a second value here
-- and every historical row stays honest about having been screened by the stub.
ALTER TABLE vendor_dd_checks
    ADD COLUMN IF NOT EXISTS screening_source VARCHAR(50);

-- Rows written before this column existed were all screened by the stub — it is
-- the only implementation there has ever been — so backfilling is a statement of
-- fact rather than an assumption. Only concluded rows are touched: a STARTED row
-- never reached screening at all.
UPDATE vendor_dd_checks
SET screening_source = 'STUB_DENYLIST'
WHERE screening_source IS NULL
  AND status = 'COMPLETED';

-- `document_reference` shipped in 000001 and nothing ever wrote to it. The
-- create path had no field for it, so every evidence row stored the empty string:
-- a column meant to reference a supporting document held "" for every record, and
-- "no document" was indistinguishable from "a document whose reference is blank".
-- The write path now accepts one and stores NULL when absent; normalise the
-- existing rows to match so a single IS NULL test answers the question.
UPDATE vendor_dd_evidence
SET document_reference = NULL
WHERE document_reference = '';

-- Enforcement for the state machine the handler applies: a check may hold a risk
-- outcome only once it has concluded, and a concluded check must carry the
-- timestamp saying when. Without this, a partially-applied conclusion is
-- representable — and one was: a completion that failed midway left STARTED rows
-- that the register could not distinguish from anything else.
--
-- `risk_outcome IS NOT NULL` is not redundant beside the IN list. A CHECK
-- constraint rejects a row only when its expression evaluates to FALSE, and
-- `NULL IN ('CLEAR','FLAGGED')` evaluates to NULL — so with the IN test alone the
-- COMPLETED branch went NULL, the disjunction went `FALSE OR FALSE OR NULL` =
-- NULL, and a COMPLETED check carrying no outcome at all was accepted. Which is
-- exactly the state this constraint exists to forbid: a concluded check whose
-- conclusion is missing.
ALTER TABLE vendor_dd_checks
    ADD CONSTRAINT vendor_dd_checks_outcome_requires_conclusion
    CHECK (
        (status = 'STARTED'   AND risk_outcome IS NULL AND completed_at IS NULL)
     OR (status = 'FAILED'    AND risk_outcome IS NULL AND completed_at IS NOT NULL)
     OR (status = 'COMPLETED' AND risk_outcome IS NOT NULL
                              AND risk_outcome IN ('CLEAR', 'FLAGGED')
                              AND completed_at IS NOT NULL)
    );
