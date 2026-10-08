package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 1 persistence: trusted signing keys, compile, sign, verify.

// PackArtifact is a compiled artifact with its registry metadata.
type PackArtifact struct {
	PackVersionID   string               `json:"pack_version_id"`
	PackRef         string               `json:"pack_ref"`
	Version         string               `json:"version"`
	ArtifactDigest  string               `json:"artifact_digest"`
	Artifact        json.RawMessage      `json:"artifact"`
	CompilerName    string               `json:"compiler_name"`
	CompilerVersion string               `json:"compiler_version"`
	Report          domain.CompileReport `json:"report"`
	CompiledAt      time.Time            `json:"compiled_at"`
	CompiledBy      string               `json:"compiled_by_principal_id"`
	Signature       *string              `json:"signature"`
	SignatureKeyRef *string              `json:"signature_key_ref"`
	SignedAt        *time.Time           `json:"signed_at"`
	SignedBy        *string              `json:"signed_by_principal_id"`
}

// CompileFailure carries the report of a rejected compile.
type CompileFailure struct{ Report domain.CompileReport }

func (e *CompileFailure) Error() string { return domain.ErrCompileFailed.Error() }
func (e *CompileFailure) Unwrap() error { return domain.ErrCompileFailed }

const artifactCols = `a.pack_version_id::text, p.pack_ref, v.version, a.artifact_digest, a.artifact, a.compiler_name,
	a.compiler_version, a.report::text, a.compiled_at, a.compiled_by_principal_id, a.signature, a.signature_key_ref,
	a.signed_at, a.signed_by_principal_id`

func scanArtifact(r pgx.Row) (*PackArtifact, error) {
	var x PackArtifact
	var art, report string
	err := r.Scan(&x.PackVersionID, &x.PackRef, &x.Version, &x.ArtifactDigest, &art, &x.CompilerName, &x.CompilerVersion,
		&report, &x.CompiledAt, &x.CompiledBy, &x.Signature, &x.SignatureKeyRef, &x.SignedAt, &x.SignedBy)
	if err != nil {
		return nil, err
	}
	x.Artifact = json.RawMessage(art)
	if err := json.Unmarshal([]byte(report), &x.Report); err != nil {
		return nil, fmt.Errorf("stored compile report is unreadable: %w", err)
	}
	return &x, nil
}

// ── trusted signing keys ────────────────────────────────────────────────────

const keyCols = `key_ref, algorithm, public_key, status, status_reason, created_by_principal_id`

func scanKey(r pgx.Row) (*domain.TrustedKey, error) {
	var k domain.TrustedKey
	if err := r.Scan(&k.KeyRef, &k.Algorithm, &k.PublicKey, &k.Status, &k.StatusReason, &k.CreatedBy); err != nil {
		return nil, err
	}
	k.PublicKeyB64 = base64.StdEncoding.EncodeToString(k.PublicKey)
	return &k, nil
}

// RegisterSigningKey registers a public key. Re-registering the same key_ref
// with the same public key is a no-op; a different key under the same ref is
// refused (a key_ref names one key forever).
func (s *PgStore) RegisterSigningKey(ctx context.Context, keyRef, algorithm string, pub []byte, actor string) (*domain.TrustedKey, bool, error) {
	k, err := scanKey(s.pool.QueryRow(ctx, `INSERT INTO pack_signing_keys (key_ref, algorithm, public_key, created_by_principal_id)
		VALUES ($1,$2,$3,$4) ON CONFLICT (key_ref) DO NOTHING RETURNING `+keyCols, keyRef, algorithm, pub, actor))
	if err == nil {
		return k, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, s.registryFail("RegisterSigningKey", err, nil)
	}
	k, err = s.GetSigningKey(ctx, keyRef)
	if err != nil {
		return nil, false, err
	}
	if k.Algorithm != algorithm || string(k.PublicKey) != string(pub) {
		return nil, false, domain.ErrKeyMismatch
	}
	return k, false, nil
}

func (s *PgStore) GetSigningKey(ctx context.Context, keyRef string) (*domain.TrustedKey, error) {
	k, err := scanKey(s.pool.QueryRow(ctx, `SELECT `+keyCols+` FROM pack_signing_keys WHERE key_ref=$1`, keyRef))
	if err != nil {
		return nil, s.registryFail("GetSigningKey", err, domain.ErrKeyNotFound)
	}
	return k, nil
}

