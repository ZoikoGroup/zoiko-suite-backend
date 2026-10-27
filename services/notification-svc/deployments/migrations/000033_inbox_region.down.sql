-- Dropping the column loses the region pin on existing receipts.
ALTER TABLE eventing_inbox DROP COLUMN IF EXISTS region;
