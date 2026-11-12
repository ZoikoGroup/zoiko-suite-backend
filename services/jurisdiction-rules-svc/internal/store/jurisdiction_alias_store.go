package store

// REF-01 aliases and code resolution (migration 000028): AmendJurisdictionAlias,
// ResolveJurisdiction, MapExternalJurisdictionCode.

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

const aliasColumns = `alias_id, jurisdiction_id, alias_system, alias_code, source_standard, source_version,
	mapping_rationale, evidence_ref, valid_from, valid_to, created_by, created_at, ended_by, ended_at, end_reason`

func scanAlias(row pgx.Row) (*domain.JurisdictionAlias, error) {
	a := &domain.JurisdictionAlias{}
	err := row.Scan(&a.AliasID, &a.JurisdictionID, &a.AliasSystem, &a.AliasCode, &a.SourceStandard, &a.SourceVersion,
		&a.MappingRationale, &a.EvidenceRef, &a.ValidFrom, &a.ValidTo, &a.CreatedBy, &a.CreatedAt, &a.EndedBy, &a.EndedAt, &a.EndReason)
	return a, err
}

func (s *PgStore) unavailable(op string, err error) error {
	s.log.Error("pg "+op+" failed", zap.Error(err))
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

// touchJurisdiction records that the jurisdiction changed without any of its
// versioned attributes changing (an alias was added or ended), which bumps its
// row_version so the event that follows carries a version a consumer can pin.
// A retired jurisdiction is not touched (the duplicate-deactivation trigger
// skips the update), so its current row is returned unchanged.
func (s *PgStore) touchJurisdiction(ctx context.Context, q Querier, id, actor string) (*domain.Jurisdiction, error) {
	j, err := scanJurisdiction(q.QueryRow(ctx, `
		UPDATE jurisdictions SET updated_at = NOW(), updated_by_principal_id = $2
		 WHERE jurisdiction_id = $1
		RETURNING `+jurisdictionColumns, id, actor))
	if err == nil {
		return j, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return s.FindByIDAny(ctx, id)
	}
	return nil, s.unavailable("touchJurisdiction", err)
}

// AddJurisdictionAliasWithQuerier records that (system, code) means the
// jurisdiction over [ValidFrom, ValidTo). The bool is true when this call
// created the mapping and false for an idempotent replay of an identical one.
//
// The jurisdiction must be in force: a retired jurisdiction gets no new codes.
// With ReplacesAliasID the old mapping of the same code is ended at ValidFrom in
// the same transaction, which is how a code is moved to another jurisdiction. A
// mapping that would overlap another meaning of the same code is a conflict,
// and the exclusion constraint makes that true under concurrency too.
func (s *PgStore) AddJurisdictionAliasWithQuerier(ctx context.Context, q Querier, p domain.AddAliasParams) (*domain.JurisdictionAlias, bool, *domain.Jurisdiction, error) {
	if err := p.Validate(); err != nil {
		return nil, false, nil, err
	}
	if _, err := s.FindByID(ctx, p.JurisdictionID); err != nil {
		return nil, false, nil, err
	}
	norm := domain.NormalizeCode(p.AliasCode)

	if p.ReplacesAliasID != "" {
		old, err := scanAlias(q.QueryRow(ctx, `
			SELECT `+aliasColumns+` FROM jurisdiction_aliases WHERE alias_id = $1 FOR UPDATE`, p.ReplacesAliasID))
		if errors.Is(err, pgx.ErrNoRows) || (err != nil && isInvalidTextRepresentation(err)) {
			return nil, false, nil, domain.ErrAliasNotFound
		}
		if err != nil {
			return nil, false, nil, s.unavailable("AddJurisdictionAlias(replaces)", err)
		}
		if old.AliasSystem != p.AliasSystem || domain.NormalizeCode(old.AliasCode) != norm {
			return nil, false, nil, fmt.Errorf("%w: replaces_alias_id names a different code", domain.ErrInvalidAlias)
		}
		if old.EndedAt != nil || (old.ValidTo != nil && !old.ValidTo.After(p.ValidFrom)) {
			return nil, false, nil, fmt.Errorf("%w: the mapping being replaced has already ended", domain.ErrInvalidAlias)
		}
		if !p.ValidFrom.After(old.ValidFrom) {
			return nil, false, nil, fmt.Errorf("%w: a replacement must take effect after the mapping it replaces starts", domain.ErrInvalidAlias)
		}
		if _, err := q.Exec(ctx, `
			UPDATE jurisdiction_aliases
			   SET valid_to = $2, ended_by = $3, ended_at = clock_timestamp(), end_reason = $4
			 WHERE alias_id = $1`, old.AliasID, p.ValidFrom, p.ActorID, "replaced by a new mapping of the same code"); err != nil {
			return nil, false, nil, s.unavailable("AddJurisdictionAlias(end replaced)", err)
		}
	}

	// An identical mapping already recorded is a replay, not a conflict.
	existing, err := scanAlias(q.QueryRow(ctx, `
		SELECT `+aliasColumns+` FROM jurisdiction_aliases
		 WHERE jurisdiction_id = $1 AND alias_system = $2 AND alias_code_norm = $3 AND valid_from = $4`,
		p.JurisdictionID, p.AliasSystem, norm, p.ValidFrom))
	switch {
	case err == nil:
		sameTo := (existing.ValidTo == nil && p.ValidTo == nil) ||
			(existing.ValidTo != nil && p.ValidTo != nil && existing.ValidTo.Equal(*p.ValidTo))
		if existing.SourceStandard == p.SourceStandard && existing.SourceVersion == p.SourceVersion &&
			existing.MappingRationale == p.MappingRationale && sameTo {
			j, jerr := s.FindByIDAny(ctx, p.JurisdictionID)
			return existing, false, j, jerr
		}
		return nil, false, nil, domain.ErrAliasConflict
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, false, nil, s.unavailable("AddJurisdictionAlias(lookup)", err)
	}

	a, err := scanAlias(q.QueryRow(ctx, `
		INSERT INTO jurisdiction_aliases (jurisdiction_id, alias_system, alias_code, source_standard, source_version,
		                                  mapping_rationale, evidence_ref, valid_from, valid_to, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10)
		RETURNING `+aliasColumns,
		p.JurisdictionID, p.AliasSystem, strings.TrimSpace(p.AliasCode), p.SourceStandard, p.SourceVersion,
		p.MappingRationale, p.EvidenceRef, p.ValidFrom, p.ValidTo, p.ActorID))
	if err != nil {
		if isExclusionViolation(err) {
			return nil, false, nil, domain.ErrAliasConflict
		}
		if isStringDataRightTruncation(err) {
			return nil, false, nil, domain.ErrInputTooLong
		}
		return nil, false, nil, s.unavailable("AddJurisdictionAlias", err)
	}
	j, err := s.touchJurisdiction(ctx, q, p.JurisdictionID, p.ActorID)
	if err != nil {
		return nil, false, nil, err
	}
	return a, true, j, nil
}

// EndJurisdictionAliasWithQuerier ends a mapping at ValidTo. The bool is true
// when this call ended it and false when it had already ended (a replay, whose
// original end is kept). An end can only bring a mapping's end forward.
func (s *PgStore) EndJurisdictionAliasWithQuerier(ctx context.Context, q Querier, p domain.EndAliasParams) (*domain.JurisdictionAlias, bool, *domain.Jurisdiction, error) {
	if err := p.Validate(); err != nil {
		return nil, false, nil, err
	}
	a, err := scanAlias(q.QueryRow(ctx, `
		SELECT `+aliasColumns+` FROM jurisdiction_aliases
		 WHERE alias_id = $1 AND jurisdiction_id = $2 FOR UPDATE`, p.AliasID, p.JurisdictionID))
	if errors.Is(err, pgx.ErrNoRows) || (err != nil && isInvalidTextRepresentation(err)) {
		return nil, false, nil, domain.ErrAliasNotFound
	}
	if err != nil {
		return nil, false, nil, s.unavailable("EndJurisdictionAlias(lookup)", err)
	}
	if a.EndedAt != nil {
		j, jerr := s.FindByIDAny(ctx, p.JurisdictionID)
		return a, false, j, jerr
	}
	if !p.ValidTo.After(a.ValidFrom) {
		return nil, false, nil, fmt.Errorf("%w: valid_to must be after the mapping's valid_from", domain.ErrInvalidAlias)
	}
	if a.ValidTo != nil && !p.ValidTo.Before(*a.ValidTo) {
		return nil, false, nil, fmt.Errorf("%w: valid_to is not before the mapping's existing end; an end can only bring it forward", domain.ErrInvalidAlias)
	}
	ended, err := scanAlias(q.QueryRow(ctx, `
		UPDATE jurisdiction_aliases
		   SET valid_to = $2, ended_by = $3, ended_at = clock_timestamp(), end_reason = $4
		 WHERE alias_id = $1
		RETURNING `+aliasColumns, p.AliasID, p.ValidTo, p.ActorID, strings.TrimSpace(p.Reason)))
	if err != nil {
		return nil, false, nil, s.unavailable("EndJurisdictionAlias", err)
	}
	j, err := s.touchJurisdiction(ctx, q, p.JurisdictionID, p.ActorID)
	if err != nil {
		return nil, false, nil, err
	}
	return ended, true, j, nil
}

// ListJurisdictionAliases returns a jurisdiction's mappings, oldest first. With
// at set, only those in force then; without it, the whole history including
// ended mappings.
func (s *PgStore) ListJurisdictionAliases(ctx context.Context, jurisdictionID string, at *time.Time) ([]*domain.JurisdictionAlias, error) {
	if _, err := s.FindByIDAny(ctx, jurisdictionID); err != nil {
		return nil, err
	}
	query := `SELECT ` + aliasColumns + ` FROM jurisdiction_aliases WHERE jurisdiction_id = $1`
	args := []any{jurisdictionID}
	if at != nil {
		query += ` AND valid_from <= $2 AND (valid_to IS NULL OR valid_to > $2)`
		args = append(args, *at)
	}
	rows, err := s.db(ctx).Query(ctx, query+` ORDER BY valid_from, alias_system, alias_code_norm`, args...)
	if err != nil {
		return nil, s.unavailable("ListJurisdictionAliases", err)
	}
	defer rows.Close()
	out := []*domain.JurisdictionAlias{}
	for rows.Next() {
		a, err := scanAlias(rows)
		if err != nil {
			return nil, s.unavailable("ListJurisdictionAliases(scan)", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, s.unavailable("ListJurisdictionAliases(rows)", err)
	}
	return out, nil
}

// ResolveCode resolves a code to exactly one jurisdiction as it was at q.AsOf,
// or says precisely why it cannot. It never falls back to a near match.
//
//   - ErrUnknownJurisdictionCode: nothing is known by that code in that system.
//   - *domain.AliasNotEffectiveError: the code is mapped, but not then.
//   - *domain.RetiredError: it resolves to a jurisdiction retired by then, with
//     the retirement and the successor.
//   - *domain.NotEffectiveError: it resolves to a jurisdiction not yet in force.
//   - *domain.AmbiguousError: more than one jurisdiction answers to an internal
//     code; the caller narrows by type.
func (s *PgStore) ResolveCode(ctx context.Context, q domain.ResolveQuery) (*domain.CodeResolution, error) {
	if domain.NormalizeCode(q.Code) == "" {
		return nil, fmt.Errorf("%w: code is required", domain.ErrInvalidAlias)
	}
	system := strings.TrimSpace(q.System)
	if system == "" || system == domain.SystemInternal {
		return s.resolveInternalCode(ctx, q)
	}
	return s.resolveAliasCode(ctx, system, q)
}

func (s *PgStore) resolveAliasCode(ctx context.Context, system string, q domain.ResolveQuery) (*domain.CodeResolution, error) {
	norm := domain.NormalizeCode(q.Code)
	a, err := scanAlias(s.db(ctx).QueryRow(ctx, `
		SELECT `+aliasColumns+` FROM jurisdiction_aliases
		 WHERE alias_system = $1 AND alias_code_norm = $2
		   AND valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3)`, system, norm, q.AsOf))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.unmappedOrUnknown(ctx, system, norm, q.AsOf)
	}
	if err != nil {
		return nil, s.unavailable("ResolveCode(alias)", err)
	}

	asOf, err := s.GetJurisdictionAsOf(ctx, a.JurisdictionID, q.AsOf)
	if err != nil {
		return nil, s.explainNotInForce(ctx, err, q.AsOf)
	}
	return resolutionOf(asOf, system, a), nil
}

// unmappedOrUnknown distinguishes a code that is mapped at some other time from
// one that was never mapped at all.
func (s *PgStore) unmappedOrUnknown(ctx context.Context, system, norm string, at time.Time) error {
	var from time.Time
	var to *time.Time
	var open bool
	err := s.db(ctx).QueryRow(ctx, `
		SELECT min(valid_from), max(valid_to), bool_or(valid_to IS NULL)
		  FROM jurisdiction_aliases WHERE alias_system = $1 AND alias_code_norm = $2
		HAVING count(*) > 0`, system, norm).Scan(&from, &to, &open)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrUnknownJurisdictionCode
	}
	if err != nil {
		return s.unavailable("ResolveCode(unmapped)", err)
	}
	if open {
		to = nil
	}
	return &domain.AliasNotEffectiveError{System: system, Code: norm, AsOf: at.UTC(), From: from, To: to}
}

// explainNotInForce turns "the jurisdiction had no version in force at the
// instant" into the right answer: retired if it was already over, otherwise
// not yet in force.
func (s *PgStore) explainNotInForce(ctx context.Context, err error, at time.Time) error {
	var ne *domain.NotEffectiveError
	if !errors.As(err, &ne) || at.Before(ne.EffectiveFrom) {
		return err
	}
	j, ferr := s.FindByIDAny(ctx, ne.JurisdictionID)
	if ferr != nil {
		return ferr
	}
	return &domain.RetiredError{
		JurisdictionID: j.JurisdictionID, Code: j.JurisdictionCode, RetiredAt: j.EffectiveTo,
		SupersededBy: j.SupersededByJurisdictionID, Reason: j.RetirementReason,
	}
}

func (s *PgStore) resolveInternalCode(ctx context.Context, q domain.ResolveQuery) (*domain.CodeResolution, error) {
	norm := domain.NormalizeCode(q.Code)
	rows, err := s.db(ctx).Query(ctx, `
		SELECT jurisdiction_id::text, jurisdiction_type, jurisdiction_name
		  FROM jurisdictions
		 WHERE upper(jurisdiction_code) = $1 AND ($2 = '' OR jurisdiction_type = $2)
		 ORDER BY jurisdiction_type, jurisdiction_id`, norm, strings.TrimSpace(q.Type))
	if err != nil {
		return nil, s.unavailable("ResolveCode(internal)", err)
	}
	var candidates []domain.AmbiguityCandidate
	for rows.Next() {
		var c domain.AmbiguityCandidate
		if err := rows.Scan(&c.JurisdictionID, &c.JurisdictionType, &c.JurisdictionName); err != nil {
			rows.Close()
			return nil, s.unavailable("ResolveCode(internal scan)", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, s.unavailable("ResolveCode(internal rows)", err)
	}
	if len(candidates) == 0 {
		return nil, domain.ErrUnknownJurisdictionCode
	}

	var inForce []*domain.JurisdictionAsOf
	var inForceCands []domain.AmbiguityCandidate
	var notInForce []error
	for _, c := range candidates {
		asOf, err := s.GetJurisdictionAsOf(ctx, c.JurisdictionID, q.AsOf)
		if err != nil {
			var ne *domain.NotEffectiveError
			if errors.As(err, &ne) {
				notInForce = append(notInForce, s.explainNotInForce(ctx, err, q.AsOf))
				continue
			}
			return nil, err
		}
		inForce = append(inForce, asOf)
		inForceCands = append(inForceCands, c)
	}
	switch {
	case len(inForce) == 1:
		return resolutionOf(inForce[0], domain.SystemInternal, nil), nil
	case len(inForce) > 1:
		return nil, &domain.AmbiguousError{Code: norm, Candidates: inForceCands}
	case len(notInForce) == 1:
		return nil, notInForce[0]
	default:
		// Several jurisdictions share the code and none is in force: say so
		// rather than pick the most plausible one.
		return nil, &domain.AmbiguousError{Code: norm, Candidates: candidates}
	}
}

func resolutionOf(a *domain.JurisdictionAsOf, via string, alias *domain.JurisdictionAlias) *domain.CodeResolution {
	return &domain.CodeResolution{
		JurisdictionID: a.JurisdictionID, JurisdictionCode: a.JurisdictionCode, JurisdictionName: a.JurisdictionName,
		JurisdictionType: a.JurisdictionType, AuthorityType: a.AuthorityType, ParentJurisdictionID: a.ParentJurisdictionID,
		AsOf: a.AsOf, VersionNo: a.VersionNo, MatchedVia: via, Alias: alias,
	}
}
