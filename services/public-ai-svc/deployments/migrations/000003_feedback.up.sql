-- Doc 01 FR-09 / S09: per-answer feedback. Rating and reason are enums;
-- no free text is stored.
ALTER TABLE public_ai.turns ADD COLUMN feedback_rating TEXT;
ALTER TABLE public_ai.turns ADD COLUMN feedback_reason TEXT;
ALTER TABLE public_ai.turns ADD COLUMN feedback_at TIMESTAMPTZ;
