-- 000001_initial_schema.up.sql
-- privacy-rights-svc — initial schema (PRV-04, ZS-SVC-W-001 §14/§15).
--
-- Fourth of five ZS-SVC-W-001 services. Three tables: rights_requests is
-- the ONE mutable table this whole privacy-domain build has (status/
-- identity_verified/outcome legitimately progress as the case moves) —
-- identity_verification_events and discovery_manifests are pure
-- append-only evidence, same doctrine as every evidence table already
-- built in this domain.
--
-- wfc_process_ref is nullable and has no foreign key: it is an opaque
-- reference to a workflow-svc instance that some OTHER process creates
-- (see internal/domain's package doc comment for why this service never
-- creates that instance itself) — attached here only if and when it
-- exists.
CREATE TABLE rights_requests (
    request_id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID,

    subject_ref              TEXT        NOT NULL,
    -- Data only — see domain.RightFamily.
    right_family             VARCHAR(32) NOT NULL,
    jurisdiction             VARCHAR(16),
    -- Proxy/representative reference, if the requester is not the
    -- subject themselves — §15.1: "preserve representative/proxy
    -- authority evidence when another person acts for the subject."
    requester_ref            TEXT,
    submitted_via            VARCHAR(64),

    -- RECEIVED | IDENTITY_VERIFIED | IN_DISCOVERY | CLOSED — see
    -- domain.RequestStatus's doc comment for why this is coarser than
    -- the spec's full task-orchestration pipeline.
    status                   VARCHAR(32) NOT NULL DEFAULT 'RECEIVED',
    identity_verified        BOOLEAN     NOT NULL DEFAULT FALSE,
    -- FULFILLED | REJECTED | WITHDRAWN — set only at closure.
    outcome                  VARCHAR(32),
    response_evidence_hash   TEXT,
    wfc_process_ref          TEXT,

    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL,
    closed_at                TIMESTAMPTZ
);

CREATE INDEX idx_rights_requests_subject ON rights_requests (subject_ref, created_at DESC);

CREATE TABLE identity_verification_events (
    event_id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID,
    request_id                UUID        NOT NULL REFERENCES rights_requests(request_id),
    verified                  BOOLEAN     NOT NULL,
    method                    VARCHAR(64) NOT NULL,
    note                      TEXT,
    verified_by_principal_id  TEXT        NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_identity_verification_events_request ON identity_verification_events (request_id, created_at DESC);

CREATE TABLE discovery_manifests (
    manifest_id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID,
    request_id                 UUID        NOT NULL REFERENCES rights_requests(request_id),
    domain                     VARCHAR(64) NOT NULL,
    content_hash               TEXT        NOT NULL,
    candidate_count            INTEGER     NOT NULL DEFAULT 0,
    evidence_ref               TEXT,
    submitted_by_principal_id  TEXT        NOT NULL,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_discovery_manifests_request ON discovery_manifests (request_id, created_at ASC);
