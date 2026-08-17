-- 000003_add_dunning_and_transfers.up.sql
-- Chunk 8 — Zoiko One Billing & Double-Charge Prevention (doc7 §O, §P3).

-- billing_source on the subscription itself — doc7 §P2: "billing_source =
-- ZOIKO_ONE_BUNDLE with bundle/contract ref." Reuses the exact same
-- BillingSource vocabulary tenant-entity-registry-svc already defined for
-- workspaces (DIRECT/ZOIKO_ONE_BUNDLE/NONE) rather than inventing a second
-- one for subscriptions.
ALTER TABLE commercial_subscriptions
    ADD COLUMN billing_source VARCHAR(32) NOT NULL DEFAULT 'DIRECT';

-- billing_source_transfers: doc7 §P3 — standalone <-> Zoiko One migration
-- through an effective-dated commercial transfer record, never a silent
-- swap. old_subscription_id/new_subscription_id are both nullable because a
-- transfer can originate a subscription that didn't exist before (fresh
-- Zoiko One entitlement) or terminate one with no replacement.
CREATE TABLE billing_source_transfers (
    transfer_id                UUID PRIMARY KEY,
    commercial_account_id       UUID NOT NULL REFERENCES commercial_accounts(commercial_account_id),
    old_billing_source           VARCHAR(32),
    new_billing_source            VARCHAR(32) NOT NULL,
    old_subscription_id            UUID REFERENCES commercial_subscriptions(subscription_id),
    new_subscription_id             UUID REFERENCES commercial_subscriptions(subscription_id),
    entitlement_continuity            BOOLEAN NOT NULL DEFAULT TRUE,
    credit_amount                      NUMERIC(14,2),
    reconciliation_status                VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    created_at                            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id                VARCHAR(255) NOT NULL
);

CREATE INDEX idx_billing_transfers_account ON billing_source_transfers (commercial_account_id);

-- subscription_status_events: append-only dunning/status-transition audit
-- trail. Doc7 §O3: "restoration is logged" — this is the log. Separate from
-- commercial_subscriptions.status itself (current state) so the full
-- PAST_DUE -> RESTRICTED -> recovered history survives even though the
-- subscription row only ever shows current status.
CREATE TABLE subscription_status_events (
    status_event_id            UUID PRIMARY KEY,
    subscription_id              UUID NOT NULL REFERENCES commercial_subscriptions(subscription_id),
    previous_status                VARCHAR(32) NOT NULL,
    new_status                       VARCHAR(32) NOT NULL,
    reason                              TEXT,
    created_at                          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id               VARCHAR(255) NOT NULL
);

CREATE INDEX idx_status_events_subscription ON subscription_status_events (subscription_id, created_at DESC);
