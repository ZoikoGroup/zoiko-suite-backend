package store

// ORG-07 parties and MDM-01 identity creation (migration 000016).
//
// Every method runs through withRLS, so the tenant is the verified one; no tenant id is read from a request.
//
// CREATE IS SERIALIZED PER TENANT. "Never creates a duplicate" is a property of the whole sequence (replay check,
// candidate search, insert): two creates that each find nothing would otherwise both insert. CreateParty takes a
// per-tenant advisory lock first, so the second create sees the first's party and matches it. Writes to an
// existing party serialize on that party's row lock, where the expected_master_version is compared.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const dateLayout = "2006-01-02"

func fmtDate(t time.Time) string { return t.UTC().Format(dateLayout) }

func fmtDatePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmtDate(*t)
}

func lockPartyCreate(ctx context.Context, tx pgx.Tx, tenantID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('party-create:' || $1::text, 0))`, tenantID)
	return err
}

const partyColumns = `party_id, tenant_id, party_kind, status, legal_entity_id, master_version, created_by, created_at`

func scanParty(row pgx.Row) (*domain.Party, error) {
	var p domain.Party
	var le *uuid.UUID
	if err := row.Scan(&p.PartyID, &p.TenantID, &p.PartyKind, &p.Status, &le, &p.MasterVersion, &p.CreatedBy, &p.CreatedAt); err != nil {
		return nil, err
	}
	if le != nil {
		s := le.String()
		p.LegalEntityID = &s
	}
	return &p, nil
}

// lockParty reads a party and holds its row lock until commit.
func lockParty(ctx context.Context, tx pgx.Tx, tenantID, partyID string) (*domain.Party, error) {
	if _, err := uuid.Parse(partyID); err != nil {
		return nil, registry.ErrNotFound
	}
	p, err := scanParty(tx.QueryRow(ctx, `SELECT `+partyColumns+` FROM parties WHERE party_id = $1 AND tenant_id = $2 FOR UPDATE`, partyID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return p, err
}

// checkPartyVersion compares the caller's expected_master_version with the locked party's (s19.1).
func checkPartyVersion(supplied, current int64, allowUnversioned bool) error {
	if supplied == 0 {
		if !allowUnversioned {
			return registry.ErrVersionRequired
		}
		return nil
	}
	if supplied != current {
		return fmt.Errorf("%w: you supplied expected_master_version %d but this party is at %d — reload and re-evaluate",
			registry.ErrVersionConflict, supplied, current)
	}
	return nil
}

// bumpParty advances master_version and returns the party as it now stands.
func bumpParty(ctx context.Context, tx pgx.Tx, tenantID, partyID string, newStatus domain.PartyStatus) (*domain.Party, error) {
	return scanParty(tx.QueryRow(ctx, `UPDATE parties SET master_version = master_version + 1,
		status = COALESCE(NULLIF($3, ''), status) WHERE party_id = $1 AND tenant_id = $2 RETURNING `+partyColumns,
		partyID, tenantID, string(newStatus)))
}

func jsonOrNull(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// ── Create ───────────────────────────────────────────────────────────────────

// CreateParty runs the MDM-01 s10.1 create flow.
func (s *PgStore) CreateParty(ctx context.Context, cmd registry.PartyCreateCommand) (*domain.CreatePartyResult, error) {
	tid := domain.TenantFromContext(ctx)
	req := cmd.Request
	s.log.Debug("store.CreateParty", zap.String("tenant_id", tid), zap.String("create_request_id", req.CreateRequestID))

	var out *domain.CreatePartyResult
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := lockPartyCreate(ctx, tx, tid); err != nil {
			return err
		}
		if res, err := replayCreate(ctx, tx, tid, cmd); err != nil || res != nil {
			out = res
			return err
		}

		decision, err := s.matchCreate(ctx, tx, tid, req)
		if err != nil {
			return err
		}
		now := time.Now().UTC()

		switch {
		case decision.SafeLink != nil:
			p, err := scanParty(tx.QueryRow(ctx, `SELECT `+partyColumns+` FROM parties WHERE party_id = $1 AND tenant_id = $2`,
				decision.SafeLink.PartyID, tid))
			if err != nil {
				return err
			}
			cand, _ := jsonOrNull(decision.SafeLink)
			if _, err := tx.Exec(ctx, `INSERT INTO party_create_requests (tenant_id, create_request_id, fingerprint, outcome, party_id, match_candidate)
				VALUES ($1,$2,$3,'MATCHED_EXISTING',$4,$5)`, tid, req.CreateRequestID, cmd.Fingerprint, p.PartyID, cand); err != nil {
				return err
			}
			out = &domain.CreatePartyResult{Outcome: domain.OutcomeMatchedExisting, Party: p, Match: decision.SafeLink}
			return nil

		case decision.Review:
			held, _ := json.Marshal(req)
			c := &domain.ResolutionCase{
				CaseID: uuid.NewString(), TenantID: tid, CaseType: decision.CaseType, Status: domain.CaseOpen,
				Candidates: decision.Candidates, CreateRequestID: req.CreateRequestID, OpenedBy: cmd.Actor, OpenedAt: now,
				Reason: "a create that resembles an existing party cannot be decided by a machine: " + describe(decision),
			}
			if err := insertCase(ctx, tx, c, held, req.CorrelationID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO party_create_requests (tenant_id, create_request_id, fingerprint, outcome, case_id)
				VALUES ($1,$2,$3,'REVIEW_REQUIRED',$4)`, tid, req.CreateRequestID, cmd.Fingerprint, c.CaseID); err != nil {
				return err
			}
			rec, err := events.ConflictOpenedRecord(*c, req.CorrelationID)
			if err != nil {
				return err
			}
			if err := s.enqueue(ctx, tx, rec); err != nil {
				return err
			}
			out = &domain.CreatePartyResult{Outcome: domain.OutcomeReviewRequired, Case: c, Candidates: decision.Candidates}
			return nil
		}

		p, err := s.insertNewParty(ctx, tx, tid, cmd, now)
		if err != nil {
			return err
		}
		out = &domain.CreatePartyResult{Outcome: domain.OutcomeCreated, Party: p}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func describe(d domain.MatchDecision) string {
	parts := make([]string, 0, len(d.Candidates))
	for _, c := range d.Candidates {
		parts = append(parts, fmt.Sprintf("%s (tier %s)", c.PartyID, c.Tier))
	}
	return strings.Join(parts, ", ")
}

