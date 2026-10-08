package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 8 persistence: the jurisdiction rollout portfolio, its
// Definition of Ready and Definition of Done evidence, expert approvals, and the
// launch gate. See domain/rollout.go. No regulatory content lives here.

// RolloutRecord is one portfolio entry.
type RolloutRecord struct {
	RolloutID        string    `json:"rollout_id"`
	FamilyRef        string    `json:"family_ref"`
	DisplayName      string    `json:"display_name"`
	Layer            string    `json:"layer"`
	JurisdictionCode *string   `json:"jurisdiction_code"`
	ScopeNote        string    `json:"scope_note"`
	Owner            string    `json:"owner"`
	SupportOwner     *string   `json:"support_owner"`
	Status           string    `json:"status"`
	CreatedBy        string    `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ChecklistState is the current answer for one checklist item.
type ChecklistState struct {
	Code        string     `json:"code"`
	Checklist   string     `json:"checklist"`
	Text        string     `json:"text"`
	Met         bool       `json:"met"`
	EvidenceRef string     `json:"evidence_ref,omitempty"`
	AttestedBy  string     `json:"attested_by,omitempty"`
	AttestedAt  *time.Time `json:"attested_at,omitempty"`
}

// ExpertApprovalRecord is one qualified expert decision.
type ExpertApprovalRecord struct {
	Expert        string    `json:"expert"`
	Qualification string    `json:"qualification"`
	Scope         string    `json:"scope"`
	Decision      string    `json:"decision"`
	Notes         string    `json:"notes"`
	DecidedAt     time.Time `json:"decided_at"`
}

// RolloutEventRecord is one status change.
type RolloutEventRecord struct {
	FromStatus *string   `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Actor      string    `json:"actor"`
	Reason     string    `json:"reason"`
	OccurredAt time.Time `json:"occurred_at"`
}

// RolloutDetail is a rollout with its evidence and its next-step blockers.
type RolloutDetail struct {
	Rollout   *RolloutRecord          `json:"rollout"`
	Checklist []ChecklistState        `json:"checklist"`
	Approvals []*ExpertApprovalRecord `json:"expert_approvals"`
	Packs     []string                `json:"pack_refs"`
	Events    []*RolloutEventRecord   `json:"events"`
	// Blockers maps each status the rollout could move to onto what stops it today.
	Blockers map[string][]string `json:"blockers"`
}

const rolloutCols = `rollout_id::text, family_ref, display_name, layer, jurisdiction_code, scope_note, owner, support_owner, status, created_by, created_at, updated_at`

func scanRollout(r pgx.Row) (*RolloutRecord, error) {
	var x RolloutRecord
	err := r.Scan(&x.RolloutID, &x.FamilyRef, &x.DisplayName, &x.Layer, &x.JurisdictionCode, &x.ScopeNote, &x.Owner, &x.SupportOwner, &x.Status, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt)
	return &x, err
}

// CreateRolloutParams adds one portfolio entry.
type CreateRolloutParams struct {
	FamilyRef, DisplayName, Layer, ScopeNote, Owner, CreatedBy string
	JurisdictionCode, SupportOwner                             *string
}

