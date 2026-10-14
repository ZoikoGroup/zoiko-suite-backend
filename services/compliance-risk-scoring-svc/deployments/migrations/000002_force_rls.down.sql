-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE risk_score_assessments NO FORCE ROW LEVEL SECURITY;
ALTER TABLE risk_factor_breakdowns NO FORCE ROW LEVEL SECURITY;
ALTER TABLE risk_threshold_rules NO FORCE ROW LEVEL SECURITY;