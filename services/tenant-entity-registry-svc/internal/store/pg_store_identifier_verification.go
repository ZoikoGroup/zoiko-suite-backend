package store

// Identifier verification on Postgres (migration 000021). See registry.IdentifierVerificationStore.
//
// Everything for one observation happens under the party's row lock, in one transaction: the observation, the state
// it caused, a stewardship case if a higher authority disputed a verified identifier, and the events. The rules that
// decide what an observation changes are domain.DecideObservation; this file only applies the decision.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

var _ registry.IdentifierVerificationStore = (*PgStore)(nil)

const observationColumns = `observation_id, identifier_id, result, source_class, source_ref, COALESCE(source_version,''), COALESCE(exchange_ref,''),
	observed_at, COALESCE(authority_status,''), evidence_ref, COALESCE(evidence_hash,''), effect, effect_note, COALESCE(case_id::text,''),
	status_before, status_after, recorded_by, recorded_at`

func scanObservation(row pgx.Row) (*domain.IdentifierObservation, error) {
	var o domain.IdentifierObservation
	if err := row.Scan(&o.ObservationID, &o.IdentifierID, &o.Result, &o.SourceClass, &o.SourceRef, &o.SourceVersion, &o.ExchangeRef,
		&o.ObservedAt, &o.AuthorityStatus, &o.EvidenceRef, &o.EvidenceHash, &o.Effect, &o.EffectNote, &o.CaseID,
		&o.StatusBefore, &o.StatusAfter, &o.RecordedBy, &o.RecordedAt); err != nil {
		return nil, err
	}
	return &o, nil
}