func (s *PgStore) ListSigningKeys(ctx context.Context) ([]*domain.TrustedKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+keyCols+` FROM pack_signing_keys ORDER BY created_at, key_ref`)
	if err != nil {
		return nil, s.registryFail("ListSigningKeys", err, nil)
	}
	defer rows.Close()
	out := []*domain.TrustedKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, s.registryFail("ListSigningKeys scan", err, nil)
		}
		out = append(out, k)
	}
	return out, s.registryFail("ListSigningKeys rows", rows.Err(), nil)
}

// SetSigningKeyStatus retires or revokes a key. The database permits only the
// forward moves ACTIVE->RETIRED, ACTIVE->REVOKED, RETIRED->REVOKED.
func (s *PgStore) SetSigningKeyStatus(ctx context.Context, keyRef, status, reason, actor string) (*domain.TrustedKey, bool, error) {
	cur, err := s.GetSigningKey(ctx, keyRef)
	if err != nil {
		return nil, false, err
	}
	if cur.Status == status {
		return cur, false, nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE pack_signing_keys SET status=$2, status_reason=$3, status_changed_at=NOW(), status_changed_by=$4
		WHERE key_ref=$1`, keyRef, status, reason, actor); err != nil {
		return nil, false, s.registryFail("SetSigningKeyStatus", err, nil)
	}
	k, err := s.GetSigningKey(ctx, keyRef)
	return k, true, err
}

// ── compile ─────────────────────────────────────────────────────────────────

// CompilePackVersion compiles a pack version that is under REVIEW.
//
// The inputs are read inside one transaction with the version row locked, so
// the artifact reflects one consistent snapshot of the registries. A failing
// compile stores nothing and returns *CompileFailure. Compiling again with
// unchanged inputs returns the stored artifact (created=false); changed
// inputs under the same version are refused (JUR-NEG-20).
func (s *PgStore) CompilePackVersion(ctx context.Context, ref, version, actor string) (*PackArtifact, bool, error) {
	tx, err := s.begin(ctx, "CompilePackVersion")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var versionID, status, manifestText, manifestDigest string
	if err := tx.QueryRow(ctx, `SELECT v.pack_version_id::text, v.status, v.manifest::text, v.manifest_digest
		FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
		WHERE p.pack_ref=$1 AND v.version=$2 FOR UPDATE OF v`, ref, version).
		Scan(&versionID, &status, &manifestText, &manifestDigest); err != nil {
		return nil, false, s.registryFail("CompilePackVersion lock", err, domain.ErrPackVersionNotFound)
	}

	existing, err := scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactCols+` FROM pack_artifacts a
		JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)
		WHERE a.pack_version_id::text=$1`, versionID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, s.registryFail("CompilePackVersion existing", err, nil)
	}
	if existing == nil && status != "REVIEW" {
		return nil, false, domain.ErrNotUnderReview
	}

	manifest, err := domain.ParseManifest([]byte(manifestText))
	if err != nil {
		return nil, false, err
	}
	in, err := s.gatherCompileInput(ctx, tx, ref, version, versionID, manifest, manifestDigest)
	if err != nil {
		return nil, false, err
	}
	out := domain.Compile(in)

	if existing != nil {
		if !out.Report.HasErrors() && out.ArtifactDigest == existing.ArtifactDigest {
			return existing, false, nil
		}
		return nil, false, domain.ErrAlreadyCompiled
	}
	if out.Report.HasErrors() {
		return nil, false, &CompileFailure{Report: out.Report}
	}

	report, _ := json.Marshal(out.Report)
	if _, err := tx.Exec(ctx, `INSERT INTO pack_artifacts (pack_version_id, artifact, artifact_digest, compiler_name, compiler_version,
			report, compiled_by_principal_id) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7)`,
		versionID, string(out.ArtifactJSON), out.ArtifactDigest, out.Report.CompilerName, out.Report.CompilerVersion,
		string(report), actor); err != nil {
		return nil, false, s.registryFail("CompilePackVersion insert", err, nil)
	}
	if _, err := tx.Exec(ctx, `UPDATE jurisdiction_pack_versions SET artifact_digest=$2, updated_at=NOW(), updated_by_principal_id=$3
		WHERE pack_version_id::text=$1`, versionID, out.ArtifactDigest, actor); err != nil {
		return nil, false, s.registryFail("CompilePackVersion version digest", err, nil)
	}
	art, err := scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactCols+` FROM pack_artifacts a
		JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)
		WHERE a.pack_version_id::text=$1`, versionID))
	if err != nil {
		return nil, false, s.registryFail("CompilePackVersion read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("CompilePackVersion commit", err, nil)
	}
	return art, true, nil
}

// gatherCompileInput reads every registry row the compiler needs.
func (s *PgStore) gatherCompileInput(ctx context.Context, q querier, ref, version, versionID string,
	m domain.PackManifest, manifestDigest string) (domain.CompileInput, error) {

	in := domain.CompileInput{PackRef: ref, Version: version, Manifest: m, ManifestDigest: manifestDigest}

	named := func(query string) ([]domain.Named, error) {
		rows, err := q.Query(ctx, query, versionID)
		if err != nil {
			return nil, s.registryFail("compile scope", err, nil)
		}
		defer rows.Close()
		var out []domain.Named
		for rows.Next() {
			var n domain.Named
			if err := rows.Scan(&n.ID, &n.Code, &n.ParentID); err != nil {
				return nil, s.registryFail("compile scope scan", err, nil)
			}
			out = append(out, n)
		}
		return out, s.registryFail("compile scope rows", rows.Err(), nil)
	}
	var err error
	if in.Jurisdictions, err = named(`SELECT j.jurisdiction_id::text, j.jurisdiction_code, j.parent_jurisdiction_id::text FROM pack_version_jurisdictions pj
		JOIN jurisdictions j USING (jurisdiction_id) WHERE pj.pack_version_id::text=$1`); err != nil {
		return in, err
	}
	if in.Regimes, err = named(`SELECT r.regime_id::text, r.regime_code, NULL::text FROM pack_version_regimes pr
		JOIN regulatory_regimes r USING (regime_id) WHERE pr.pack_version_id::text=$1`); err != nil {
		return in, err
	}

	// Rule modules and their provenance.
	ruleRows, err := q.Query(ctx, `SELECT jurisdiction_rule_id::text, jurisdiction_id::text, rule_domain, rule_code, rule_name,
			effective_from, effective_to, rule_payload::text, rule_status, regime_id::text, interpretation_id::text,
			supersedes_rule_id::text, precedence, to_char(published_on,'YYYY-MM-DD'), rule_parameters::text
		FROM jurisdiction_rules WHERE jurisdiction_rule_id::text = ANY($1)`, m.RuleModules)
	if err != nil {
		return in, s.registryFail("compile rules", err, nil)
	}
	found := map[string]bool{}
	for ruleRows.Next() {
		var r domain.CompileRule
		var payload string
		var params *string
		if err := ruleRows.Scan(&r.RuleID, &r.JurisdictionID, &r.RuleDomain, &r.RuleCode, &r.RuleName, &r.EffectiveFrom,
			&r.EffectiveTo, &payload, &r.RuleStatus, &r.RegimeID, &r.InterpretationID, &r.SupersedesRuleID,
			&r.Precedence, &r.PublishedOn, &params); err != nil {
			ruleRows.Close()
			return in, s.registryFail("compile rules scan", err, nil)
		}
		r.Payload = json.RawMessage(payload)
		if params != nil {
			r.Parameters = json.RawMessage(*params)
		}
		found[r.RuleID] = true
		in.Rules = append(in.Rules, r)
	}
	ruleRows.Close()
	if err := ruleRows.Err(); err != nil {
		return in, s.registryFail("compile rules rows", err, nil)
	}
	for _, id := range m.RuleModules {
		if !found[id] {
			in.MissingRuleIDs = append(in.MissingRuleIDs, id)
		}
	}

	linkRows, err := q.Query(ctx, `SELECT jurisdiction_rule_id::text, source_id::text FROM rule_sources
		WHERE jurisdiction_rule_id::text = ANY($1)`, m.RuleModules)
	if err != nil {
		return in, s.registryFail("compile rule sources", err, nil)
	}
	links := map[string][]string{}
	sourceIDs := map[string]bool{}
	for linkRows.Next() {
		var rid, sid string
		if err := linkRows.Scan(&rid, &sid); err != nil {
			linkRows.Close()
			return in, s.registryFail("compile rule sources scan", err, nil)
		}
		links[rid] = append(links[rid], sid)
		sourceIDs[sid] = true
	}
	linkRows.Close()
	interpIDs := map[string]bool{}
	for i := range in.Rules {
		in.Rules[i].SourceIDs = links[in.Rules[i].RuleID]
		if in.Rules[i].InterpretationID != nil {
			interpIDs[*in.Rules[i].InterpretationID] = true
		}
	}
	if err := s.gatherCalendarObligationModules(ctx, q, m, &in, sourceIDs, interpIDs); err != nil {
		return in, err
	}

	// Interpretations (and the sources they cite are not pulled in: only the
	// sources the rules themselves cite are part of the artifact).
	for id := range interpIDs {
		it, err := scanInterp(q.QueryRow(ctx, `SELECT `+interpCols+` FROM interpretation_records WHERE interpretation_id::text=$1`, id))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // reported as unapproved: the rule cites an interpretation that cannot be read
			}
			return in, s.registryFail("compile interpretation", err, nil)
		}
		srcs, err := s.interpretationSources(ctx, q, id)
		if err != nil {
			return in, err
		}
		in.Interpretations = append(in.Interpretations, domain.CompileInterpretation{
			InterpretationID: it.InterpretationID, JurisdictionID: it.JurisdictionID, RegimeID: it.RegimeID,
			Subject: it.Subject, Decision: it.Decision, Rationale: it.Rationale, Approved: it.Status == "APPROVED", SourceIDs: srcs})
	}

	for id := range sourceIDs {
		x, err := scanSource(q.QueryRow(ctx, `SELECT `+sourceCols+` FROM regulatory_sources WHERE source_id::text=$1`, id))
		if err != nil {
			return in, s.registryFail("compile source", err, nil)
		}
		in.Sources = append(in.Sources, domain.CompileSource{
			SourceID: x.SourceID, JurisdictionID: x.JurisdictionID, Authority: x.Authority, SourceType: x.SourceType,
			AuthorityLevel: x.AuthorityLevel, Title: x.Title, OfficialIdentifier: x.OfficialIdentifier, PublishedOn: x.PublishedOn,
			EffectiveOn: x.EffectiveOn, Location: x.Location, SnapshotHash: x.SnapshotHash, SnapshotRef: x.SnapshotRef,
			Language: x.Language, Reviewed: x.ReviewedByPrincipalID != nil, Superseded: x.SupersededBySourceID != nil})
	}

	// Dependencies: what the registry knows about each pinned pack.
	groups := []struct {
		name string
		deps []domain.PackDependency
	}{{"dependencies", m.Dependencies}, {"schema_dependencies", m.SchemaDependencies}}
	for _, g := range groups {
		for _, d := range g.deps {
			cd := domain.CompileDependency{Ref: d.Ref, Version: d.Version, Group: g.name}
			var registered bool
			if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jurisdiction_packs WHERE pack_ref=$1)`, d.Ref).Scan(&registered); err != nil {
				return in, s.registryFail("compile dependency", err, nil)
			}
			cd.IsPack = registered
			if registered {
				var st string
				var dig *string
				err := q.QueryRow(ctx, `SELECT v.status, v.artifact_digest FROM jurisdiction_pack_versions v
					JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 AND v.version=$2`, d.Ref, d.Version).Scan(&st, &dig)
				switch {
				case err == nil:
					cd.Found, cd.Status, cd.ArtifactDigest = true, st, dig
				case errors.Is(err, pgx.ErrNoRows):
				default:
					return in, s.registryFail("compile dependency version", err, nil)
				}
			}
			in.Dependencies = append(in.Dependencies, cd)
		}
	}
	return in, nil
}

