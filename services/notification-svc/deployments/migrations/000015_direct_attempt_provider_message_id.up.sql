-- 000015_direct_attempt_provider_message_id.up.sql
-- ZS-SVC-Y-001 Wave 1 (audit finding F-12; sections 7.5, NP-24, NP-27).
--
-- A provider callback (bounce, complaint, delivery) names a message by its
-- provider message id. The webhook lookup searched only the ledger's
-- delivery_attempts, and the direct send path recorded the id nowhere a lookup
-- could find it, so a hard bounce or complaint for a direct send never matched
-- its notification and never created a suppression.
--
-- The id is stored in its own column, extracted from the transport receipt when
-- the attempt is ACCEPTED. It is NOT backfilled from provider_response: that text
-- is the transport's free-form sentence, and parsing history to invent an id would
-- be a guess presented as evidence. Rows written before this migration therefore
-- keep a NULL id and remain unmatchable, which is the truthful state.
--
-- UNIQUE: "every provider message id must map to exactly one canonical attempt"
-- (section 7.5, NP-27). A second attempt claiming an existing id is refused
-- rather than merged silently. The SMTP transport mints a fresh UUID Message-ID
-- per submission, so a legitimate send cannot collide.

ALTER TABLE notification_delivery_attempts
    ADD COLUMN IF NOT EXISTS provider_message_id VARCHAR(255);

CREATE UNIQUE INDEX IF NOT EXISTS uq_nda_provider_message_id
    ON notification_delivery_attempts (provider_message_id)
    WHERE provider_message_id IS NOT NULL;
