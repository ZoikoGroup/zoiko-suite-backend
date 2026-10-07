-- 000013_add_com05_payment_attempts.down.sql
DROP TABLE IF EXISTS payment_attempts;
DROP FUNCTION IF EXISTS enforce_payment_attempt_lifecycle();