// CreateRollout adds a portfolio entry in PLANNED.
func (s *PgStore) CreateRollout(ctx context.Context, p CreateRolloutParams) (*RolloutRecord, error) {
	r, err := scanRollout(s.pool.QueryRow(ctx, `INSERT INTO jurisdiction_rollouts
		(family_ref, display_name, layer, jurisdiction_code, scope_note, owner, support_owner, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+rolloutCols,
		p.FamilyRef, p.DisplayName, p.Layer, p.JurisdictionCode, p.ScopeNote, p.Owner, p.SupportOwner, p.CreatedBy))
	if err != nil {
		return nil, s.registryFail("CreateRollout", err, nil)
	}
	return r, nil
}

// SeedPortfolio adds the s32 initial portfolio as PLANNED entries with no owner. It is idempotent.
// Seeding states intent only: no entry means certified rules exist.
func (s *PgStore) SeedPortfolio(ctx context.Context, actor string) (created, existing int, err error) {
	for _, e := range domain.InitialPortfolio {
		var code *string
		if e.JurisdictionCode != "" {
			c := e.JurisdictionCode
			code = &c
		}
		tag, err := s.pool.Exec(ctx, `INSERT INTO jurisdiction_rollouts (family_ref, display_name, layer, jurisdiction_code, scope_note, owner, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (family_ref) DO NOTHING`, e.FamilyRef, e.DisplayName, e.Layer, code, e.Note, domain.UnassignedOwner, actor)
		if err != nil {
			return created, existing, s.registryFail("SeedPortfolio", err, nil)
		}
		if tag.RowsAffected() == 1 {
			created++
		} else {
			existing++
		}
	}
	return created, existing, nil
}

// ListRollouts lists portfolio entries, optionally by status.
func (s *PgStore) ListRollouts(ctx context.Context, status string, limit, offset int) ([]*RolloutRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+rolloutCols+` FROM jurisdiction_rollouts WHERE ($1 = '' OR status = $1) ORDER BY family_ref LIMIT $2 OFFSET $3`, status, limit, offset)
	if err != nil {
		return nil, s.registryFail("ListRollouts", err, nil)
	}
	defer rows.Close()
	out := []*RolloutRecord{}
	for rows.Next() {
		r, err := scanRollout(rows)
		if err != nil {
			return nil, s.registryFail("ListRollouts scan", err, nil)
		}
		out = append(out, r)
	}
	return out, s.registryFail("ListRollouts rows", rows.Err(), nil)
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// snapshot reads the state a transition is judged on.
func (s *PgStore) snapshot(ctx context.Context, q rowQuerier, id string, lock bool) (*RolloutRecord, domain.RolloutSnapshot, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	rec, err := scanRollout(q.QueryRow(ctx, `SELECT `+rolloutCols+` FROM jurisdiction_rollouts WHERE rollout_id::text=$1`+suffix, id))
	if err != nil {
		return nil, domain.RolloutSnapshot{}, s.registryFail("rollout snapshot", err, domain.ErrRolloutNotFound)
	}
	snap := domain.RolloutSnapshot{Status: rec.Status, Owner: rec.Owner, Met: map[string]bool{}}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (item_code) item_code, met FROM rollout_attestations WHERE rollout_id=$1::uuid
		ORDER BY item_code, attested_at DESC, attestation_id DESC`, rec.RolloutID)
	if err != nil {
		return nil, snap, s.registryFail("rollout snapshot attestations", err, nil)
	}
	for rows.Next() {
		var code string
		var met bool
		if err := rows.Scan(&code, &met); err != nil {
			rows.Close()
			return nil, snap, s.registryFail("rollout snapshot attestations scan", err, nil)
		}
		snap.Met[code] = met
	}
	rows.Close()
	rows, err = q.Query(ctx, `SELECT DISTINCT ON (expert) expert, decision FROM rollout_expert_approvals WHERE rollout_id=$1::uuid
		ORDER BY expert, decided_at DESC, approval_id DESC`, rec.RolloutID)
	if err != nil {
		return nil, snap, s.registryFail("rollout snapshot experts", err, nil)
	}
	for rows.Next() {
		var who, decision string
		if err := rows.Scan(&who, &decision); err != nil {
			rows.Close()
			return nil, snap, s.registryFail("rollout snapshot experts scan", err, nil)
		}
		snap.Experts = append(snap.Experts, who)
		if decision == "APPROVE" {
			snap.ApprovedExperts++
		}
	}
	rows.Close()
	if err := q.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE EXISTS (SELECT 1 FROM jurisdiction_pack_versions v WHERE v.pack_id = rp.pack_id AND v.status = 'RELEASED'))
		FROM rollout_packs rp WHERE rp.rollout_id=$1::uuid`, rec.RolloutID).Scan(&snap.LinkedPacks, &snap.ReleasedLinked); err != nil {
		return nil, snap, s.registryFail("rollout snapshot packs", err, nil)
	}
	return rec, snap, nil
}

// GetRollout returns a rollout with its checklist state, expert decisions, packs, history and blockers.
func (s *PgStore) GetRollout(ctx context.Context, id string) (*RolloutDetail, error) {
	rec, snap, err := s.snapshot(ctx, s.pool, id, false)
	if err != nil {
		return nil, err
	}
	d := &RolloutDetail{Rollout: rec, Checklist: []ChecklistState{}, Approvals: []*ExpertApprovalRecord{}, Packs: []string{}, Events: []*RolloutEventRecord{}, Blockers: map[string][]string{}}

	latest := map[string]ChecklistState{}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (item_code) item_code, met, evidence_ref, attested_by, attested_at FROM rollout_attestations
		WHERE rollout_id=$1::uuid ORDER BY item_code, attested_at DESC, attestation_id DESC`, rec.RolloutID)
	if err != nil {
		return nil, s.registryFail("GetRollout attestations", err, nil)
	}
	for rows.Next() {
		var c ChecklistState
		var at time.Time
		if err := rows.Scan(&c.Code, &c.Met, &c.EvidenceRef, &c.AttestedBy, &at); err != nil {
			rows.Close()
			return nil, s.registryFail("GetRollout attestations scan", err, nil)
		}
		c.AttestedAt = &at
		latest[c.Code] = c
	}
	rows.Close()
	for _, list := range [][]domain.ChecklistItem{domain.ReadyChecklist, domain.DoneChecklist} {
		for _, it := range list {
			c := latest[it.Code]
			c.Code, c.Checklist, c.Text = it.Code, it.Checklist, it.Text
			d.Checklist = append(d.Checklist, c)
		}
	}

	arows, err := s.pool.Query(ctx, `SELECT expert, qualification, scope, decision, notes, decided_at FROM rollout_expert_approvals WHERE rollout_id=$1::uuid ORDER BY decided_at, approval_id`, rec.RolloutID)
	if err != nil {
		return nil, s.registryFail("GetRollout approvals", err, nil)
	}
	for arows.Next() {
		var a ExpertApprovalRecord
		if err := arows.Scan(&a.Expert, &a.Qualification, &a.Scope, &a.Decision, &a.Notes, &a.DecidedAt); err != nil {
			arows.Close()
			return nil, s.registryFail("GetRollout approvals scan", err, nil)
		}
		d.Approvals = append(d.Approvals, &a)
	}
	arows.Close()

	prows, err := s.pool.Query(ctx, `SELECT p.pack_ref FROM rollout_packs rp JOIN jurisdiction_packs p USING (pack_id) WHERE rp.rollout_id=$1::uuid ORDER BY p.pack_ref`, rec.RolloutID)
	if err != nil {
		return nil, s.registryFail("GetRollout packs", err, nil)
	}
	for prows.Next() {
		var ref string
		if err := prows.Scan(&ref); err != nil {
			prows.Close()
			return nil, s.registryFail("GetRollout packs scan", err, nil)
		}
		d.Packs = append(d.Packs, ref)
	}
	prows.Close()

	erows, err := s.pool.Query(ctx, `SELECT from_status, to_status, actor, reason, occurred_at FROM rollout_events WHERE rollout_id=$1::uuid ORDER BY occurred_at, event_id`, rec.RolloutID)
	if err != nil {
		return nil, s.registryFail("GetRollout events", err, nil)
	}
	for erows.Next() {
		var e RolloutEventRecord
		if err := erows.Scan(&e.FromStatus, &e.ToStatus, &e.Actor, &e.Reason, &e.OccurredAt); err != nil {
			erows.Close()
			return nil, s.registryFail("GetRollout events scan", err, nil)
		}
		d.Events = append(d.Events, &e)
	}
	erows.Close()

	for _, to := range []string{domain.RolloutAuthoring, domain.RolloutReady, domain.RolloutLaunched, domain.RolloutSuspended, domain.RolloutRetired} {
		if b := domain.TransitionBlockers(snap, to, "", "x"); len(b) > 0 && !strings.HasPrefix(b[0], "a rollout in ") {
			d.Blockers[to] = b
		} else if len(b) == 0 {
			d.Blockers[to] = []string{}
		}
	}
	return d, nil
}

