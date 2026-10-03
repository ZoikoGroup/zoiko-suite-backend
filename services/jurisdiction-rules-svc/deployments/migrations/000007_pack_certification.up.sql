-- 000007_pack_certification.up.sql
-- ZS-JUR-001 Wave 3: test bundles, test runs, independent reviews and the
-- signed certification record that moves a pack version REVIEW -> CERTIFIED.
-- Additive; 000005 and 000006 are untouched. Everything here is append-only:
-- certification evidence is never edited or deleted (s3, s22, s28).

CREATE FUNCTION jur_forbid_update() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: rows are never updated (ZS-JUR-001)', TG_TABLE_NAME USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;

-- ── test bundles (revisions are appended; the latest is the current suite) ──
CREATE TABLE pack_test_bundles (
    bundle_id       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_version_id UUID         NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    revision        INTEGER      NOT NULL,
    bundle          TEXT         NOT NULL,
    bundle_digest   VARCHAR(71)  NOT NULL,
    submitted_by    TEXT         NOT NULL,
    submitted_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_pack_test_bundle_revision UNIQUE (pack_version_id, revision),
    CONSTRAINT ck_pack_test_bundle_digest CHECK (bundle_digest ~ '^sha256:[0-9a-f]{64}$')
);

-- ── test runs ───────────────────────────────────────────────────────────────
CREATE TABLE pack_test_runs (
    run_id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_version_id UUID         NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    bundle_id       UUID         NOT NULL REFERENCES pack_test_bundles(bundle_id),
    bundle_digest   VARCHAR(71)  NOT NULL,
    artifact_digest VARCHAR(71)  NOT NULL,
    passed          BOOLEAN      NOT NULL,
    result          JSONB        NOT NULL,
    harness_version VARCHAR(32)  NOT NULL,
    executed_by     TEXT         NOT NULL,
    executed_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_pack_test_runs_version ON pack_test_runs (pack_version_id, executed_at DESC);

-- ── independent reviews (s20: tax / legal / accounting / technical) ─────────
CREATE TABLE pack_reviews (
    review_id       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_version_id UUID         NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    artifact_digest VARCHAR(71)  NOT NULL,
    reviewer        TEXT         NOT NULL,
    role            VARCHAR(64)  NOT NULL,
    decision        VARCHAR(16)  NOT NULL,
    findings        TEXT         NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_pack_review_decision CHECK (decision IN ('APPROVE', 'REJECT')),
    CONSTRAINT ck_pack_review_reject_needs_findings CHECK (decision <> 'REJECT' OR length(btrim(findings)) > 0)
);
CREATE INDEX idx_pack_reviews_version ON pack_reviews (pack_version_id, created_at);

-- ── certification (the signed release-authorizing record) ───────────────────
CREATE TABLE pack_certifications (
    certification_id  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_version_id   UUID         NOT NULL UNIQUE REFERENCES jurisdiction_pack_versions(pack_version_id),
    artifact_digest   VARCHAR(71)  NOT NULL,
    bundle_digest     VARCHAR(71)  NOT NULL,
    test_run_id       UUID         NOT NULL REFERENCES pack_test_runs(run_id),
    min_reviews       INTEGER      NOT NULL,
    report            JSONB        NOT NULL,
    report_digest     VARCHAR(71)  NOT NULL,
    signature         TEXT         NOT NULL,
    signature_key_ref VARCHAR(128) NOT NULL REFERENCES pack_signing_keys(key_ref),
    certified_by      TEXT         NOT NULL,
    certified_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_pack_cert_min_reviews CHECK (min_reviews >= 1)
);

-- ── guards ──────────────────────────────────────────────────────────────────
CREATE TRIGGER trg_pack_test_bundles_append_only BEFORE UPDATE ON pack_test_bundles FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_pack_test_bundles_no_delete   BEFORE DELETE ON pack_test_bundles FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_pack_test_runs_append_only    BEFORE UPDATE ON pack_test_runs    FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_pack_test_runs_no_delete      BEFORE DELETE ON pack_test_runs    FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_pack_reviews_append_only      BEFORE UPDATE ON pack_reviews      FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_pack_reviews_no_delete        BEFORE DELETE ON pack_reviews      FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
CREATE TRIGGER trg_pack_certs_append_only        BEFORE UPDATE ON pack_certifications FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_pack_certs_no_delete          BEFORE DELETE ON pack_certifications FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

-- Tests, runs and reviews only exist while the version is under REVIEW.
CREATE FUNCTION jur_cert_evidence_guard() RETURNS trigger AS $$
BEGIN
    IF (SELECT status FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id) <> 'REVIEW' THEN
        RAISE EXCEPTION '% can only be recorded while the pack version is under REVIEW', TG_TABLE_NAME USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_test_bundles_under_review BEFORE INSERT ON pack_test_bundles FOR EACH ROW EXECUTE FUNCTION jur_cert_evidence_guard();
CREATE TRIGGER trg_pack_test_runs_under_review    BEFORE INSERT ON pack_test_runs    FOR EACH ROW EXECUTE FUNCTION jur_cert_evidence_guard();

-- A reviewer is independent of everyone who built the thing under review:
-- the version's author, the compiler, and every test author (s20, s28).
CREATE FUNCTION jur_review_guard() RETURNS trigger AS $$
BEGIN
    IF (SELECT status FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id) <> 'REVIEW' THEN
        RAISE EXCEPTION 'reviews can only be recorded while the pack version is under REVIEW' USING ERRCODE = '23514';
    END IF;
    IF NEW.reviewer = (SELECT created_by_principal_id FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id)
       OR NEW.reviewer IN (SELECT compiled_by_principal_id FROM pack_artifacts WHERE pack_version_id = NEW.pack_version_id)
       OR NEW.reviewer IN (SELECT submitted_by FROM pack_test_bundles WHERE pack_version_id = NEW.pack_version_id) THEN
        RAISE EXCEPTION 'a reviewer must be independent of the version author, the compiler and the test authors'
            USING ERRCODE = '23514', CONSTRAINT = 'ck_pack_review_independent';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_reviews_guard BEFORE INSERT ON pack_reviews FOR EACH ROW EXECUTE FUNCTION jur_review_guard();

-- A certification can only be written when every gate holds, whatever code
-- tries to write it: signed artifact with the certified digest, a PASSED run of
-- exactly that artifact and bundle, and an independent certifier who is not a
-- reviewer of the version.
CREATE FUNCTION jur_certification_guard() RETURNS trigger AS $$
DECLARE
    a_signature TEXT;
    a_digest    TEXT;
    a_compiler  TEXT;
    r_passed    BOOLEAN;
    r_bundle    TEXT;
    r_artifact  TEXT;
BEGIN
    IF (SELECT status FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id) <> 'REVIEW' THEN
        RAISE EXCEPTION 'only a version under REVIEW can be certified' USING ERRCODE = '23514';
    END IF;
    SELECT signature, artifact_digest, compiled_by_principal_id INTO a_signature, a_digest, a_compiler
        FROM pack_artifacts WHERE pack_version_id = NEW.pack_version_id;
    IF a_digest IS NULL OR a_signature IS NULL OR a_digest <> NEW.artifact_digest THEN
        RAISE EXCEPTION 'certification requires a signed artifact with the certified digest' USING ERRCODE = '23514';
    END IF;
    SELECT passed, bundle_digest, artifact_digest INTO r_passed, r_bundle, r_artifact
        FROM pack_test_runs WHERE run_id = NEW.test_run_id AND pack_version_id = NEW.pack_version_id;
    IF r_passed IS DISTINCT FROM TRUE OR r_bundle <> NEW.bundle_digest OR r_artifact <> NEW.artifact_digest THEN
        RAISE EXCEPTION 'certification requires a PASSED test run of exactly this artifact and bundle' USING ERRCODE = '23514';
    END IF;
    IF NEW.certified_by = (SELECT created_by_principal_id FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id)
       OR NEW.certified_by = a_compiler
       OR NEW.certified_by IN (SELECT submitted_by FROM pack_test_bundles WHERE pack_version_id = NEW.pack_version_id)
       OR NEW.certified_by IN (SELECT reviewer FROM pack_reviews WHERE pack_version_id = NEW.pack_version_id AND decision = 'APPROVE') THEN
        RAISE EXCEPTION 'the certifier must be independent of the author, compiler, test authors and reviewers'
            USING ERRCODE = '23514', CONSTRAINT = 'ck_pack_cert_independent';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_certifications_guard BEFORE INSERT ON pack_certifications FOR EACH ROW EXECUTE FUNCTION jur_certification_guard();

-- The CERTIFIED status itself can only be reached through a certification row.
CREATE FUNCTION jur_pack_certified_status_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.status = 'CERTIFIED' AND OLD.status <> 'CERTIFIED'
       AND NOT EXISTS (SELECT 1 FROM pack_certifications WHERE pack_version_id = NEW.pack_version_id) THEN
        RAISE EXCEPTION 'a pack version becomes CERTIFIED only through a certification record' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_certified_status_guard BEFORE UPDATE ON jurisdiction_pack_versions
    FOR EACH ROW EXECUTE FUNCTION jur_pack_certified_status_guard();
