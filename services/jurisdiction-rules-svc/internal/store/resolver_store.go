package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 2 persistence: the registry reads a runtime resolver needs
// (it implements resolver.Source) and the RuleDecisionEvidence ledger.

const eligibleCols = `v.pack_version_id::text, p.pack_ref, v.version, v.status,
	(v.effective_from::timestamp AT TIME ZONE 'UTC'),
	CASE WHEN v.effective_to IS NULL THEN NULL ELSE (v.effective_to::timestamp AT TIME ZONE 'UTC') END,
	ARRAY(SELECT j.jurisdiction_code FROM pack_version_jurisdictions pj
		JOIN jurisdictions j USING (jurisdiction_id) WHERE pj.pack_version_id = v.pack_version_id ORDER BY 1)`

func scanEligible(r pgx.Row) (domain.EligiblePack, error) {
	var p domain.EligiblePack
	err := r.Scan(&p.PackVersionID, &p.PackRef, &p.Version, &p.Status, &p.EffectiveFrom, &p.EffectiveTo, &p.ScopeCodes)
	return p, err
}

// ListEligiblePacks returns the pack versions whose status is in statuses.
func (s *PgStore) ListEligiblePacks(ctx context.Context, statuses []string, ring, region string) ([]domain.EligiblePack, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+eligibleCols+` FROM jurisdiction_pack_versions v
		JOIN jurisdiction_packs p USING (pack_id) WHERE v.status = ANY($1)
		  AND ($2 = '' OR $3 = '' OR EXISTS (SELECT 1 FROM pack_deployments d WHERE d.pack_version_id = v.pack_version_id
		        AND d.ring_code = $2 AND d.region_code = $3 AND d.status = 'ACTIVE'))
		ORDER BY p.pack_ref, v.version`, statuses, ring, region)
	if err != nil {
		return nil, s.registryFail("ListEligiblePacks", err, nil)
	}
	defer rows.Close()
	out := []domain.EligiblePack{}
	for rows.Next() {
		p, err := scanEligible(rows)
		if err != nil {
			return nil, s.registryFail("ListEligiblePacks scan", err, nil)
		}
		out = append(out, p)
	}
	return out, s.registryFail("ListEligiblePacks rows", rows.Err(), nil)
}

// FindPackVersion returns one pack version at any status (pinned replay).
func (s *PgStore) FindPackVersion(ctx context.Context, ref, version string) (*domain.EligiblePack, error) {
	p, err := scanEligible(s.pool.QueryRow(ctx, `SELECT `+eligibleCols+` FROM jurisdiction_pack_versions v
		JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 AND v.version=$2`, ref, version))
	if err != nil {
		return nil, s.registryFail("FindPackVersion", err, domain.ErrPackVersionNotFound)
	}
	return &p, nil
}

// LoadPack reads the artifact, its signature and key, and the certification
// with its key, in one consistent read. Nothing is verified here: the caller
// (the resolver) decides, and a missing piece is simply left nil so that it
// surfaces as a verification reason.
func (s *PgStore) LoadPack(ctx context.Context, packVersionID string) (*domain.LoadedPack, error) {
	lp := &domain.LoadedPack{PackVersionID: packVersionID}
	var vdigest *string
	err := s.pool.QueryRow(ctx, `SELECT a.artifact, a.artifact_digest, v.artifact_digest, a.signature, a.signature_key_ref
		FROM pack_artifacts a JOIN jurisdiction_pack_versions v USING (pack_version_id)
		WHERE a.pack_version_id::text=$1`, packVersionID).
		Scan(&lp.ArtifactJSON, &lp.ArtifactDigest, &vdigest, &lp.Signature, &lp.KeyRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// An eligible version with no artifact is itself a failure; report it as one.
			lp.ArtifactJSON, lp.ArtifactDigest = "", ""
			return lp, nil
		}
		return nil, s.registryFail("LoadPack artifact", err, nil)
	}
	if vdigest != nil {
		lp.VersionDigest = *vdigest
	}
	if lp.KeyRef != nil {
		if k, kerr := s.GetSigningKey(ctx, *lp.KeyRef); kerr == nil {
			lp.Key = k
		} else if !errors.Is(kerr, domain.ErrKeyNotFound) {
			return nil, kerr
		}
	}
	var c domain.LoadedCertification
	err = s.pool.QueryRow(ctx, `SELECT certification_id::text, artifact_digest, report::text, report_digest, signature, signature_key_ref
		FROM pack_certifications WHERE pack_version_id::text=$1`, packVersionID).
		Scan(&c.CertificationID, &c.ArtifactDigest, &c.ReportJSON, &c.ReportDigest, &c.Signature, &c.KeyRef)
	switch {
	case err == nil:
		if k, kerr := s.GetSigningKey(ctx, c.KeyRef); kerr == nil {
			c.Key = k
		} else if !errors.Is(kerr, domain.ErrKeyNotFound) {
			return nil, kerr
		}
		lp.Cert = &c
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return nil, s.registryFail("LoadPack certification", err, nil)
	}
	return lp, nil
}

