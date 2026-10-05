-- 000018: the delegation projection keeps the upstream version and the
-- delegation's own ceiling (delegated-authority-svc, ORG-06).
--
-- source_version: events about one grant arrive in order per partition, but a
-- replay (consumer restart, redelivery) can bring an old authority.delegated
-- back after the grant was suspended or revoked, and the upsert would have
-- re-activated it. A projection change now applies only if it is not older
-- than what is stored.
--
-- delegation_limit_*: the producer has published authority_limit_* since
-- 28 Sep and this consumer dropped them, so a delegation capped at 500.00
-- conferred uncapped authority at decision time.
ALTER TABLE delegated_authorities
    ADD COLUMN IF NOT EXISTS source_version            BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS delegation_limit_minor    BIGINT,
    ADD COLUMN IF NOT EXISTS delegation_limit_currency VARCHAR(3),
    ADD COLUMN IF NOT EXISTS delegation_limit_quantity BIGINT;
