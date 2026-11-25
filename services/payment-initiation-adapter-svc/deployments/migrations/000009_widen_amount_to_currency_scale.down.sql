-- Reverses 000009. Narrowing back to NUMERIC(18,2) rounds any row with a
-- 3rd or 4th decimal place (a BHD-style amount) -- only safe to run before
-- any such row exists.
ALTER TABLE payment_initiation_attempts
    ALTER COLUMN amount TYPE NUMERIC(18, 2);
