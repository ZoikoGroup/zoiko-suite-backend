package store

// MDM-05 matching, stewardship decisions, merge, split and merge history (migration 000017).
//
// A MERGE IS A GOVERNED RELATIONSHIP CHANGE, NOT A ROW MERGE (s4.7). The survivor keeps its id. Every absorbed
// party keeps its row, status MERGED and resolving to the survivor, so its id is never reused and still resolves.
// Identifiers and roles move by ending the old row and writing a new one that names where it came from. What was
// decided, by whom, why and with what evidence is an append-only lineage; a wrong merge is corrected by a split
// that adds lineage, never by erasing the error.
//
// Merge and split serialize per tenant (an advisory lock) and then on the parties' row locks, taken in id order,
// so the chain rules are checked against a structure nobody else is changing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

func lockPartyMerge(ctx context.Context, tx pgx.Tx, tenantID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('party-merge:' || $1::text, 0))`, tenantID)
	return err
}

// lockCase reads a case and holds its row lock until commit; (nil, nil) when absent.
func lockCase(ctx context.Context, tx pgx.Tx, tid, caseID string) (*domain.ResolutionCase, []byte, error) {
	if _, err := uuid.Parse(caseID); err != nil {
		return nil, nil, nil
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM resolution_cases WHERE case_id = $1 AND tenant_id = $2 FOR UPDATE`, caseID, tid); err != nil {
		return nil, nil, err
	}
	c, err := loadCase(ctx, tx, tid, caseID)
	if err != nil || c == nil {
		return c, nil, err
	}
	var held []byte
	if err := tx.QueryRow(ctx, `SELECT held_request FROM resolution_cases WHERE case_id = $1`, caseID).Scan(&held); err != nil {
		return nil, nil, err
	}
	return c, held, nil
}

// ── Match ────────────────────────────────────────────────────────────────────

// MatchParties answers what a create would do, and why, without writing.
func (s *PgStore) MatchParties(ctx context.Context, req domain.MatchPartiesRequest) (*domain.MatchPartiesResult, error) {
	tid := domain.TenantFromContext(ctx)
	out := &domain.MatchPartiesResult{Verdict: "NO_MATCH", Candidates: []domain.MatchCandidate{}}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		d, err := s.matchCreate(ctx, tx, tid, req.AsCreate())
		if err != nil {
			return err
		}
		switch {
		case d.SafeLink != nil:
			out.Verdict = "SAFE_LINK"
		case d.Review:
			out.Verdict, out.CaseType = string(domain.OutcomeReviewRequired), d.CaseType
		}
		if d.Candidates != nil {
			out.Candidates = d.Candidates
		}
		return nil
	})
	return out, err
}

// ── Cases ────────────────────────────────────────────────────────────────────

