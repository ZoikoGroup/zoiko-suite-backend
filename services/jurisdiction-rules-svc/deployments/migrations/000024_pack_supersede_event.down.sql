-- 000024 down: restores the 000009 action list for NEW rows. Release events are append-only, so SUPERSEDE rows already written cannot be
-- rewritten or removed; NOT VALID keeps them while refusing new ones. A version already SUPERSEDED stays so.
ALTER TABLE pack_release_events
    DROP CONSTRAINT IF EXISTS ck_release_action,
    DROP CONSTRAINT IF EXISTS ck_release_reason_required,
    ADD CONSTRAINT ck_release_action CHECK (action IN ('RELEASE', 'WITHDRAW', 'BLOCK', 'UNBLOCK')) NOT VALID,
    ADD CONSTRAINT ck_release_reason_required CHECK (action NOT IN ('WITHDRAW', 'BLOCK') OR length(btrim(coalesce(reason, ''))) > 0);
