ALTER TABLE public_ai.sessions
    DROP COLUMN IF EXISTS origin,
    DROP COLUMN IF EXISTS tokens_used,
    DROP COLUMN IF EXISTS last_seen_at;
