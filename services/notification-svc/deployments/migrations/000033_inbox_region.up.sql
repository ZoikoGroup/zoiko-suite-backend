-- INT-NP-045: pin consumer receipts to a region so repair cannot cross residency boundaries.
ALTER TABLE eventing_inbox ADD COLUMN IF NOT EXISTS region TEXT;
