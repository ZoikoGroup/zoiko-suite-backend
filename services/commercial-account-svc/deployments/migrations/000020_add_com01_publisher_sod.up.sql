-- 000020_add_com01_publisher_sod.up.sql
--
-- COM-01 §4.1: "publisher cannot self-approve material price changes."
-- The existing price_versions_approver_is_independent constraint keeps the
-- approver away from the creator/submitter. This one closes the remaining
-- maker-checker edge: the principal that publishes a version may not also be
-- the principal that approved it. It applies to every published version, not
-- only a subset classified as "material", because this service has no
-- materiality classifier and must not invent one at the database boundary.

ALTER TABLE product_price_versions
    ADD CONSTRAINT price_versions_publisher_is_independent CHECK (
        published_by_principal_id IS NULL
        OR approved_by_principal_id IS NULL
        OR published_by_principal_id <> approved_by_principal_id
    );
