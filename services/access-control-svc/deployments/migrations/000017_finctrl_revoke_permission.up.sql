-- Migration 000017: register FINCTRL_REVOKE (financial-control-svc
-- SPEC_DEVIATIONS A1/B6: a certification can now be revoked,
-- ZS-CONTROL-001 §7/§27). The command exists in financial-control-svc
-- already; without this row no role can hold the action and every
-- revoke request answers 403.
--
-- LEGACY naming and STANDARD risk tier, matching every sibling FINCTRL_*
-- action registered in 000008 — the service itself also refuses a
-- revoker who certified the run being revoked, whatever is granted here,
-- the same independence guard FINCTRL_CERTIFY's callers already rely on.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('FINCTRL_REVOKE', 'LEGACY', 'STANDARD', '')
ON CONFLICT (action_name) DO NOTHING;
