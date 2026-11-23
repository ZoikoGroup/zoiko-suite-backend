-- Reversible only while nothing has been scrubbed yet: if any row's
-- account_identifier is NULL (cmd/scrub-legacy-raw already ran), this fails
-- rather than silently losing the "scrubbed" signal by rejecting the ALTER.
ALTER TABLE payee_destinations ALTER COLUMN account_identifier SET NOT NULL;
