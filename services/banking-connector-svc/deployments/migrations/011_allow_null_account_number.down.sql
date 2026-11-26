-- Reversible only while nothing has been scrubbed yet: if any row's
-- account_number is NULL (cmd/scrub-legacy-raw already ran), this fails
-- rather than silently losing the "scrubbed" signal by rejecting the ALTER.
ALTER TABLE bank_connections ALTER COLUMN account_number SET NOT NULL;
