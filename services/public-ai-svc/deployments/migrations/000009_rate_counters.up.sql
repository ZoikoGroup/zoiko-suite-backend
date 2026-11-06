-- Doc 06 §13: per-IP and per-session quotas hold across replicas. Each
-- replica keeps its in-memory token bucket (burst), and every allowed request
-- also counts in this shared fixed one-minute window; above the per-minute
-- limit the request is refused on every replica. Keys are HMACs with a
-- shared secret salt (PUBLIC_AI_RATE_KEY_SALT), never addresses or grants.
-- Windows older than a few minutes are swept by the service.
CREATE TABLE public_ai.rate_counters (
    counter_key  TEXT        NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    hits         INTEGER     NOT NULL,
    PRIMARY KEY (counter_key, window_start)
);
CREATE INDEX rate_counters_window ON public_ai.rate_counters (window_start);