// replayCreate answers a repeated create_request_id with what the first call returned.
func replayCreate(ctx context.Context, tx pgx.Tx, tid string, cmd registry.PartyCreateCommand) (*domain.CreatePartyResult, error) {
	var fp, outcome string
	var partyID, caseID *uuid.UUID
	var cand []byte
	err := tx.QueryRow(ctx, `SELECT fingerprint, outcome, party_id, case_id, match_candidate FROM party_create_requests
		WHERE tenant_id = $1 AND create_request_id = $2`, tid, cmd.Request.CreateRequestID).Scan(&fp, &outcome, &partyID, &caseID, &cand)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fp != cmd.Fingerprint {
		return nil, registry.ErrCreateRequestMismatch
	}
	res := &domain.CreatePartyResult{Outcome: domain.CreateOutcome(outcome), Replayed: true}
	if partyID != nil {
		p, err := scanParty(tx.QueryRow(ctx, `SELECT `+partyColumns+` FROM parties WHERE party_id = $1 AND tenant_id = $2`, partyID.String(), tid))
		if err != nil {
			return nil, err
		}
		res.Party = p
	}
	if caseID != nil {
		c, err := loadCase(ctx, tx, tid, caseID.String())
		if err != nil {
			return nil, err
		}
		res.Case = c
		if c != nil {
			res.Candidates = c.Candidates
		}
	}
	if len(cand) > 0 {
		var m domain.MatchCandidate
		if err := json.Unmarshal(cand, &m); err == nil {
			res.Match = &m
		}
	}
	return res, nil
}