// SetRolloutOwner assigns the accountable owner and support owner. The new owner must not already have
// attested to or approved this rollout (they would be marking their own work).
func (s *PgStore) SetRolloutOwner(ctx context.Context, id, owner string, supportOwner *string) (*RolloutRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, s.registryFail("SetRolloutOwner begin", err, nil)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rec, snap, err := s.snapshot(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if rec.Status == domain.RolloutLaunched || rec.Status == domain.RolloutRetired {
		return nil, fmt.Errorf("%w: the owner of a %s rollout cannot change", domain.ErrConflict, rec.Status)
	}
	var clash int
	if err := tx.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM rollout_attestations WHERE rollout_id=$1::uuid AND attested_by=$2)
		+ (SELECT COUNT(*) FROM rollout_expert_approvals WHERE rollout_id=$1::uuid AND expert=$2)`, rec.RolloutID, owner).Scan(&clash); err != nil {
		return nil, s.registryFail("SetRolloutOwner clash", err, nil)
	}
	_ = snap
	if clash > 0 {
		return nil, fmt.Errorf("%w: %s has already attested to or approved this rollout and cannot own it", domain.ErrNotIndependent, owner)
	}
	out, err := scanRollout(tx.QueryRow(ctx, `UPDATE jurisdiction_rollouts SET owner=$2, support_owner=$3, updated_at=NOW() WHERE rollout_id::text=$1 RETURNING `+rolloutCols, id, owner, supportOwner))
	if err != nil {
		return nil, s.registryFail("SetRolloutOwner", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("SetRolloutOwner commit", err, nil)
	}
	return out, nil
}

// LinkRolloutPacks links packs (by pack_ref) to the rollout. Linking is additive and idempotent.
func (s *PgStore) LinkRolloutPacks(ctx context.Context, id string, packRefs []string, actor string) error {
	if _, err := scanRollout(s.pool.QueryRow(ctx, `SELECT `+rolloutCols+` FROM jurisdiction_rollouts WHERE rollout_id::text=$1`, id)); err != nil {
		return s.registryFail("LinkRolloutPacks", err, domain.ErrRolloutNotFound)
	}
	for _, ref := range packRefs {
		tag, err := s.pool.Exec(ctx, `INSERT INTO rollout_packs (rollout_id, pack_id, linked_by)
			SELECT $1::uuid, pack_id, $3 FROM jurisdiction_packs WHERE pack_ref=$2 ON CONFLICT DO NOTHING`, id, ref, actor)
		if err != nil {
			return s.registryFail("LinkRolloutPacks insert", err, nil)
		}
		if tag.RowsAffected() == 0 {
			var n int
			if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM jurisdiction_packs WHERE pack_ref=$1`, ref).Scan(&n); err != nil {
				return s.registryFail("LinkRolloutPacks check", err, nil)
			}
			if n == 0 {
				return domain.ErrPackNotFound
			}
		}
	}
	return nil
}

