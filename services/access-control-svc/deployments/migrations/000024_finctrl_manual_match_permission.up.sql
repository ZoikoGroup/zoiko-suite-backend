-- Migration 000024: register FINCTRL_MANUAL_MATCH (financial-control-svc
-- SPEC_DEVIATIONS A5/000011: a human can now pair two records left unmatched
-- by automatic matching, with permission, reason and evidence. Without this
-- row no role can hold the action and every manual-match request answers 403.
--
-- LEGACY naming and STANDARD risk tier, matching every sibling FINCTRL_*
-- action (000008).

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('FINCTRL_MANUAL_MATCH', 'LEGACY', 'STANDARD', '')
ON CONFLICT (action_name) DO NOTHING;
