-- Doc 06 §17: idle expiry in addition to the fixed maximum, and §13:
-- per-session token total for staged throttling. Counts only, no content.
ALTER TABLE public_ai.sessions
    ADD COLUMN last_seen_at TIMESTAMPTZ,
    ADD COLUMN tokens_used  BIGINT NOT NULL DEFAULT 0,
    -- Doc 05 §08: the grant is bound to the origin that created it.
    ADD COLUMN origin       TEXT NOT NULL DEFAULT '';
