package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 3 persistence: test bundles, test runs, independent
// reviews and certification. Everything is append-only evidence; the
// database triggers in migration 000007 re-check each gate independently.

type TestBundleRecord struct {
	BundleID      string          `json:"bundle_id"`
	PackVersionID string          `json:"pack_version_id"`
	Revision      int             `json:"revision"`
	Bundle        json.RawMessage `json:"bundle"`
	BundleDigest  string          `json:"bundle_digest"`
	SubmittedBy   string          `json:"submitted_by"`
	SubmittedAt   time.Time       `json:"submitted_at"`
}

type TestRunRecord struct {
	RunID          string               `json:"run_id"`
	BundleID       string               `json:"bundle_id"`
	BundleDigest   string               `json:"bundle_digest"`
	ArtifactDigest string               `json:"artifact_digest"`
	Passed         bool                 `json:"passed"`
	Result         domain.TestRunResult `json:"result"`
	HarnessVersion string               `json:"harness_version"`
	ExecutedBy     string               `json:"executed_by"`
	ExecutedAt     time.Time            `json:"executed_at"`
}

type ReviewRecord struct {
	ReviewID       string    `json:"review_id"`
	ArtifactDigest string    `json:"artifact_digest"`
	Reviewer       string    `json:"reviewer"`
	Role           string    `json:"role"`
	Decision       string    `json:"decision"`
	Findings       string    `json:"findings"`
	CreatedAt      time.Time `json:"created_at"`
}

type CertificationRecord struct {
	CertificationID string          `json:"certification_id"`
	PackRef         string          `json:"pack_ref"`
	Version         string          `json:"version"`
	ArtifactDigest  string          `json:"artifact_digest"`
	BundleDigest    string          `json:"bundle_digest"`
	TestRunID       string          `json:"test_run_id"`
	MinReviews      int             `json:"min_reviews"`
	Report          json.RawMessage `json:"report"`
	ReportDigest    string          `json:"report_digest"`
	Signature       string          `json:"signature"`
	SignatureKeyRef string          `json:"signature_key_ref"`
	CertifiedBy     string          `json:"certified_by"`
	CertifiedAt     time.Time       `json:"certified_at"`
}

// CertificationBlocked lists every unmet gate, so one attempt tells the
// caller everything that stands in the way.
type CertificationBlocked struct{ Reasons []string }

var ErrCertificationBlocked = errors.New("certification blocked")

func (e *CertificationBlocked) Error() string { return ErrCertificationBlocked.Error() }
func (e *CertificationBlocked) Unwrap() error { return ErrCertificationBlocked }

// versionForUpdate locks a version row and returns its id, status and author.
func (s *PgStore) versionForUpdate(ctx context.Context, tx pgx.Tx, ref, version string, lock bool) (id, status, createdBy string, err error) {
	q := `SELECT v.pack_version_id::text, v.status, v.created_by_principal_id FROM jurisdiction_pack_versions v
		JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 AND v.version=$2`
	if lock {
		q += ` FOR UPDATE OF v`
	}
	err = tx.QueryRow(ctx, q, ref, version).Scan(&id, &status, &createdBy)
	if err != nil {
		err = s.registryFail("version lookup", err, domain.ErrPackVersionNotFound)
	}
	return
}

// ── test bundles ────────────────────────────────────────────────────────────

