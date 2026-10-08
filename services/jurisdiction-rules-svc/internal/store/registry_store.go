package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 0 registry persistence: regimes, source register,
// interpretation records, packs and pack versions, rule provenance.
//
// The database triggers in migration 000005 are the final guard for the
// document's invariants; the checks here exist to return a precise error
// instead of a raw constraint failure.

const (
	regimeCols = `regime_id, regime_code, regime_name, description, active_flag, created_at, created_by_principal_id, schema_version`

	sourceCols = `source_id, jurisdiction_id, authority, source_type, authority_level, title, official_identifier,
		to_char(published_on,'YYYY-MM-DD'), to_char(effective_on,'YYYY-MM-DD'), location, snapshot_hash, snapshot_ref,
		language, translation_ref, interpretation_notes, reviewed_by_principal_id, reviewed_at,
		superseded_by_source_id, superseded_at, created_at, created_by_principal_id, schema_version`

	interpCols = `interpretation_id, jurisdiction_id, regime_id, subject, decision, rationale, status,
		approved_by_principal_id, approved_at, created_at, created_by_principal_id, schema_version`

	packCols = `pack_id, pack_ref, pack_name, owner, support_owner, created_at, created_by_principal_id, schema_version`

	packVersionCols = `v.pack_version_id, v.pack_id, p.pack_ref, v.version, v.status,
		to_char(v.effective_from,'YYYY-MM-DD'), to_char(v.effective_to,'YYYY-MM-DD'), v.manifest::text,
		v.manifest_digest, v.artifact_digest, v.test_bundle_digest, v.signature, v.signature_key_ref,
		v.source_register_version, v.created_at, v.created_by_principal_id, v.updated_at,
		v.updated_by_principal_id, v.schema_version`
)

// registryFail maps a low-level error onto a domain error.
func (s *PgStore) registryFail(op string, err error, notFound error) error {
	if err == nil {
		return nil
	}
	if notFound != nil && (errors.Is(err, pgx.ErrNoRows) || isInvalidTextRepresentation(err)) {
		return notFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23503":
			return domain.ErrInvalidReference
		case pgErr.Code == "23514" && strings.Contains(pgErr.ConstraintName, "independent"):
			return domain.ErrNotIndependent
		case pgErr.Code == "23514" || pgErr.Code == "23505":
			// Trigger-enforced immutability or a uniqueness race.
			return fmt.Errorf("%w: %s", domain.ErrConflict, pgErr.Message)
		}
	}
	s.log.Error("pg "+op+" failed", zap.Error(err))
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

func (s *PgStore) begin(ctx context.Context, op string) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg "+op+": begin failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return tx, nil
}

// ── scanners ────────────────────────────────────────────────────────────────

func scanRegime(r pgx.Row) (*domain.Regime, error) {
	var x domain.Regime
	err := r.Scan(&x.RegimeID, &x.RegimeCode, &x.RegimeName, &x.Description, &x.ActiveFlag, &x.CreatedAt,
		&x.CreatedByPrincipalID, &x.SchemaVersion)
	return &x, err
}

func scanSource(r pgx.Row) (*domain.RegulatorySource, error) {
	var x domain.RegulatorySource
	err := r.Scan(&x.SourceID, &x.JurisdictionID, &x.Authority, &x.SourceType, &x.AuthorityLevel, &x.Title,
		&x.OfficialIdentifier, &x.PublishedOn, &x.EffectiveOn, &x.Location, &x.SnapshotHash, &x.SnapshotRef,
		&x.Language, &x.TranslationRef, &x.InterpretationNotes, &x.ReviewedByPrincipalID, &x.ReviewedAt,
		&x.SupersededBySourceID, &x.SupersededAt, &x.CreatedAt, &x.CreatedByPrincipalID, &x.SchemaVersion)
	return &x, err
}

func scanInterp(r pgx.Row) (*domain.InterpretationRecord, error) {
	var x domain.InterpretationRecord
	err := r.Scan(&x.InterpretationID, &x.JurisdictionID, &x.RegimeID, &x.Subject, &x.Decision, &x.Rationale,
		&x.Status, &x.ApprovedByPrincipal, &x.ApprovedAt, &x.CreatedAt, &x.CreatedByPrincipalID, &x.SchemaVersion)
	return &x, err
}

func scanPack(r pgx.Row) (*domain.Pack, error) {
	var x domain.Pack
	err := r.Scan(&x.PackID, &x.PackRef, &x.PackName, &x.Owner, &x.SupportOwner, &x.CreatedAt,
		&x.CreatedByPrincipalID, &x.SchemaVersion)
	return &x, err
}