// VerifyPartyIdentifier records one observation and applies s14.1 steps 5 to 7.
func (s *PgStore) VerifyPartyIdentifier(ctx context.Context, identifierID string, req domain.VerifyIdentifierRequest, actor string, allowUnversioned bool) (*registry.IdentifierVerifyOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(identifierID); err != nil {
		return nil, registry.ErrNotFound
	}
	var out *registry.IdentifierVerifyOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		// 1. Resolve the identifier and its party; the party's lock serialises every observation of its identifiers.
		var partyID string
		if err := tx.QueryRow(ctx, `SELECT party_id::text FROM party_identifiers WHERE identifier_id = $1 AND tenant_id = $2`, identifierID, tid).Scan(&partyID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrNotFound
			}
			return err
		}
		p, err := lockParty(ctx, tx, tid, partyID)
		if err != nil {
			return err
		}
		if err := checkPartyVersion(req.ExpectedMasterVersion, p.MasterVersion, allowUnversioned); err != nil {
			return err
		}
		if err := partyWritable(p); err != nil {
			return err
		}
		var ident domain.PartyIdentifier
		var norm, idStatus, vSource string
		var last *time.Time
		err = tx.QueryRow(ctx, `SELECT scheme, scope, value, value_normalized, status, verification_status, COALESCE(verified_source_class,''),
			last_observed_at, evidence_ref, linked_at FROM party_identifiers WHERE identifier_id = $1 AND tenant_id = $2 FOR UPDATE`, identifierID, tid).
			Scan(&ident.Scheme, &ident.Scope, &ident.Value, &norm, &idStatus, &ident.VerificationStatus, &vSource, &last, &ident.EvidenceRef, &ident.LinkedAt)
		if err != nil {
			return err
		}
		ident.IdentifierID, ident.PartyID = identifierID, partyID
		if idStatus == "ENDED" {
			return fmtStateConflict("the identifier has ended (it moved in a merge or was replaced); verify the one that replaced it")
		}
		// 2. Scheme rules apply to the comparison form only; the entered value is untouched.
		if ident.Scheme == domain.SchemeLEI && !domain.ValidLEI(strings.ToUpper(norm)) {
			return fmtInvalid("the value is not a well-formed ISO 17442 LEI (20 characters with a valid check)")
		}

		now := time.Now().UTC()
		before := ident.VerificationStatus
		// 5, 7. Precedence, freshness and acceptance.
		d := domain.DecideObservation(domain.VerificationState{Status: before, SourceClass: domain.SourceClass(vSource), LastObservedAt: last},
			ident.Scheme, req, now)

		obs := domain.IdentifierObservation{ObservationID: uuid.NewString(), IdentifierID: identifierID, Result: req.Result, SourceClass: req.SourceClass,
			SourceRef: strings.TrimSpace(req.SourceRef), SourceVersion: strings.TrimSpace(req.SourceVersion), ExchangeRef: strings.TrimSpace(req.ExchangeRef),
			ObservedAt: req.ObservedAt.UTC(), AuthorityStatus: strings.ToUpper(strings.TrimSpace(req.AuthorityStatus)), EvidenceRef: strings.TrimSpace(req.EvidenceRef),
			EvidenceHash: req.EvidenceHash, Effect: d.Effect, EffectNote: d.Note, StatusBefore: before, StatusAfter: d.NewStatus, RecordedBy: actor, RecordedAt: now}

		// 6. A higher authority that disagrees with a verified identifier opens a case; the identifier is not overwritten.
		var kase *domain.ResolutionCase
		if d.Conflict {
			kase = &domain.ResolutionCase{CaseID: uuid.NewString(), TenantID: tid, CaseType: domain.CaseVerificationConflict, Status: domain.CaseOpen,
				SubjectPartyID: &partyID, OpenedBy: actor, OpenedAt: now, RecordVersion: 1,
				Candidates: []domain.MatchCandidate{{PartyID: partyID, Tier: domain.TierD, Score: 1,
					Basis: []string{"a " + string(req.SourceClass) + " source reports " + req.Result + " for an identifier verified by " + vSource}}},
				Reason: "a higher-authority source disagrees with a verified identifier; the identifier is CONFLICTED until a steward decides"}
			if err := insertCase(ctx, tx, kase, nil, req.CorrelationID); err != nil {
				return err
			}
			obs.CaseID = kase.CaseID
		}
		// 4. Capture the response, source, version, time and evidence.
		if _, err := tx.Exec(ctx, `INSERT INTO identifier_verifications
			(observation_id, tenant_id, identifier_id, party_id, scheme, result, source_class, source_ref, source_version, exchange_ref, observed_at,
			 authority_status, evidence_ref, evidence_hash, effect, effect_note, case_id, status_before, status_after, recorded_by, recorded_at, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),$11,NULLIF($12,''),$13,NULLIF($14,''),$15,$16,NULLIF($17,'')::uuid,$18,$19,$20,$21,NULLIF($22,''))`,
			obs.ObservationID, tid, identifierID, partyID, string(ident.Scheme), obs.Result, string(obs.SourceClass), obs.SourceRef, obs.SourceVersion,
			obs.ExchangeRef, obs.ObservedAt, obs.AuthorityStatus, obs.EvidenceRef, obs.EvidenceHash, obs.Effect, obs.EffectNote, obs.CaseID,
			before, d.NewStatus, actor, now, req.CorrelationID); err != nil {
			return err
		}
		if kase != nil {
			if _, err := tx.Exec(ctx, `UPDATE resolution_cases SET identifier_id = $2, observation_id = $3 WHERE case_id = $1`, kase.CaseID, identifierID, obs.ObservationID); err != nil {
				return err
			}
		}

		np := p
		if d.Effect != domain.EffectNoChange {
			lastObs := last
			if d.AdvanceTime {
				t := obs.ObservedAt
				lastObs = &t
			}
			if _, err := tx.Exec(ctx, `UPDATE party_identifiers SET verification_status = $2, verified_source_class = NULLIF($3,''),
				verified_at = CASE WHEN $4 THEN $5::timestamptz ELSE verified_at END, last_observed_at = $6 WHERE identifier_id = $1`,
				identifierID, d.NewStatus, string(d.NewSource), d.Effect == domain.EffectVerified || d.Effect == domain.EffectSourceUpgraded, now, lastObs); err != nil {
				return err
			}
			if np, err = bumpParty(ctx, tx, tid, partyID, ""); err != nil {
				return err
			}
		}
		ident.VerificationStatus = d.NewStatus

		// LEI alignment (s13.3): a verified LEI is compared with the legal entity profile's. ORG-03 owns the profile,
		// so a difference is reported and never written over.
		alignment := domain.LEINotApplicable
		if ident.Scheme == domain.SchemeLEI && d.NewStatus == domain.IdentVerified {
			alignment = domain.LEINotLinked
			if np.LegalEntityID != nil {
				var profileLEI string
				err := tx.QueryRow(ctx, `SELECT COALESCE(lei::text,'') FROM legal_entity_profile_versions WHERE legal_entity_id = $1 AND tenant_id = $2
					AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3) ORDER BY effective_from DESC, version_number DESC LIMIT 1`,
					*np.LegalEntityID, tid, now).Scan(&profileLEI)
				switch {
				case errors.Is(err, pgx.ErrNoRows) || (err == nil && strings.TrimSpace(profileLEI) == ""):
					alignment = domain.LEIProfileAbsent
				case err != nil:
					return err
				case strings.EqualFold(strings.TrimSpace(profileLEI), strings.TrimSpace(norm)):
					alignment = domain.LEIAligned
				default:
					alignment = domain.LEIDiffers
				}
			}
		}

		out = &registry.IdentifierVerifyOutcome{Identifier: ident, Observation: obs, Party: *np, Case: kase, LEIAlignment: alignment}

		// Publish only what the acceptance rules passed; a conflict is its own event.
		if d.Effect == domain.EffectVerified || d.Effect == domain.EffectSourceUpgraded {
			rec, err := events.PartyIdentifierVerifiedRecord(*np, ident, obs, actor, req.CorrelationID)
			if err != nil {
				return err
			}
			if err := s.enqueue(ctx, tx, rec); err != nil {
				return err
			}
		}
		if kase != nil {
			rec, err := events.ConflictOpenedRecord(*kase, req.CorrelationID)
			if err != nil {
				return err
			}
			return s.enqueue(ctx, tx, rec)
		}
		return nil
	})
	return out, err
}

