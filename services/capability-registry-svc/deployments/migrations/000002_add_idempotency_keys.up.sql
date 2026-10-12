-- 000002_add_idempotency_keys.up.sql
-- capability-registry-svc — INV-08 replay protection.
--
-- The envelope contract (internal/envelope/policy.go) has always required an
-- Idempotency-Key header on every write, but until now nothing in the store
-- layer ever used the header's value: it was validated present and then
-- discarded. A retried write (client timeout, double-click, at-least-once
-- redelivery) created a brand-new, duplicate resource instead of being
-- recognized as a replay.
--
-- Registry resources are platform-wide, but idempotency records remain scoped
-- by tenant as well as operation and principal so one tenant cannot replay
-- another tenant's response.
CREATE TABLE idempotency_keys (
    tenant_id       VARCHAR(255) NOT NULL,
    operation       VARCHAR(64)  NOT NULL,
    principal_id    VARCHAR(255) NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    request_sha256  VARCHAR(64)  NOT NULL,
    resource_id     TEXT         NOT NULL,
    response_status SMALLINT     NOT NULL,
    response_body   JSONB        NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, operation, principal_id, idempotency_key)
);
