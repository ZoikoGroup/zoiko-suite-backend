-- Recipient address, delivery evidence, and in-app read state.
--
-- Three gaps closed together because they are one gap: this register recorded
-- that a notification was SENT without recording where it went, what took it,
-- or whether the person ever saw it. Every one of those is the question asked
-- when somebody says they were not notified.

-- â”€â”€ Where it went â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
--
-- The register stored recipient_principal_id and no address. There was
-- therefore no address anywhere in the system at send time â€” the delivery
-- adapter was a stub that logged and claimed success, so nothing ever needed
-- one. With a real provider behind EMAIL, the address is resolved from
-- identity-context-svc and snapshotted here.
--
-- Snapshotted, not referenced: resolving it again at read time answers "where
-- would we send this today", and the question a delivery register exists to
-- answer is "where did we actually send it". Those differ precisely when it
-- matters, which is after somebody's address has changed.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS recipient_address TEXT;

-- ZS-SVC-Y-001 Â§0.4 names "mandatory notices being sent to an unverified or
-- stale free-text address with no recipient provenance" as a thing this
-- control plane exists to prevent. Provenance is only a control if it is
-- recorded, so an address resolved from the identity authority and one handed
-- over by a calling service are distinguishable after the fact.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS recipient_address_source VARCHAR(32);

-- â”€â”€ What took it â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
--
-- Acceptance evidence: which provider accepted the message and under what
-- identifier. Deliberately NOT named delivered_at or receipt â€” Â§0.4 forbids
-- treating a provider's "accepted" as proof that a person received, read or
-- was legally served with a notice, and a column called provider_response
-- cannot be misread as the stronger claim.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS provider_response TEXT;

-- â”€â”€ Whether it was seen â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
--
-- IN_APP notices are delivered by existing in this table; the recipient reads
-- them back from it. Until now nothing recorded that they had, so no unread
-- count was expressible and every notice stayed new forever.
--
-- NULL means unread. Set once, on the recipient's own assertion â€” the store
-- writes COALESCE(read_at, now) so re-opening an inbox cannot move a first
-- read forward into a last look.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS read_at TIMESTAMP WITH TIME ZONE;

-- â”€â”€ Constraints â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
--
-- NOT VALID throughout, for the reason 000002 sets out at length: it enforces
-- every INSERT and UPDATE from here on and skips the scan that would reject
-- the table over rows already written. Those rows are the audit trail of what
-- this service actually did, and a migration that quietly rewrites them is
-- worth less than one that leaves an awkward row visible. Run
-- ALTER TABLE ... VALIDATE CONSTRAINT once the backlog is known clean.

-- Each check is scoped by conrelid, not by conname alone. Constraint names are
-- unique per table, not per database, so a bare name match can find somebody
-- else's constraint and silently skip adding ours -- leaving the column
-- unconstrained with the migration reporting success.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'notifications_address_source_known'
           AND conrelid = 'notifications'::regclass
    ) THEN
        ALTER TABLE notifications
            ADD CONSTRAINT notifications_address_source_known
            CHECK (recipient_address_source IS NULL
                   OR recipient_address_source IN ('IDENTITY_CONTEXT', 'REQUEST')) NOT VALID;
    END IF;

    -- An address with no provenance is the state the spec forbids, and it is
    -- reachable by a bug rather than by a caller: the two columns are written
    -- by the same code path, so one without the other means that path is
    -- wrong. Cheaper to be told by the database than to discover it in a
    -- dispute about which address a statutory notice went to.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'notifications_address_has_provenance'
           AND conrelid = 'notifications'::regclass
    ) THEN
        ALTER TABLE notifications
            ADD CONSTRAINT notifications_address_has_provenance
            CHECK ((recipient_address IS NULL) = (recipient_address_source IS NULL)) NOT VALID;
    END IF;

    -- Read state belongs to IN_APP alone. This service cannot observe whether
    -- an email was opened, and a read_at on an EMAIL row would be an assertion
    -- it has no way to make -- the same overstatement as calling provider
    -- acceptance a delivery.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'notifications_read_state_is_in_app'
           AND conrelid = 'notifications'::regclass
    ) THEN
        ALTER TABLE notifications
            ADD CONSTRAINT notifications_read_state_is_in_app
            CHECK (read_at IS NULL OR channel = 'IN_APP') NOT VALID;
    END IF;

    -- A notice cannot have been read before it was recorded.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'notifications_read_after_created'
           AND conrelid = 'notifications'::regclass
    ) THEN
        ALTER TABLE notifications
            ADD CONSTRAINT notifications_read_after_created
            CHECK (read_at IS NULL OR read_at >= created_at) NOT VALID;
    END IF;
END $$;

-- â”€â”€ Index â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
--
-- The unread badge is polled by every signed-in session, which makes it the
-- most frequent read this service serves. Partial, because it only ever
-- answers this one question and the qualifying rows are a small and shrinking
-- fraction of the register -- an index over every notification would be
-- mostly rows that can never match.
CREATE INDEX IF NOT EXISTS idx_notifications_unread
    ON notifications (tenant_id, recipient_principal_id)
    WHERE read_at IS NULL AND channel = 'IN_APP';