// ListResolutionCases pages stewardship cases, oldest first. The cursor is "<opened_at>|<case_id>".
func (s *PgStore) ListResolutionCases(ctx context.Context, status, caseType string, limit int, after string) ([]domain.ResolutionCase, string, error) {
	tid := domain.TenantFromContext(ctx)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var at *time.Time
	var id string
	if after != "" {
		ts, cid, ok := strings.Cut(after, "|")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if _, perr := uuid.Parse(cid); !ok || err != nil || perr != nil {
			return nil, "", fmt.Errorf("%w: the cursor is not one this service issued", registry.ErrInvalidInput)
		}
		at, id = &t, cid
	}
	var ids []string
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT case_id FROM resolution_cases
			WHERE tenant_id = $1 AND ($2::text = '' OR status = $2) AND ($3::text = '' OR case_type = $3)
			  AND ($4::timestamptz IS NULL OR (opened_at, case_id) > ($4, $5::uuid))
			ORDER BY opened_at, case_id LIMIT $6`, tid, status, caseType, at, nullUUID(id), limit+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, c)
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(ids) > limit {
		ids = ids[:limit]
	}
	out := []domain.ResolutionCase{}
	err = s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		for _, cid := range ids {
			c, err := loadCase(ctx, tx, tid, cid)
			if err != nil {
				return err
			}
			if c != nil {
				out = append(out, *c)
			}
		}
		return nil
	})
	if err == nil && len(out) == limit {
		last := out[len(out)-1]
		var more bool
		_ = s.withRLS(ctx, tid, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM resolution_cases WHERE tenant_id = $1
				AND ($2::text = '' OR status = $2) AND ($3::text = '' OR case_type = $3) AND (opened_at, case_id) > ($4, $5::uuid))`,
				tid, status, caseType, last.OpenedAt, last.CaseID).Scan(&more)
		})
		if more {
			next = last.OpenedAt.UTC().Format(time.RFC3339Nano) + "|" + last.CaseID
		}
	}
	return out, next, err
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// settleCase records a decision on a locked case.
func settleCase(ctx context.Context, tx pgx.Tx, c *domain.ResolutionCase, status, decision, actor, note string, resolved *string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE resolution_cases SET status = $2, decision = $3, decided_by = $4, decided_at = $5,
		decision_note = $6, resolved_party_id = $7, record_version = record_version + 1 WHERE case_id = $1`,
		c.CaseID, status, decision, actor, now, note, resolved)
	if err != nil {
		return err
	}
	c.Status, c.Decision, c.DecidedBy, c.DecidedAt, c.DecisionNote, c.ResolvedPartyID = status, decision, actor, &now, note, resolved
	c.RecordVersion++
	return nil
}

// DecideResolutionCase settles a case.
func (s *PgStore) DecideResolutionCase(ctx context.Context, caseID string, req domain.CaseDecisionRequest, actor string, allowUnversioned bool) (*registry.CaseDecisionOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.CaseDecisionOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := lockPartyMerge(ctx, tx, tid); err != nil {
			return err
		}
		c, held, err := lockCase(ctx, tx, tid, caseID)
		if err != nil {
			return err
		}
		if c == nil {
			return registry.ErrNotFound
		}
		if c.Status != domain.CaseOpen {
			return fmt.Errorf("%w: the case is already %s", registry.ErrStateConflict, c.Status)
		}
		if err := checkPartyVersion(req.ExpectedCaseVersion, c.RecordVersion, allowUnversioned); err != nil {
			return err
		}
		allowed := false
		for _, d := range domain.DecisionsFor(c.CaseType) {
			allowed = allowed || d == req.Decision
		}
		if !allowed {
			return fmt.Errorf("%w: a %s case is decided by %s; a merge is its own command", registry.ErrInvalidInput, c.CaseType,
				strings.Join(domain.DecisionsFor(c.CaseType), ", "))
		}
		if actor == c.OpenedBy {
			return fmt.Errorf("%w: the opener of a case cannot decide it", registry.ErrSelfApproval)
		}
		now := time.Now().UTC()
		status := domain.CaseResolved
		var resolved *string
		var party *domain.Party

		switch req.Decision {
		case domain.DecisionCreateNew:
			var hr domain.CreatePartyRequest
			if len(held) == 0 || json.Unmarshal(held, &hr) != nil {
				return fmt.Errorf("%w: the case holds no create request", registry.ErrInvalidInput)
			}
			if err := lockPartyCreate(ctx, tx, tid); err != nil {
				return err
			}
			p, err := s.insertNewParty(ctx, tx, tid, registry.PartyCreateCommand{Request: hr, Actor: c.OpenedBy,
				Fingerprint: hr.Fingerprint(), SkipRequestRecord: true}, now)
			if err != nil {
				return err
			}
			party = p
			resolved = &p.PartyID

		case domain.DecisionLinkExisting:
			named := false
			for _, cand := range c.Candidates {
				named = named || cand.PartyID == req.PartyID
			}
			if !named {
				return fmt.Errorf("%w: party %s is not one of the case's candidates", registry.ErrInvalidInput, req.PartyID)
			}
			p, err := scanParty(tx.QueryRow(ctx, `SELECT `+partyColumns+` FROM parties WHERE party_id = $1 AND tenant_id = $2`, req.PartyID, tid))
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrNotFound
			}
			if err != nil {
				return err
			}
			if err := partyWritable(p); err != nil {
				return err
			}
			party, resolved = p, &p.PartyID

		case domain.DecisionDismiss:
			status = domain.CaseDismissed

		case domain.DecisionKeepHolder:
			// The identifier stays where it is.

		case domain.DecisionKeepVerified, domain.DecisionAcceptProposed:
			p, err := s.decideFactConflict(ctx, tx, tid, c, req.Decision, actor, req.CorrelationID, now)
			if err != nil {
				return err
			}
			party, resolved = p, c.SubjectPartyID

		case domain.DecisionMoveToRequester:
			p, err := s.moveIdentifier(ctx, tx, tid, c, held, actor, req.CorrelationID, now)
			if err != nil {
				return err
			}
			party, resolved = p, c.SubjectPartyID
		}

		if err := settleCase(ctx, tx, c, status, req.Decision, actor, req.Note, resolved, now); err != nil {
			return err
		}
		rec, err := events.ConflictResolvedRecord(*c, req.CorrelationID)
		if err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
		out = &registry.CaseDecisionOutcome{Case: *c, Party: party}
		return nil
	})
	return out, err
}

// decideFactConflict keeps the verified fact (the proposal is REJECTED) or accepts the proposal: the verified fact
// is superseded, never edited, and the accepted fact is written fresh, verified now, so knowledge time stays true.
func (s *PgStore) decideFactConflict(ctx context.Context, tx pgx.Tx, tid string, c *domain.ResolutionCase, decision, actor, corr string, now time.Time) (*domain.Party, error) {
	if c.SubjectPartyID == nil || c.ProposedFactID == nil {
		return nil, fmt.Errorf("%w: the case names no fact", registry.ErrInvalidInput)
	}
	p, err := lockParty(ctx, tx, tid, *c.SubjectPartyID)
	if err != nil {
		return nil, err
	}
	var f domain.PartyFact
	var from time.Time
	var to *time.Time
	var norm string
	var tokens []string
	var lang, script *string
	err = tx.QueryRow(ctx, `SELECT fact_id, fact_type, value, value_normalized, tokens, language, script, valid_from, valid_to, status,
		source_class, evidence_ref, proposed_by FROM party_facts WHERE fact_id = $1 AND tenant_id = $2 FOR UPDATE`, *c.ProposedFactID, tid).
		Scan(&f.FactID, &f.FactType, &f.Value, &norm, &tokens, &lang, &script, &from, &to, &f.Status, &f.SourceClass, &f.EvidenceRef, &f.ProposedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if f.Status != domain.FactProposed {
		return nil, fmt.Errorf("%w: the proposed fact is already %s", registry.ErrStateConflict, f.Status)
	}
	if decision == domain.DecisionKeepVerified {
		if _, err := tx.Exec(ctx, `UPDATE party_facts SET status = 'REJECTED' WHERE fact_id = $1`, f.FactID); err != nil {
			return nil, err
		}
		return p, nil
	}

	// The verified facts it contradicted stop being the view's answer from now on, and an earlier knowledge time
	// still sees them. What is NOW believed about the time before the accepted fact starts is the old value, so a
	// copy of each, ended where the accepted fact begins, is recorded now.
	orows, err := tx.Query(ctx, `SELECT fact_id, value, value_normalized, tokens, language, script, valid_from, source_class, evidence_ref, proposed_by
		FROM party_facts WHERE tenant_id = $1 AND party_id = $2 AND fact_type = $3 AND status = 'VERIFIED' AND superseded_at IS NULL
		  AND valid_from < COALESCE($5::date, 'infinity'::date) AND COALESCE(valid_to, 'infinity'::date) > $4::date`,
		tid, p.PartyID, string(f.FactType), from, to)
	if err != nil {
		return nil, err
	}
	type oldFact struct {
		id, value, norm, class, evidence, by string
		tokens                               []string
		lang, script                         *string
		from                                 time.Time
	}
	var olds []oldFact
	for orows.Next() {
		var o oldFact
		if err := orows.Scan(&o.id, &o.value, &o.norm, &o.tokens, &o.lang, &o.script, &o.from, &o.class, &o.evidence, &o.by); err != nil {
			orows.Close()
			return nil, err
		}
		olds = append(olds, o)
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return nil, err
	}
	np, err := bumpParty(ctx, tx, tid, p.PartyID, activateOnLegalName(p, f.FactType))
	if err != nil {
		return nil, err
	}
	for _, o := range olds {
		if _, err := tx.Exec(ctx, `UPDATE party_facts SET status = 'SUPERSEDED', superseded_at = $2 WHERE fact_id = $1`, o.id, now); err != nil {
			return nil, err
		}
		if o.from.Before(from) {
			if _, err := tx.Exec(ctx, `INSERT INTO party_facts (fact_id, tenant_id, party_id, fact_type, value, value_normalized, tokens, language, script,
				valid_from, valid_to, recorded_at, status, source_class, evidence_ref, proposed_by, correlation_id, recorded_version)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'VERIFIED',$13,$14,$15,NULLIF($16,''),$17)`,
				uuid.NewString(), tid, p.PartyID, string(f.FactType), o.value, o.norm, o.tokens, o.lang, o.script, o.from, from, now,
				o.class, o.evidence, o.by, corr, np.MasterVersion); err != nil {
				return nil, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE party_facts SET status = 'PROMOTED', superseded_at = $2 WHERE fact_id = $1`, f.FactID, now); err != nil {
		return nil, err
	}
	f.FactID = uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO party_facts (fact_id, tenant_id, party_id, fact_type, value, value_normalized, tokens, language, script,
		valid_from, valid_to, recorded_at, status, source_class, evidence_ref, proposed_by, correlation_id, recorded_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'VERIFIED',$13,$14,$15,NULLIF($16,''),$17)`,
		f.FactID, tid, p.PartyID, string(f.FactType), f.Value, norm, tokens, lang, script, from, to, now,
		string(f.SourceClass), f.EvidenceRef, f.ProposedBy, corr, np.MasterVersion); err != nil {
		return nil, err
	}
	f.ValidFrom, f.ValidTo, f.Status, f.RecordedAt = fmtDate(from), fmtDatePtr(to), domain.FactVerified, now
	rec, err := events.PartyFactRecord(events.EventPartyFactPublished, *np, f, corr)
	if err != nil {
		return nil, err
	}
	return np, s.enqueue(ctx, tx, rec)
}

func activateOnLegalName(p *domain.Party, t domain.FactType) domain.PartyStatus {
	if t == domain.FactLegalName && p.Status == domain.PartyCandidate {
		return domain.PartyActive
	}
	return ""
}

// moveIdentifier gives the identifier a conflict case was about to the party that asked for it, ending the holder's.
func (s *PgStore) moveIdentifier(ctx context.Context, tx pgx.Tx, tid string, c *domain.ResolutionCase, held []byte, actor, corr string, now time.Time) (*domain.Party, error) {
	var req domain.LinkIdentifierRequest
	if c.SubjectPartyID == nil || len(held) == 0 || json.Unmarshal(held, &req) != nil {
		return nil, fmt.Errorf("%w: the case holds no identifier request", registry.ErrInvalidInput)
	}
	ids := []string{}
	for _, cand := range c.Candidates {
		ids = append(ids, cand.PartyID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p, err := lockParty(ctx, tx, tid, id)
		if err != nil {
			return nil, err
		}
		if err := partyWritable(p); err != nil {
			return nil, err
		}
	}
	scope := domain.NormalizeScope(req.Scheme, req.Scope)
	if _, err := tx.Exec(ctx, `UPDATE party_identifiers SET status = 'ENDED', ended_at = $5, ended_reason = 'MOVED_BY_DECISION'
		WHERE tenant_id = $1 AND scheme = $2 AND scope = $3 AND value_normalized = $4 AND status = 'ACTIVE'`,
		tid, string(req.Scheme), scope, domain.NormalizeIdentifier(req.Value), now); err != nil {
		return nil, err
	}
	np, err := bumpParty(ctx, tx, tid, *c.SubjectPartyID, "")
	if err != nil {
		return nil, err
	}
	ident, err := insertIdentifier(ctx, tx, tid, *c.SubjectPartyID, req.PartyIdentifierInput, req.SourceClass, req.EvidenceRef, actor, corr, now)
	if err != nil {
		return nil, err
	}
	rec, err := events.PartyIdentifierRecord(*np, *ident, actor, corr)
	if err != nil {
		return nil, err
	}
	return np, s.enqueue(ctx, tx, rec)
}

// ── Merge ────────────────────────────────────────────────────────────────────

type identRow struct {
	id, scheme, scope, value, norm, verification, class, evidence string
}

func activeIdentifiers(ctx context.Context, tx pgx.Tx, tid, partyID string) ([]identRow, error) {
	rows, err := tx.Query(ctx, `SELECT identifier_id, scheme, scope, value, value_normalized, verification_status, source_class, evidence_ref
		FROM party_identifiers WHERE tenant_id = $1 AND party_id = $2 AND status = 'ACTIVE' ORDER BY identifier_id`, tid, partyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identRow
	for rows.Next() {
		var r identRow
		if err := rows.Scan(&r.id, &r.scheme, &r.scope, &r.value, &r.norm, &r.verification, &r.class, &r.evidence); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// copyIdentifier writes r onto partyID, naming the row it came from.
func copyIdentifier(ctx context.Context, tx pgx.Tx, tid, partyID string, r identRow, actor, corr string, now time.Time) (string, error) {
	id := uuid.NewString()
	_, err := tx.Exec(ctx, `INSERT INTO party_identifiers (identifier_id, tenant_id, party_id, scheme, scope, value, value_normalized,
		verification_status, source_class, evidence_ref, linked_by, linked_at, correlation_id, moved_from_identifier_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),$14)`,
		id, tid, partyID, r.scheme, r.scope, r.value, r.norm, r.verification, r.class, r.evidence, actor, now, corr, r.id)
	return id, err
}

func endIdentifier(ctx context.Context, tx pgx.Tx, id, reason string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE party_identifiers SET status = 'ENDED', ended_at = $2, ended_reason = $3 WHERE identifier_id = $1`, id, now, reason)
	return err
}

