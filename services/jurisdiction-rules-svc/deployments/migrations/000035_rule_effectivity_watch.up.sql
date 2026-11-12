-- 000035_rule_effectivity_watch.up.sql
--
-- ZS-JUR-001 s26 lists regulatory-rule.effective and .expired: a rule in force
-- reaching the start or the end of its effective interval. Those instants are
-- not writes, so nothing announced them. A watcher scans for rules whose
-- effective_from or effective_to fell in (scanned_through, now] and moves the
-- watermark in the same transaction as the outbox rows it writes, so each
-- crossing is announced once, by one replica, and none is skipped after a
-- restart. The first scan starts the watermark at that moment: crossings
-- before the watcher existed are history, not news.

CREATE TABLE IF NOT EXISTS rule_effectivity_watch (
    watch_id         SMALLINT     PRIMARY KEY DEFAULT 1 CHECK (watch_id = 1),
    scanned_through  TIMESTAMPTZ  NOT NULL,
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE ON rule_effectivity_watch TO app_jurisdiction_rules;
    END IF;
END $$;