// SubmitTestBundle appends a bundle revision. Identical content to the latest
// revision is a no-op. Only a version under REVIEW accepts tests.
func (s *PgStore) SubmitTestBundle(ctx context.Context, ref, version string, canonical []byte, digest, actor string) (*TestBundleRecord, bool, error) {
	tx, err := s.begin(ctx, "SubmitTestBundle")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, status, _, err := s.versionForUpdate(ctx, tx, ref, version, true)
	if err != nil {
		return nil, false, err
	}
	if status != "REVIEW" {
		return nil, false, domain.ErrNotUnderReview
	}
	if latest, lerr := scanBundle(tx.QueryRow(ctx, bundleSelect+` WHERE pack_version_id::text=$1 ORDER BY revision DESC LIMIT 1`, id)); lerr == nil {
		if latest.BundleDigest == digest {
			return latest, false, nil
		}
	} else if !errors.Is(lerr, pgx.ErrNoRows) {
		return nil, false, s.registryFail("SubmitTestBundle latest", lerr, nil)
	}
	rec, err := scanBundle(tx.QueryRow(ctx, `INSERT INTO pack_test_bundles (pack_version_id, revision, bundle, bundle_digest, submitted_by)
		VALUES ($1::uuid, COALESCE((SELECT MAX(revision) FROM pack_test_bundles WHERE pack_version_id = $1::uuid), 0) + 1, $2, $3, $4)
		RETURNING bundle_id::text, pack_version_id::text, revision, bundle, bundle_digest, submitted_by, submitted_at`,
		id, string(canonical), digest, actor))
	if err != nil {
		return nil, false, s.registryFail("SubmitTestBundle insert", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("SubmitTestBundle commit", err, nil)
	}
	return rec, true, nil
}

const bundleSelect = `SELECT bundle_id::text, pack_version_id::text, revision, bundle, bundle_digest, submitted_by, submitted_at FROM pack_test_bundles`

func scanBundle(r pgx.Row) (*TestBundleRecord, error) {
	var x TestBundleRecord
	var b string
	if err := r.Scan(&x.BundleID, &x.PackVersionID, &x.Revision, &b, &x.BundleDigest, &x.SubmittedBy, &x.SubmittedAt); err != nil {
		return nil, err
	}
	x.Bundle = json.RawMessage(b)
	return &x, nil
}

func (s *PgStore) GetLatestTestBundle(ctx context.Context, ref, version string) (*TestBundleRecord, error) {
	v, err := s.GetPackVersion(ctx, ref, version)
	if err != nil {
		return nil, err
	}
	x, err := scanBundle(s.pool.QueryRow(ctx, bundleSelect+` WHERE pack_version_id::text=$1 ORDER BY revision DESC LIMIT 1`, v.PackVersionID))
	if err != nil {
		return nil, s.registryFail("GetLatestTestBundle", err, domain.ErrNoTestBundle)
	}
	return x, nil
}

// ── test runs ───────────────────────────────────────────────────────────────

const runSelect = `SELECT run_id::text, bundle_id::text, bundle_digest, artifact_digest, passed, result::text, harness_version, executed_by, executed_at FROM pack_test_runs`

func scanRun(r pgx.Row) (*TestRunRecord, error) {
	var x TestRunRecord
	var res string
	if err := r.Scan(&x.RunID, &x.BundleID, &x.BundleDigest, &x.ArtifactDigest, &x.Passed, &res, &x.HarnessVersion, &x.ExecutedBy, &x.ExecutedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(res), &x.Result); err != nil {
		return nil, fmt.Errorf("stored test result is unreadable: %w", err)
	}
	return &x, nil
}

// RunTests executes the latest bundle against the compiled artifact and
// records the run, passed or failed: a failed run is evidence too.
func (s *PgStore) RunTests(ctx context.Context, ref, version, actor string) (*TestRunRecord, error) {
	tx, err := s.begin(ctx, "RunTests")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, status, _, err := s.versionForUpdate(ctx, tx, ref, version, true)
	if err != nil {
		return nil, err
	}
	if status != "REVIEW" {
		return nil, domain.ErrNotUnderReview
	}
	var artText, artDigest string
	if err := tx.QueryRow(ctx, `SELECT artifact, artifact_digest FROM pack_artifacts WHERE pack_version_id::text=$1`, id).Scan(&artText, &artDigest); err != nil {
		return nil, s.registryFail("RunTests artifact", err, domain.ErrNotCompiled)
	}
	b, err := scanBundle(tx.QueryRow(ctx, bundleSelect+` WHERE pack_version_id::text=$1 ORDER BY revision DESC LIMIT 1`, id))
	if err != nil {
		return nil, s.registryFail("RunTests bundle", err, domain.ErrNoTestBundle)
	}
	// The tested bytes must be the signed bytes: recheck the digest first.
	if d, derr := domain.DigestOf([]byte(artText)); derr != nil || d != artDigest {
		return nil, domain.ErrConflict
	}
	doc, err := domain.ParseArtifact([]byte(artText))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrConflict, err)
	}
	bundle, err := domain.ParseTestBundle(b.Bundle)
	if err != nil {
		return nil, err
	}
	result := domain.RunTestBundle(doc, bundle)
	raw, _ := json.Marshal(result)
	rec, err := scanRun(tx.QueryRow(ctx, `INSERT INTO pack_test_runs (pack_version_id, bundle_id, bundle_digest, artifact_digest, passed, result, harness_version, executed_by)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8)
		RETURNING run_id::text, bundle_id::text, bundle_digest, artifact_digest, passed, result::text, harness_version, executed_by, executed_at`,
		id, b.BundleID, b.BundleDigest, artDigest, result.Passed, string(raw), domain.HarnessVersion, actor))
	if err != nil {
		return nil, s.registryFail("RunTests insert", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("RunTests commit", err, nil)
	}
	return rec, nil
}

