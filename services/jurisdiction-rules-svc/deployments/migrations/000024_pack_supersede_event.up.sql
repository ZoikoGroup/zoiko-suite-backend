-- 000024_pack_supersede_event.up.sql
-- ZS-JUR-001 s23: a RELEASED version can be marked SUPERSEDED by a newer one ("newer version exists; historical resolution still
-- permitted"). The status edge RELEASED -> SUPERSEDED has existed since 000005 but nothing could take it. Taking it is recorded as a
-- release event like every other lifecycle move, so the event action list gains SUPERSEDE, and (like WITHDRAW and BLOCK) it must say why.

ALTER TABLE pack_release_events
    DROP CONSTRAINT ck_release_action,
    DROP CONSTRAINT ck_release_reason_required,
    ADD CONSTRAINT ck_release_action CHECK (action IN ('RELEASE', 'WITHDRAW', 'BLOCK', 'UNBLOCK', 'SUPERSEDE')),
    ADD CONSTRAINT ck_release_reason_required CHECK (action NOT IN ('WITHDRAW', 'BLOCK', 'SUPERSEDE') OR length(btrim(coalesce(reason, ''))) > 0);
