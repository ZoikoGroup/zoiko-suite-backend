DROP TRIGGER IF EXISTS high_risk_change_requests_decision_only ON high_risk_change_requests;
DROP FUNCTION IF EXISTS reject_decided_change_request_mutation();

DROP TRIGGER IF EXISTS profile_change_events_append_only ON profile_change_events;
DROP TRIGGER IF EXISTS payment_terms_periods_append_only ON payment_terms_periods;
DROP FUNCTION IF EXISTS reject_evidence_mutation();

DROP TRIGGER IF EXISTS supplier_financial_profiles_retired_immutable ON supplier_financial_profiles;
DROP FUNCTION IF EXISTS reject_retired_profile_mutation();