func (s *PgStore) ListTestRuns(ctx context.Context, ref, version string, limit int) ([]*TestRunRecord, error) {
	v, err := s.GetPackVersion(ctx, ref, version)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, runSelect+` WHERE pack_version_id::text=$1 ORDER BY executed_at DESC, run_id LIMIT $2`, v.PackVersionID, clampLimit(limit, 20, 100))
	if err != nil {
		return nil, s.registryFail("ListTestRuns", err, nil)
	}
	defer rows.Close()
	out := []*TestRunRecord{}
	for rows.Next() {
		x, err := scanRun(rows)
		if err != nil {
			return nil, s.registryFail("ListTestRuns scan", err, nil)
		}
		out = append(out, x)
	}
	return out, s.registryFail("ListTestRuns rows", rows.Err(), nil)
}

// ── reviews ─────────────────────────────────────────────────────────────────

// AddReview records an independent review of the compiled artifact. The
// reviewer must differ from the version author, compiler and test authors
// (enforced here for a precise error, and again by trigger).
func (s *PgStore) AddReview(ctx context.Context, ref, version, reviewer, role, decision, findings string) (*ReviewRecord, error) {
	tx, err := s.begin(ctx, "AddReview")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, status, author, err := s.versionForUpdate(ctx, tx, ref, version, true)
	if err != nil {
		return nil, err
	}
	if status != "REVIEW" {
		return nil, domain.ErrNotUnderReview
	}
	var artDigest, compiler string
	if err := tx.QueryRow(ctx, `SELECT artifact_digest, compiled_by_principal_id FROM pack_artifacts WHERE pack_version_id::text=$1`, id).Scan(&artDigest, &compiler); err != nil {
		return nil, s.registryFail("AddReview artifact", err, domain.ErrNotCompiled)
	}
	builders, err := s.builders(ctx, tx, id, author, compiler)
	if err != nil {
		return nil, err
	}
	if builders[reviewer] {
		return nil, domain.ErrNotIndependent
	}
	var rec ReviewRecord
	if err := tx.QueryRow(ctx, `INSERT INTO pack_reviews (pack_version_id, artifact_digest, reviewer, role, decision, findings)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING review_id::text, artifact_digest, reviewer, role, decision, findings, created_at`,
		id, artDigest, reviewer, role, decision, findings).
		Scan(&rec.ReviewID, &rec.ArtifactDigest, &rec.Reviewer, &rec.Role, &rec.Decision, &rec.Findings, &rec.CreatedAt); err != nil {
		return nil, s.registryFail("AddReview insert", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("AddReview commit", err, nil)
	}
	return &rec, nil
}

// builders returns everyone who authored, compiled or wrote tests for a version.
func (s *PgStore) builders(ctx context.Context, q querier, versionID, author, compiler string) (map[string]bool, error) {
	out := map[string]bool{author: true, compiler: true}
	rows, err := q.Query(ctx, `SELECT DISTINCT submitted_by FROM pack_test_bundles WHERE pack_version_id::text=$1`, versionID)
	if err != nil {
		return nil, s.registryFail("builders", err, nil)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, s.registryFail("builders scan", err, nil)
		}
		out[p] = true
	}
	return out, s.registryFail("builders rows", rows.Err(), nil)
}