// AddAttestation records one Definition of Ready or Done answer. The latest answer for an item is current.
func (s *PgStore) AddAttestation(ctx context.Context, id, checklist, item string, met bool, evidence, actor string) error {
	if _, err := scanRollout(s.pool.QueryRow(ctx, `SELECT `+rolloutCols+` FROM jurisdiction_rollouts WHERE rollout_id::text=$1`, id)); err != nil {
		return s.registryFail("AddAttestation", err, domain.ErrRolloutNotFound)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO rollout_attestations (rollout_id, checklist, item_code, met, evidence_ref, attested_by) VALUES ($1::uuid,$2,$3,$4,$5,$6)`,
		id, checklist, item, met, evidence, actor)
	return s.registryFail("AddAttestation insert", err, nil)
}

// AddExpertApproval records a qualified expert decision.
func (s *PgStore) AddExpertApproval(ctx context.Context, id, expert, qualification, scope, decision, notes string) error {
	if _, err := scanRollout(s.pool.QueryRow(ctx, `SELECT `+rolloutCols+` FROM jurisdiction_rollouts WHERE rollout_id::text=$1`, id)); err != nil {
		return s.registryFail("AddExpertApproval", err, domain.ErrRolloutNotFound)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO rollout_expert_approvals (rollout_id, expert, qualification, scope, decision, notes) VALUES ($1::uuid,$2,$3,$4,$5,$6)`,
		id, expert, qualification, scope, decision, notes)
	return s.registryFail("AddExpertApproval insert", err, nil)
}

