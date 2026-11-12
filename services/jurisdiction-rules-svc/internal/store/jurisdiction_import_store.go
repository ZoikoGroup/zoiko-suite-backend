package store

// REF-01 ImportJurisdictionUpdate (migration 000029): propose, approve or
// reject an externally sourced update to the jurisdiction registry.
//
// Proposing validates every row against the registry as it is now and keeps the
// result, row by row; it changes nothing. Approving, by someone other than the
// proposer, applies every row in one transaction: if any row cannot be applied
// none is, and the error names the row. There is no partial silent import.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

const importBatchColumns = `batch_id, source_standard, source_version, import_hash, evidence_ref, summary,
	status, item_count, invalid_count, proposed_by, proposed_at, decided_by, decided_at, decision_reason,
	added_count, updated_count, retired_count, unchanged_count`

const importItemColumns = `item_id, item_no, action, target_ref_id, target_code, target_type, jurisdiction_name,
	authority_type, parent_ref_id, parent_code, parent_type, effective_from, successor_ref_id, successor_code,
	successor_type, note, validation_errors, outcome, jurisdiction_id`

// hierarchyLockKey serialises every change of the hierarchy, so two approved
// imports cannot move jurisdictions in opposite directions at once and leave a
// cycle that neither saw.
const hierarchyLockKey = "jurisdiction_hierarchy"

