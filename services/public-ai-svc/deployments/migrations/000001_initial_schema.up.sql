-- public-ai-svc (Suivo R0) schema. Dedicated schema + role; no other
-- service's tables are referenced (INV-01, ADR-001).
-- No table stores raw visitor messages, prompts or model output text other
-- than the validated public response envelope (Doc 05 §24, Doc 06 §21).

CREATE SCHEMA IF NOT EXISTS public_ai;

CREATE TABLE public_ai.sessions (
    session_id        TEXT PRIMARY KEY,
    grant_hash        BYTEA NOT NULL UNIQUE,
    created_at        TIMESTAMPTZ NOT NULL,
    expires_at        TIMESTAMPTZ NOT NULL,
    locale            TEXT NOT NULL DEFAULT '',
    market_code       TEXT NOT NULL DEFAULT '',
    page_context      TEXT NOT NULL DEFAULT '',
    turn_seq          INTEGER NOT NULL DEFAULT 0,
    active_turn_id    TEXT,
    active_until      TIMESTAMPTZ,
    deleted_at        TIMESTAMPTZ
);
CREATE INDEX sessions_expires_idx ON public_ai.sessions (expires_at);

-- Idempotency for POST /sessions. Stores only a fingerprint and a result
-- reference (Doc 06 §17), never the grant.
CREATE TABLE public_ai.session_idempotency (
    idempotency_key   TEXT PRIMARY KEY,
    request_hash      BYTEA NOT NULL,
    session_id        TEXT NOT NULL REFERENCES public_ai.sessions (session_id) ON DELETE CASCADE,
    created_at        TIMESTAMPTZ NOT NULL
);

CREATE TABLE public_ai.turns (
    turn_id           TEXT PRIMARY KEY,
    session_id        TEXT NOT NULL REFERENCES public_ai.sessions (session_id) ON DELETE CASCADE,
    client_turn_id    TEXT NOT NULL,
    request_hash      BYTEA NOT NULL,
    turn_sequence     INTEGER NOT NULL,
    status            TEXT NOT NULL DEFAULT '',
    intent_id         TEXT NOT NULL DEFAULT '',
    -- Exact bytes of the validated envelope so idempotent replay is byte-identical.
    response          BYTEA,
    completed         BOOLEAN NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL,
    UNIQUE (session_id, client_turn_id)
);

-- Signed, immutable release artifacts (Doc 03 §11, Doc 08 §06).
-- kind: 'kb' (PUBLIC_APPROVED knowledge + routes) or 'claims' (P1 registry).
CREATE TABLE public_ai.releases (
    release_id        TEXT PRIMARY KEY,
    kind              TEXT NOT NULL CHECK (kind IN ('kb', 'claims')),
    manifest          BYTEA NOT NULL,
    signature         BYTEA NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL
);

-- Atomic pointer swap: promotion only changes this row (Doc 03 §11).
CREATE TABLE public_ai.release_pointer (
    kind              TEXT PRIMARY KEY CHECK (kind IN ('kb', 'claims')),
    release_id        TEXT NOT NULL REFERENCES public_ai.releases (release_id),
    updated_at        TIMESTAMPTZ NOT NULL
);

-- Monotonic revocation ledger. The watermark only ever grows; rollback of a
-- release pointer never removes a row here (Doc 08 RB-02).
CREATE TABLE public_ai.revocations (
    watermark         BIGSERIAL PRIMARY KEY,
    target_kind       TEXT NOT NULL CHECK (target_kind IN ('source', 'claim')),
    target_id         TEXT NOT NULL,
    reason_code       TEXT NOT NULL,
    effective_at      TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE FUNCTION public_ai.reject_revocation_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'revocations are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER revocations_append_only
    BEFORE UPDATE OR DELETE ON public_ai.revocations
    FOR EACH ROW EXECUTE FUNCTION public_ai.reject_revocation_change();

CREATE TABLE public_ai.conflicts (
    conflict_id       TEXT PRIMARY KEY,
    subject_key       TEXT NOT NULL,
    claim_ids         TEXT[] NOT NULL,
    detection_type    TEXT NOT NULL,
    severity          TEXT NOT NULL,
    resolution_state  TEXT NOT NULL DEFAULT 'OPEN',
    detected_at       TIMESTAMPTZ NOT NULL
);

-- Feature flags and kill switch (Doc 04 §20, Doc 08 §26).
CREATE TABLE public_ai.flags (
    name              TEXT PRIMARY KEY,
    value             JSONB NOT NULL,
    actor_ref         TEXT NOT NULL,
    approval_ref      TEXT NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

-- Aggregate token usage for spend ceilings (Doc 06 §13, Doc 08 §25).
CREATE TABLE public_ai.usage_counters (
    bucket            TEXT PRIMARY KEY,
    tokens            BIGINT NOT NULL DEFAULT 0
);

-- Transactional outbox (Doc 05 §20).
CREATE TABLE public_ai.outbox (
    outbox_id         BIGSERIAL PRIMARY KEY,
    event_id          TEXT NOT NULL UNIQUE,
    topic             TEXT NOT NULL,
    event_type        TEXT NOT NULL,
    payload           BYTEA NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    published_at      TIMESTAMPTZ,
    attempts          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX outbox_unpublished_idx ON public_ai.outbox (outbox_id) WHERE published_at IS NULL;