func (s *PgStore) ListReviews(ctx context.Context, ref, version string) ([]*ReviewRecord, error) {
	v, err := s.GetPackVersion(ctx, ref, version)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT review_id::text, artifact_digest, reviewer, role, decision, findings, created_at
		FROM pack_reviews WHERE pack_version_id::text=$1 ORDER BY created_at, review_id`, v.PackVersionID)
	if err != nil {
		return nil, s.registryFail("ListReviews", err, nil)
	}
	defer rows.Close()
	out := []*ReviewRecord{}
	for rows.Next() {
		var x ReviewRecord
		if err := rows.Scan(&x.ReviewID, &x.ArtifactDigest, &x.Reviewer, &x.Role, &x.Decision, &x.Findings, &x.CreatedAt); err != nil {
			return nil, s.registryFail("ListReviews scan", err, nil)
		}
		out = append(out, &x)
	}
	return out, s.registryFail("ListReviews rows", rows.Err(), nil)
}

// ── certification ───────────────────────────────────────────────────────────

const certSelect = `SELECT c.certification_id::text, p.pack_ref, v.version, c.artifact_digest, c.bundle_digest, c.test_run_id::text,
	c.min_reviews, c.report::text, c.report_digest, c.signature, c.signature_key_ref, c.certified_by, c.certified_at
	FROM pack_certifications c JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)`

func scanCert(r pgx.Row) (*CertificationRecord, error) {
	var x CertificationRecord
	var rep string
	if err := r.Scan(&x.CertificationID, &x.PackRef, &x.Version, &x.ArtifactDigest, &x.BundleDigest, &x.TestRunID, &x.MinReviews,
		&rep, &x.ReportDigest, &x.Signature, &x.SignatureKeyRef, &x.CertifiedBy, &x.CertifiedAt); err != nil {
		return nil, err
	}
	x.Report = json.RawMessage(rep)
	return &x, nil
}

// CertifyPackVersion certifies a pack version under REVIEW and moves it to
// CERTIFIED, atomically, only when EVERY gate holds:
//
//   - the artifact is compiled, signed, and verifies (digest, key, signature);
//   - the latest test bundle was run against exactly this artifact and the
//     run PASSED with complete coverage;
//   - at least minReviews distinct independent reviewers APPROVE this artifact
//     and no reviewer's latest decision is REJECT;
//   - the certifier is independent of the author, compiler, test authors and
//     of every approving reviewer.
//
// All unmet gates are returned together in *CertificationBlocked. The signed
// certification report is produced with the service signer; with no signer
// configured certification fails closed. Certifying twice returns the original.
func (s *PgStore) CertifyPackVersion(ctx context.Context, ref, version, certifier string, minReviews int, signer domain.Signer) (*CertificationRecord, bool, error) {
	if signer == nil {
		return nil, false, domain.ErrSigningNotConfigured
	}
	if minReviews < 1 {
		minReviews = 1
	}
	tx, err := s.begin(ctx, "CertifyPackVersion")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id, status, author, err := s.versionForUpdate(ctx, tx, ref, version, true)
	if err != nil {
		return nil, false, err
	}
	if existing, cerr := scanCert(tx.QueryRow(ctx, certSelect+` WHERE c.pack_version_id::text=$1`, id)); cerr == nil {
		return existing, false, nil
	} else if !errors.Is(cerr, pgx.ErrNoRows) {
		return nil, false, s.registryFail("CertifyPackVersion existing", cerr, nil)
	}
	if status != "REVIEW" {
		return nil, false, domain.ErrNotUnderReview
	}
	// An emergency hotfix always needs the enhanced review (s24): at least two
	// independent approving reviewers, whatever the deployment default is.
	if _, herr := s.getHotfixTx(ctx, tx, id); herr == nil && minReviews < 2 {
		minReviews = 2
	}

	var reasons []string
	block := func(format string, a ...any) { reasons = append(reasons, fmt.Sprintf(format, a...)) }

	// Gate 1: artifact compiled, signed, verifies.
	art, aerr := scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactCols+` FROM pack_artifacts a
		JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)
		WHERE a.pack_version_id::text=$1`, id))
	var compiler string
	switch {
	case errors.Is(aerr, pgx.ErrNoRows):
		block("not_compiled: the version has no compiled artifact")
	case aerr != nil:
		return nil, false, s.registryFail("CertifyPackVersion artifact", aerr, nil)
	default:
		compiler = art.CompiledBy
		var key *domain.TrustedKey
		if art.SignatureKeyRef != nil {
			if k, kerr := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM pack_signing_keys WHERE key_ref=$1`, *art.SignatureKeyRef)); kerr == nil {
				key = k
			}
		}
		vr := domain.VerifyArtifact(domain.VerifyInput{ArtifactJSON: string(art.Artifact), StoredDigest: art.ArtifactDigest,
			VersionDigest: art.ArtifactDigest, Signature: art.Signature, SignatureKeyID: art.SignatureKeyRef, Key: key})
		if !vr.Verified {
			block("artifact_unverified: %v", vr.Reasons)
		}
	}

	// Gate 2: the latest bundle ran against this artifact and passed.
	var run *TestRunRecord
	bundle, berr := scanBundle(tx.QueryRow(ctx, bundleSelect+` WHERE pack_version_id::text=$1 ORDER BY revision DESC LIMIT 1`, id))
	switch {
	case errors.Is(berr, pgx.ErrNoRows):
		block("no_test_bundle: no test bundle has been submitted")
	case berr != nil:
		return nil, false, s.registryFail("CertifyPackVersion bundle", berr, nil)
	case art != nil:
		r, rerr := scanRun(tx.QueryRow(ctx, runSelect+` WHERE pack_version_id::text=$1 AND bundle_id::text=$2 AND artifact_digest=$3
			ORDER BY executed_at DESC, run_id LIMIT 1`, id, bundle.BundleID, art.ArtifactDigest))
		switch {
		case errors.Is(rerr, pgx.ErrNoRows):
			block("tests_not_run: the latest test bundle (revision %d) has not been run against this artifact", bundle.Revision)
		case rerr != nil:
			return nil, false, s.registryFail("CertifyPackVersion run", rerr, nil)
		case !r.Passed:
			block("tests_failed: the latest run failed (%d failed cases, %d coverage gaps)", r.Result.Failed, len(r.Result.CoverageGaps))
		default:
			run = r
		}
	}

	// Gate 3: independent reviews.
	revRows, err := tx.Query(ctx, `SELECT DISTINCT ON (reviewer, role) review_id::text, reviewer, role, decision, artifact_digest
		FROM pack_reviews WHERE pack_version_id::text=$1 ORDER BY reviewer, role, created_at DESC, review_id DESC`, id)
	if err != nil {
		return nil, false, s.registryFail("CertifyPackVersion reviews", err, nil)
	}
	type rv struct{ id, reviewer, role, decision, digest string }
	var latest []rv
	for revRows.Next() {
		var x rv
		if err := revRows.Scan(&x.id, &x.reviewer, &x.role, &x.decision, &x.digest); err != nil {
			revRows.Close()
			return nil, false, s.registryFail("CertifyPackVersion reviews scan", err, nil)
		}
		latest = append(latest, x)
	}
	revRows.Close()
	approvers := map[string]bool{}
	var counted []rv
	for _, x := range latest {
		if art != nil && x.digest != art.ArtifactDigest {
			continue // a review of different content is not a review of this
		}
		counted = append(counted, x)
		if x.decision == "REJECT" {
			block("review_rejected: %s (%s) rejected this artifact", x.reviewer, x.role)
		} else {
			approvers[x.reviewer] = true
		}
	}
	if len(approvers) < minReviews {
		block("insufficient_reviews: %d independent approving reviewer(s), %d required", len(approvers), minReviews)
	}

	// Gate 4: the certifier is independent.
	if art != nil {
		builders, berr := s.builders(ctx, tx, id, author, compiler)
		if berr != nil {
			return nil, false, berr
		}
		if builders[certifier] || approvers[certifier] {
			block("certifier_not_independent: the certifier must differ from the author, compiler, test authors and reviewers")
		}
	}

	if len(reasons) > 0 {
		return nil, false, &CertificationBlocked{Reasons: reasons}
	}

	// Signer must be the registered ACTIVE key (same rule as artifact signing).
	key, kerr := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM pack_signing_keys WHERE key_ref=$1`, signer.KeyRef()))
	if kerr != nil || key.Status != domain.KeyActive || key.Algorithm != domain.AlgorithmEd25519 ||
		string(key.PublicKey) != string(signer.PublicKey()) {
		return nil, false, domain.ErrSigningKeyNotActive
	}

	sort.Slice(counted, func(a, b int) bool { return counted[a].id < counted[b].id })
	reviewList := make([]map[string]any, 0, len(counted))
	for _, x := range counted {
		reviewList = append(reviewList, map[string]any{"review_id": x.id, "reviewer": x.reviewer, "role": x.role, "decision": x.decision})
	}
	report := map[string]any{
		"format": "zs-jur-001/pack-certification", "format_version": "1",
		"pack_ref": ref, "pack_version": version, "artifact_digest": art.ArtifactDigest,
		"bundle_digest": bundle.BundleDigest, "bundle_revision": bundle.Revision, "test_run_id": run.RunID,
		"harness_version": run.HarnessVersion,
		"test_summary": map[string]any{"total": run.Result.Total, "failed": run.Result.Failed, "by_class": run.Result.ByClass,
			"coverage_complete": run.Result.CoverageComplete},
		"classes_not_implemented": run.Result.ClassesNotImplemented,
		"min_reviews":             minReviews, "reviews": reviewList, "certified_by": certifier,
	}
	rawReport, err := json.Marshal(report)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	canonical, err := domain.CanonicalJSON(rawReport)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	reportDigest, _ := domain.DigestOf(canonical)
	sig, err := signer.Sign(domain.CertificationSigningMessage(reportDigest))
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	rec, err := scanCert(tx.QueryRow(ctx, `WITH ins AS (
			INSERT INTO pack_certifications (pack_version_id, artifact_digest, bundle_digest, test_run_id, min_reviews, report, report_digest,
				signature, signature_key_ref, certified_by)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10) RETURNING *)
		SELECT c.certification_id::text, p.pack_ref, v.version, c.artifact_digest, c.bundle_digest, c.test_run_id::text,
			c.min_reviews, c.report::text, c.report_digest, c.signature, c.signature_key_ref, c.certified_by, c.certified_at
		FROM ins c JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)`,
		id, art.ArtifactDigest, bundle.BundleDigest, run.RunID, minReviews, string(canonical), reportDigest,
		base64.StdEncoding.EncodeToString(sig), signer.KeyRef(), certifier))
	if err != nil {
		return nil, false, s.registryFail("CertifyPackVersion insert", err, nil)
	}
	if _, err := tx.Exec(ctx, `UPDATE jurisdiction_pack_versions SET status='CERTIFIED', test_bundle_digest=$2, updated_at=NOW(), updated_by_principal_id=$3
		WHERE pack_version_id::text=$1`, id, bundle.BundleDigest, certifier); err != nil {
		return nil, false, s.registryFail("CertifyPackVersion status", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("CertifyPackVersion commit", err, nil)
	}
	return rec, true, nil
}

// GetCertification returns the certification and its live signature verdict.
func (s *PgStore) GetCertification(ctx context.Context, ref, version string) (*CertificationRecord, domain.VerificationResult, error) {
	if _, err := s.GetPackVersion(ctx, ref, version); err != nil {
		return nil, domain.VerificationResult{}, err
	}
	c, err := scanCert(s.pool.QueryRow(ctx, certSelect+` WHERE p.pack_ref=$1 AND v.version=$2`, ref, version))
	if err != nil {
		return nil, domain.VerificationResult{}, s.registryFail("GetCertification", err, domain.ErrNotCertified)
	}
	var key *domain.TrustedKey
	if k, kerr := s.GetSigningKey(ctx, c.SignatureKeyRef); kerr == nil {
		key = k
	}
	sig, ref2 := c.Signature, c.SignatureKeyRef
	return c, domain.VerifyCertification(string(c.Report), c.ReportDigest, &sig, &ref2, key), nil
}