// ── read, sign, verify ──────────────────────────────────────────────────────

func (s *PgStore) GetPackArtifact(ctx context.Context, ref, version string) (*PackArtifact, error) {
	if _, err := s.GetPackVersion(ctx, ref, version); err != nil {
		return nil, err
	}
	a, err := scanArtifact(s.pool.QueryRow(ctx, `SELECT `+artifactCols+` FROM pack_artifacts a
		JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)
		WHERE p.pack_ref=$1 AND v.version=$2`, ref, version))
	if err != nil {
		return nil, s.registryFail("GetPackArtifact", err, domain.ErrNotCompiled)
	}
	return a, nil
}

// SignPackVersion signs the compiled artifact's digest with the service's
// signer. The signing key must be registered, ACTIVE, and carry the same
// public key as the signer. A corrupt stored artifact is never signed. The
// signature is write-once; signing again is a no-op returning the original.
func (s *PgStore) SignPackVersion(ctx context.Context, ref, version string, signer domain.Signer, actor string) (*PackArtifact, bool, error) {
	if signer == nil {
		return nil, false, domain.ErrSigningNotConfigured
	}
	tx, err := s.begin(ctx, "SignPackVersion")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactCols+` FROM pack_artifacts a
		JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)
		WHERE p.pack_ref=$1 AND v.version=$2 FOR UPDATE OF a`, ref, version))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, verr := s.GetPackVersion(ctx, ref, version); verr != nil {
				return nil, false, verr
			}
			return nil, false, domain.ErrNotCompiled
		}
		return nil, false, s.registryFail("SignPackVersion lock", err, nil)
	}
	if cur.Signature != nil {
		return cur, false, nil
	}

	key, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM pack_signing_keys WHERE key_ref=$1`, signer.KeyRef()))
	if err != nil || key.Status != domain.KeyActive || key.Algorithm != domain.AlgorithmEd25519 ||
		string(key.PublicKey) != string(signer.PublicKey()) {
		return nil, false, domain.ErrSigningKeyNotActive
	}
	if d, derr := domain.DigestOf(cur.Artifact); derr != nil || d != cur.ArtifactDigest {
		return nil, false, domain.ErrConflict // stored bytes no longer match their digest: refuse to sign
	}

	sig, err := signer.Sign(domain.SigningMessage(cur.ArtifactDigest))
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	b64 := base64.StdEncoding.EncodeToString(sig)
	if _, err := tx.Exec(ctx, `UPDATE pack_artifacts SET signature=$2, signature_key_ref=$3, signed_at=NOW(), signed_by_principal_id=$4
		WHERE pack_version_id::text=$1`, cur.PackVersionID, b64, signer.KeyRef(), actor); err != nil {
		return nil, false, s.registryFail("SignPackVersion artifact", err, nil)
	}
	if _, err := tx.Exec(ctx, `UPDATE jurisdiction_pack_versions SET signature=$2, signature_key_ref=$3, updated_at=NOW(), updated_by_principal_id=$4
		WHERE pack_version_id::text=$1`, cur.PackVersionID, b64, signer.KeyRef(), actor); err != nil {
		return nil, false, s.registryFail("SignPackVersion version", err, nil)
	}
	out, err := scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactCols+` FROM pack_artifacts a
		JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)
		WHERE a.pack_version_id::text=$1`, cur.PackVersionID))
	if err != nil {
		return nil, false, s.registryFail("SignPackVersion read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("SignPackVersion commit", err, nil)
	}
	return out, true, nil
}

// VerifyPackArtifact re-verifies the stored artifact from first principles:
// recomputed digest, recorded digests, signature against the registered key.
// It fails closed.
func (s *PgStore) VerifyPackArtifact(ctx context.Context, ref, version string) (*PackArtifact, domain.VerificationResult, error) {
	a, err := s.GetPackArtifact(ctx, ref, version)
	if err != nil {
		return nil, domain.VerificationResult{}, err
	}
	var versionDigest *string
	if err := s.pool.QueryRow(ctx, `SELECT artifact_digest FROM jurisdiction_pack_versions WHERE pack_version_id::text=$1`, a.PackVersionID).
		Scan(&versionDigest); err != nil {
		return nil, domain.VerificationResult{}, s.registryFail("VerifyPackArtifact", err, nil)
	}
	in := domain.VerifyInput{ArtifactJSON: string(a.Artifact), StoredDigest: a.ArtifactDigest,
		Signature: a.Signature, SignatureKeyID: a.SignatureKeyRef}
	if versionDigest != nil {
		in.VersionDigest = *versionDigest
	} else {
		in.VersionDigest = "sha256:" + "missing"
	}
	if a.SignatureKeyRef != nil {
		if k, kerr := s.GetSigningKey(ctx, *a.SignatureKeyRef); kerr == nil {
			in.Key = k
		} else if !errors.Is(kerr, domain.ErrKeyNotFound) {
			return nil, domain.VerificationResult{}, kerr
		}
	}
	return a, domain.VerifyArtifact(in), nil
}
