-- TAX-03 Tax Determination required business/source inputs
-- (ZS-ARCH-SVC-001 v2.0 §9.J).
--
-- The doc requires: "Seller/buyer; establishments; ship-from/to; supply
-- location/date; product/service classification; taxable amount; currency;
-- exemption facts; B2B/B2C facts." Taxable amount and currency were already
-- here. This migration adds the rest.
--
-- WHY THESE FIELDS DECIDE THE ANSWER
--
-- A tax determination is not "amount x rate". Which rate applies at all depends
-- on who is selling, who is buying, where each is established, where the goods
-- or services move, what is being supplied, and whether the buyer is a business
-- with its own registration. The service previously took a jurisdiction and a
-- category and applied a rate, which means the caller had already decided the
-- question this service exists to answer.
--
-- BACKFILL
--
-- Existing rows predate the input contract and cannot have these facts invented
-- for them. Text columns backfill to 'UNSPECIFIED' and are identifiable as
-- pre-contract by that value; the defaults are dropped immediately so no future
-- insert acquires one silently.

ALTER TABLE tax_determinations
    -- Seller / buyer. Party references, unvalidated: ORG-07 Party/Counterparty
    -- exists as counterparty-management-svc but owns counterparties, not the
    -- selling entity, and there is no single party master both sides resolve
    -- against. Recorded as supplied.
    ADD COLUMN IF NOT EXISTS seller_party_id TEXT NOT NULL DEFAULT 'UNSPECIFIED',
    ADD COLUMN IF NOT EXISTS buyer_party_id  TEXT NOT NULL DEFAULT 'UNSPECIFIED',

    -- Establishments. Nullable: ORG-08 Address & Establishment does not exist,
    -- so nothing can issue or validate an establishment id. Carried so a caller
    -- that tracks them elsewhere can preserve the link.
    ADD COLUMN IF NOT EXISTS seller_establishment_id TEXT,
    ADD COLUMN IF NOT EXISTS buyer_establishment_id  TEXT,

    -- Ship-from / ship-to, as jurisdiction references. Both validated against
    -- jurisdiction-rules-svc, which does exist. Nullable because a supply of
    -- services frequently has neither.
    ADD COLUMN IF NOT EXISTS ship_from_jurisdiction_id TEXT,
    ADD COLUMN IF NOT EXISTS ship_to_jurisdiction_id   TEXT,

    -- Supply location and date. supply_jurisdiction_id is the place of supply —
    -- the jurisdiction whose rules govern this transaction.
    --
    -- place_of_supply_basis records HOW that jurisdiction was arrived at, and
    -- today the only possible value is CALLER_ASSERTED. Place-of-supply rules
    -- are jurisdiction-pack data (TAX-02) and no pack carries them, so this
    -- service cannot derive the place of supply from establishments and supply
    -- kind the way §9.J describes. Recording the basis keeps that visible in the
    -- evidence instead of letting a caller-supplied jurisdiction read as a
    -- determination the engine made. When packs carry the rules, the basis
    -- becomes RULE_DERIVED and the disagreement between the two is auditable.
    ADD COLUMN IF NOT EXISTS supply_jurisdiction_id TEXT NOT NULL DEFAULT 'UNSPECIFIED',
    ADD COLUMN IF NOT EXISTS supply_date            DATE,
    ADD COLUMN IF NOT EXISTS place_of_supply_basis  TEXT NOT NULL DEFAULT 'CALLER_ASSERTED',

    -- Product / service classification. Free text: INV-01 Item/Product Master
    -- does not exist, and commodity-code schemes are jurisdiction data that
    -- belongs in a pack, not a column constraint.
    ADD COLUMN IF NOT EXISTS product_classification TEXT NOT NULL DEFAULT 'UNSPECIFIED',
    -- What is being supplied. Drives place-of-supply treatment everywhere that
    -- distinguishes goods from services from digital services.
    ADD COLUMN IF NOT EXISTS supply_kind TEXT NOT NULL DEFAULT 'UNSPECIFIED',

    -- B2B / B2C facts. buyer_tax_registration_id is the buyer's registration in
    -- the place of supply — its presence is what makes a cross-border B2B
    -- supply reverse-charge in most VAT systems.
    ADD COLUMN IF NOT EXISTS supply_type              TEXT NOT NULL DEFAULT 'UNSPECIFIED',
    ADD COLUMN IF NOT EXISTS buyer_tax_registration_id TEXT,

    -- Exemption facts. exempt_amount already existed and said only how much;
    -- these say why, and what substantiates it. INV-10: an exemption claimed
    -- with no reason is an assertion nobody can defend at audit.
    ADD COLUMN IF NOT EXISTS exemption_reason          TEXT,
    ADD COLUMN IF NOT EXISTS exemption_certificate_ref TEXT,

    -- Server-resolved (§9.J "Registrations"): the seller's tax registration in
    -- the place of supply, read from tenant-entity-registry-svc's tax identity
    -- bundles. Nullable — an unregistered seller is a real and common state, and
    -- is precisely the fact that decides whether tax is charged at all.
    ADD COLUMN IF NOT EXISTS seller_registration_id     TEXT,
    ADD COLUMN IF NOT EXISTS seller_registration_status TEXT;

ALTER TABLE tax_determinations
    ALTER COLUMN seller_party_id        DROP DEFAULT,
    ALTER COLUMN buyer_party_id         DROP DEFAULT,
    ALTER COLUMN supply_jurisdiction_id DROP DEFAULT,
    ALTER COLUMN product_classification DROP DEFAULT,
    ALTER COLUMN supply_kind            DROP DEFAULT,
    ALTER COLUMN supply_type            DROP DEFAULT;
-- place_of_supply_basis keeps its default: CALLER_ASSERTED is the correct value
-- for every row until place-of-supply rules exist, not a backfill marker.

CREATE INDEX IF NOT EXISTS idx_tax_determinations_supply
    ON tax_determinations (tenant_id, supply_jurisdiction_id, supply_date);

-- Returns are prepared per counterparty as often as per jurisdiction.
CREATE INDEX IF NOT EXISTS idx_tax_determinations_buyer
    ON tax_determinations (tenant_id, buyer_party_id);
