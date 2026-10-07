package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"zoiko.io/currency-registry-svc/internal/domain"
	"zoiko.io/currency-registry-svc/internal/events"
)

// MaxImportRows bounds one import (the request body is also size-capped).
const MaxImportRows = 5000

// ImportInput is the ImportCurrencyUpdate command.
type ImportInput struct {
	Meta
	SourceName    string
	SourceVersion string
	ManifestHash  string
	EffectiveAt   *time.Time
	Rows          []domain.ImportRow
}

type changeKind int

const (
	kindUnchanged changeKind = iota
	kindCreate
	kindMinorUnit
	kindAttrs
	kindBoth
)

type rowPlan struct {
	row      domain.ImportRow
	mu       int
	existing *domain.Currency
	kind     changeKind
	prevMU   *int
}

// ImportCurrencyUpdate verifies, validates and applies a source import.
//
// Outcomes, in order:
//  1. manifest hash does not match the recomputed hash -> SOURCE_UNVERIFIED,
//     nothing is written.
//  2. (source, version) already imported: same hash -> the ORIGINAL result is
//     returned (replayed); different hash -> DUPLICATE_CANDIDATE.
//  3. any invalid row -> the WHOLE import is stored as QUARANTINED, nothing is
//     applied, CurrencyImportQuarantined is enqueued.
//  4. otherwise every row is applied in this one transaction and the import is
//     stored as APPLIED.
func (s *Service) ImportCurrencyUpdate(ctx context.Context, in ImportInput) (*domain.Import, bool, error) {
	if err := in.Meta.check(); err != nil {
		return nil, false, err
	}
	if in.SourceName == "" || in.SourceVersion == "" || in.ManifestHash == "" {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "source_name, source_version and manifest_hash are required")
	}
	if len(in.Rows) == 0 {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "rows must not be empty")
	}
	if len(in.Rows) > MaxImportRows {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "at most %d rows per import", MaxImportRows)
	}

	// Verify BEFORE opening a write path: an unverified manifest leaves no trace.
	computed := domain.ManifestHash(in.Rows)
	if domain.NormalizeHash(in.ManifestHash) != computed {
		return nil, false, domain.Errf(domain.CodeSourceUnverified,
			"manifest_hash does not match the hash recomputed from rows; nothing was applied")
	}

	var res *domain.Import
	var replayed bool
	err := s.store.InTx(ctx, in.TenantID, func(tx Tx) error {
		sourceReplay := false
		out, idemReplay, err := idempotent(ctx, tx, in.Meta, "import", func() (*domain.Import, error) {
			if err := tx.LockKey(ctx, "import:"+in.SourceName+":"+in.SourceVersion); err != nil {
				return nil, err
			}
			existing, err := tx.GetImportBySourceVersion(ctx, in.SourceName, in.SourceVersion)
			if err != nil {
				return nil, err
			}
			if existing != nil {
				if existing.ManifestHash == computed {
					sourceReplay = true
					return existing, nil
				}
				return nil, domain.Errf(domain.CodeDuplicateCandidate,
					"source %q version %q was already imported with a different manifest (import %s)",
					in.SourceName, in.SourceVersion, existing.ImportID)
			}
			return s.runImport(ctx, tx, in, computed)
		})
		if err != nil {
			return err
		}
		res, replayed = out, idemReplay || sourceReplay
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return res, replayed, nil
}

func (s *Service) runImport(ctx context.Context, tx Tx, in ImportInput, hash string) (*domain.Import, error) {
	now := s.now()
	effective := now
	if in.EffectiveAt != nil {
		effective = in.EffectiveAt.UTC()
	}

	plans, rowErrs, err := s.planImport(ctx, tx, in.Rows, effective)
	if err != nil {
		return nil, err
	}

	imp := &domain.Import{
		ImportID:      s.newID(),
		SourceName:    in.SourceName,
		SourceVersion: in.SourceVersion,
		ManifestHash:  hash,
		RowCount:      len(in.Rows),
		EffectiveAt:   effective,
		Actor:         in.Actor,
		ActorTenantID: in.TenantID,
		Reason:        in.Reason,
		CorrelationID: in.CorrelationID,
		CreatedAt:     now,
		Rows:          in.Rows,
	}

	if len(rowErrs) > 0 {
		imp.Status = domain.ImportQuarantined
		imp.RowErrors = rowErrs
		imp.QuarantineReason = fmt.Sprintf("%d validation error(s); the whole import was quarantined and nothing was applied", len(rowErrs))
		if err := tx.InsertImport(ctx, imp); err != nil {
			return nil, err
		}
		if err := s.enqueue(ctx, tx, events.Event{
			Type: events.EventCurrencyImportQuarantined, TenantID: in.TenantID, Scope: events.ScopeGlobal,
			ObjectType: events.ObjectTypeCurrencyImport, ObjectID: imp.ImportID, ObjectVersion: 1,
			EffectiveAt: effective, RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
			Data: map[string]any{
				"source_name": in.SourceName, "source_version": in.SourceVersion, "manifest_hash": hash,
				"row_count": len(in.Rows), "error_count": len(rowErrs), "quarantine_reason": imp.QuarantineReason,
			},
		}); err != nil {
			return nil, err
		}
		return imp, nil
	}

	imp.Status = domain.ImportApplied
	sum := &domain.ImportSummary{}
	imp.Summary = sum
	if err := tx.InsertImport(ctx, imp); err != nil {
		return nil, err
	}
	for _, p := range plans {
		if err := s.applyPlan(ctx, tx, in, imp, p, effective, now, sum); err != nil {
			return nil, err
		}
	}
	return imp, nil
}

// planImport validates every row against its shape, against the rest of the
// file and against the registry, WITHOUT writing anything.
func (s *Service) planImport(ctx context.Context, tx Tx, rows []domain.ImportRow, effective time.Time) ([]rowPlan, []domain.RowError, error) {
	var errs []domain.RowError
	addErr := func(i int, format string, a ...any) {
		errs = append(errs, domain.RowError{Row: i, Message: fmt.Sprintf(format, a...)})
	}

	alphaAt := map[string]int{}
	numAt := map[string]int{}
	plans := make([]rowPlan, 0, len(rows))

	for i, r := range rows {
		bad := false
		for _, m := range domain.CheckRowShape(r) {
			addErr(i, "%s", m)
			bad = true
		}
		if r.AlphaCode != "" {
			if j, dup := alphaAt[r.AlphaCode]; dup {
				addErr(i, "duplicate alpha_code %q (also row %d)", r.AlphaCode, j)
				bad = true
			} else {
				alphaAt[r.AlphaCode] = i
			}
		}
		if r.NumericCode != "" {
			if j, dup := numAt[r.NumericCode]; dup {
				addErr(i, "duplicate numeric_code %q (also row %d)", r.NumericCode, j)
				bad = true
			} else {
				numAt[r.NumericCode] = i
			}
		}
		if bad {
			continue
		}

		mu, _ := domain.ParseMinorUnit(r) // shape already checked
		p := rowPlan{row: r, mu: mu}

		byAlpha, err := tx.FindCurrenciesByAlpha(ctx, r.AlphaCode)
		if err != nil {
			return nil, nil, err
		}
		cur := nonRetired(byAlpha)
		switch {
		case cur == nil && len(byAlpha) > 0:
			addErr(i, "alpha_code %q belongs to a RETIRED currency; re-introducing a retired code is not supported by import", r.AlphaCode)
			continue
		case cur != nil:
			if cur.NumericCode != r.NumericCode {
				addErr(i, "numeric_code %q conflicts with registered numeric_code %q for alpha_code %q", r.NumericCode, cur.NumericCode, r.AlphaCode)
				continue
			}
			p.existing = cur
		default:
			byNum, err := tx.FindCurrenciesByNumeric(ctx, r.NumericCode)
			if err != nil {
				return nil, nil, err
			}
			if other := nonRetired(byNum); other != nil {
				addErr(i, "numeric_code %q is already registered to alpha_code %q", r.NumericCode, other.AlphaCode)
				continue
			}
			p.kind = kindCreate
		}

		if p.existing != nil {
			vs, err := tx.ListMinorUnitVersions(ctx, p.existing.CurrencyID)
			if err != nil {
				return nil, nil, err
			}
			muChanged, attrChanged := false, p.existing.Name != r.Name || p.existing.FundOrMetalFlag != r.FundOrMetalFlag
			if n := len(vs); n > 0 {
				latest := vs[n-1]
				prev := latest.MinorUnit
				p.prevMU = &prev
				muChanged = latest.MinorUnit != mu
				if muChanged && !effective.After(latest.ValidFrom) {
					addErr(i, "minor_unit change for %q must take effect after the current version's valid_from (%s); backdated corrections are not supported",
						r.AlphaCode, latest.ValidFrom.UTC().Format(time.RFC3339))
					continue
				}
			} else {
				muChanged = true // a currency with no minor-unit version gets one
			}
			switch {
			case muChanged && attrChanged:
				p.kind = kindBoth
			case muChanged:
				p.kind = kindMinorUnit
			case attrChanged:
				p.kind = kindAttrs
			default:
				p.kind = kindUnchanged
			}
		}
		plans = append(plans, p)
	}
	return plans, errs, nil
}

func (s *Service) applyPlan(ctx context.Context, tx Tx, in ImportInput, imp *domain.Import, p rowPlan, effective, now time.Time, sum *domain.ImportSummary) error {
	if p.kind == kindUnchanged {
		sum.Unchanged++
		return nil
	}
	evidence := fmt.Sprintf("currency-import/%s", imp.ImportID)
	var cur domain.Currency
	changeLabel := ""

	if p.kind == kindCreate {
		cur = domain.Currency{
			CurrencyID: s.newID(), AlphaCode: p.row.AlphaCode, NumericCode: p.row.NumericCode, Name: p.row.Name,
			FundOrMetalFlag: p.row.FundOrMetalFlag, Status: domain.StatusKnown, ValidFrom: effective, Version: 1,
			LastImportID: imp.ImportID, LastImportActor: in.Actor, CreatedAt: now, RecordedAt: now,
		}
		if err := tx.InsertCurrency(ctx, &cur); err != nil {
			return err
		}
		sum.Created++
		changeLabel = "CREATED"
	} else {
		cur = *p.existing
		prevVersion := cur.Version
		cur.Name, cur.FundOrMetalFlag = p.row.Name, p.row.FundOrMetalFlag
		cur.Version++
		cur.LastImportID, cur.LastImportActor = imp.ImportID, in.Actor
		cur.RecordedAt = now
		if err := tx.UpdateCurrency(ctx, &cur, prevVersion); err != nil {
			if err == domain.ErrVersionConflictStore {
				return domain.Errf(domain.CodeVersionConflict, "currency %s changed while the import was applying; retry", cur.AlphaCode)
			}
			return err
		}
		switch p.kind {
		case kindMinorUnit:
			sum.MinorUnitChanged++
			changeLabel = "MINOR_UNIT_CHANGED"
		case kindAttrs:
			sum.AttributesChanged++
			changeLabel = "ATTRIBUTES_CHANGED"
		default:
			sum.MinorUnitChanged++
			sum.AttributesChanged++
			changeLabel = "MINOR_UNIT_AND_ATTRIBUTES_CHANGED"
		}
	}

	if p.kind == kindCreate || p.kind == kindMinorUnit || p.kind == kindBoth {
		// New version row; the previous one is never touched (append-only).
		mvFrom := effective
		if err := tx.InsertMinorUnitVersion(ctx, &domain.MinorUnitVersion{
			CurrencyID: cur.CurrencyID, MinorUnit: p.mu, ValidFrom: mvFrom, SourceVersion: imp.SourceVersion,
			ImportID: imp.ImportID, EvidenceRef: evidence, RecordedAt: now,
		}); err != nil {
			return err
		}
	}

	data := map[string]any{
		"alpha_code": cur.AlphaCode, "numeric_code": cur.NumericCode, "change_kind": changeLabel,
		"status": string(cur.Status), "minor_unit": p.mu, "import_id": imp.ImportID,
		"source_name": imp.SourceName, "source_version": imp.SourceVersion,
	}
	if p.prevMU != nil {
		data["previous_minor_unit"] = *p.prevMU
	}
	return s.enqueue(ctx, tx, events.Event{
		Type: events.EventCurrencyUpdated, TenantID: in.TenantID, Scope: events.ScopeGlobal,
		ObjectType: events.ObjectTypeCurrency, ObjectID: cur.CurrencyID, ObjectVersion: cur.Version,
		EffectiveAt: effective, RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
		Data: data,
	})
}

// GetImport returns the evidence record of an import, looked up by
// (source, version).
func (s *Service) GetImport(ctx context.Context, source, version string) (*domain.Import, error) {
	var out *domain.Import
	err := s.store.InTx(ctx, "", func(tx Tx) error {
		imp, err := tx.GetImportBySourceVersion(ctx, strings.TrimSpace(source), strings.TrimSpace(version))
		if err != nil {
			return err
		}
		out = imp
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, domain.Errf(domain.CodeNotFound, "no import for that source and version")
	}
	return out, nil
}