// ListIdentifierVerifications lists an identifier's observations, newest first.
func (s *PgStore) ListIdentifierVerifications(ctx context.Context, identifierID string) ([]domain.IdentifierObservation, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(identifierID); err != nil {
		return nil, registry.ErrNotFound
	}
	out := []domain.IdentifierObservation{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM party_identifiers WHERE identifier_id = $1 AND tenant_id = $2`, identifierID, tid).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+observationColumns+` FROM identifier_verifications WHERE identifier_id = $1 AND tenant_id = $2
			ORDER BY recorded_at DESC, observation_id`, identifierID, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanObservation(rows)
			if err != nil {
				return err
			}
			out = append(out, *o)
		}
		return rows.Err()
	})
	return out, err
}

// decideDisputed settles a case that disputes something verified: a fact conflict or a verification conflict.
func (s *PgStore) decideDisputed(ctx context.Context, tx pgx.Tx, tid string, c *domain.ResolutionCase, decision, actor, corr string, now time.Time) (*domain.Party, error) {
	if c.CaseType == domain.CaseVerificationConflict {
		return s.decideVerificationConflict(ctx, tx, tid, c, decision)
	}
	return s.decideFactConflict(ctx, tx, tid, c, decision, actor, corr, now)
}

// decideVerificationConflict keeps the identifier verified (KEEP_VERIFIED) or accepts the higher authority's
// observation (ACCEPT_OBSERVATION: the identifier is FAILED). The observations stay as the record of both.
func (s *PgStore) decideVerificationConflict(ctx context.Context, tx pgx.Tx, tid string, c *domain.ResolutionCase, decision string) (*domain.Party, error) {
	if c.SubjectPartyID == nil {
		return nil, fmtInvalid("the case names no party")
	}
	var identifierID *string
	if err := tx.QueryRow(ctx, `SELECT identifier_id::text FROM resolution_cases WHERE case_id = $1`, c.CaseID).Scan(&identifierID); err != nil {
		return nil, err
	}
	if identifierID == nil {
		return nil, fmtInvalid("the case names no identifier")
	}
	p, err := lockParty(ctx, tx, tid, *c.SubjectPartyID)
	if err != nil {
		return nil, err
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT verification_status FROM party_identifiers WHERE identifier_id = $1 AND tenant_id = $2 FOR UPDATE`, *identifierID, tid).Scan(&status); err != nil {
		return nil, err
	}
	if status != domain.IdentConflicted {
		return nil, fmtStateConflict("the identifier is " + status + ", not CONFLICTED")
	}
	to := domain.IdentVerified
	if decision == domain.DecisionAcceptObservation {
		to = domain.IdentFailed
	}
	if _, err := tx.Exec(ctx, `UPDATE party_identifiers SET verification_status = $2::text, verified_source_class = CASE WHEN $2::text = 'FAILED' THEN NULL ELSE verified_source_class END
		WHERE identifier_id = $1`, *identifierID, to); err != nil {
		return nil, err
	}
	return bumpParty(ctx, tx, tid, p.PartyID, "")
}

func fmtStateConflict(msg string) error { return fmt.Errorf("%w: %s", registry.ErrStateConflict, msg) }
func fmtInvalid(msg string) error       { return fmt.Errorf("%w: %s", registry.ErrInvalidInput, msg) }