func scanPackVersion(r pgx.Row) (*domain.PackVersion, error) {
	var x domain.PackVersion
	var manifest string
	err := r.Scan(&x.PackVersionID, &x.PackID, &x.PackRef, &x.Version, &x.Status, &x.EffectiveFrom, &x.EffectiveTo,
		&manifest, &x.ManifestDigest, &x.ArtifactDigest, &x.TestBundleDigest, &x.Signature, &x.SignatureKeyRef,
		&x.SourceRegisterVersion, &x.CreatedAt, &x.CreatedByPrincipalID, &x.UpdatedAt, &x.UpdatedByPrincipalID,
		&x.SchemaVersion)
	x.Manifest = json.RawMessage(manifest)
	return &x, err
}

// ── regimes ─────────────────────────────────────────────────────────────────

// CreateRegime inserts a regime idempotently by code. A replay with a
// different name is a conflict, not a silent rename.
func (s *PgStore) CreateRegime(ctx context.Context, p domain.CreateRegimeParams) (*domain.Regime, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO regulatory_regimes (regime_code, regime_name, description, created_by_principal_id)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (regime_code) DO NOTHING
		RETURNING `+regimeCols, p.RegimeCode, p.RegimeName, p.Description, p.CreatedBy)
	x, err := scanRegime(row)
	if err == nil {
		return x, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, s.registryFail("CreateRegime", err, nil)
	}
	x, err = scanRegime(s.pool.QueryRow(ctx, `SELECT `+regimeCols+` FROM regulatory_regimes WHERE regime_code=$1`, p.RegimeCode))
	if err != nil {
		return nil, false, s.registryFail("CreateRegime lookup", err, nil)
	}
	if x.RegimeName != p.RegimeName {
		return nil, false, domain.ErrConflict
	}
	return x, false, nil
}

func (s *PgStore) GetRegime(ctx context.Context, id string) (*domain.Regime, error) {
	x, err := scanRegime(s.pool.QueryRow(ctx, `SELECT `+regimeCols+` FROM regulatory_regimes WHERE regime_id::text=$1 OR regime_code=$1`, id))
	if err != nil {
		return nil, s.registryFail("GetRegime", err, domain.ErrRegimeNotFound)
	}
	return x, nil
}

func (s *PgStore) ListRegimes(ctx context.Context) ([]*domain.Regime, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+regimeCols+` FROM regulatory_regimes ORDER BY regime_code`)
	if err != nil {
		return nil, s.registryFail("ListRegimes", err, nil)
	}
	defer rows.Close()
	out := []*domain.Regime{}
	for rows.Next() {
		x, err := scanRegime(rows)
		if err != nil {
			return nil, s.registryFail("ListRegimes scan", err, nil)
		}
		out = append(out, x)
	}
	return out, s.registryFail("ListRegimes rows", rows.Err(), nil)
}

// ── source register ─────────────────────────────────────────────────────────

// CreateSource captures a source idempotently on (jurisdiction, authority,
// snapshot hash). The same bytes captured twice is the same evidence; the
// same hash with a different title or location is a conflict.
func (s *PgStore) CreateSource(ctx context.Context, p domain.CreateSourceParams) (*domain.RegulatorySource, bool, error) {
	if _, err := s.FindByIDAny(ctx, p.JurisdictionID); err != nil {
		return nil, false, err
	}
	if p.Language == "" {
		p.Language = "en"
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO regulatory_sources (jurisdiction_id, authority, source_type, authority_level, title,
			official_identifier, published_on, effective_on, location, snapshot_hash, snapshot_ref, language,
			translation_ref, interpretation_notes, created_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7::date,$8::date,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (jurisdiction_id, authority, snapshot_hash) DO NOTHING
		RETURNING `+sourceCols,
		p.JurisdictionID, p.Authority, p.SourceType, p.AuthorityLevel, p.Title, p.OfficialIdentifier,
		p.PublishedOn, p.EffectiveOn, p.Location, p.SnapshotHash, p.SnapshotRef, p.Language,
		p.TranslationRef, p.InterpretationNotes, p.CreatedBy)
	x, err := scanSource(row)
	if err == nil {
		return x, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, s.registryFail("CreateSource", err, nil)
	}
	x, err = scanSource(s.pool.QueryRow(ctx, `SELECT `+sourceCols+` FROM regulatory_sources
		WHERE jurisdiction_id=$1 AND authority=$2 AND snapshot_hash=$3`, p.JurisdictionID, p.Authority, p.SnapshotHash))
	if err != nil {
		return nil, false, s.registryFail("CreateSource lookup", err, nil)
	}
	if x.Title != p.Title || x.Location != p.Location || x.SourceType != p.SourceType || x.AuthorityLevel != p.AuthorityLevel {
		return nil, false, domain.ErrConflict
	}
	return x, false, nil
}

func (s *PgStore) GetSource(ctx context.Context, id string) (*domain.RegulatorySource, error) {
	x, err := scanSource(s.pool.QueryRow(ctx, `SELECT `+sourceCols+` FROM regulatory_sources WHERE source_id::text=$1`, id))
	if err != nil {
		return nil, s.registryFail("GetSource", err, domain.ErrSourceNotFound)
	}
	return x, nil
}

