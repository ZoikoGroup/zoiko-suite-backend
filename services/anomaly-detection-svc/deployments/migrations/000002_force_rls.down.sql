-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE anomaly_detection_rules NO FORCE ROW LEVEL SECURITY;
ALTER TABLE anomaly_records NO FORCE ROW LEVEL SECURITY;