// ── decision evidence ───────────────────────────────────────────────────────

// DecisionRecord is one resolution to be written to the evidence ledger.
type DecisionRecord struct {
	// Kind is RULE (default) or OBLIGATION.
	Kind              string
	RequestedBy       string
	CorrelationID     string
	IdempotencyKey    *string
	RequestDigest     string
	RequestJSON       []byte
	EffectiveAt       time.Time
	Outcome           string
	PackRef           *string
	PackVersion       *string
	PackVersionID     *string
	ArtifactDigest    *string
	CertificationID   *string
	RuleID            *string
	RuleContentDigest *string
	Basis             *string
	ResponseJSON      []byte
	ResolverRing      string
	ResolverRegion    string
}

// StoredDecision is a decision as read back.
type StoredDecision struct {
	DecisionID     string          `json:"decision_id"`
	RequestedBy    string          `json:"requested_by"`
	RequestDigest  string          `json:"request_digest"`
	Outcome        string          `json:"outcome"`
	PackRef        *string         `json:"pack_ref"`
	PackVersion    *string         `json:"pack_version"`
	ArtifactDigest *string         `json:"artifact_digest"`
	RuleID         *string         `json:"rule_id"`
	CreatedAt      time.Time       `json:"created_at"`
	Response       json.RawMessage `json:"response"`
}

const decisionCols = `decision_id::text, requested_by, request_digest, outcome, pack_ref, pack_version, artifact_digest,
	rule_id::text, created_at, response::text`

func scanDecision(r pgx.Row) (*StoredDecision, error) {
	var d StoredDecision
	var resp string
	if err := r.Scan(&d.DecisionID, &d.RequestedBy, &d.RequestDigest, &d.Outcome, &d.PackRef, &d.PackVersion, &d.ArtifactDigest,
		&d.RuleID, &d.CreatedAt, &resp); err != nil {
		return nil, err
	}
	d.Response = json.RawMessage(resp)
	return &d, nil
}

// RecordDecision appends a decision. With an idempotency key, a repeat from
// the same caller with the same request returns the stored decision
// (replayed=true); the same key with a different request is
// ErrIdempotencyConflict.
func (s *PgStore) RecordDecision(ctx context.Context, rec DecisionRecord) (*StoredDecision, bool, error) {
	d, err := scanDecision(s.pool.QueryRow(ctx, `INSERT INTO rule_decision_evidence
		(requested_by, correlation_id, idempotency_key, request_digest, request, effective_at, outcome, pack_ref, pack_version,
		 pack_version_id, artifact_digest, certification_id, rule_id, rule_content_digest, basis, response, resolver_ring, resolver_region, decision_kind)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,$10::uuid,$11,$12::uuid,$13::uuid,$14,$15,$16::jsonb,$17,$18,$19)
		ON CONFLICT (requested_by, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING `+decisionCols,
		rec.RequestedBy, nullIfEmpty(rec.CorrelationID), rec.IdempotencyKey, rec.RequestDigest, string(rec.RequestJSON), rec.EffectiveAt,
		rec.Outcome, rec.PackRef, rec.PackVersion, rec.PackVersionID, rec.ArtifactDigest, rec.CertificationID, rec.RuleID,
		rec.RuleContentDigest, rec.Basis, string(rec.ResponseJSON), nullIfEmpty(rec.ResolverRing), nullIfEmpty(rec.ResolverRegion), kindOrRule(rec.Kind)))
	if err == nil {
		return d, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || rec.IdempotencyKey == nil {
		return nil, false, s.registryFail("RecordDecision", err, nil)
	}
	existing, err := scanDecision(s.pool.QueryRow(ctx, `SELECT `+decisionCols+` FROM rule_decision_evidence
		WHERE requested_by=$1 AND idempotency_key=$2`, rec.RequestedBy, *rec.IdempotencyKey))
	if err != nil {
		return nil, false, s.registryFail("RecordDecision lookup", err, nil)
	}
	if existing.RequestDigest != rec.RequestDigest {
		return nil, false, domain.ErrIdempotencyConflict
	}
	return existing, true, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetDecision reads one stored decision.
func (s *PgStore) GetDecision(ctx context.Context, id string) (*StoredDecision, error) {
	d, err := scanDecision(s.pool.QueryRow(ctx, `SELECT `+decisionCols+` FROM rule_decision_evidence WHERE decision_id::text=$1`, id))
	if err != nil {
		return nil, s.registryFail("GetDecision", err, domain.ErrDecisionNotFound)
	}
	return d, nil
}

func kindOrRule(k string) string {
	if k == "" {
		return "RULE"
	}
	return k
}