// lockedParties locks the given parties in id order and returns them by id; ErrNotFound if any is absent.
func lockedParties(ctx context.Context, tx pgx.Tx, tid string, ids []string) (map[string]*domain.Party, error) {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	rows, err := tx.Query(ctx, `SELECT `+partyColumns+` FROM parties WHERE tenant_id = $1 AND party_id = ANY($2::uuid[]) ORDER BY party_id FOR UPDATE`, tid, sorted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*domain.Party{}
	for rows.Next() {
		p, err := scanParty(rows)
		if err != nil {
			return nil, err
		}
		out[p.PartyID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if out[id] == nil {
			return nil, fmt.Errorf("%w: party %s", registry.ErrNotFound, id)
		}
	}
	return out, nil
}

// MergeParties is MDM-05 s18.2.
func (s *PgStore) MergeParties(ctx context.Context, caseID string, req domain.MergePartiesRequest, actor string) (*registry.MergeOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.MergeOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		// 1. Lock the candidate identities against concurrent material merge and split decisions.
		if err := lockPartyMerge(ctx, tx, tid); err != nil {
			return err
		}
		c, _, err := lockCase(ctx, tx, tid, caseID)
		if err != nil {
			return err
		}
		if c == nil {
			return registry.ErrNotFound
		}
		if c.Status != domain.CaseOpen {
			return fmt.Errorf("%w: the case is already %s", registry.ErrStateConflict, c.Status)
		}
		if c.CaseType != domain.CaseDuplicateReview && c.CaseType != domain.CaseIdentifierConflict {
			return fmt.Errorf("%w: a %s case is not decided by a merge", registry.ErrInvalidInput, c.CaseType)
		}
		if actor == c.OpenedBy {
			return fmt.Errorf("%w: the opener of a case cannot decide its merge", registry.ErrSelfApproval)
		}
		named := map[string]bool{}
		for _, cand := range c.Candidates {
			named[cand.PartyID] = true
		}
		if c.SubjectPartyID != nil {
			named[*c.SubjectPartyID] = true
		}
		all := append([]string{req.SurvivorPartyID}, req.MergedPartyIDs...)
		for _, id := range all {
			if !named[id] {
				return fmt.Errorf("%w: party %s is not one of the case's parties", registry.ErrInvalidInput, id)
			}
		}

		// 2. Revalidate tenant scope, identity and current version just before the decision commits.
		parties, err := lockedParties(ctx, tx, tid, all)
		if err != nil {
			return err
		}
		survivor := parties[req.SurvivorPartyID]
		legalEntities := 0
		for _, id := range all {
			p := parties[id]
			if p.Status != domain.PartyActive && p.Status != domain.PartyCandidate {
				return fmt.Errorf("%w: party %s is %s", registry.ErrMergeProhibited, id, p.Status)
			}
			if err := checkPartyVersion(req.ExpectedMasterVersions[id], p.MasterVersion, false); err != nil {
				return err
			}
			if p.PartyKind != survivor.PartyKind {
				return fmt.Errorf("%w: a %s party is not merged with a %s party", registry.ErrMergeProhibited, p.PartyKind, survivor.PartyKind)
			}
			if p.LegalEntityID != nil {
				legalEntities++
			}
		}
		if legalEntities > 1 {
			// s18.1 tier D: a merge that would collapse distinct legal entities is prohibited.
			return fmt.Errorf("%w: it would collapse distinct legal entities", registry.ErrMergeProhibited)
		}
		for _, id := range req.MergedPartyIDs {
			var absorbed bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM parties WHERE tenant_id = $1 AND merged_into_party_id = $2)`, tid, id).Scan(&absorbed); err != nil {
				return err
			}
			if absorbed {
				return fmt.Errorf("%w: party %s already absorbed other parties; merges do not chain", registry.ErrMergeProhibited, id)
			}
		}

		now := time.Now().UTC()
		var lineage []domain.LineageEvent
		aliases := make([]domain.Party, 0, len(req.MergedPartyIDs))
		summary := map[string]any{}
		survivorLE := survivor.LegalEntityID
		mergedIDs := append([]string(nil), req.MergedPartyIDs...)
		sort.Strings(mergedIDs)

		for _, dupID := range mergedIDs {
			dup := parties[dupID]
			details := map[string]any{"previous_status": string(dup.Status)}

			// Identifiers: end the duplicate's, and give the survivor a copy that names its source unless it holds one.
			held, err := activeIdentifiers(ctx, tx, tid, survivor.PartyID)
			if err != nil {
				return err
			}
			has := map[string]bool{}
			for _, h := range held {
				has[h.scheme+"|"+h.scope+"|"+h.norm] = true
			}
			dupIdents, err := activeIdentifiers(ctx, tx, tid, dupID)
			if err != nil {
				return err
			}
			moved, dropped := []string{}, 0
			for _, r := range dupIdents {
				if err := endIdentifier(ctx, tx, r.id, "MERGED", now); err != nil {
					return err
				}
				if has[r.scheme+"|"+r.scope+"|"+r.norm] {
					dropped++
					continue
				}
				if _, err := copyIdentifier(ctx, tx, tid, survivor.PartyID, r, actor, req.CorrelationID, now); err != nil {
					return err
				}
				has[r.scheme+"|"+r.scope+"|"+r.norm] = true
				moved = append(moved, r.id)
			}
			details["identifiers_moved"], details["identifiers_already_held"] = moved, dropped

			// Roles: the survivor gains the roles it lacked; the duplicate's roles end. A domain re-points what it
			// owns through the alias.
			rrows, err := tx.Query(ctx, `SELECT role_id, role_type, valid_from, evidence_ref FROM party_roles
				WHERE tenant_id = $1 AND party_id = $2 AND removed_at IS NULL ORDER BY role_id`, tid, dupID)
			if err != nil {
				return err
			}
			type roleRow struct {
				id, typ, evidence string
				from              time.Time
			}
			var roles []roleRow
			for rrows.Next() {
				var r roleRow
				if err := rrows.Scan(&r.id, &r.typ, &r.from, &r.evidence); err != nil {
					rrows.Close()
					return err
				}
				roles = append(roles, r)
			}
			rrows.Close()
			if err := rrows.Err(); err != nil {
				return err
			}
			rolesMoved, rolesHeld := []string{}, []string{}
			for _, r := range roles {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM party_roles WHERE tenant_id = $1 AND party_id = $2
					AND role_type = $3 AND removed_at IS NULL)`, tid, survivor.PartyID, r.typ).Scan(&exists); err != nil {
					return err
				}
				if exists {
					rolesHeld = append(rolesHeld, r.typ)
				} else {
					if _, err := tx.Exec(ctx, `INSERT INTO party_roles (role_id, tenant_id, party_id, role_type, valid_from, evidence_ref, added_by, added_at, correlation_id)
						VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''))`, uuid.NewString(), tid, survivor.PartyID, r.typ, r.from,
						"merged from party "+dupID+": "+r.evidence, actor, now, req.CorrelationID); err != nil {
						return err
					}
					rolesMoved = append(rolesMoved, r.typ)
				}
				if _, err := tx.Exec(ctx, `UPDATE party_roles SET valid_to = GREATEST(valid_from, CURRENT_DATE), removed_by = $2, removed_at = $3,
					remove_reason = 'party merged into ' || $4::text WHERE role_id = $1`, r.id, actor, now, survivor.PartyID); err != nil {
					return err
				}
			}
			details["roles_moved"], details["roles_already_held"] = rolesMoved, rolesHeld

			// A legal entity is one party: the survivor adopts the link (the duplicate's is released first).
			if dup.LegalEntityID != nil {
				if _, err := tx.Exec(ctx, `UPDATE parties SET legal_entity_id = NULL WHERE party_id = $1`, dupID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE parties SET legal_entity_id = $2 WHERE party_id = $1`, survivor.PartyID, *dup.LegalEntityID); err != nil {
					return err
				}
				survivorLE = dup.LegalEntityID
				details["legal_entity_adopted"] = *dup.LegalEntityID
			}

			merged, err := scanParty(tx.QueryRow(ctx, `UPDATE parties SET status = 'MERGED', merged_into_party_id = $3,
				master_version = master_version + 1 WHERE party_id = $1 AND tenant_id = $2 RETURNING `+partyColumns, dupID, tid, survivor.PartyID))
			if err != nil {
				return err
			}
			aliases = append(aliases, *merged)

			raw, _ := json.Marshal(details)
			le := domain.LineageEvent{LineageID: uuid.NewString(), EventType: domain.LineageMerge, CaseID: caseID, SurvivorID: survivor.PartyID,
				PartyID: dupID, Actor: actor, IndependentOf: c.OpenedBy, Reason: req.Reason, EvidenceRef: req.EvidenceRef, OccurredAt: now, Details: details}
			if _, err := tx.Exec(ctx, `INSERT INTO party_lineage (lineage_id, tenant_id, event_type, case_id, survivor_id, party_id, actor,
				independent_of, reason, evidence_ref, occurred_at, correlation_id, details) VALUES ($1,$2,'MERGE',$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12)`,
				le.LineageID, tid, caseID, survivor.PartyID, dupID, actor, c.OpenedBy, req.Reason, req.EvidenceRef, now, req.CorrelationID, raw); err != nil {
				return err
			}
			lineage = append(lineage, le)
			summary[dupID] = details
		}
		_ = survivorLE

		np, err := bumpParty(ctx, tx, tid, survivor.PartyID, "")
		if err != nil {
			return err
		}
		resolved := np.PartyID
		if err := settleCase(ctx, tx, c, domain.CaseResolved, domain.DecisionMerge, actor, req.Reason, &resolved, now); err != nil {
			return err
		}
		rec, err := events.PartiesMergedRecord(*np, mergedIDs, caseID, actor, req.CorrelationID, summary)
		if err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
		if rec, err = events.ConflictResolvedRecord(*c, req.CorrelationID); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
		fresh, err := scanParty(tx.QueryRow(ctx, `SELECT `+partyColumns+` FROM parties WHERE party_id = $1`, np.PartyID))
		if err != nil {
			return err
		}
		out = &registry.MergeOutcome{Survivor: *fresh, Aliases: aliases, Lineage: lineage, Case: *c}
		return nil
	})
	return out, err
}

// ── Split ────────────────────────────────────────────────────────────────────

// SplitParties corrects a merge: the party is distinct again, and the merge stays in the lineage.
func (s *PgStore) SplitParties(ctx context.Context, caseID string, req domain.SplitPartiesRequest, actor string) (*registry.SplitOutcome, error) {
	tid := domain.TenantFromContext(ctx)
	var out *registry.SplitOutcome
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := lockPartyMerge(ctx, tx, tid); err != nil {
			return err
		}
		c, _, err := lockCase(ctx, tx, tid, caseID)
		if err != nil {
			return err
		}
		if c == nil {
			return registry.ErrNotFound
		}

		// The latest word on this party must be the merge this case made: not another case's, and not already undone.
		var lin domain.LineageEvent
		var rawDetails []byte
		err = tx.QueryRow(ctx, `SELECT lineage_id, event_type, case_id, survivor_id, party_id, actor, independent_of, reason, evidence_ref, occurred_at, details
			FROM party_lineage WHERE tenant_id = $1 AND party_id = $2 ORDER BY occurred_at DESC, lineage_id DESC LIMIT 1`, tid, req.MergedPartyID).
			Scan(&lin.LineageID, &lin.EventType, &lin.CaseID, &lin.SurvivorID, &lin.PartyID, &lin.Actor, &lin.IndependentOf,
				&lin.Reason, &lin.EvidenceRef, &lin.OccurredAt, &rawDetails)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: party %s was never merged", registry.ErrStateConflict, req.MergedPartyID)
		}
		if err != nil {
			return err
		}
		if lin.EventType != domain.LineageMerge || lin.CaseID != caseID {
			return fmt.Errorf("%w: party %s was not merged by this case, or was already split", registry.ErrStateConflict, req.MergedPartyID)
		}
		_ = json.Unmarshal(rawDetails, &lin.Details)
		if actor == lin.Actor {
			return fmt.Errorf("%w: the principal who merged cannot split", registry.ErrSelfApproval)
		}

		parties, err := lockedParties(ctx, tx, tid, []string{lin.SurvivorID, req.MergedPartyID})
		if err != nil {
			return err
		}
		survivor, alias := parties[lin.SurvivorID], parties[req.MergedPartyID]
		if alias.Status != domain.PartyMerged || alias.MergedInto == nil || *alias.MergedInto != lin.SurvivorID {
			return fmt.Errorf("%w: party %s is not merged into %s", registry.ErrStateConflict, req.MergedPartyID, lin.SurvivorID)
		}
		if survivor.Status == domain.PartyMerged || survivor.Status == domain.PartyInactive {
			return fmt.Errorf("%w: the survivor is %s", registry.ErrMergeProhibited, survivor.Status)
		}
		for _, p := range []*domain.Party{alias, survivor} {
			if err := checkPartyVersion(req.ExpectedMasterVersions[p.PartyID], p.MasterVersion, false); err != nil {
				return err
			}
		}

		now := time.Now().UTC()
		details := map[string]any{"restores_merge": lin.LineageID}

		// Identifiers that moved to the survivor go back: the survivor's copy ends, the alias gets a new row.
		crow, err := tx.Query(ctx, `SELECT c.identifier_id, c.scheme, c.scope, c.value, c.value_normalized, c.verification_status, c.source_class, c.evidence_ref
			FROM party_identifiers c JOIN party_identifiers o ON o.identifier_id = c.moved_from_identifier_id
			WHERE c.tenant_id = $1 AND c.party_id = $2 AND c.status = 'ACTIVE' AND o.party_id = $3 AND o.ended_reason = 'MERGED'
			ORDER BY c.identifier_id`, tid, survivor.PartyID, alias.PartyID)
		if err != nil {
			return err
		}
		var copies []identRow
		for crow.Next() {
			var r identRow
			if err := crow.Scan(&r.id, &r.scheme, &r.scope, &r.value, &r.norm, &r.verification, &r.class, &r.evidence); err != nil {
				crow.Close()
				return err
			}
			copies = append(copies, r)
		}
		crow.Close()
		if err := crow.Err(); err != nil {
			return err
		}
		restored := []string{}
		for _, r := range copies {
			if err := endIdentifier(ctx, tx, r.id, "SPLIT", now); err != nil {
				return err
			}
			id, err := copyIdentifier(ctx, tx, tid, alias.PartyID, r, actor, req.CorrelationID, now)
			if err != nil {
				return err
			}
			restored = append(restored, id)
		}
		details["identifiers_restored"] = restored

		if adopted, _ := lin.Details["legal_entity_adopted"].(string); adopted != "" && survivor.LegalEntityID != nil && *survivor.LegalEntityID == adopted {
			if _, err := tx.Exec(ctx, `UPDATE parties SET legal_entity_id = NULL WHERE party_id = $1`, survivor.PartyID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE parties SET legal_entity_id = $2 WHERE party_id = $1`, alias.PartyID, adopted); err != nil {
				return err
			}
			details["legal_entity_returned"] = adopted
		}

		prev, _ := lin.Details["previous_status"].(string)
		if prev == "" {
			prev = string(domain.PartyCandidate)
		}
		restoredParty, err := scanParty(tx.QueryRow(ctx, `UPDATE parties SET status = $3, merged_into_party_id = NULL,
			master_version = master_version + 1 WHERE party_id = $1 AND tenant_id = $2 RETURNING `+partyColumns, alias.PartyID, tid, prev))
		if err != nil {
			return err
		}
		np, err := bumpParty(ctx, tx, tid, survivor.PartyID, "")
		if err != nil {
			return err
		}

		raw, _ := json.Marshal(details)
		le := domain.LineageEvent{LineageID: uuid.NewString(), EventType: domain.LineageSplit, CaseID: caseID, SurvivorID: survivor.PartyID,
			PartyID: alias.PartyID, Actor: actor, IndependentOf: lin.Actor, Reason: req.Reason, EvidenceRef: req.EvidenceRef, OccurredAt: now, Details: details}
		if _, err := tx.Exec(ctx, `INSERT INTO party_lineage (lineage_id, tenant_id, event_type, case_id, survivor_id, party_id, actor,
			independent_of, reason, evidence_ref, occurred_at, correlation_id, details) VALUES ($1,$2,'SPLIT',$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12)`,
			le.LineageID, tid, caseID, survivor.PartyID, alias.PartyID, actor, lin.Actor, req.Reason, req.EvidenceRef, now, req.CorrelationID, raw); err != nil {
			return err
		}
		rec, err := events.PartySplitRecord(*np, alias.PartyID, caseID, actor, req.CorrelationID, details)
		if err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, rec); err != nil {
			return err
		}
		out = &registry.SplitOutcome{Restored: *restoredParty, Survivor: *np, Lineage: le}
		return nil
	})
	return out, err
}