// ListSources lists sources, newest first, optionally filtered.
func (s *PgStore) ListSources(ctx context.Context, jurisdictionID, authority string, limit, offset int) ([]*domain.RegulatorySource, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sourceCols+` FROM regulatory_sources
		WHERE ($1 = '' OR jurisdiction_id::text = $1) AND ($2 = '' OR authority = $2)
		ORDER BY created_at DESC, source_id LIMIT $3 OFFSET $4`,
		jurisdictionID, authority, clampLimit(limit, 50, 200), offset)
	if err != nil {
		return nil, s.registryFail("ListSources", err, nil)
	}
	defer rows.Close()
	out := []*domain.RegulatorySource{}
	for rows.Next() {
		x, err := scanSource(rows)
		if err != nil {
			return nil, s.registryFail("ListSources scan", err, nil)
		}
		out = append(out, x)
	}
	return out, s.registryFail("ListSources rows", rows.Err(), nil)
}

// ReviewSource records the independent review, once. A replay by the same
// reviewer is a no-op; a second, different reviewer is refused.
func (s *PgStore) ReviewSource(ctx context.Context, id, reviewer string) (*domain.RegulatorySource, bool, error) {
	cur, err := s.GetSource(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if cur.CreatedByPrincipalID == reviewer {
		return nil, false, domain.ErrNotIndependent
	}
	if cur.ReviewedByPrincipalID != nil {
		if *cur.ReviewedByPrincipalID == reviewer {
			return cur, false, nil
		}
		return nil, false, domain.ErrAlreadyDecided
	}
	x, err := scanSource(s.pool.QueryRow(ctx, `UPDATE regulatory_sources
		SET reviewed_by_principal_id=$2, reviewed_at=NOW()
		WHERE source_id::text=$1 AND reviewed_by_principal_id IS NULL
		RETURNING `+sourceCols, id, reviewer))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrAlreadyDecided // lost a race
		}
		return nil, false, s.registryFail("ReviewSource", err, domain.ErrSourceNotFound)
	}
	return x, true, nil
}

// SupersedeSource links a source to the one that replaces it. The old row is
// preserved (s31: source withdrawal preserves the record).
func (s *PgStore) SupersedeSource(ctx context.Context, id, replacementID string) (*domain.RegulatorySource, bool, error) {
	cur, err := s.GetSource(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if cur.SupersededBySourceID != nil {
		if *cur.SupersededBySourceID == replacementID {
			return cur, false, nil
		}
		return nil, false, domain.ErrAlreadyDecided
	}
	if id == replacementID {
		return nil, false, domain.ErrInvalidReference
	}
	if _, err := s.GetSource(ctx, replacementID); err != nil {
		if errors.Is(err, domain.ErrSourceNotFound) {
			return nil, false, domain.ErrInvalidReference
		}
		return nil, false, err
	}
	x, err := scanSource(s.pool.QueryRow(ctx, `UPDATE regulatory_sources
		SET superseded_by_source_id=$2, superseded_at=NOW()
		WHERE source_id::text=$1 AND superseded_by_source_id IS NULL
		RETURNING `+sourceCols, id, replacementID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrAlreadyDecided
		}
		return nil, false, s.registryFail("SupersedeSource", err, domain.ErrSourceNotFound)
	}
	return x, true, nil
}

// ── interpretation records ──────────────────────────────────────────────────

func (s *PgStore) interpretationSources(ctx context.Context, q querier, id string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT source_id::text FROM interpretation_sources WHERE interpretation_id::text=$1 ORDER BY source_id`, id)
	if err != nil {
		return nil, s.registryFail("interpretationSources", err, nil)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, s.registryFail("interpretationSources scan", err, nil)
		}
		out = append(out, sid)
	}
	return out, s.registryFail("interpretationSources rows", rows.Err(), nil)
}

// CreateInterpretation records a PENDING interpretation tied to at least one
// existing, not-superseded source.
func (s *PgStore) CreateInterpretation(ctx context.Context, p domain.CreateInterpretationParams) (*domain.InterpretationRecord, error) {
	if len(p.SourceIDs) == 0 {
		return nil, domain.ErrInvalidReference
	}
	if _, err := s.FindByIDAny(ctx, p.JurisdictionID); err != nil {
		return nil, err
	}
	tx, err := s.begin(ctx, "CreateInterpretation")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var live int
	if err := tx.QueryRow(ctx, `SELECT COUNT(DISTINCT source_id) FROM regulatory_sources
		WHERE source_id::text = ANY($1) AND superseded_by_source_id IS NULL`, p.SourceIDs).Scan(&live); err != nil {
		return nil, s.registryFail("CreateInterpretation sources", err, domain.ErrInvalidReference)
	}
	if live != len(uniq(p.SourceIDs)) {
		return nil, domain.ErrInvalidReference
	}

	x, err := scanInterp(tx.QueryRow(ctx, `INSERT INTO interpretation_records
		(jurisdiction_id, regime_id, subject, decision, rationale, created_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+interpCols,
		p.JurisdictionID, p.RegimeID, p.Subject, p.Decision, p.Rationale, p.CreatedBy))
	if err != nil {
		return nil, s.registryFail("CreateInterpretation insert", err, nil)
	}
	for _, sid := range uniq(p.SourceIDs) {
		if _, err := tx.Exec(ctx, `INSERT INTO interpretation_sources (interpretation_id, source_id) VALUES ($1,$2)`,
			x.InterpretationID, sid); err != nil {
			return nil, s.registryFail("CreateInterpretation link", err, nil)
		}
	}
	x.SourceIDs, err = s.interpretationSources(ctx, tx, x.InterpretationID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("CreateInterpretation commit", err, nil)
	}
	return x, nil
}

