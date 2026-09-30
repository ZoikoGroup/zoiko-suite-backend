-- ZS-SVC-Y-001 NCD-04: Delivery Evidence, Bounce, Complaint & Suppression (§7).
--
-- Evidence is a ledger, not a status column. Each row is one normalized fact
-- with its source, its confidence and what it does NOT prove (§7.1). A
-- provider's later correction is a NEW row pointing at the one it corrects
-- (NP-59), never an UPDATE — "historical observations are not silently
-- rewritten" (§9.2).
CREATE TABLE IF NOT EXISTS ncd_delivery_evidence (
    evidence_id             UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    communication_id        UUID         NOT NULL REFERENCES ncd_communications (communication_id),
    attempt_id              UUID         REFERENCES ncd_attempts (attempt_id),
    notice_id               UUID,
    evidence_type           VARCHAR(40)  NOT NULL CHECK (evidence_type IN (
                                'PROVIDER_ACCEPTED','PROVIDER_REJECTED','PROVIDER_UNKNOWN',
                                'MAILBOX_ACCEPTED','BOUNCED','DEFERRED',
                                'NETWORK_DELIVERY_STATUS','PUSH_ACCEPTED','TOKEN_INVALID',
                                'IN_APP_DELIVERED','SHOWN','OPENED','ACTIONED',
                                'OPEN_SIGNAL','LINK_ACTION','COMPLAINT',
                                'ACKNOWLEDGED','DECLINED','DISPUTED',
                                'OPERATOR_EVIDENCE_ADDED','UNKNOWN_RESOLVED','CANCELLED',
                                'RECONCILIATION_EXCEPTION')),
    -- The state the fact supports, in the vocabulary of §2.3.
    normalized_state        VARCHAR(30)  NOT NULL,
    source                  VARCHAR(30)  NOT NULL CHECK (source IN (
                                'PROVIDER_API','PROVIDER_CALLBACK','IN_APP','HUMAN','OPERATOR','RECONCILIATION','SYSTEM')),
    confidence              VARCHAR(10)  NOT NULL CHECK (confidence IN ('HIGH','MEDIUM','LOW')),
    -- §7.1 "Confidence/limits": stated on the fact so no reader has to
    -- remember that a provider acceptance is not a delivery.
    does_not_prove          TEXT         NOT NULL,
    binding_id              VARCHAR(64),
    provider_event_id       VARCHAR(255),
    payload_hash            VARCHAR(64),
    observed_at             TIMESTAMPTZ  NOT NULL,
    received_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    supersedes_evidence_id  UUID REFERENCES ncd_delivery_evidence (evidence_id),
    actor_principal_id      VARCHAR(255),
    details                 JSONB        NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_ncd_evidence_comm ON ncd_delivery_evidence (tenant_id, communication_id, received_at);
CREATE INDEX IF NOT EXISTS idx_ncd_evidence_reputation ON ncd_delivery_evidence (tenant_id, binding_id, evidence_type, received_at);
-- NP-24: a duplicated provider callback is deduplicated by provider event id,
-- within tenant/provider scope (§7.5). Tenant is part of the key: without it
-- an event id seen in one tenant silently swallowed the same id in another —
-- reported DUPLICATE against a row the caller cannot even see under RLS.
DROP INDEX IF EXISTS idx_ncd_evidence_provider_event;
CREATE UNIQUE INDEX IF NOT EXISTS idx_ncd_evidence_provider_event ON ncd_delivery_evidence (tenant_id, binding_id, provider_event_id)
    WHERE provider_event_id IS NOT NULL;

DROP TRIGGER IF EXISTS trg_ncd_evidence_append_only ON ncd_delivery_evidence;
CREATE TRIGGER trg_ncd_evidence_append_only BEFORE UPDATE OR DELETE ON ncd_delivery_evidence
    FOR EACH ROW EXECUTE FUNCTION ncd_reject_evidence_mutation();

ALTER TABLE ncd_delivery_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE ncd_delivery_evidence FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ncd_delivery_evidence_tenant ON ncd_delivery_evidence;
CREATE POLICY ncd_delivery_evidence_tenant ON ncd_delivery_evidence FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
-- Reputation is computed per binding across tenants by the worker (§7.4);
-- the SELECT-only hatch lets it count without reading any tenant's content.
DROP POLICY IF EXISTS ncd_delivery_evidence_platform_read ON ncd_delivery_evidence;
CREATE POLICY ncd_delivery_evidence_platform_read ON ncd_delivery_evidence FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.platform_scope', true), ''), 'false') = 'true');