// matchCreate loads the existing parties a request could resemble and runs the MDM-05 ladder over them.
func (s *PgStore) matchCreate(ctx context.Context, tx pgx.Tx, tid string, req domain.CreatePartyRequest) (domain.MatchDecision, error) {
	// A legal entity is one party: a create that names an ORG-03 legal entity a party already stands for is that
	// party, whatever its names say.
	if req.LegalEntityID != "" {
		var id string
		err := tx.QueryRow(ctx, `SELECT party_id FROM parties WHERE tenant_id = $1 AND legal_entity_id = $2`, tid, req.LegalEntityID).Scan(&id)
		if err == nil {
			c := domain.MatchCandidate{PartyID: id, Tier: domain.TierA, Score: 1, Basis: []string{"the party already stands for this legal entity"}}
			return domain.MatchDecision{SafeLink: &c, Candidates: []domain.MatchCandidate{c}}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.MatchDecision{}, err
		}
	}

	mr := req.MatchRequest()
	keys := make([]string, 0, len(mr.Identifiers))
	for _, i := range mr.Identifiers {
		keys = append(keys, string(i.Scheme)+"|"+i.Scope+"|"+i.Value)
	}
	var tokens []string
	for _, n := range mr.Names {
		tokens = append(tokens, domain.NameTokens(n)...)
	}

	rows, err := tx.Query(ctx, `
		SELECT party_id FROM party_identifiers
		 WHERE tenant_id = $1 AND status = 'ACTIVE' AND scheme || '|' || scope || '|' || value_normalized = ANY($2)
		UNION
		SELECT party_id FROM party_facts
		 WHERE tenant_id = $1 AND fact_type IN ('LEGAL_NAME','TRADING_NAME','FORMER_NAME')
		   AND status IN ('VERIFIED','PROPOSED') AND tokens && $3`, tid, keys, tokens)
	if err != nil {
		return domain.MatchDecision{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return domain.MatchDecision{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return domain.MatchDecision{}, err
	}
	if len(ids) == 0 {
		return domain.MatchDecision{}, nil
	}

	known := map[string]*domain.KnownParty{}
	krows, err := tx.Query(ctx, `
		SELECT p.party_id, p.party_kind,
		       COALESCE((SELECT f.value FROM party_facts f WHERE f.tenant_id = p.tenant_id AND f.party_id = p.party_id
		                  AND f.fact_type = 'COUNTRY' AND f.status = 'VERIFIED' AND f.superseded_at IS NULL
		                  ORDER BY f.valid_from DESC LIMIT 1), '')
		  FROM parties p WHERE p.tenant_id = $1 AND p.party_id = ANY($2::uuid[])`, tid, ids)
	if err != nil {
		return domain.MatchDecision{}, err
	}
	for krows.Next() {
		var k domain.KnownParty
		if err := krows.Scan(&k.PartyID, &k.Kind, &k.Country); err != nil {
			krows.Close()
			return domain.MatchDecision{}, err
		}
		known[k.PartyID] = &k
	}
	krows.Close()
	if err := krows.Err(); err != nil {
		return domain.MatchDecision{}, err
	}

	nrows, err := tx.Query(ctx, `SELECT party_id, value_normalized FROM party_facts
		 WHERE tenant_id = $1 AND party_id = ANY($2::uuid[]) AND fact_type IN ('LEGAL_NAME','TRADING_NAME','FORMER_NAME')
		   AND status IN ('VERIFIED','PROPOSED')`, tid, ids)
	if err != nil {
		return domain.MatchDecision{}, err
	}
	for nrows.Next() {
		var id, n string
		if err := nrows.Scan(&id, &n); err != nil {
			nrows.Close()
			return domain.MatchDecision{}, err
		}
		if k := known[id]; k != nil {
			k.Names = append(k.Names, n)
		}
	}
	nrows.Close()
	if err := nrows.Err(); err != nil {
		return domain.MatchDecision{}, err
	}

	irows, err := tx.Query(ctx, `SELECT party_id, scheme, scope, value_normalized, verification_status = 'VERIFIED'
		  FROM party_identifiers WHERE tenant_id = $1 AND party_id = ANY($2::uuid[]) AND status = 'ACTIVE'`, tid, ids)
	if err != nil {
		return domain.MatchDecision{}, err
	}
	for irows.Next() {
		var id string
		var ki domain.KnownIdentifier
		if err := irows.Scan(&id, &ki.Scheme, &ki.Scope, &ki.Value, &ki.Verified); err != nil {
			irows.Close()
			return domain.MatchDecision{}, err
		}
		if k := known[id]; k != nil {
			k.Identifiers = append(k.Identifiers, ki)
		}
	}
	irows.Close()
	if err := irows.Err(); err != nil {
		return domain.MatchDecision{}, err
	}

	list := make([]domain.KnownParty, 0, len(known))
	for _, k := range known {
		list = append(list, *k)
	}
	return domain.ClassifyMatch(mr, list), nil
}

// insertNewParty reserves the party id and persists its sourced facts and identifiers, then its event.
func (s *PgStore) insertNewParty(ctx context.Context, tx pgx.Tx, tid string, cmd registry.PartyCreateCommand, now time.Time) (*domain.Party, error) {
	req := cmd.Request
	status := domain.PartyCandidate
	if req.SourceClass.Authoritative() {
		status = domain.PartyActive
	}
	var le any
	if req.LegalEntityID != "" {
		le = req.LegalEntityID
	}
	id := uuid.NewString()
	p, err := scanParty(tx.QueryRow(ctx, `INSERT INTO parties (party_id, tenant_id, party_kind, status, legal_entity_id, master_version, created_by, created_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5,1,$6,$7,NULLIF($8,'')) RETURNING `+partyColumns,
		id, tid, string(req.PartyKind), string(status), le, cmd.Actor, now, req.CorrelationID))
	if err != nil {
		return nil, err
	}

	validFrom, _ := domain.ParseDate(req.ValidFrom)
	factStatus := domain.FactProposed
	if req.SourceClass.Authoritative() {
		factStatus = domain.FactVerified
	}
	insert := func(t domain.FactType, value, lang, script string) error {
		norm, tokens := value, []string{}
		if t.IsName() {
			norm = domain.NormalizeName(value)
			tokens = domain.NameTokens(norm)
		}
		_, err := tx.Exec(ctx, `INSERT INTO party_facts (fact_id, tenant_id, party_id, fact_type, value, value_normalized, tokens,
			language, script, valid_from, recorded_at, status, source_class, evidence_ref, proposed_by, correlation_id, recorded_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14,$15,NULLIF($16,''),1)`,
			uuid.NewString(), tid, id, string(t), strings.TrimSpace(value), norm, tokens, lang, script, validFrom, now,
			string(factStatus), string(req.SourceClass), req.EvidenceRef, cmd.Actor, req.CorrelationID)
		return err
	}
	for _, n := range req.Names {
		if err := insert(n.Type, n.Value, n.Language, n.Script); err != nil {
			return nil, err
		}
	}
	if req.Country != "" {
		if err := insert(domain.FactCountry, strings.ToUpper(req.Country), "", ""); err != nil {
			return nil, err
		}
	}
	for _, i := range req.AllIdentifiers() {
		if _, err := insertIdentifier(ctx, tx, tid, id, i, req.SourceClass, req.EvidenceRef, cmd.Actor, req.CorrelationID, now); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO party_create_requests (tenant_id, create_request_id, fingerprint, outcome, party_id)
		VALUES ($1,$2,$3,'CREATED',$4)`, tid, req.CreateRequestID, cmd.Fingerprint, id); err != nil {
		return nil, err
	}
	rec, err := events.PartyCreatedRecord(*p, req.CorrelationID)
	if err != nil {
		return nil, err
	}
	return p, s.enqueue(ctx, tx, rec)
}

func insertIdentifier(ctx context.Context, tx pgx.Tx, tid, partyID string, in domain.PartyIdentifierInput,
	defaultClass domain.SourceClass, defaultEvidence, actor, corr string, now time.Time) (*domain.PartyIdentifier, error) {
	class := in.SourceClass
	if class == "" {
		class = defaultClass
	}
	evidence := in.EvidenceRef
	if evidence == "" {
		evidence = defaultEvidence
	}
	verification := "UNVERIFIED"
	if class.Authoritative() {
		verification = "VERIFIED"
	}
	out := &domain.PartyIdentifier{
		IdentifierID: uuid.NewString(), PartyID: partyID, Scheme: in.Scheme, Scope: domain.NormalizeScope(in.Scheme, in.Scope),
		Value: strings.TrimSpace(in.Value), VerificationStatus: verification, EvidenceRef: evidence, LinkedAt: now,
	}
	_, err := tx.Exec(ctx, `INSERT INTO party_identifiers (identifier_id, tenant_id, party_id, scheme, scope, value, value_normalized,
		verification_status, source_class, evidence_ref, linked_by, linked_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''))`,
		out.IdentifierID, tid, partyID, string(in.Scheme), out.Scope, out.Value, domain.NormalizeIdentifier(in.Value),
		verification, string(class), evidence, actor, now, corr)
	return out, err
}

// ── Cases ────────────────────────────────────────────────────────────────────

func insertCase(ctx context.Context, tx pgx.Tx, c *domain.ResolutionCase, held []byte, corr string) error {
	cand, err := json.Marshal(c.Candidates)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO resolution_cases (case_id, tenant_id, case_type, status, subject_party_id, candidates, held_request,
		create_request_id, reason, opened_by, opened_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,$11,NULLIF($12,''))`,
		c.CaseID, c.TenantID, c.CaseType, c.Status, c.SubjectPartyID, cand, held, c.CreateRequestID, c.Reason, c.OpenedBy, c.OpenedAt, corr)
	return err
}

func loadCase(ctx context.Context, tx pgx.Tx, tid, caseID string) (*domain.ResolutionCase, error) {
	if _, err := uuid.Parse(caseID); err != nil {
		return nil, nil
	}
	var c domain.ResolutionCase
	var cand []byte
	var subject *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT case_id, tenant_id, case_type, status, subject_party_id, candidates, COALESCE(create_request_id,''),
		reason, opened_by, opened_at FROM resolution_cases WHERE case_id = $1 AND tenant_id = $2`, caseID, tid).
		Scan(&c.CaseID, &c.TenantID, &c.CaseType, &c.Status, &subject, &cand, &c.CreateRequestID, &c.Reason, &c.OpenedBy, &c.OpenedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if subject != nil {
		s := subject.String()
		c.SubjectPartyID = &s
	}
	if err := json.Unmarshal(cand, &c.Candidates); err != nil {
		return nil, err
	}
	if c.Candidates == nil {
		c.Candidates = []domain.MatchCandidate{}
	}
	return &c, nil
}

// GetResolutionCase returns one stewardship case; (nil, nil) when absent in the caller's tenant.
func (s *PgStore) GetResolutionCase(ctx context.Context, caseID string) (*domain.ResolutionCase, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.ResolutionCase
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var err error
		out, err = loadCase(ctx, tx, tid, caseID)
		return err
	})
	return out, err
}

// ── Read ─────────────────────────────────────────────────────────────────────

const factColumns = `fact_id, fact_type, value, COALESCE(language,''), COALESCE(script,''), valid_from, valid_to, recorded_at,
	status, source_class, evidence_ref, proposed_by`

func scanFact(row pgx.Row) (domain.PartyFact, error) {
	var f domain.PartyFact
	var from time.Time
	var to *time.Time
	err := row.Scan(&f.FactID, &f.FactType, &f.Value, &f.Language, &f.Script, &from, &to, &f.RecordedAt,
		&f.Status, &f.SourceClass, &f.EvidenceRef, &f.ProposedBy)
	f.ValidFrom, f.ValidTo = fmtDate(from), fmtDatePtr(to)
	return f, err
}

// GetPartyView returns the master view in force at asOf as known at knownAt.
func (s *PgStore) GetPartyView(ctx context.Context, partyID string, asOf, knownAt time.Time, reveal bool) (*domain.PartyView, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(partyID); err != nil {
		return nil, nil
	}
	day := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	var out *domain.PartyView
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		p, err := scanParty(tx.QueryRow(ctx, `SELECT `+partyColumns+` FROM parties WHERE party_id = $1 AND tenant_id = $2`, partyID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		v := &domain.PartyView{Party: *p, AsOf: fmtDate(day), KnownAt: knownAt.UTC(),
			Names: []domain.PartyFact{}, Facts: []domain.PartyFact{}, PendingFacts: []domain.PartyFact{},
			Identifiers: []domain.PartyIdentifier{}, Roles: []domain.PartyRole{}, QualityFlags: []string{}}
		// A party that did not yet exist is not in the view.
		if p.CreatedAt.After(knownAt) {
			return nil
		}

		rows, err := tx.Query(ctx, `SELECT `+factColumns+` FROM party_facts
			WHERE tenant_id = $1 AND party_id = $2 AND recorded_at <= $4
			  AND ((status IN ('VERIFIED','SUPERSEDED') AND valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3)
			        AND (superseded_at IS NULL OR superseded_at > $4))
			       OR (status = 'PROPOSED'))
			ORDER BY fact_type, valid_from, recorded_at`, tid, partyID, day, knownAt)
		if err != nil {
			return err
		}
		for rows.Next() {
			f, err := scanFact(rows)
			if err != nil {
				rows.Close()
				return err
			}
			switch {
			case f.Status == domain.FactProposed:
				v.PendingFacts = append(v.PendingFacts, f)
			case f.FactType.IsName():
				v.Names = append(v.Names, f)
			default:
				v.Facts = append(v.Facts, f)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		irows, err := tx.Query(ctx, `SELECT identifier_id, scheme, scope, value, verification_status, evidence_ref, linked_at
			FROM party_identifiers WHERE tenant_id = $1 AND party_id = $2 AND status = 'ACTIVE' AND linked_at <= $3
			ORDER BY scheme, scope, linked_at`, tid, partyID, knownAt)
		if err != nil {
			return err
		}
		for irows.Next() {
			var i domain.PartyIdentifier
			if err := irows.Scan(&i.IdentifierID, &i.Scheme, &i.Scope, &i.Value, &i.VerificationStatus, &i.EvidenceRef, &i.LinkedAt); err != nil {
				irows.Close()
				return err
			}
			i.PartyID = partyID
			if !reveal && i.Scheme.Sensitive() {
				i.Value, i.Masked = domain.MaskIdentifier(i.Scheme, i.Value), true
			}
			v.Identifiers = append(v.Identifiers, i)
		}
		irows.Close()
		if err := irows.Err(); err != nil {
			return err
		}

		rrows, err := tx.Query(ctx, `SELECT role_id, role_type, valid_from,
			       CASE WHEN removed_at IS NOT NULL AND removed_at > $4 THEN NULL ELSE valid_to END,
			       evidence_ref, added_by, added_at, COALESCE(removed_by,''), COALESCE(remove_reason,'')
			  FROM party_roles
			 WHERE tenant_id = $1 AND party_id = $2 AND added_at <= $4 AND valid_from <= $3
			   AND (valid_to IS NULL OR valid_to > $3 OR (removed_at IS NOT NULL AND removed_at > $4))
			 ORDER BY role_type, valid_from`, tid, partyID, day, knownAt)
		if err != nil {
			return err
		}
		for rrows.Next() {
			var r domain.PartyRole
			var from time.Time
			var to *time.Time
			if err := rrows.Scan(&r.RoleID, &r.RoleType, &from, &to, &r.EvidenceRef, &r.AddedBy, &r.AddedAt, &r.RemovedBy, &r.RemoveNote); err != nil {
				rrows.Close()
				return err
			}
			r.ValidFrom, r.ValidTo = fmtDate(from), fmtDatePtr(to)
			v.Roles = append(v.Roles, r)
		}
		rrows.Close()
		if err := rrows.Err(); err != nil {
			return err
		}

		hasLegal, hasVerifiedID := false, false
		for _, n := range v.Names {
			if n.FactType == domain.FactLegalName && n.Status == domain.FactVerified {
				hasLegal = true
			}
		}
		for _, i := range v.Identifiers {
			if i.VerificationStatus == "VERIFIED" {
				hasVerifiedID = true
			}
		}
		if !hasLegal {
			v.QualityFlags = append(v.QualityFlags, "NO_VERIFIED_LEGAL_NAME")
		}
		if !hasVerifiedID {
			v.QualityFlags = append(v.QualityFlags, "NO_VERIFIED_IDENTIFIER")
		}
		if len(v.PendingFacts) > 0 {
			v.QualityFlags = append(v.QualityFlags, "UNVERIFIED_FACTS_PENDING")
		}
		var open bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM resolution_cases WHERE tenant_id = $1 AND status = 'OPEN'
			AND (subject_party_id = $2 OR candidates @> jsonb_build_array(jsonb_build_object('party_id', $2::text))))`,
			tid, partyID).Scan(&open); err != nil {
			return err
		}
		if open {
			v.QualityFlags = append(v.QualityFlags, "OPEN_RESOLUTION_CASE")
		}
		out = v
		return nil
	})
	return out, err
}

