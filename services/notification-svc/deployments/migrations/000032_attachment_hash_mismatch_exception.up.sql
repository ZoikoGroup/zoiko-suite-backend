-- Migration 000032: NP-34's provider attachment hash mismatch is a durable
-- incident, not a generic delivery failure. The exception kind is explicit so
-- operators can find and reconcile the exact package that changed.
ALTER TABLE ncd_exceptions DROP CONSTRAINT IF EXISTS ncd_exceptions_kind_check;
ALTER TABLE ncd_exceptions ADD CONSTRAINT ncd_exceptions_kind_check
    CHECK (kind IN (
        'UNKNOWN_UNRESOLVED','PROVIDER_ID_COLLISION','MISSING_CALLBACK',
        'FALLBACK_BLOCKED','NO_ROUTE','DELIVERY_EXPIRED','DEADLINE_AT_RISK',
        'ACK_EXPIRED','RECORD_HANDOFF_PENDING','STREAM_PAUSED','CALLBACK_REJECTED',
        'ENDPOINT_REMEDIATION','MISDELIVERY_INCIDENT','REPUTATION_ALERT',
        'ATTACHMENT_HASH_MISMATCH'));

ALTER TABLE ncd_delivery_evidence DROP CONSTRAINT IF EXISTS ncd_delivery_evidence_evidence_type_check;
ALTER TABLE ncd_delivery_evidence ADD CONSTRAINT ncd_delivery_evidence_evidence_type_check
    CHECK (evidence_type IN (
        'PROVIDER_ACCEPTED','PROVIDER_REJECTED','PROVIDER_UNKNOWN',
        'MAILBOX_ACCEPTED','BOUNCED','DEFERRED',
        'NETWORK_DELIVERY_STATUS','PUSH_ACCEPTED','TOKEN_INVALID',
        'IN_APP_DELIVERED','SHOWN','OPENED','ACTIONED',
        'OPEN_SIGNAL','LINK_ACTION','COMPLAINT',
        'ACKNOWLEDGED','DECLINED','DISPUTED',
        'OPERATOR_EVIDENCE_ADDED','UNKNOWN_RESOLVED','CANCELLED',
        'RECONCILIATION_EXCEPTION','ATTACHMENT_MISMATCH'));
