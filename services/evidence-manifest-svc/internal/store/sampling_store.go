package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

func (s *PgStore) CreateSamplingParameterSet(ctx context.Context, p domain.CreateSamplingParameterSetParams) (*domain.SamplingParameterSet, error) {
	var out *domain.SamplingParameterSet
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO sampling_parameter_sets (tenant_id, approach, tolerable_misstatement, expected_misstatement, confidence_level, key_item_threshold)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING param_set_id, tenant_id, version, approach, tolerable_misstatement, expected_misstatement, confidence_level, key_item_threshold, created_at`,
			svcmiddleware.TenantFromContext(ctx), p.Approach, p.TolerableMisstatement, p.ExpectedMisstatement, p.ConfidenceLevel, p.KeyItemThreshold)
		ps := &domain.SamplingParameterSet{}
		if err := row.Scan(&ps.ParamSetID, &ps.TenantID, &ps.Version, &ps.Approach, &ps.TolerableMisstatement, &ps.ExpectedMisstatement, &ps.ConfidenceLevel, &ps.KeyItemThreshold, &ps.CreatedAt); err != nil {
			return err
		}
		out = ps
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

const sampleDesignColumns = `design_id, tenant_id, population_id, objective, param_set_id, sample_size, status, approved_by_principal_id, created_by_principal_id, created_at`

func scanSampleDesign(row pgx.Row) (*domain.SampleDesign, error) {
	d := &domain.SampleDesign{}
	err := row.Scan(&d.DesignID, &d.TenantID, &d.PopulationID, &d.Objective, &d.ParamSetID, &d.SampleSize, &d.Status, &d.ApprovedByPrincipalID, &d.CreatedByPrincipalID, &d.CreatedAt)
	return d, err
}

// CreateSampleDesign requires the target population to already be
// FROZEN or IN_USE — "live queries prohibited for evidential use," the
// same check SelectSample repeats defensively below.
func (s *PgStore) CreateSampleDesign(ctx context.Context, p domain.CreateSampleDesignParams) (*domain.SampleDesign, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.SampleDesign
	created := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var popStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM audit_populations WHERE population_id=$1`, p.PopulationID).Scan(&popStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrPopulationNotFound
			}
			return err
		}
		if popStatus != string(domain.PopulationFrozen) && popStatus != string(domain.PopulationInUse) {
			return domain.ErrPopulationNotFrozen
		}

		row := tx.QueryRow(ctx, `INSERT INTO sample_designs (tenant_id, population_id, objective, param_set_id, sample_size, created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING RETURNING `+sampleDesignColumns,
			svcmiddleware.TenantFromContext(ctx), p.PopulationID, p.Objective, p.ParamSetID, p.SampleSize, p.CreatedByPrincipalID, p.CorrelationID)
		var err error
		out, err = scanSampleDesign(row)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanSampleDesign(tx.QueryRow(ctx, `SELECT `+sampleDesignColumns+` FROM sample_designs WHERE tenant_id::text=$1 AND correlation_id=$2`,
			svcmiddleware.TenantFromContext(ctx), p.CorrelationID))
		return err
	})
	if errors.Is(err, domain.ErrPopulationNotFound) || errors.Is(err, domain.ErrPopulationNotFrozen) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetSampleDesign(ctx context.Context, tenantID, designID string) (*domain.SampleDesign, error) {
	var out *domain.SampleDesign
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanSampleDesign(tx.QueryRow(ctx, `SELECT `+sampleDesignColumns+` FROM sample_designs WHERE design_id=$1`, designID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSampleDesignNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ApproveSampleDesign refuses self-approval — same maker/checker posture
// as every other approval gate in this build.
func (s *PgStore) ApproveSampleDesign(ctx context.Context, p domain.ApproveSampleDesignParams) (*domain.SampleDesign, bool, error) {
	var out *domain.SampleDesign
	changed := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanSampleDesign(tx.QueryRow(ctx, `SELECT `+sampleDesignColumns+` FROM sample_designs WHERE design_id=$1 FOR UPDATE`, p.DesignID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSampleDesignNotFound
		}
		if err != nil {
			return err
		}
		if current.Status == domain.SampleDesignApproved {
			out = current
			return nil
		}
		if current.Status != domain.SampleDesignDraft {
			return domain.ErrSampleDesignInvalidState
		}
		if current.CreatedByPrincipalID == p.ActorPrincipalID {
			return domain.ErrSampleDesignInvalidState
		}
		out, err = scanSampleDesign(tx.QueryRow(ctx, `UPDATE sample_designs SET status=$1, approved_by_principal_id=$2 WHERE design_id=$3 RETURNING `+sampleDesignColumns,
			domain.SampleDesignApproved, p.ActorPrincipalID, p.DesignID))
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrSampleDesignNotFound) || errors.Is(err, domain.ErrSampleDesignInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// SelectSample is the real, deterministic selection algorithm: key items
// (>= key_item_threshold) are always taken, never probabilistically;
// everything else is systematic-interval sampled over the population's
// own frozen ordinal order, with the seed/interval/start_point all stored
// verbatim so ReproduceSelection can recompute the identical set later.
func (s *PgStore) SelectSample(ctx context.Context, p domain.SelectSampleParams) (*domain.SampleSelection, []*domain.SampleItem, bool, error) {
	if p.CorrelationID == "" {
		return nil, nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var outSel *domain.SampleSelection
	var outItems []*domain.SampleItem
	created := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		design, err := scanSampleDesign(tx.QueryRow(ctx, `SELECT `+sampleDesignColumns+` FROM sample_designs WHERE design_id=$1 FOR UPDATE`, p.DesignID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSampleDesignNotFound
		}
		if err != nil {
			return err
		}
		if design.Status == domain.SampleDesignSelected {
			outSel, outItems, err = loadSelection(ctx, tx, design.DesignID)
			return err
		}
		if design.Status != domain.SampleDesignApproved {
			return domain.ErrSampleDesignInvalidState
		}

		var popStatus, digest string
		if err := tx.QueryRow(ctx, `SELECT status, COALESCE(digest_sha256,'') FROM audit_populations WHERE population_id=$1`, design.PopulationID).Scan(&popStatus, &digest); err != nil {
			return err
		}
		if popStatus != string(domain.PopulationFrozen) && popStatus != string(domain.PopulationInUse) {
			return domain.ErrPopulationNotFrozen
		}

		var paramSetVersion int
		var keyItemThreshold *float64
		if err := tx.QueryRow(ctx, `SELECT version, key_item_threshold FROM sampling_parameter_sets WHERE param_set_id=$1`, design.ParamSetID).Scan(&paramSetVersion, &keyItemThreshold); err != nil {
			return err
		}

		// Key items: always selected, never probabilistic.
		keyRows, err := tx.Query(ctx, `SELECT row_id, ordinal FROM population_rows WHERE population_id=$1 AND amount >= $2 ORDER BY ordinal`,
			design.PopulationID, keyItemThreshold)
		if err != nil {
			return err
		}
		type rowRef struct {
			ID      string
			Ordinal int64
		}
		var keyItemRows []rowRef
		keyOrdinals := map[int64]bool{}
		if keyItemThreshold != nil {
			for keyRows.Next() {
				var rr rowRef
				if err := keyRows.Scan(&rr.ID, &rr.Ordinal); err != nil {
					keyRows.Close()
					return err
				}
				keyItemRows = append(keyItemRows, rr)
				keyOrdinals[rr.Ordinal] = true
			}
			if err := keyRows.Err(); err != nil {
				return err
			}
		}
		keyRows.Close()

		var totalRows int64
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM population_rows WHERE population_id=$1`, design.PopulationID).Scan(&totalRows); err != nil {
			return err
		}
		remainingCount := totalRows - int64(len(keyItemRows))
		if remainingCount < 0 {
			remainingCount = 0
		}

		seedInput := fmt.Sprintf("%s|%s|%d|%s", design.DesignID, design.ParamSetID, paramSetVersion, digest)
		seedHash := sha256.Sum256([]byte(seedInput))
		rngSeed := hex.EncodeToString(seedHash[:])

		var interval, startPoint float64 = 0, 0
		var nonKeySelectedOrdinals map[int64]bool
		if design.SampleSize > 0 && remainingCount > 0 {
			interval = float64(remainingCount) / float64(design.SampleSize)
			if interval < 1 {
				interval = 1
			}
			seedInt := new(big.Int).SetBytes(seedHash[:8])
			intervalInt := int64(interval)
			if intervalInt < 1 {
				intervalInt = 1
			}
			startPoint = float64(new(big.Int).Mod(seedInt, big.NewInt(intervalInt)).Int64())

			nonKeySelectedOrdinals = map[int64]bool{}
			pos := startPoint
			// Walk the non-key ordinals in order, selecting every
			// interval-th one — reproducible purely from
			// (start_point, interval) plus the frozen ordinal sequence.
			nkRows, err := tx.Query(ctx, `SELECT ordinal FROM population_rows WHERE population_id=$1 ORDER BY ordinal`, design.PopulationID)
			if err != nil {
				return err
			}
			var nonKeyIndex int64 = -1
			for nkRows.Next() {
				var ord int64
				if err := nkRows.Scan(&ord); err != nil {
					nkRows.Close()
					return err
				}
				if keyOrdinals[ord] {
					continue
				}
				nonKeyIndex++
				if float64(nonKeyIndex) >= pos && int64(len(nonKeySelectedOrdinals)) < int64(design.SampleSize) {
					nonKeySelectedOrdinals[ord] = true
					pos += interval
				}
			}
			if err := nkRows.Err(); err != nil {
				nkRows.Close()
				return err
			}
			nkRows.Close()
		}

		var intervalPtr, startPtr *float64
		if remainingCount > 0 && design.SampleSize > 0 {
			intervalPtr, startPtr = &interval, &startPoint
		}
		selRow := tx.QueryRow(ctx, `INSERT INTO sample_selections (design_id, tenant_id, method, rng_seed, interval_size, start_point, param_set_id, param_set_version, population_digest_sha256)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING selection_id, design_id, tenant_id, method, rng_seed, interval_size, start_point, param_set_id, param_set_version, population_digest_sha256, selected_at`,
			design.DesignID, svcmiddleware.TenantFromContext(ctx), domain.SamplingMethodSystematicInterval, rngSeed, intervalPtr, startPtr, design.ParamSetID, paramSetVersion, digest)
		sel := &domain.SampleSelection{}
		if err := selRow.Scan(&sel.SelectionID, &sel.DesignID, &sel.TenantID, &sel.Method, &sel.RNGSeed, &sel.IntervalSize, &sel.StartPoint,
			&sel.ParamSetID, &sel.ParamSetVersion, &sel.PopulationDigestSHA256, &sel.SelectedAt); err != nil {
			return err
		}

		var items []*domain.SampleItem
		for _, kr := range keyItemRows {
			item, err := insertSampleItem(ctx, tx, sel.SelectionID, kr.ID, true)
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		for ord := range nonKeySelectedOrdinals {
			var rowID string
			if err := tx.QueryRow(ctx, `SELECT row_id FROM population_rows WHERE population_id=$1 AND ordinal=$2`, design.PopulationID, ord).Scan(&rowID); err != nil {
				return err
			}
			item, err := insertSampleItem(ctx, tx, sel.SelectionID, rowID, false)
			if err != nil {
				return err
			}
			items = append(items, item)
		}

		if _, err := tx.Exec(ctx, `UPDATE sample_designs SET status=$1 WHERE design_id=$2`, domain.SampleDesignSelected, design.DesignID); err != nil {
			return err
		}

		outSel = sel
		outItems = items
		created = true
		return nil
	})
	if errors.Is(err, domain.ErrSampleDesignNotFound) || errors.Is(err, domain.ErrSampleDesignInvalidState) || errors.Is(err, domain.ErrPopulationNotFrozen) {
		return nil, nil, false, err
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return outSel, outItems, created, nil
}

func insertSampleItem(ctx context.Context, tx pgx.Tx, selectionID, populationRowID string, isKeyItem bool) (*domain.SampleItem, error) {
	row := tx.QueryRow(ctx, `INSERT INTO sample_items (selection_id, tenant_id, population_row_id, is_key_item)
		SELECT $1, tenant_id, $2, $3 FROM sample_selections WHERE selection_id=$1
		RETURNING item_id, selection_id, tenant_id, population_row_id, is_key_item, status`,
		selectionID, populationRowID, isKeyItem)
	item := &domain.SampleItem{}
	if err := row.Scan(&item.ItemID, &item.SelectionID, &item.TenantID, &item.PopulationRowID, &item.IsKeyItem, &item.Status); err != nil {
		return nil, err
	}
	return item, nil
}

func loadSelection(ctx context.Context, tx pgx.Tx, designID string) (*domain.SampleSelection, []*domain.SampleItem, error) {
	sel := &domain.SampleSelection{}
	err := tx.QueryRow(ctx, `SELECT selection_id, design_id, tenant_id, method, rng_seed, interval_size, start_point, param_set_id, param_set_version, population_digest_sha256, selected_at
		FROM sample_selections WHERE design_id=$1`, designID).Scan(
		&sel.SelectionID, &sel.DesignID, &sel.TenantID, &sel.Method, &sel.RNGSeed, &sel.IntervalSize, &sel.StartPoint,
		&sel.ParamSetID, &sel.ParamSetVersion, &sel.PopulationDigestSHA256, &sel.SelectedAt)
	if err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT item_id, selection_id, tenant_id, population_row_id, is_key_item, status FROM sample_items WHERE selection_id=$1`, sel.SelectionID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var items []*domain.SampleItem
	for rows.Next() {
		item := &domain.SampleItem{}
		if err := rows.Scan(&item.ItemID, &item.SelectionID, &item.TenantID, &item.PopulationRowID, &item.IsKeyItem, &item.Status); err != nil {
			return nil, nil, err
		}
		items = append(items, item)
	}
	return sel, items, rows.Err()
}

// GetItemResults returns every sample item for a design's own selection,
// each with its (possibly empty) execution history.
func (s *PgStore) GetItemResults(ctx context.Context, tenantID, designID string) ([]*domain.SampleItem, error) {
	var out []*domain.SampleItem
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT si.item_id, si.selection_id, si.tenant_id, si.population_row_id, si.is_key_item, si.status
			FROM sample_items si JOIN sample_selections ss ON ss.selection_id=si.selection_id WHERE ss.design_id=$1`, designID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item := &domain.SampleItem{}
			if err := rows.Scan(&item.ItemID, &item.SelectionID, &item.TenantID, &item.PopulationRowID, &item.IsKeyItem, &item.Status); err != nil {
				return err
			}
			out = append(out, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ReproduceSelection recomputes the exact ordinal set SelectSample derived
// from a selection's own stored inputs and compares it against the
// item set actually recorded — the real proof of "selection method/seed
// preserved" and reproducibility.
func (s *PgStore) ReproduceSelection(ctx context.Context, tenantID, selectionID string) (matches bool, err error) {
	err = s.withTenant(ctx, func(tx pgx.Tx) error {
		var designID, populationID string
		var intervalSize, startPoint *float64
		if scanErr := tx.QueryRow(ctx, `SELECT design_id, interval_size, start_point FROM sample_selections WHERE selection_id=$1`, selectionID).Scan(&designID, &intervalSize, &startPoint); scanErr != nil {
			return scanErr
		}
		if scanErr := tx.QueryRow(ctx, `SELECT population_id FROM sample_designs WHERE design_id=$1`, designID).Scan(&populationID); scanErr != nil {
			return scanErr
		}

		recordedOrdinals := map[int64]bool{}
		rows, qErr := tx.Query(ctx, `SELECT pr.ordinal FROM sample_items si JOIN population_rows pr ON pr.row_id=si.population_row_id WHERE si.selection_id=$1 AND si.is_key_item=false`, selectionID)
		if qErr != nil {
			return qErr
		}
		for rows.Next() {
			var ord int64
			if scanErr := rows.Scan(&ord); scanErr != nil {
				rows.Close()
				return scanErr
			}
			recordedOrdinals[ord] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		if intervalSize == nil || startPoint == nil {
			matches = len(recordedOrdinals) == 0
			return nil
		}

		var keyOrdinals = map[int64]bool{}
		keyRows, qErr := tx.Query(ctx, `SELECT pr.ordinal FROM sample_items si JOIN population_rows pr ON pr.row_id=si.population_row_id WHERE si.selection_id=$1 AND si.is_key_item=true`, selectionID)
		if qErr != nil {
			return qErr
		}
		for keyRows.Next() {
			var ord int64
			if scanErr := keyRows.Scan(&ord); scanErr != nil {
				keyRows.Close()
				return scanErr
			}
			keyOrdinals[ord] = true
		}
		keyRows.Close()

		allRows, qErr := tx.Query(ctx, `SELECT ordinal FROM population_rows WHERE population_id=$1 ORDER BY ordinal`, populationID)
		if qErr != nil {
			return qErr
		}
		recomputed := map[int64]bool{}
		pos := *startPoint
		var nonKeyIndex int64 = -1
		for allRows.Next() {
			var ord int64
			if scanErr := allRows.Scan(&ord); scanErr != nil {
				allRows.Close()
				return scanErr
			}
			if keyOrdinals[ord] {
				continue
			}
			nonKeyIndex++
			if float64(nonKeyIndex) >= pos && int64(len(recomputed)) < int64(len(recordedOrdinals)) {
				recomputed[ord] = true
				pos += *intervalSize
			}
		}
		if err := allRows.Err(); err != nil {
			allRows.Close()
			return err
		}
		allRows.Close()

		matches = len(recomputed) == len(recordedOrdinals)
		if matches {
			for ord := range recordedOrdinals {
				if !recomputed[ord] {
					matches = false
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return matches, nil
}

// RecordItemResult is AUD-NEG-013's own mechanism: the execution row is
// always inserted (evidence is never discarded), but the item's status
// only advances to TESTED when the tested objective matches the sample
// design's own objective — a mismatch is flagged and the item is left
// non-terminal, so it cannot silently count as completed.
func (s *PgStore) RecordItemResult(ctx context.Context, p domain.RecordItemResultParams) (*domain.SampleItem, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.SampleItem
	matched := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var status, designObjective string
		if err := tx.QueryRow(ctx, `SELECT si.status, sd.objective FROM sample_items si
			JOIN sample_selections ss ON ss.selection_id=si.selection_id JOIN sample_designs sd ON sd.design_id=ss.design_id
			WHERE si.item_id=$1`, p.ItemID).Scan(&status, &designObjective); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrSampleItemNotFound
			}
			return err
		}
		if status != string(domain.SampleItemSelected) {
			return domain.ErrSampleItemInvalidState
		}

		if _, err := tx.Exec(ctx, `INSERT INTO sample_executions (item_id, tenant_id, action, objective_tested, result, exception_amount, actor_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, p.ItemID, svcmiddleware.TenantFromContext(ctx), domain.SampleExecutionResult, p.ObjectiveTested, p.Result, p.ExceptionAmount, p.ActorPrincipalID, p.CorrelationID); err != nil {
			return err
		}

		if p.ObjectiveTested != designObjective {
			item, err := scanSampleItem(tx.QueryRow(ctx, `SELECT item_id, selection_id, tenant_id, population_row_id, is_key_item, status FROM sample_items WHERE item_id=$1`, p.ItemID))
			if err != nil {
				return err
			}
			out = item
			return nil
		}

		row, err := scanSampleItem(tx.QueryRow(ctx, `UPDATE sample_items SET status=$1 WHERE item_id=$2 RETURNING item_id, selection_id, tenant_id, population_row_id, is_key_item, status`,
			domain.SampleItemTested, p.ItemID))
		if err != nil {
			return err
		}
		out = row
		matched = true
		return nil
	})
	if errors.Is(err, domain.ErrSampleItemNotFound) || errors.Is(err, domain.ErrSampleItemInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if !matched {
		return out, false, domain.ErrObjectiveMismatch
	}
	return out, matched, nil
}

func scanSampleItem(row pgx.Row) (*domain.SampleItem, error) {
	item := &domain.SampleItem{}
	err := row.Scan(&item.ItemID, &item.SelectionID, &item.TenantID, &item.PopulationRowID, &item.IsKeyItem, &item.Status)
	return item, err
}

// RecordNonresponse and AddAlternativeProcedure are the ONLY paths that
// can move a SELECTED item off SELECTED without a matching RESULT — "no
// silent item replacement": the item's own population_row_id is
// immutable (trg_reject_sample_item_replacement), only its status moves.
func (s *PgStore) RecordNonresponse(ctx context.Context, p domain.RecordNonresponseParams) (*domain.SampleItem, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.SampleItem
	changed := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO sample_executions (item_id, tenant_id, action, objective_tested, actor_principal_id, correlation_id)
			VALUES ($1,$2,$3,'',$4,$5)`, p.ItemID, svcmiddleware.TenantFromContext(ctx), domain.SampleExecutionNonresponse, p.ActorPrincipalID, p.CorrelationID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `UPDATE sample_items SET status=$1 WHERE item_id=$2 AND status=$3 RETURNING item_id, selection_id, tenant_id, population_row_id, is_key_item, status`,
			domain.SampleItemNonresponse, p.ItemID, domain.SampleItemSelected)
		var err error
		out, err = scanSampleItem(row)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sample_items WHERE item_id=$1)`, p.ItemID).Scan(&exists); chkErr != nil {
				return chkErr
			}
			if !exists {
				return domain.ErrSampleItemNotFound
			}
			return domain.ErrSampleItemInvalidState
		}
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrSampleItemNotFound) || errors.Is(err, domain.ErrSampleItemInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

func (s *PgStore) AddAlternativeProcedure(ctx context.Context, p domain.AddAlternativeProcedureParams) (*domain.SampleItem, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.SampleItem
	changed := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM sample_items WHERE item_id=$1`, p.ItemID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrSampleItemNotFound
			}
			return err
		}
		if status != string(domain.SampleItemSelected) && status != string(domain.SampleItemNonresponse) {
			return domain.ErrSampleItemInvalidState
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sample_executions (item_id, tenant_id, action, objective_tested, result, actor_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, p.ItemID, svcmiddleware.TenantFromContext(ctx), domain.SampleExecutionAlternativeProcedure, p.ObjectiveTested, p.Result, p.ActorPrincipalID, p.CorrelationID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `UPDATE sample_items SET status=$1 WHERE item_id=$2 RETURNING item_id, selection_id, tenant_id, population_row_id, is_key_item, status`,
			domain.SampleItemAlternativeProcedure, p.ItemID)
		var err error
		out, err = scanSampleItem(row)
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrSampleItemNotFound) || errors.Is(err, domain.ErrSampleItemInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// EvaluateSample is the real proof of AUD-NEG-014 "sampling evaluation
// omits known exception": completeness_ok is computed from a real query
// (every SELECTED-turned-item must have left SELECTED), and a false value
// blocks the design from ever reaching EVALUATED.
func (s *PgStore) EvaluateSample(ctx context.Context, p domain.EvaluateSampleParams) (*domain.SampleEvaluation, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.SampleEvaluation
	changed := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		design, err := scanSampleDesign(tx.QueryRow(ctx, `SELECT `+sampleDesignColumns+` FROM sample_designs WHERE design_id=$1 FOR UPDATE`, p.DesignID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSampleDesignNotFound
		}
		if err != nil {
			return err
		}
		if design.Status != domain.SampleDesignSelected && design.Status != domain.SampleDesignTesting {
			return domain.ErrSampleDesignInvalidState
		}

		var incompleteCount int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM sample_items si JOIN sample_selections ss ON ss.selection_id=si.selection_id
			WHERE ss.design_id=$1 AND si.status=$2`, p.DesignID, domain.SampleItemSelected).Scan(&incompleteCount); err != nil {
			return err
		}
		completenessOK := incompleteCount == 0

		var exceptionCount int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM sample_executions se JOIN sample_items si ON si.item_id=se.item_id JOIN sample_selections ss ON ss.selection_id=si.selection_id
			WHERE ss.design_id=$1 AND se.action=$2 AND se.result IS NOT NULL AND se.result <> ''`, p.DesignID, domain.SampleExecutionResult).Scan(&exceptionCount); err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `INSERT INTO sample_evaluations (design_id, tenant_id, known_exception_count, completeness_ok, correlation_id)
			VALUES ($1,$2,$3,$4,$5) RETURNING evaluation_id, design_id, tenant_id, projected_misstatement, known_exception_count, completeness_ok, evaluated_at`,
			p.DesignID, svcmiddleware.TenantFromContext(ctx), exceptionCount, completenessOK, p.CorrelationID)
		eval := &domain.SampleEvaluation{}
		if err := row.Scan(&eval.EvaluationID, &eval.DesignID, &eval.TenantID, &eval.ProjectedMisstatement, &eval.KnownExceptionCount, &eval.CompletenessOK, &eval.EvaluatedAt); err != nil {
			return err
		}

		if completenessOK {
			if _, err := tx.Exec(ctx, `UPDATE sample_designs SET status=$1 WHERE design_id=$2`, domain.SampleDesignEvaluated, p.DesignID); err != nil {
				return err
			}
		}
		out = eval
		changed = true
		if !completenessOK {
			return domain.ErrSampleEvaluationIncomplete
		}
		return nil
	})
	if errors.Is(err, domain.ErrSampleEvaluationIncomplete) {
		return out, changed, err
	}
	if errors.Is(err, domain.ErrSampleDesignNotFound) || errors.Is(err, domain.ErrSampleDesignInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// SupersedeSample is AUD-04's own AUD-NEG-011 mechanism — see
// domain.SupersedeSampleParams's own doc comment: parameters cannot be
// changed in place (sampling_parameter_sets is fully immutable), so a
// changed-parameters scenario always produces a NEW design against a NEW
// parameter set, with the old design explicitly marked INVALIDATED.
func (s *PgStore) SupersedeSample(ctx context.Context, p domain.SupersedeSampleParams) (*domain.SampleDesign, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.SampleDesign
	created := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		old, err := scanSampleDesign(tx.QueryRow(ctx, `SELECT `+sampleDesignColumns+` FROM sample_designs WHERE design_id=$1 FOR UPDATE`, p.DesignID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSampleDesignNotFound
		}
		if err != nil {
			return err
		}
		if old.Status == domain.SampleDesignInvalidated {
			return domain.ErrSampleDesignInvalidState
		}

		row := tx.QueryRow(ctx, `INSERT INTO sample_designs (tenant_id, population_id, objective, param_set_id, sample_size, created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+sampleDesignColumns,
			svcmiddleware.TenantFromContext(ctx), old.PopulationID, old.Objective, p.NewParamSetID, old.SampleSize, p.ActorPrincipalID, p.CorrelationID)
		newDesign, err := scanSampleDesign(row)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sample_designs SET status=$1 WHERE design_id=$2`, domain.SampleDesignInvalidated, old.DesignID); err != nil {
			return err
		}
		out = newDesign
		created = true
		return nil
	})
	if errors.Is(err, domain.ErrSampleDesignNotFound) || errors.Is(err, domain.ErrSampleDesignInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}
