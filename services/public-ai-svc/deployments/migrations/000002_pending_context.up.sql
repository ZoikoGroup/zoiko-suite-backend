-- Conversation carry-over after a clarifying question (Doc 02 section 10).
-- Holds intent and slot values only, never visitor text; wiped on DELETE.
ALTER TABLE public_ai.sessions ADD COLUMN pending_context BYTEA;
