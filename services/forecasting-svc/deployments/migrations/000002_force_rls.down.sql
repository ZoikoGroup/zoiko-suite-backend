-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE forecast_models NO FORCE ROW LEVEL SECURITY;
ALTER TABLE forecast_projections NO FORCE ROW LEVEL SECURITY;