DROP TRIGGER IF EXISTS trg_reject_expense_claim_events_mutation ON expense_claim_events;
DROP TRIGGER IF EXISTS trg_reject_expense_line_mutation ON expense_lines;
DROP TRIGGER IF EXISTS trg_reject_expense_claim_mutation ON expense_claims;
DROP FUNCTION IF EXISTS reject_evidence_mutation();
DROP FUNCTION IF EXISTS reject_expense_line_mutation();
DROP FUNCTION IF EXISTS reject_expense_claim_mutation();
