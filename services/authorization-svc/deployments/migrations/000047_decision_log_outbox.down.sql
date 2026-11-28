-- 000047 down: the decision-log outbox. Rows not yet delivered are lost with
-- it; deliver or export them before reverting.
DROP TABLE IF EXISTS decision_log_outbox;