func scanImportBatch(row pgx.Row) (*domain.ImportBatch, error) {
	var b domain.ImportBatch
	err := row.Scan(&b.BatchID, &b.SourceStandard, &b.SourceVersion, &b.ImportHash, &b.EvidenceRef, &b.Summary,
		&b.Status, &b.ItemCount, &b.InvalidCount, &b.ProposedBy, &b.ProposedAt, &b.DecidedBy, &b.DecidedAt, &b.DecisionReason,
		&b.Added, &b.Updated, &b.Retired, &b.Unchanged)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func nilIfBlank(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func refParts(r *domain.JurisdictionRef) (id, code, typ *string) {
	if r == nil {
		return nil, nil, nil
	}
	return nilIfBlank(r.JurisdictionID), nilIfBlank(r.Code), nilIfBlank(r.Type)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func refOf(id, code, typ *string) *domain.JurisdictionRef {
	if id == nil && code == nil && typ == nil {
		return nil
	}
	return &domain.JurisdictionRef{JurisdictionID: deref(id), Code: deref(code), Type: deref(typ)}
}

// loadImportItems returns a batch's rows in order, with their row ids alongside.
func (s *PgStore) loadImportItems(ctx context.Context, q Querier, batchID string) ([]domain.ImportItem, []string, error) {
	rows, err := q.Query(ctx, `SELECT `+importItemColumns+` FROM jurisdiction_import_items WHERE batch_id = $1 ORDER BY item_no`, batchID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	var items []domain.ImportItem
	var ids []string
	for rows.Next() {
		var it domain.ImportItem
		var itemID string
		var tID, tCode, tType, name, auth, pID, pCode, pType, sID, sCode, sType, note, outcome, jID *string
		if err := rows.Scan(&itemID, &it.ItemNo, &it.Action, &tID, &tCode, &tType, &name, &auth, &pID, &pCode, &pType,
			&it.EffectiveFrom, &sID, &sCode, &sType, &note, &it.ValidationErrors, &outcome, &jID); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if t := refOf(tID, tCode, tType); t != nil {
			it.Target = *t
		}
		it.JurisdictionName, it.AuthorityType, it.Note = deref(name), deref(auth), deref(note)
		it.Parent, it.Successor = refOf(pID, pCode, pType), refOf(sID, sCode, sType)
		it.Outcome, it.JurisdictionID = outcome, jID
		if it.ValidationErrors == nil {
			it.ValidationErrors = []string{}
		}
		items = append(items, it)
		ids = append(ids, itemID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return items, ids, nil
}

// GetJurisdictionImport returns a batch with its rows.
func (s *PgStore) GetJurisdictionImport(ctx context.Context, batchID string) (*domain.ImportBatch, error) {
	return s.getImport(ctx, s.db(ctx), batchID, false)
}

func (s *PgStore) getImport(ctx context.Context, q Querier, batchID string, lock bool) (*domain.ImportBatch, error) {
	query := `SELECT ` + importBatchColumns + ` FROM jurisdiction_import_batches WHERE batch_id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	b, err := scanImportBatch(q.QueryRow(ctx, query, batchID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidTextRepresentation(err) {
			return nil, domain.ErrImportNotFound
		}
		s.log.Error("pg getImport failed", zap.String("batch_id", batchID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	items, _, err := s.loadImportItems(ctx, q, b.BatchID)
	if err != nil {
		return nil, err
	}
	b.Items = items
	return b, nil
}

// ListJurisdictionImports lists batches, newest first, optionally of one status.
// Rows are not included: read one batch for them.
func (s *PgStore) ListJurisdictionImports(ctx context.Context, status string, limit int) ([]*domain.ImportBatch, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT `+importBatchColumns+` FROM jurisdiction_import_batches
		 WHERE ($1 = '' OR status = $1) ORDER BY proposed_at DESC, batch_id LIMIT $2`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	out := []*domain.ImportBatch{}
	for rows.Next() {
		b, err := scanImportBatch(rows)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ProposeJurisdictionImportWithQuerier is ImportJurisdictionUpdate. It returns
// the batch, and true when this call created it. The same source version with
// the same content returns the first batch (an idempotent replay); the same
// source version with different content is ErrImportVersionConflict.
func (s *PgStore) ProposeJurisdictionImportWithQuerier(ctx context.Context, q Querier, p domain.ProposeImportParams) (*domain.ImportBatch, bool, error) {
	if err := p.Validate(); err != nil {
		return nil, false, err
	}
	standard, version := strings.TrimSpace(p.SourceStandard), strings.TrimSpace(p.SourceVersion)
	hash := domain.ImportHash(standard, version, p.Items)

	// One proposal per source version at a time, so the check below and the
	// insert cannot race.
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "jurisdiction_import:"+standard+":"+version); err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if existing, err := s.liveImport(ctx, q, standard, version); err != nil {
		return nil, false, err
	} else if existing != nil {
		if existing.ImportHash != hash {
			return nil, false, domain.ErrImportVersionConflict
		}
		return existing, false, nil
	}

	items, invalid, err := s.previewImportItems(ctx, q, p.Items)
	if err != nil {
		return nil, false, err
	}

	var batchID string
	if err := q.QueryRow(ctx, `
		INSERT INTO jurisdiction_import_batches
			(source_standard, source_version, import_hash, evidence_ref, summary, item_count, invalid_count, proposed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING batch_id`,
		standard, version, hash, strings.TrimSpace(p.EvidenceRef), strings.TrimSpace(p.Summary), len(items), invalid, p.ActorID).Scan(&batchID); err != nil {
		if isUniqueViolation(err, "uq_jib_live_source_version") {
			return nil, false, domain.ErrImportVersionConflict
		}
		s.log.Error("pg ProposeJurisdictionImport failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	for _, it := range items {
		tID, tCode, tType := refParts(&it.Target)
		pID, pCode, pType := refParts(it.Parent)
		sID, sCode, sType := refParts(it.Successor)
		if _, err := q.Exec(ctx, `
			INSERT INTO jurisdiction_import_items
				(batch_id, item_no, action, target_ref_id, target_code, target_type, jurisdiction_name, authority_type,
				 parent_ref_id, parent_code, parent_type, effective_from, successor_ref_id, successor_code, successor_type,
				 note, validation_errors)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			batchID, it.ItemNo, it.Action, tID, tCode, tType, nilIfBlank(it.JurisdictionName), nilIfBlank(it.AuthorityType),
			pID, pCode, pType, it.EffectiveFrom, sID, sCode, sType, nilIfBlank(it.Note), it.ValidationErrors); err != nil {
			s.log.Error("pg ProposeJurisdictionImport item failed", zap.Int("item_no", it.ItemNo), zap.Error(err))
			return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}
	b, err := s.getImport(ctx, q, batchID, false)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// liveImport is the proposed or approved import of a source version, or nil.
func (s *PgStore) liveImport(ctx context.Context, q Querier, standard, version string) (*domain.ImportBatch, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT batch_id FROM jurisdiction_import_batches
		 WHERE source_standard = $1 AND source_version = $2 AND status IN ('PROPOSED', 'APPROVED')`, standard, version).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return s.getImport(ctx, q, id, false)
}

// previewImportItems validates every row against the registry as it is now,
// without changing it, and returns the rows with their errors and how many
// failed. A row may name a jurisdiction an earlier row of the same batch adds.
func (s *PgStore) previewImportItems(ctx context.Context, q Querier, inputs []domain.ImportItemInput) ([]domain.ImportItem, int, error) {
	ic := &importRun{s: s, q: q, dryRun: true, pending: map[string][]string{}}
	items := make([]domain.ImportItem, len(inputs))
	seen := map[string]int{}
	invalid := 0
	for i, raw := range inputs {
		n := raw.Normalised()
		it := domain.ImportItem{ItemNo: i + 1, ImportItemInput: n, ValidationErrors: n.Validate()}
		if len(it.ValidationErrors) == 0 {
			if key := importRowKey(n); key != "" {
				if first, dup := seen[key]; dup {
					it.ValidationErrors = []string{fmt.Sprintf("row %d already addresses this jurisdiction", first)}
				} else {
					seen[key] = it.ItemNo
				}
			}
		}
		if len(it.ValidationErrors) == 0 {
			_, msg, err := ic.process(ctx, it, "", importEvidence{})
			if err != nil {
				return nil, 0, err
			}
			if msg != "" {
				it.ValidationErrors = []string{msg}
			}
		}
		if len(it.ValidationErrors) > 0 {
			invalid++
		}
		if it.ValidationErrors == nil {
			it.ValidationErrors = []string{}
		}
		items[i] = it
	}
	return items, invalid, nil
}

// importRowKey identifies what a row addresses, so two rows cannot address the
// same jurisdiction in one batch. An ADD is identified by code, type and parent
// (the registry's own identity); an UPDATE or RETIRE by its target.
func importRowKey(it domain.ImportItemInput) string {
	if it.Action == domain.ImportActionAdd {
		parent := ""
		if it.Parent != nil {
			parent = it.Parent.JurisdictionID + "|" + it.Parent.Code + "|" + it.Parent.Type
		}
		return "ADD|" + it.Target.Code + "|" + it.Target.Type + "|" + parent
	}
	return "TARGET|" + it.Target.JurisdictionID + "|" + it.Target.Code + "|" + it.Target.Type
}

// RejectJurisdictionImportWithQuerier rejects a proposed batch. The proposer may
// reject their own batch (withdraw it); a reason is required. Nothing in the
// registry changes, and the source version is free to be proposed again.
func (s *PgStore) RejectJurisdictionImportWithQuerier(ctx context.Context, q Querier, p domain.DecideImportParams) (*domain.ImportBatch, error) {
	if strings.TrimSpace(p.ActorID) == "" {
		return nil, fmt.Errorf("%w: an actor is required", domain.ErrInvalidImport)
	}
	if strings.TrimSpace(p.Reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required to reject an import", domain.ErrInvalidImport)
	}
	b, err := s.getImport(ctx, q, p.BatchID, true)
	if err != nil {
		return nil, err
	}
	if b.Status != domain.ImportProposed {
		return nil, domain.ErrImportDecided
	}
	if _, err := q.Exec(ctx, `UPDATE jurisdiction_import_batches
		   SET status = 'REJECTED', decided_by = $2, decided_at = clock_timestamp(), decision_reason = $3
		 WHERE batch_id = $1`, b.BatchID, p.ActorID, strings.TrimSpace(p.Reason)); err != nil {
		s.log.Error("pg RejectJurisdictionImport failed", zap.String("batch_id", b.BatchID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return s.getImport(ctx, q, b.BatchID, false)
}

// ApproveJurisdictionImportWithQuerier approves a proposed batch and applies it.
//
// It refuses, in order: an unknown or already decided batch, an approver who is
// the proposer (ErrNotIndependent), and a batch with rows that failed
// validation. It then applies every row in order inside q's transaction. A row
// that cannot be applied returns a *domain.ImportItemError and the caller's
// transaction must roll back, so the registry is never left half imported.
// The changes it made are returned for the caller to announce.
func (s *PgStore) ApproveJurisdictionImportWithQuerier(ctx context.Context, q Querier, p domain.DecideImportParams) (*domain.ImportBatch, []domain.AppliedChange, error) {
	if strings.TrimSpace(p.ActorID) == "" {
		return nil, nil, fmt.Errorf("%w: an actor is required", domain.ErrInvalidImport)
	}
	b, err := s.getImport(ctx, q, p.BatchID, true)
	if err != nil {
		return nil, nil, err
	}
	if b.Status != domain.ImportProposed {
		return nil, nil, domain.ErrImportDecided
	}
	if p.ActorID == b.ProposedBy {
		return nil, nil, domain.ErrNotIndependent
	}
	if b.InvalidCount > 0 {
		return nil, nil, domain.ErrImportHasInvalidRows
	}

	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, hierarchyLockKey); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	items, itemIDs, err := s.loadImportItems(ctx, q, b.BatchID)
	if err != nil {
		return nil, nil, err
	}
	ev := importEvidence{
		BatchID: b.BatchID, Reason: fmt.Sprintf("import %s %s (batch %s): %s", b.SourceStandard, b.SourceVersion, b.BatchID, b.Summary),
		EvidenceRef: b.EvidenceRef,
	}
	ic := &importRun{s: s, q: q, pending: map[string][]string{}}

	var changes []domain.AppliedChange
	counts := map[string]int{}
	for i, it := range items {
		res, msg, err := ic.process(ctx, it, p.ActorID, ev)
		if err != nil {
			return nil, nil, err
		}
		if msg != "" {
			return nil, nil, &domain.ImportItemError{ItemNo: it.ItemNo, Message: msg}
		}
		counts[res.outcome]++
		if res.change != nil {
			changes = append(changes, *res.change)
		}
		if _, err := q.Exec(ctx, `UPDATE jurisdiction_import_items SET outcome = $2, jurisdiction_id = $3 WHERE item_id = $1`,
			itemIDs[i], res.outcome, res.jurisdictionID); err != nil {
			s.log.Error("pg ApproveJurisdictionImport outcome failed", zap.Int("item_no", it.ItemNo), zap.Error(err))
			return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}
	if err := setImportEvidence(ctx, q, nil, "", ""); err != nil {
		return nil, nil, err
	}

	if _, err := q.Exec(ctx, `UPDATE jurisdiction_import_batches
		   SET status = 'APPROVED', decided_by = $2, decided_at = clock_timestamp(), decision_reason = $3,
		       added_count = $4, updated_count = $5, retired_count = $6, unchanged_count = $7
		 WHERE batch_id = $1`,
		b.BatchID, p.ActorID, nilIfBlank(strings.TrimSpace(p.Reason)),
		counts[domain.OutcomeAdded], counts[domain.OutcomeUpdated], counts[domain.OutcomeRetired], counts[domain.OutcomeUnchanged]); err != nil {
		s.log.Error("pg ApproveJurisdictionImport decide failed", zap.String("batch_id", b.BatchID), zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	out, err := s.getImport(ctx, q, b.BatchID, false)
	if err != nil {
		return nil, nil, err
	}
	return out, changes, nil
}

// ── applying one row ─────────────────────────────────────────────────────────

// importEvidence is what the version history records against every change an
// import makes.
type importEvidence struct {
	BatchID     string
	Reason      string
	EvidenceRef string
}

// importRun carries the state of one pass over a batch's rows: either a dry run
// that only checks them (proposing) or the real application (approving).
type importRun struct {
	s      *PgStore
	q      Querier
	dryRun bool
	// pending holds, in a dry run, the jurisdictions earlier rows would add, by
	// code and type, so a later row can name one as its parent or successor.
	pending map[string][]string
	seq     int
}

type rowResult struct {
	outcome        string
	jurisdictionID string
	change         *domain.AppliedChange
}

func pendingKey(code, typ string) string { return code + "\x00" + typ }

const dryRunID = "dry-run:"

// resolve finds the one active jurisdiction a reference names. A message (not
// an error) is returned when it names none, or more than one: the registry does
// not choose.
func (c *importRun) resolve(ctx context.Context, ref domain.JurisdictionRef, label string) (id, msg string, err error) {
	if ref.JurisdictionID != "" {
		if _, ferr := c.s.findActiveWith(ctx, c.q, ref.JurisdictionID); ferr != nil {
			if errors.Is(ferr, domain.ErrJurisdictionNotFound) {
				return "", fmt.Sprintf("%s: no active jurisdiction has id %s", label, ref.JurisdictionID), nil
			}
			return "", "", ferr
		}
		return ref.JurisdictionID, "", nil
	}
	rows, qerr := c.q.Query(ctx, `
		SELECT jurisdiction_id FROM jurisdictions
		 WHERE jurisdiction_code = $1 AND jurisdiction_type = $2
		   AND active_flag = TRUE AND (effective_to IS NULL OR effective_to > NOW())
		 ORDER BY jurisdiction_id`, ref.Code, ref.Type)
	if qerr != nil {
		return "", "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qerr)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var one string
		if err := rows.Scan(&one); err != nil {
			return "", "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		ids = append(ids, one)
	}
	if err := rows.Err(); err != nil {
		return "", "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if c.dryRun {
		ids = append(ids, c.pending[pendingKey(ref.Code, ref.Type)]...)
	}
	switch len(ids) {
	case 0:
		return "", fmt.Sprintf("%s: no active jurisdiction is %s", label, ref.String()), nil
	case 1:
		return ids[0], "", nil
	}
	return "", fmt.Sprintf("%s: %d active jurisdictions are %s; name the one meant by jurisdiction_id", label, len(ids), ref.String()), nil
}

// process checks one row and, unless this is a dry run, applies it. It returns
// a message when the row cannot be applied, and an error only for a failure of
// the store itself.
func (c *importRun) process(ctx context.Context, it domain.ImportItem, actor string, ev importEvidence) (rowResult, string, error) {
	switch it.Action {
	case domain.ImportActionAdd:
		return c.add(ctx, it, actor, ev)
	case domain.ImportActionUpdate:
		return c.update(ctx, it, actor, ev)
	case domain.ImportActionRetire:
		return c.retire(ctx, it, actor, ev)
	}
	return rowResult{}, "unknown action", nil
}

func isDry(id string) bool { return strings.HasPrefix(id, dryRunID) }

func (c *importRun) add(ctx context.Context, it domain.ImportItem, actor string, ev importEvidence) (rowResult, string, error) {
	var parentID *string
	if it.Parent != nil {
		id, msg, err := c.resolve(ctx, *it.Parent, "parent")
		if err != nil || msg != "" {
			return rowResult{}, msg, err
		}
		parentID = &id
	}
	// The registry's own identity is code, type and parent: a row that adds one
	// that exists (active or retired) is a mistake, not a no-op.
	if parentID == nil || !isDry(*parentID) {
		var exists bool
		if err := c.q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM jurisdictions
			 WHERE jurisdiction_code = $1 AND jurisdiction_type = $2
			   AND COALESCE(parent_jurisdiction_id, '00000000-0000-0000-0000-000000000000'::uuid)
			     = COALESCE($3::uuid, '00000000-0000-0000-0000-000000000000'::uuid))`,
			it.Target.Code, it.Target.Type, parentID).Scan(&exists); err != nil {
			return rowResult{}, "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if exists {
			return rowResult{}, fmt.Sprintf("%s already exists under that parent (it may be retired); use UPDATE to change it", it.Target.String()), nil
		}
	}

	if c.dryRun {
		c.seq++
		key := pendingKey(it.Target.Code, it.Target.Type)
		c.pending[key] = append(c.pending[key], fmt.Sprintf("%s%d", dryRunID, c.seq))
		return rowResult{outcome: domain.OutcomeAdded}, "", nil
	}

	if err := setImportEvidence(ctx, c.q, nil, rowReason(ev, it), ev.EvidenceRef); err != nil {
		return rowResult{}, "", err
	}
	j, created, err := c.s.CreateJurisdictionWithQuerier(ctx, c.q, domain.CreateJurisdictionParams{
		JurisdictionCode: it.Target.Code, JurisdictionName: it.JurisdictionName, JurisdictionType: it.Target.Type,
		ParentJurisdictionID: parentID, AuthorityType: it.AuthorityType, EffectiveFrom: *it.EffectiveFrom,
		ActiveFlag: true, CreatedByPrincipalID: actor,
	})
	if err != nil {
		if msg := importWriteMessage(err); msg != "" {
			return rowResult{}, msg, nil
		}
		return rowResult{}, "", err
	}
	if !created {
		return rowResult{}, fmt.Sprintf("%s already exists under that parent", it.Target.String()), nil
	}
	return rowResult{
		outcome: domain.OutcomeAdded, jurisdictionID: j.JurisdictionID,
		change: &domain.AppliedChange{ItemNo: it.ItemNo, Outcome: domain.OutcomeAdded, Jurisdiction: j},
	}, "", nil
}

func (c *importRun) update(ctx context.Context, it domain.ImportItem, actor string, ev importEvidence) (rowResult, string, error) {
	targetID, msg, err := c.resolve(ctx, it.Target, "target")
	if err != nil || msg != "" {
		return rowResult{}, msg, err
	}
	var newParent *string
	if it.Parent != nil {
		id, msg, err := c.resolve(ctx, *it.Parent, "parent")
		if err != nil || msg != "" {
			return rowResult{}, msg, err
		}
		newParent = &id
	}
	if isDry(targetID) || (newParent != nil && isDry(*newParent)) {
		return rowResult{outcome: domain.OutcomeUpdated}, "", nil
	}

	cur, err := c.s.findActiveWith(ctx, c.q, targetID)
	if err != nil {
		return rowResult{}, "", err
	}
	name, authority, parent := cur.JurisdictionName, cur.AuthorityType, cur.ParentJurisdictionID
	if it.JurisdictionName != "" {
		name = it.JurisdictionName
	}
	if it.AuthorityType != "" {
		authority = it.AuthorityType
	}
	if newParent != nil {
		parent = newParent
	}
	parentChanged := !equalStrPtr(cur.ParentJurisdictionID, parent)
	attrsChanged := name != cur.JurisdictionName || authority != cur.AuthorityType
	if !parentChanged && !attrsChanged {
		return rowResult{outcome: domain.OutcomeUnchanged, jurisdictionID: cur.JurisdictionID}, "", nil
	}

	if parentChanged {
		if parent != nil {
			if *parent == cur.JurisdictionID {
				return rowResult{}, "a jurisdiction cannot be its own parent", nil
			}
			var cycle bool
			if err := c.q.QueryRow(ctx, `
				WITH RECURSIVE anc AS (
					SELECT jurisdiction_id, parent_jurisdiction_id FROM jurisdictions WHERE jurisdiction_id = $1
					UNION
					SELECT j.jurisdiction_id, j.parent_jurisdiction_id
					  FROM jurisdictions j JOIN anc a ON j.jurisdiction_id = a.parent_jurisdiction_id)
				SELECT EXISTS (SELECT 1 FROM anc WHERE jurisdiction_id = $2)`, *parent, cur.JurisdictionID).Scan(&cycle); err != nil {
				return rowResult{}, "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
			if cycle {
				return rowResult{}, fmt.Sprintf("moving %s under that parent would create a cycle in the hierarchy", it.Target.String()), nil
			}
		}
	}
	// A change takes effect from effective_from, which cannot precede the
	// version it replaces.
	if it.EffectiveFrom != nil {
		var open time.Time
		if err := c.q.QueryRow(ctx, `SELECT valid_from FROM jurisdiction_versions WHERE jurisdiction_id = $1 AND valid_to IS NULL`,
			cur.JurisdictionID).Scan(&open); err != nil {
			return rowResult{}, "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if !it.EffectiveFrom.After(open) {
			return rowResult{}, fmt.Sprintf("effective_from %s does not follow the version in force, which began %s",
				it.EffectiveFrom.UTC().Format(time.RFC3339), open.UTC().Format(time.RFC3339)), nil
		}
	}
	if c.dryRun {
		return rowResult{outcome: domain.OutcomeUpdated}, "", nil
	}

	if err := setImportEvidence(ctx, c.q, it.EffectiveFrom, rowReason(ev, it), ev.EvidenceRef); err != nil {
		return rowResult{}, "", err
	}
	after, err := scanJurisdiction(c.q.QueryRow(ctx, `
		UPDATE jurisdictions
		   SET jurisdiction_name = $2, authority_type = $3, parent_jurisdiction_id = $4,
		       updated_at = NOW(), updated_by_principal_id = $5
		 WHERE jurisdiction_id = $1
		RETURNING `+jurisdictionColumns, cur.JurisdictionID, name, authority, parent, actor))
	if err != nil {
		if isUniqueViolation(err, "idx_jurisdictions_code_type_parent_unique") {
			return rowResult{}, fmt.Sprintf("%s already exists under that parent", it.Target.String()), nil
		}
		if msg := importWriteMessage(err); msg != "" {
			return rowResult{}, msg, nil
		}
		c.s.log.Error("pg import update failed", zap.String("jurisdiction_id", cur.JurisdictionID), zap.Error(err))
		return rowResult{}, "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return rowResult{
		outcome: domain.OutcomeUpdated, jurisdictionID: after.JurisdictionID,
		change: &domain.AppliedChange{
			ItemNo: it.ItemNo, Outcome: domain.OutcomeUpdated, Jurisdiction: after, Before: cur,
			ParentChanged: parentChanged, AttributesChanged: attrsChanged,
		},
	}, "", nil
}

func (c *importRun) retire(ctx context.Context, it domain.ImportItem, actor string, ev importEvidence) (rowResult, string, error) {
	targetID, msg, err := c.resolve(ctx, it.Target, "target")
	if err != nil || msg != "" {
		return rowResult{}, msg, err
	}
	var successor *string
	if it.Successor != nil {
		id, msg, err := c.resolve(ctx, *it.Successor, "successor")
		if err != nil || msg != "" {
			return rowResult{}, msg, err
		}
		if id == targetID {
			return rowResult{}, "a jurisdiction cannot succeed itself", nil
		}
		successor = &id
	}
	if c.dryRun || isDry(targetID) {
		return rowResult{outcome: domain.OutcomeRetired}, "", nil
	}

	before, err := c.s.findActiveWith(ctx, c.q, targetID)
	if err != nil {
		return rowResult{}, "", err
	}
	if err := setImportEvidence(ctx, c.q, nil, rowReason(ev, it), ev.EvidenceRef); err != nil {
		return rowResult{}, "", err
	}
	after, retired, err := c.s.RetireJurisdictionWithQuerier(ctx, c.q, domain.RetireJurisdictionParams{
		JurisdictionID: targetID, ActorID: actor, Reason: rowReason(ev, it), EvidenceRef: ev.EvidenceRef, SupersededBy: successor,
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidSuccessor) {
			return rowResult{}, "successor: it must be a different, active jurisdiction", nil
		}
		return rowResult{}, "", err
	}
	if !retired {
		return rowResult{outcome: domain.OutcomeUnchanged, jurisdictionID: targetID}, "", nil
	}
	return rowResult{
		outcome: domain.OutcomeRetired, jurisdictionID: after.JurisdictionID,
		change: &domain.AppliedChange{ItemNo: it.ItemNo, Outcome: domain.OutcomeRetired, Jurisdiction: after, Before: before},
	}, "", nil
}

// rowReason is the reason recorded on the history for one row: the batch it
// came from and, if the source gave one, the row's own note.
func rowReason(ev importEvidence, it domain.ImportItem) string {
	if it.Note != "" {
		return ev.Reason + "; " + it.Note
	}
	return ev.Reason
}

// importWriteMessage turns the registry's own refusals into the row's message.
func importWriteMessage(err error) string {
	switch {
	case errors.Is(err, domain.ErrParentNotFound):
		return "parent: that jurisdiction does not exist"
	case errors.Is(err, domain.ErrCyclicHierarchy):
		return "that parent would create a cycle in the hierarchy"
	case errors.Is(err, domain.ErrInputTooLong):
		return "a value is longer than the registry allows"
	case errors.Is(err, domain.ErrConflict):
		return "a jurisdiction with that code, type and parent already exists with different attributes"
	}
	return ""
}

// setImportEvidence sets, for the rest of the transaction, what the version
// history records against the next change: the instant it took effect, why, and
// the evidence. Empty values clear them.
func setImportEvidence(ctx context.Context, q Querier, validFrom *time.Time, reason, evidence string) error {
	vf := ""
	if validFrom != nil {
		vf = validFrom.UTC().Format(time.RFC3339Nano)
	}
	if _, err := q.Exec(ctx, `SELECT set_config('app.valid_from', $1, true), set_config('app.reason', $2, true), set_config('app.evidence_ref', $3, true)`,
		vf, reason, evidence); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// ── lookups that run inside the caller's transaction ─────────────────────────

// findActiveWith is FindByID on q: the active, in-force jurisdiction with that
// id. Inside a transaction it sees what the transaction has already written,
// which an import needs when a later row names a jurisdiction an earlier one
// added.
func (s *PgStore) findActiveWith(ctx context.Context, q Querier, jurisdictionID string) (*domain.Jurisdiction, error) {
	j, err := scanJurisdiction(q.QueryRow(ctx, `
		SELECT `+jurisdictionColumns+` FROM jurisdictions
		 WHERE jurisdiction_id = $1 AND active_flag = TRUE AND (effective_to IS NULL OR effective_to > NOW())`, jurisdictionID))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !isInvalidTextRepresentation(err) {
			s.log.Error("pg findActiveWith failed", zap.String("jurisdiction_id", jurisdictionID), zap.Error(err))
		}
		return nil, notFoundOr(err, domain.ErrJurisdictionNotFound)
	}
	return j, nil
}

// findAnyWith is FindByIDAny on q.
func (s *PgStore) findAnyWith(ctx context.Context, q Querier, jurisdictionID string) (*domain.Jurisdiction, error) {
	j, err := scanJurisdiction(q.QueryRow(ctx, `SELECT `+jurisdictionColumns+` FROM jurisdictions WHERE jurisdiction_id = $1`, jurisdictionID))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && !isInvalidTextRepresentation(err) {
			s.log.Error("pg findAnyWith failed", zap.String("jurisdiction_id", jurisdictionID), zap.Error(err))
		}
		return nil, notFoundOr(err, domain.ErrJurisdictionNotFound)
	}
	return j, nil
}
