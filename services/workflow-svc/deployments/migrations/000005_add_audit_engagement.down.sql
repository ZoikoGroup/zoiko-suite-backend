DROP TRIGGER IF EXISTS trg_reject_audit_engagement_transition_mutation ON audit_engagement_transitions;
DROP FUNCTION IF EXISTS reject_audit_engagement_transition_mutation();
DROP TABLE IF EXISTS audit_engagement_transitions;
DROP TABLE IF EXISTS audit_engagements;
