-- Migration: 000010_certification_evidence.up.sql
--
-- SPEC_DEVIATIONS A12. A certification's sealed manifest recorded counts, the
-- pinned rules and their digest, the result and the assessment, but not what
-- it was sealed over or by whose authority: ZS-SVC-Z-001 INV-21
-- "Certification binds exact observation/reconciliation/lineage evidence",
-- DQC-05 "signed/immutable certification record binding exact evidence set and
-- authority", ZS-CONTROL-001 Invariant 9 "a certified control result always
-- identifies the certifier, certification authority, time, run version,
-- unresolved exceptions and impact assessment". The shape mirrors
-- financial-control-svc's control_certifications (its 000007).
--
-- certifications gains, each also in the manifest hash:
--   authority_snapshot  authorization-svc's live decision for the certifier,
--                       RECONCILIATION_CERTIFY and the run's entity
--                       (access_decision_id, decision_basis,
--                       policy_set_version, ...);
--   populations         every side of the generation certified (the run's
--                       latest, A8), by snapshot id and content_hash;
--   manual_matches      every manual match of the run, with its reason,
--                       its evidence and the exceptions it resolved.
--
-- match_results gains evidence: for a manual match, both frozen items, their
-- generation and the comparison under the run's pinned tolerance that let the
-- pairing through (§10 "Manual match — requires permission, reason and
-- evidence"), written in the transaction that applies the match.
--
-- Both tables are immutable (reject_immutable_row), so the new columns are
-- written once, on insert. No backfill: a certification sealed, or a manual
-- match made, before this migration recorded none of this, and none is
-- invented. The checks are NOT VALID so those rows stay as they are; every new
-- row must satisfy them.

ALTER TABLE certifications
    ADD COLUMN authority_snapshot JSONB,
    ADD COLUMN populations        JSONB,
    ADD COLUMN manual_matches     JSONB;

ALTER TABLE certifications
    ADD CONSTRAINT certifications_evidence_check CHECK (
        jsonb_typeof(authority_snapshot) = 'object'
        AND coalesce(authority_snapshot->>'access_decision_id', '') <> ''
        AND authority_snapshot->>'decision_outcome' = 'GRANTED'
        AND authority_snapshot->>'principal_id' = sealed_by
        AND jsonb_typeof(populations) = 'array'
        AND jsonb_array_length(populations) > 0
        AND jsonb_typeof(manual_matches) = 'array'
    ) NOT VALID;

ALTER TABLE match_results
    ADD COLUMN evidence JSONB;

ALTER TABLE match_results
    ADD CONSTRAINT match_results_manual_evidence_check CHECK (
        NOT manual OR jsonb_typeof(evidence) = 'object'
    ) NOT VALID;
