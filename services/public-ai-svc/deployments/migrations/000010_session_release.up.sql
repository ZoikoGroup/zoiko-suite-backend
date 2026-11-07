-- Doc 04 §16 PublicChatSession.release_id: the knowledge release active when
-- the session started, so a conversation can be traced to the content it
-- was answered from (each turn also records its own release).
ALTER TABLE public_ai.sessions ADD COLUMN release_id TEXT NOT NULL DEFAULT '';