// TransitionRollout moves a rollout, refusing with the full list of blockers when a gate is not met.
// The database enforces the same gates as a second line of defence.
func (s *PgStore) TransitionRollout(ctx context.Context, id, to, actor, reason string) (*RolloutRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, s.registryFail("TransitionRollout begin", err, nil)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rec, snap, err := s.snapshot(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if b := domain.TransitionBlockers(snap, to, actor, reason); len(b) > 0 {
		return nil, &domain.RolloutBlockedError{Blockers: b}
	}
	out, err := scanRollout(tx.QueryRow(ctx, `UPDATE jurisdiction_rollouts SET status=$2 WHERE rollout_id::text=$1 RETURNING `+rolloutCols, id, to))
	if err != nil {
		return nil, s.registryFail("TransitionRollout update", err, nil)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rollout_events (rollout_id, from_status, to_status, actor, reason) VALUES ($1::uuid,$2,$3,$4,$5)`, rec.RolloutID, rec.Status, to, actor, reason); err != nil {
		return nil, s.registryFail("TransitionRollout event", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("TransitionRollout commit", err, nil)
	}
	return out, nil
}

// SupportView answers whether a jurisdiction is supported for production. It never guesses (JUR-NEG-22).
type SupportView struct {
	JurisdictionCode string        `json:"jurisdiction_code"`
	Supported        bool          `json:"supported"`
	Outcome          string        `json:"outcome"`
	Rollouts         []SupportItem `json:"rollouts"`
	Explanation      string        `json:"explanation"`
}

// SupportItem is one rollout that names the jurisdiction.
type SupportItem struct {
	FamilyRef string `json:"family_ref"`
	Status    string `json:"status"`
}

// JurisdictionSupport reports production support for a jurisdiction code: it is supported only when a
// LAUNCHED rollout names it AND one of that rollout's packs has a RELEASED version that covers it.
func (s *PgStore) JurisdictionSupport(ctx context.Context, code string) (*SupportView, error) {
	rows, err := s.pool.Query(ctx, `SELECT rollout_id::text, family_ref, status FROM jurisdiction_rollouts
		WHERE jurisdiction_code=$1 AND status <> 'RETIRED' ORDER BY family_ref`, code)
	if err != nil {
		return nil, s.registryFail("JurisdictionSupport", err, nil)
	}
	v := &SupportView{JurisdictionCode: code, Rollouts: []SupportItem{}}
	var launched []string
	suspended := false
	for rows.Next() {
		var id, fam, st string
		if err := rows.Scan(&id, &fam, &st); err != nil {
			rows.Close()
			return nil, s.registryFail("JurisdictionSupport scan", err, nil)
		}
		v.Rollouts = append(v.Rollouts, SupportItem{FamilyRef: fam, Status: st})
		if st == domain.RolloutLaunched {
			launched = append(launched, id)
		}
		suspended = suspended || st == domain.RolloutSuspended
	}
	rows.Close()
	switch {
	case len(v.Rollouts) == 0:
		v.Outcome, v.Explanation = domain.SupportNoRollout, "no rollout names this jurisdiction: it is not supported and no tax treatment is guessed"
	case len(launched) > 0:
		covered := false
		for _, id := range launched {
			if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM rollout_packs rp
				JOIN jurisdiction_pack_versions v ON v.pack_id = rp.pack_id AND v.status = 'RELEASED'
				JOIN pack_version_jurisdictions pj ON pj.pack_version_id = v.pack_version_id
				JOIN jurisdictions j ON j.jurisdiction_id = pj.jurisdiction_id
				WHERE rp.rollout_id=$1::uuid AND j.jurisdiction_code=$2)`, id, code).Scan(&covered); err != nil {
				return nil, s.registryFail("JurisdictionSupport coverage", err, nil)
			}
			if covered {
				break
			}
		}
		if covered {
			v.Supported, v.Outcome, v.Explanation = true, domain.SupportSupported, "a launched rollout names this jurisdiction and a released pack covers it"
		} else {
			v.Outcome, v.Explanation = domain.SupportNoReleasedPk, "the rollout is launched but no linked pack with a RELEASED version covers this jurisdiction (for example it was withdrawn)"
		}
	case suspended:
		v.Outcome, v.Explanation = domain.SupportSuspended, "support for this jurisdiction is suspended"
	default:
		v.Outcome, v.Explanation = domain.SupportNotYet, "a rollout exists but has not been launched: local expert review and certification are not complete"
	}
	return v, nil
}
