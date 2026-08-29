DROP TRIGGER IF EXISTS trg_reject_proposal_events_mutation ON proposal_events;
DROP TRIGGER IF EXISTS trg_reject_proposal_item_mutation ON proposal_items;
DROP TRIGGER IF EXISTS trg_reject_proposal_mutation ON payment_proposals;
DROP FUNCTION IF EXISTS reject_evidence_mutation();
DROP FUNCTION IF EXISTS reject_proposal_item_mutation();
DROP FUNCTION IF EXISTS reject_proposal_mutation();