// ── Facts ────────────────────────────────────────────────────────────────────

// ProposePartyFact records a sourced, effective-dated fact without ever overwriting a verified one.
func (s *PgStore) ProposePartyFact(ctx context.Context, partyID string, req domain.ProposeFactRequest, actor string, allowUnversioned bool) (*registry.PartyFactOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.PartyFactOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		p, err := lockParty(ctx, tx, tid, partyID)
		if err != nil {
			return err
		}
		if err := checkPartyVersion(req.ExpectedMasterVersion, p.MasterVersion, allowUnversioned); err != nil {
			return err
		}
		if p.Status == domain.PartyInactive {
			return fmt.Errorf("%w: the party is INACTIVE", registry.ErrStateConflict)
		}
		from, _ := domain.ParseDate(req.ValidFrom)
		var to *time.Time
		if req.ValidTo != "" {
			t, _ := domain.ParseDate(req.ValidTo)
			to = &t
		}
		value := strings.TrimSpace(req.Value)
		norm, tokens := value, []string{}
		if req.FactType.IsName() {
			norm = domain.NormalizeName(value)
			tokens = domain.NameTokens(norm)
		} else if req.FactType == domain.FactCountry {
			value, norm = strings.ToUpper(value), strings.ToUpper(value)
		}

		// Verified facts of this type that overlap the proposed period.
		orows, err := tx.Query(ctx, `SELECT `+factColumns+`, value_normalized FROM party_facts
			WHERE tenant_id = $1 AND party_id = $2 AND fact_type = $3 AND status = 'VERIFIED' AND superseded_at IS NULL
			  AND valid_from < COALESCE($5::date, 'infinity'::date) AND COALESCE(valid_to, 'infinity'::date) > $4`,
			tid, partyID, string(req.FactType), from, to)
		if err != nil {
			return err
		}
		var overlapping []domain.PartyFact
		sameValue := map[string]bool{}
		for orows.Next() {
			var f domain.PartyFact
			var vf time.Time
			var vt *time.Time
			var vn string
			if err := orows.Scan(&f.FactID, &f.FactType, &f.Value, &f.Language, &f.Script, &vf, &vt, &f.RecordedAt,
				&f.Status, &f.SourceClass, &f.EvidenceRef, &f.ProposedBy, &vn); err != nil {
				orows.Close()
				return err
			}
			f.ValidFrom, f.ValidTo = fmtDate(vf), fmtDatePtr(vt)
			overlapping = append(overlapping, f)
			sameValue[f.FactID] = vn == norm
		}
		orows.Close()
		if err := orows.Err(); err != nil {
			return err
		}
		// Restating a verified fact is not a change.
		for _, f := range overlapping {
			if sameValue[f.FactID] {
				out = &registry.PartyFactOutcome{Fact: f, Party: *p, NoOp: true}
				return nil
			}
		}
		// A name that is not the legal name co-exists with the others; only the identical value is a restatement.
		conflict := req.FactType.SingleValued() && len(overlapping) > 0

		status := domain.FactProposed
		if req.SourceClass.Authoritative() && !conflict {
			status = domain.FactVerified
		}
		now := time.Now().UTC()
		fact := domain.PartyFact{
			FactID: uuid.NewString(), FactType: req.FactType, Value: value, Language: req.Language, Script: req.Script,
			ValidFrom: fmtDate(from), ValidTo: fmtDatePtr(to), RecordedAt: now, Status: status,
			SourceClass: req.SourceClass, EvidenceRef: req.EvidenceRef, ProposedBy: actor,
		}
		next := domain.PartyStatus("")
		if status == domain.FactVerified && req.FactType == domain.FactLegalName && p.Status == domain.PartyCandidate {
			next = domain.PartyActive
		}
		np, err := bumpParty(ctx, tx, tid, partyID, next)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO party_facts (fact_id, tenant_id, party_id, fact_type, value, value_normalized, tokens,
			language, script, valid_from, valid_to, recorded_at, status, source_class, evidence_ref, proposed_by, correlation_id, recorded_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14,$15,$16,NULLIF($17,''),$18)`,
			fact.FactID, tid, partyID, string(req.FactType), value, norm, tokens, req.Language, req.Script, from, to, now,
			string(status), string(req.SourceClass), req.EvidenceRef, actor, req.CorrelationID, np.MasterVersion); err != nil {
			return err
		}
		out = &registry.PartyFactOutcome{Fact: fact, Party: *np}

		rec, err := events.PartyFactRecord(events.EventPartyProfileAmended, *np, fact, req.CorrelationID)
		if err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
		if status == domain.FactVerified {
			rec, err := events.PartyFactRecord(events.EventPartyFactPublished, *np, fact, req.CorrelationID)
			if err != nil {
				return err
			}
			if err := s.enqueue(ctx, tx, rec); err != nil {
				return err
			}
		}
		if conflict {
			c := &domain.ResolutionCase{
				CaseID: uuid.NewString(), TenantID: tid, CaseType: domain.CaseFactConflict, Status: domain.CaseOpen,
				SubjectPartyID: &partyID, Candidates: []domain.MatchCandidate{}, OpenedBy: actor, OpenedAt: now,
				Reason: fmt.Sprintf("a proposed %s contradicts the verified %s in force; the verified fact stands until a steward decides", req.FactType, req.FactType),
			}
			if err := insertCase(ctx, tx, c, nil, req.CorrelationID); err != nil {
				return err
			}
			out.Case = c
			rec, err := events.ConflictOpenedRecord(*c, req.CorrelationID)
			if err != nil {
				return err
			}
			return s.enqueue(ctx, tx, rec)
		}
		return nil
	})
	return out, err
}

// ── Roles ────────────────────────────────────────────────────────────────────

// AddPartyRole gives a party a role.
func (s *PgStore) AddPartyRole(ctx context.Context, partyID string, req domain.AddPartyRoleRequest, actor string, allowUnversioned bool) (*registry.PartyRoleOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.PartyRoleOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		p, err := lockParty(ctx, tx, tid, partyID)
		if err != nil {
			return err
		}
		if err := checkPartyVersion(req.ExpectedMasterVersion, p.MasterVersion, allowUnversioned); err != nil {
			return err
		}
		if p.Status == domain.PartyInactive {
			return fmt.Errorf("%w: the party is INACTIVE", registry.ErrStateConflict)
		}
		from, _ := domain.ParseDate(req.ValidFrom)
		now := time.Now().UTC()
		role := domain.PartyRole{RoleID: uuid.NewString(), RoleType: req.RoleType, ValidFrom: fmtDate(from),
			EvidenceRef: req.EvidenceRef, AddedBy: actor, AddedAt: now}
		np, err := bumpParty(ctx, tx, tid, partyID, "")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO party_roles (role_id, tenant_id, party_id, role_type, valid_from, evidence_ref, added_by, added_at, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''))`,
			role.RoleID, tid, partyID, string(req.RoleType), from, req.EvidenceRef, actor, now, req.CorrelationID); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: the party already has a live %s role", registry.ErrConflict, req.RoleType)
			}
			return err
		}
		out = &registry.PartyRoleOutcome{Role: role, Party: *np}
		rec, err := events.PartyRoleRecord(events.EventPartyRoleAdded, *np, role, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// RemovePartyRole ends a role; the row stays.
func (s *PgStore) RemovePartyRole(ctx context.Context, partyID, roleID string, req domain.RemovePartyRoleRequest, actor string, allowUnversioned bool) (*registry.PartyRoleOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.PartyRoleOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		p, err := lockParty(ctx, tx, tid, partyID)
		if err != nil {
			return err
		}
		if err := checkPartyVersion(req.ExpectedMasterVersion, p.MasterVersion, allowUnversioned); err != nil {
			return err
		}
		if _, err := uuid.Parse(roleID); err != nil {
			return registry.ErrNotFound
		}
		var role domain.PartyRole
		var from time.Time
		var removedAt *time.Time
		err = tx.QueryRow(ctx, `SELECT role_id, role_type, valid_from, evidence_ref, added_by, added_at, removed_at FROM party_roles
			WHERE role_id = $1 AND party_id = $2 AND tenant_id = $3 FOR UPDATE`, roleID, partyID, tid).
			Scan(&role.RoleID, &role.RoleType, &from, &role.EvidenceRef, &role.AddedBy, &role.AddedAt, &removedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if removedAt != nil {
			return fmt.Errorf("%w: the role was already removed", registry.ErrStateConflict)
		}
		end, _ := domain.ParseDate(req.EndDate)
		if end.Before(from) {
			return fmt.Errorf("%w: end_date is before the role began", registry.ErrInvalidInput)
		}
		now := time.Now().UTC()
		np, err := bumpParty(ctx, tx, tid, partyID, "")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE party_roles SET valid_to = $2, removed_by = $3, removed_at = $4, remove_reason = $5
			WHERE role_id = $1`, roleID, end, actor, now, req.Reason); err != nil {
			return err
		}
		role.ValidFrom, role.ValidTo, role.RemovedBy, role.RemoveNote = fmtDate(from), fmtDate(end), actor, req.Reason
		out = &registry.PartyRoleOutcome{Role: role, Party: *np}
		rec, err := events.PartyRoleRecord(events.EventPartyRoleRemoved, *np, role, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// ── Identifiers ──────────────────────────────────────────────────────────────

// LinkPartyIdentifier links an external identifier, or opens a conflict case when another party holds it.
func (s *PgStore) LinkPartyIdentifier(ctx context.Context, partyID string, req domain.LinkIdentifierRequest, actor string, allowUnversioned bool) (*registry.PartyIdentifierOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.PartyIdentifierOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		p, err := lockParty(ctx, tx, tid, partyID)
		if err != nil {
			return err
		}
		if err := checkPartyVersion(req.ExpectedMasterVersion, p.MasterVersion, allowUnversioned); err != nil {
			return err
		}
		if p.Status == domain.PartyInactive {
			return fmt.Errorf("%w: the party is INACTIVE", registry.ErrStateConflict)
		}
		scope := domain.NormalizeScope(req.Scheme, req.Scope)
		var holder, existingID string
		var existing domain.PartyIdentifier
		err = tx.QueryRow(ctx, `SELECT party_id, identifier_id, scheme, scope, value, verification_status, evidence_ref, linked_at
			FROM party_identifiers WHERE tenant_id = $1 AND scheme = $2 AND scope = $3 AND value_normalized = $4 AND status = 'ACTIVE'`,
			tid, string(req.Scheme), scope, domain.NormalizeIdentifier(req.Value)).
			Scan(&holder, &existingID, &existing.Scheme, &existing.Scope, &existing.Value, &existing.VerificationStatus, &existing.EvidenceRef, &existing.LinkedAt)
		switch {
		case err == nil && holder == partyID:
			existing.IdentifierID, existing.PartyID = existingID, partyID
			out = &registry.PartyIdentifierOutcome{Identifier: &existing, Party: *p, NoOp: true}
			return nil
		case err == nil:
			now := time.Now().UTC()
			c := &domain.ResolutionCase{
				CaseID: uuid.NewString(), TenantID: tid, CaseType: domain.CaseIdentifierConflict, Status: domain.CaseOpen,
				SubjectPartyID: &partyID, OpenedBy: actor, OpenedAt: now,
				Candidates: []domain.MatchCandidate{
					{PartyID: holder, Tier: domain.TierD, Score: 1, Basis: []string{"holds the identifier " + string(req.Scheme) + " " + scope}},
					{PartyID: partyID, Tier: domain.TierD, Score: 1, Basis: []string{"was asked to link the same identifier"}},
				},
				Reason: "an identifier maps to one party; a second party was asked to link it",
			}
			if err := insertCase(ctx, tx, c, nil, req.CorrelationID); err != nil {
				return err
			}
			out = &registry.PartyIdentifierOutcome{Party: *p, Case: c}
			rec, err := events.ConflictOpenedRecord(*c, req.CorrelationID)
			if err != nil {
				return err
			}
			return s.enqueue(ctx, tx, rec)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		now := time.Now().UTC()
		np, err := bumpParty(ctx, tx, tid, partyID, "")
		if err != nil {
			return err
		}
		ident, err := insertIdentifier(ctx, tx, tid, partyID, req.PartyIdentifierInput, req.SourceClass, req.EvidenceRef, actor, req.CorrelationID, now)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: the identifier was linked meanwhile", registry.ErrConflict)
			}
			return err
		}
		out = &registry.PartyIdentifierOutcome{Identifier: ident, Party: *np}
		rec, err := events.PartyIdentifierRecord(*np, *ident, actor, req.CorrelationID)
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, rec)
	})
	return out, err
}

// FindPartiesByIdentifier is FindByExternalIdentifier.
func (s *PgStore) FindPartiesByIdentifier(ctx context.Context, scheme domain.IdentifierScheme, scope, value string) ([]domain.PartySummary, error) {
	tid := domain.TenantFromContext(ctx)
	scope = domain.NormalizeScope(scheme, scope)
	var out []domain.PartySummary
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, partySummarySQL+` JOIN party_identifiers i ON i.tenant_id = p.tenant_id AND i.party_id = p.party_id
			WHERE p.tenant_id = $1 AND i.scheme = $2 AND i.scope = $3 AND i.value_normalized = $4 AND i.status = 'ACTIVE'
			ORDER BY p.party_id`, tid, string(scheme), scope, domain.NormalizeIdentifier(value))
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanSummaries(rows)
		return err
	})
	if out == nil {
		out = []domain.PartySummary{}
	}
	return out, err
}

// ── Search ───────────────────────────────────────────────────────────────────

const partySummarySQL = `SELECT p.party_id, p.party_kind, p.status,
	COALESCE((SELECT f.value FROM party_facts f WHERE f.tenant_id = p.tenant_id AND f.party_id = p.party_id AND f.fact_type = 'LEGAL_NAME'
	           AND f.status IN ('VERIFIED','PROPOSED') AND f.superseded_at IS NULL
	           ORDER BY (f.status = 'VERIFIED') DESC, f.valid_from DESC LIMIT 1), '')
	FROM parties p`

func scanSummaries(rows pgx.Rows) ([]domain.PartySummary, error) {
	var out []domain.PartySummary
	for rows.Next() {
		var s domain.PartySummary
		if err := rows.Scan(&s.PartyID, &s.PartyKind, &s.Status, &s.LegalName); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SearchParties reads the authoritative tables. The cursor is the last party id of the previous page.
func (s *PgStore) SearchParties(ctx context.Context, q domain.PartySearchQuery) ([]domain.PartySummary, string, error) {
	tid := domain.TenantFromContext(ctx)
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var after *string
	if q.After != "" {
		if _, err := uuid.Parse(q.After); err != nil {
			return nil, "", fmt.Errorf("%w: the cursor is not one this service issued", registry.ErrInvalidInput)
		}
		after = &q.After
	}
	norm := domain.NormalizeName(q.Text)
	var out []domain.PartySummary
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, partySummarySQL+`
			WHERE p.tenant_id = $1
			  AND ($2::text = '' OR p.party_kind = $2)
			  AND ($3::text = '' OR p.status = $3)
			  AND ($4::text = '' OR EXISTS (SELECT 1 FROM party_roles r WHERE r.tenant_id = p.tenant_id AND r.party_id = p.party_id
			        AND r.role_type = $4 AND r.removed_at IS NULL))
			  AND ($5::text = '' OR EXISTS (SELECT 1 FROM party_facts f WHERE f.tenant_id = p.tenant_id AND f.party_id = p.party_id
			        AND f.fact_type IN ('LEGAL_NAME','TRADING_NAME','FORMER_NAME','DISPLAY_NAME','LOCAL_SCRIPT_NAME')
			        AND f.status IN ('VERIFIED','PROPOSED') AND f.tokens @> $6::text[]))
			  AND ($7::uuid IS NULL OR p.party_id > $7::uuid)
			ORDER BY p.party_id LIMIT $8`,
			tid, string(q.Kind), string(q.Status), string(q.Role), norm, domain.NameTokens(norm), after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = scanSummaries(rows)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].PartyID
	}
	if out == nil {
		out = []domain.PartySummary{}
	}
	return out, next, nil
}

var _ registry.PartyStore = (*PgStore)(nil)