func (s *PgStore) GetInterpretation(ctx context.Context, id string) (*domain.InterpretationRecord, error) {
	x, err := scanInterp(s.pool.QueryRow(ctx, `SELECT `+interpCols+` FROM interpretation_records WHERE interpretation_id::text=$1`, id))
	if err != nil {
		return nil, s.registryFail("GetInterpretation", err, domain.ErrInterpretationNotFound)
	}
	x.SourceIDs, err = s.interpretationSources(ctx, s.pool, x.InterpretationID)
	return x, err
}

// ApproveInterpretation moves PENDING to APPROVED. The approver must differ
// from the author, and every linked source must have had its independent
// review and must not be superseded.
func (s *PgStore) ApproveInterpretation(ctx context.Context, id, approver string) (*domain.InterpretationRecord, bool, error) {
	cur, err := s.GetInterpretation(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if cur.Status == "APPROVED" {
		if cur.ApprovedByPrincipal != nil && *cur.ApprovedByPrincipal == approver {
			return cur, false, nil
		}
		return nil, false, domain.ErrAlreadyDecided
	}
	if cur.CreatedByPrincipalID == approver {
		return nil, false, domain.ErrNotIndependent
	}
	var unready int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM regulatory_sources rs
		JOIN interpretation_sources i ON i.source_id = rs.source_id
		WHERE i.interpretation_id::text=$1 AND (rs.reviewed_by_principal_id IS NULL OR rs.superseded_by_source_id IS NOT NULL)`,
		id).Scan(&unready); err != nil {
		return nil, false, s.registryFail("ApproveInterpretation sources", err, nil)
	}
	if unready > 0 {
		return nil, false, domain.ErrSourceNotReady
	}
	x, err := scanInterp(s.pool.QueryRow(ctx, `UPDATE interpretation_records
		SET status='APPROVED', approved_by_principal_id=$2, approved_at=NOW()
		WHERE interpretation_id::text=$1 AND status='PENDING' RETURNING `+interpCols, id, approver))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrAlreadyDecided
		}
		return nil, false, s.registryFail("ApproveInterpretation", err, domain.ErrInterpretationNotFound)
	}
	x.SourceIDs = cur.SourceIDs
	return x, true, nil
}

// ── packs ───────────────────────────────────────────────────────────────────

func (s *PgStore) CreatePack(ctx context.Context, p domain.CreatePackParams) (*domain.Pack, bool, error) {
	x, err := scanPack(s.pool.QueryRow(ctx, `INSERT INTO jurisdiction_packs (pack_ref, pack_name, owner, support_owner, created_by_principal_id)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (pack_ref) DO NOTHING RETURNING `+packCols,
		p.PackRef, p.PackName, p.Owner, p.SupportOwner, p.CreatedBy))
	if err == nil {
		return x, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, s.registryFail("CreatePack", err, nil)
	}
	x, err = scanPack(s.pool.QueryRow(ctx, `SELECT `+packCols+` FROM jurisdiction_packs WHERE pack_ref=$1`, p.PackRef))
	if err != nil {
		return nil, false, s.registryFail("CreatePack lookup", err, nil)
	}
	if x.PackName != p.PackName || x.Owner != p.Owner {
		return nil, false, domain.ErrConflict
	}
	return x, false, nil
}

func (s *PgStore) GetPack(ctx context.Context, ref string) (*domain.Pack, error) {
	x, err := scanPack(s.pool.QueryRow(ctx, `SELECT `+packCols+` FROM jurisdiction_packs WHERE pack_ref=$1`, ref))
	if err != nil {
		return nil, s.registryFail("GetPack", err, domain.ErrPackNotFound)
	}
	return x, nil
}

func (s *PgStore) ListPacks(ctx context.Context, limit, offset int) ([]*domain.Pack, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+packCols+` FROM jurisdiction_packs ORDER BY pack_ref LIMIT $1 OFFSET $2`,
		clampLimit(limit, 50, 200), offset)
	if err != nil {
		return nil, s.registryFail("ListPacks", err, nil)
	}
	defer rows.Close()
	out := []*domain.Pack{}
	for rows.Next() {
		x, err := scanPack(rows)
		if err != nil {
			return nil, s.registryFail("ListPacks scan", err, nil)
		}
		out = append(out, x)
	}
	return out, s.registryFail("ListPacks rows", rows.Err(), nil)
}

// CreatePackVersion records a DRAFT pack version from a validated manifest.
//
// Resolves the manifest's jurisdiction codes and regime codes against the
// registries (a client cannot name something that does not exist), refuses a
// version that is not strictly greater than every existing one, and treats a
// byte-different manifest under an existing version as an immutable-version
// collision (JUR-NEG-20).
func (s *PgStore) CreatePackVersion(ctx context.Context, p domain.CreatePackVersionParams) (*domain.PackVersion, bool, error) {
	tx, err := s.begin(ctx, "CreatePackVersion")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var packID string
	if err := tx.QueryRow(ctx, `SELECT pack_id::text FROM jurisdiction_packs WHERE pack_ref=$1 FOR UPDATE`, p.PackRef).Scan(&packID); err != nil {
		return nil, false, s.registryFail("CreatePackVersion lock", err, domain.ErrPackNotFound)
	}

	// Existing versions: replay, collision, or ordering.
	vrows, err := tx.Query(ctx, `SELECT version, manifest_digest FROM jurisdiction_pack_versions WHERE pack_id=$1`, packID)
	if err != nil {
		return nil, false, s.registryFail("CreatePackVersion versions", err, nil)
	}
	newest := ""
	var sameVersionDigest string
	for vrows.Next() {
		var ver, dig string
		if err := vrows.Scan(&ver, &dig); err != nil {
			vrows.Close()
			return nil, false, s.registryFail("CreatePackVersion scan", err, nil)
		}
		if ver == p.Manifest.PackVersion {
			sameVersionDigest = dig
		}
		if newest == "" || domain.CompareVersions(ver, newest) > 0 {
			newest = ver
		}
	}
	vrows.Close()
	if err := vrows.Err(); err != nil {
		return nil, false, s.registryFail("CreatePackVersion rows", err, nil)
	}
	if sameVersionDigest != "" {
		if sameVersionDigest != p.ManifestDigest {
			return nil, false, domain.ErrConflict // same version, different content
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, s.registryFail("CreatePackVersion commit", err, nil)
		}
		v, err := s.GetPackVersion(ctx, p.PackRef, p.Manifest.PackVersion)
		return v, false, err
	}
	if newest != "" && domain.CompareVersions(p.Manifest.PackVersion, newest) <= 0 {
		return nil, false, domain.ErrVersionNotNewer
	}

	jurIDs, err := s.resolveCodes(ctx, tx, `SELECT jurisdiction_id::text FROM jurisdictions
		WHERE jurisdiction_code=$1 AND active_flag AND effective_from <= NOW()
		  AND (effective_to IS NULL OR effective_to > NOW())`, p.Manifest.JurisdictionIDs, "jurisdiction")
	if err != nil {
		return nil, false, err
	}
	regIDs, err := s.resolveCodes(ctx, tx, `SELECT regime_id::text FROM regulatory_regimes
		WHERE regime_code=$1 AND active_flag`, p.Manifest.Regimes, "regime")
	if err != nil {
		return nil, false, err
	}
	if len(p.Manifest.RuleModules) > 0 {
		var found int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM jurisdiction_rules WHERE jurisdiction_rule_id::text = ANY($1)`,
			p.Manifest.RuleModules).Scan(&found); err != nil {
			return nil, false, s.registryFail("CreatePackVersion rules", err, nil)
		}
		if found != len(uniq(p.Manifest.RuleModules)) {
			return nil, false, fmt.Errorf("%w: rule_modules names a rule that does not exist", domain.ErrManifestInvalid)
		}
	}
	if len(p.Manifest.CalendarModules) > 0 {
		var found int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM regulatory_calendar_versions WHERE calendar_version_id::text = ANY($1)`, p.Manifest.CalendarModules).Scan(&found); err != nil {
			return nil, false, s.registryFail("CreatePackVersion calendars", err, nil)
		}
		if found != len(uniq(p.Manifest.CalendarModules)) {
			return nil, false, fmt.Errorf("%w: calendar_modules names a calendar version that does not exist", domain.ErrManifestInvalid)
		}
	}
	if len(p.Manifest.ObligationModules) > 0 {
		var found int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM obligation_rules WHERE obligation_rule_id::text = ANY($1)`, p.Manifest.ObligationModules).Scan(&found); err != nil {
			return nil, false, s.registryFail("CreatePackVersion obligations", err, nil)
		}
		if found != len(uniq(p.Manifest.ObligationModules)) {
			return nil, false, fmt.Errorf("%w: obligation_modules names an obligation rule that does not exist", domain.ErrManifestInvalid)
		}
	}

	var versionID string
	if err := tx.QueryRow(ctx, `INSERT INTO jurisdiction_pack_versions
		(pack_id, version, effective_from, effective_to, manifest, manifest_digest, source_register_version, created_by_principal_id)
		VALUES ($1,$2,$3::date,$4::date,$5::jsonb,$6,$7,$8) RETURNING pack_version_id::text`,
		packID, p.Manifest.PackVersion, p.Manifest.EffectiveFrom, p.Manifest.EffectiveTo, string(p.ManifestJSON),
		p.ManifestDigest, p.Manifest.SourceRegisterVersion, p.CreatedBy).Scan(&versionID); err != nil {
		return nil, false, s.registryFail("CreatePackVersion insert", err, nil)
	}
	for _, id := range jurIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO pack_version_jurisdictions VALUES ($1,$2)`, versionID, id); err != nil {
			return nil, false, s.registryFail("CreatePackVersion jurisdictions", err, nil)
		}
	}
	for _, id := range regIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO pack_version_regimes VALUES ($1,$2)`, versionID, id); err != nil {
			return nil, false, s.registryFail("CreatePackVersion regimes", err, nil)
		}
	}
	for _, id := range uniq(p.Manifest.CalendarModules) {
		if _, err := tx.Exec(ctx, `INSERT INTO pack_version_calendars VALUES ($1,$2::uuid)`, versionID, id); err != nil {
			return nil, false, s.registryFail("CreatePackVersion calendar link", err, nil)
		}
	}
	for _, id := range uniq(p.Manifest.ObligationModules) {
		if _, err := tx.Exec(ctx, `INSERT INTO pack_version_obligations VALUES ($1,$2::uuid)`, versionID, id); err != nil {
			return nil, false, s.registryFail("CreatePackVersion obligation link", err, nil)
		}
	}
	for _, d := range append(append([]domain.PackDependency{}, p.Manifest.Dependencies...), p.Manifest.SchemaDependencies...) {
		if _, err := tx.Exec(ctx, `INSERT INTO pack_dependencies VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, versionID, d.Ref, d.Version); err != nil {
			return nil, false, s.registryFail("CreatePackVersion dependencies", err, nil)
		}
	}
	v, err := scanPackVersion(tx.QueryRow(ctx, `SELECT `+packVersionCols+` FROM jurisdiction_pack_versions v
		JOIN jurisdiction_packs p USING (pack_id) WHERE v.pack_version_id::text=$1`, versionID))
	if err != nil {
		return nil, false, s.registryFail("CreatePackVersion read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("CreatePackVersion commit", err, nil)
	}
	return v, true, nil
}

// resolveCodes maps each code to exactly one id, or fails naming the code.
func (s *PgStore) resolveCodes(ctx context.Context, q querier, query string, codes []string, kind string) ([]string, error) {
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		rows, err := q.Query(ctx, query, code)
		if err != nil {
			return nil, s.registryFail("resolve "+kind, err, nil)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, s.registryFail("resolve "+kind+" scan", err, nil)
			}
			ids = append(ids, id)
		}
		rows.Close()
		switch len(ids) {
		case 1:
			out = append(out, ids[0])
		case 0:
			return nil, fmt.Errorf("%w: %s %q is not an active registry entry", domain.ErrManifestInvalid, kind, code)
		default:
			return nil, fmt.Errorf("%w: %s %q is ambiguous (%d matches)", domain.ErrManifestInvalid, kind, code, len(ids))
		}
	}
	return out, nil
}

func (s *PgStore) GetPackVersion(ctx context.Context, ref, version string) (*domain.PackVersion, error) {
	v, err := scanPackVersion(s.pool.QueryRow(ctx, `SELECT `+packVersionCols+` FROM jurisdiction_pack_versions v
		JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 AND v.version=$2`, ref, version))
	if err != nil {
		return nil, s.registryFail("GetPackVersion", err, domain.ErrPackVersionNotFound)
	}
	return v, nil
}

func (s *PgStore) ListPackVersions(ctx context.Context, ref string) ([]*domain.PackVersion, error) {
	if _, err := s.GetPack(ctx, ref); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+packVersionCols+` FROM jurisdiction_pack_versions v
		JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 ORDER BY v.created_at DESC, v.version DESC`, ref)
	if err != nil {
		return nil, s.registryFail("ListPackVersions", err, nil)
	}
	defer rows.Close()
	out := []*domain.PackVersion{}
	for rows.Next() {
		v, err := scanPackVersion(rows)
		if err != nil {
			return nil, s.registryFail("ListPackVersions scan", err, nil)
		}
		out = append(out, v)
	}
	return out, s.registryFail("ListPackVersions rows", rows.Err(), nil)
}

// SubmitPackVersionForReview moves DRAFT to REVIEW. The later states need the
// compiler, test harness and certification (Waves 1 and 3); the database
// trigger already forbids skipping them.
func (s *PgStore) SubmitPackVersionForReview(ctx context.Context, ref, version, actor string) (*domain.PackVersion, bool, error) {
	cur, err := s.GetPackVersion(ctx, ref, version)
	if err != nil {
		return nil, false, err
	}
	if cur.Status == "REVIEW" {
		return cur, false, nil
	}
	if cur.Status != "DRAFT" {
		return nil, false, domain.ErrNotDraft
	}
	tag, err := s.pool.Exec(ctx, `UPDATE jurisdiction_pack_versions SET status='REVIEW', updated_at=NOW(), updated_by_principal_id=$2
		WHERE pack_version_id::text=$1 AND status='DRAFT'`, cur.PackVersionID, actor)
	if err != nil {
		return nil, false, s.registryFail("SubmitPackVersionForReview", err, nil)
	}
	if tag.RowsAffected() == 0 {
		return nil, false, domain.ErrNotDraft
	}
	v, err := s.GetPackVersion(ctx, ref, version)
	return v, true, err
}

// ── rule provenance ─────────────────────────────────────────────────────────

// SetRuleProvenance replaces the provenance of a DRAFT rule version. Released
// (non-DRAFT) rules are immutable. An interpretation, when given, must be
// APPROVED and belong to the rule's jurisdiction or one of its ancestors.
func (s *PgStore) SetRuleProvenance(ctx context.Context, p domain.SetRuleProvenanceParams) (*domain.RuleProvenance, error) {
	tx, err := s.begin(ctx, "SetRuleProvenance")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var jurID, ruleDomain, ruleCode, status string
	if err := tx.QueryRow(ctx, `SELECT jurisdiction_id::text, rule_domain, rule_code, rule_status
		FROM jurisdiction_rules WHERE jurisdiction_rule_id::text=$1 FOR UPDATE`, p.JurisdictionRuleID).
		Scan(&jurID, &ruleDomain, &ruleCode, &status); err != nil {
		return nil, s.registryFail("SetRuleProvenance lock", err, domain.ErrRuleNotFound)
	}
	if status != statusDraft {
		return nil, domain.ErrNotDraft
	}

	if p.RegimeID != nil {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM regulatory_regimes WHERE regime_id::text=$1`, *p.RegimeID).Scan(&n); err != nil || n == 0 {
			return nil, domain.ErrInvalidReference
		}
	}
	if p.InterpretationID != nil {
		var istatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM interpretation_records WHERE interpretation_id::text=$1`, *p.InterpretationID).Scan(&istatus); err != nil {
			return nil, domain.ErrInvalidReference
		}
		if istatus != "APPROVED" {
			return nil, domain.ErrUnapprovedInterpretation
		}
		var inChain bool
		if err := tx.QueryRow(ctx, `WITH RECURSIVE chain AS (
				SELECT jurisdiction_id, parent_jurisdiction_id FROM jurisdictions WHERE jurisdiction_id::text=$1
				UNION ALL
				SELECT j.jurisdiction_id, j.parent_jurisdiction_id FROM jurisdictions j
				JOIN chain c ON j.jurisdiction_id = c.parent_jurisdiction_id)
			SELECT EXISTS (SELECT 1 FROM chain c JOIN interpretation_records i ON i.jurisdiction_id = c.jurisdiction_id
				WHERE i.interpretation_id::text=$2)`, jurID, *p.InterpretationID).Scan(&inChain); err != nil || !inChain {
			return nil, domain.ErrInvalidReference
		}
	}
	if p.SupersedesRuleID != nil {
		if *p.SupersedesRuleID == p.JurisdictionRuleID {
			return nil, domain.ErrInvalidReference
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM jurisdiction_rules
			WHERE jurisdiction_rule_id::text=$1 AND jurisdiction_id::text=$2 AND rule_domain=$3 AND rule_code=$4`,
			*p.SupersedesRuleID, jurID, ruleDomain, ruleCode).Scan(&n); err != nil || n == 0 {
			return nil, domain.ErrInvalidReference // must be an earlier version of the same rule
		}
	}
	if len(p.SourceIDs) > 0 {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(DISTINCT source_id) FROM regulatory_sources
			WHERE source_id::text = ANY($1) AND superseded_by_source_id IS NULL`, p.SourceIDs).Scan(&n); err != nil ||
			n != len(uniq(p.SourceIDs)) {
			return nil, domain.ErrInvalidReference
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE jurisdiction_rules SET regime_id=$2, interpretation_id=$3, supersedes_rule_id=$4,
			precedence=$5, published_on=$6::date, updated_at=NOW(), updated_by_principal_id=$7
		WHERE jurisdiction_rule_id::text=$1`,
		p.JurisdictionRuleID, p.RegimeID, p.InterpretationID, p.SupersedesRuleID, p.Precedence, p.PublishedOn, p.ActorID); err != nil {
		return nil, s.registryFail("SetRuleProvenance update", err, nil)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM rule_sources WHERE jurisdiction_rule_id::text=$1`, p.JurisdictionRuleID); err != nil {
		return nil, s.registryFail("SetRuleProvenance unlink", err, nil)
	}
	for _, sid := range uniq(p.SourceIDs) {
		if _, err := tx.Exec(ctx, `INSERT INTO rule_sources (jurisdiction_rule_id, source_id) VALUES ($1,$2)`, p.JurisdictionRuleID, sid); err != nil {
			return nil, s.registryFail("SetRuleProvenance link", err, nil)
		}
	}
	out, err := s.ruleProvenance(ctx, tx, p.JurisdictionRuleID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("SetRuleProvenance commit", err, nil)
	}
	return out, nil
}

func (s *PgStore) GetRuleProvenance(ctx context.Context, ruleID string) (*domain.RuleProvenance, error) {
	return s.ruleProvenance(ctx, s.pool, ruleID)
}

func (s *PgStore) ruleProvenance(ctx context.Context, q querier, ruleID string) (*domain.RuleProvenance, error) {
	var x domain.RuleProvenance
	err := q.QueryRow(ctx, `SELECT jurisdiction_rule_id::text, regime_id::text, interpretation_id::text,
			supersedes_rule_id::text, precedence, to_char(published_on,'YYYY-MM-DD')
		FROM jurisdiction_rules WHERE jurisdiction_rule_id::text=$1`, ruleID).
		Scan(&x.JurisdictionRuleID, &x.RegimeID, &x.InterpretationID, &x.SupersedesRuleID, &x.Precedence, &x.PublishedOn)
	if err != nil {
		return nil, s.registryFail("ruleProvenance", err, domain.ErrRuleNotFound)
	}
	rows, err := q.Query(ctx, `SELECT source_id::text FROM rule_sources WHERE jurisdiction_rule_id::text=$1 ORDER BY source_id`, ruleID)
	if err != nil {
		return nil, s.registryFail("ruleProvenance sources", err, nil)
	}
	defer rows.Close()
	x.SourceIDs = []string{}
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, s.registryFail("ruleProvenance scan", err, nil)
		}
		x.SourceIDs = append(x.SourceIDs, sid)
	}
	return &x, s.registryFail("ruleProvenance rows", rows.Err(), nil)
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ── rule parameters (ZS-JUR-001 Wave 4, tax half) ───────────────────────────

// SetRuleParameters replaces the typed calculation parameters of a DRAFT rule.
// The parameters are validated (decimal strings only, closed families) and,
// like the rest of a rule's provenance, are immutable once the rule leaves
// DRAFT (trigger-enforced). Passing nil clears them.
func (s *PgStore) SetRuleParameters(ctx context.Context, ruleID string, params json.RawMessage, actor string) (json.RawMessage, error) {
	if len(params) > 0 {
		if _, err := domain.ParseRuleParameters(params); err != nil {
			return nil, err
		}
		if why := domain.FloatInJSON(params); why != "" {
			return nil, fmt.Errorf("%w: parameters contain %s; write rates and amounts as decimal strings", domain.ErrParametersInvalid, why)
		}
	}
	tx, err := s.begin(ctx, "SetRuleParameters")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	if err := tx.QueryRow(ctx, `SELECT rule_status FROM jurisdiction_rules WHERE jurisdiction_rule_id::text=$1 FOR UPDATE`, ruleID).Scan(&status); err != nil {
		return nil, s.registryFail("SetRuleParameters lock", err, domain.ErrRuleNotFound)
	}
	if status != statusDraft {
		return nil, domain.ErrNotDraft
	}
	var arg any
	if len(params) > 0 {
		canon, cerr := domain.CanonicalJSON(params)
		if cerr != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrParametersInvalid, cerr)
		}
		arg = string(canon)
	}
	if _, err := tx.Exec(ctx, `UPDATE jurisdiction_rules SET rule_parameters=$2::jsonb, updated_at=NOW(), updated_by_principal_id=$3 WHERE jurisdiction_rule_id::text=$1`,
		ruleID, arg, actor); err != nil {
		return nil, s.registryFail("SetRuleParameters update", err, nil)
	}
	out, err := s.getRuleParametersTx(ctx, tx, ruleID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("SetRuleParameters commit", err, nil)
	}
	return out, nil
}

func (s *PgStore) getRuleParametersTx(ctx context.Context, q querier, ruleID string) (json.RawMessage, error) {
	var p *string
	if err := q.QueryRow(ctx, `SELECT rule_parameters::text FROM jurisdiction_rules WHERE jurisdiction_rule_id::text=$1`, ruleID).Scan(&p); err != nil {
		return nil, s.registryFail("getRuleParameters", err, domain.ErrRuleNotFound)
	}
	if p == nil {
		return nil, nil
	}
	return json.RawMessage(*p), nil
}

// GetRuleParameters returns a rule's parameters (nil when it has none).
func (s *PgStore) GetRuleParameters(ctx context.Context, ruleID string) (json.RawMessage, error) {
	return s.getRuleParametersTx(ctx, s.pool, ruleID)
}