// ── History ──────────────────────────────────────────────────────────────────

// ExplainMergeHistory answers where a party's id resolves and every merge and split that touched it.
func (s *PgStore) ExplainMergeHistory(ctx context.Context, partyID string) (*domain.MergeHistory, error) {
	tid := domain.TenantFromContext(ctx)
	if _, err := uuid.Parse(partyID); err != nil {
		return nil, nil
	}
	var out *domain.MergeHistory
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		p, err := scanParty(tx.QueryRow(ctx, `SELECT `+partyColumns+` FROM parties WHERE party_id = $1 AND tenant_id = $2`, partyID, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		h := &domain.MergeHistory{PartyID: partyID, Status: p.Status, ResolvesTo: partyID, AliasIDs: []string{}, Lineage: []domain.LineageEvent{}}
		if p.MergedInto != nil {
			h.ResolvesTo = *p.MergedInto
		}
		arows, err := tx.Query(ctx, `SELECT party_id FROM parties WHERE tenant_id = $1 AND merged_into_party_id = $2 ORDER BY party_id`, tid, partyID)
		if err != nil {
			return err
		}
		for arows.Next() {
			var id string
			if err := arows.Scan(&id); err != nil {
				arows.Close()
				return err
			}
			h.AliasIDs = append(h.AliasIDs, id)
		}
		arows.Close()
		if err := arows.Err(); err != nil {
			return err
		}
		lrows, err := tx.Query(ctx, `SELECT lineage_id, event_type, case_id, survivor_id, party_id, actor, independent_of, reason, evidence_ref, occurred_at, details
			FROM party_lineage WHERE tenant_id = $1 AND (party_id = $2 OR survivor_id = $2) ORDER BY occurred_at, lineage_id`, tid, partyID)
		if err != nil {
			return err
		}
		defer lrows.Close()
		for lrows.Next() {
			var l domain.LineageEvent
			var raw []byte
			if err := lrows.Scan(&l.LineageID, &l.EventType, &l.CaseID, &l.SurvivorID, &l.PartyID, &l.Actor, &l.IndependentOf,
				&l.Reason, &l.EvidenceRef, &l.OccurredAt, &raw); err != nil {
				return err
			}
			_ = json.Unmarshal(raw, &l.Details)
			h.Lineage = append(h.Lineage, l)
		}
		out = h
		return lrows.Err()
	})
	return out, err
}

var _ registry.PartyResolutionStore = (*PgStore)(nil)
