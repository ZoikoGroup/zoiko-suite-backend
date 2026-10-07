-- 000023_recipient_preferences.up.sql
-- ZS-SVC-Y-001 NCD-02 section 5.3 (preference versus permission), INV-23, NP-19, NP-20.
--
-- A recipient's convenience preferences: which channels they have muted, and the hours
-- in which routine messages should wait. A preference is a SEPARATE state dimension from
-- privacy permission and from mandatory notices: it can delay or mute a routine message,
-- it can never create permission to send, and it never touches a security or
-- transactional notice (that bypass is enforced in the evaluator, not here).
--
-- Quiet hours are civil time in the recipient's own IANA time zone (INV-23): the zone is
-- stored, never an offset, so daylight saving is handled by the zone database. The zone is
-- mandatory, because it must never be guessed (NP-20).

CREATE TABLE IF NOT EXISTS recipient_preferences (
    tenant_id          VARCHAR(255) NOT NULL,
    principal_id       VARCHAR(255) NOT NULL,
    time_zone          VARCHAR(64)  NOT NULL,
    quiet_start        TIME,
    quiet_end          TIME,
    muted_channels     JSONB        NOT NULL DEFAULT '[]'::jsonb,
    version            INTEGER      NOT NULL DEFAULT 1,
    updated_by         VARCHAR(255) NOT NULL,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, principal_id),
    CONSTRAINT ck_pref_quiet_pair CHECK ((quiet_start IS NULL) = (quiet_end IS NULL)),
    CONSTRAINT ck_pref_quiet_window CHECK (quiet_start IS NULL OR quiet_start <> quiet_end),
    CONSTRAINT ck_pref_tz_shape CHECK (time_zone ~ '^[A-Za-z][A-Za-z0-9_+./-]{0,63}$'),
    CONSTRAINT ck_pref_muted_is_array CHECK (jsonb_typeof(muted_channels) = 'array')
);

ALTER TABLE recipient_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE recipient_preferences FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS pref_tenant_isolation ON recipient_preferences;
CREATE POLICY pref_tenant_isolation ON recipient_preferences FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
