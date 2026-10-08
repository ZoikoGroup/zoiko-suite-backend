package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 7 persistence: release lifecycle, deployment rings and
// regions, rollback, emergency hotfix. Every command is one transaction with
// the version row locked, and every refusal lists ALL unmet gates at once.

// OpsBlocked carries every unmet gate of a lifecycle or deployment command.
type OpsBlocked struct{ Reasons []string }

var ErrOpsBlocked = errors.New("operation blocked")

func (e *OpsBlocked) Error() string { return ErrOpsBlocked.Error() }
func (e *OpsBlocked) Unwrap() error { return ErrOpsBlocked }

// ReleaseResult is the outcome of a lifecycle command.
type ReleaseResult struct {
	PackRef    string `json:"pack_ref"`
	Version    string `json:"version"`
	FromStatus string `json:"from_status"`
	ToStatus   string `json:"to_status"`
	Changed    bool   `json:"changed"`
}

type Ring struct {
	RingCode       string    `json:"ring_code"`
	Ordinal        int       `json:"ordinal"`
	MinSoakSeconds int       `json:"min_soak_seconds"`
	Description    *string   `json:"description"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedBy      string    `json:"created_by_principal_id"`
}

type Region struct {
	RegionCode  string    `json:"region_code"`
	Description *string   `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by_principal_id"`
}

type Deployment struct {
	DeploymentID    string     `json:"deployment_id"`
	PackRef         string     `json:"pack_ref"`
	Version         string     `json:"version"`
	RingCode        string     `json:"ring_code"`
	RegionCode      string     `json:"region_code"`
	Status          string     `json:"status"`
	DeployedAt      time.Time  `json:"deployed_at"`
	DeployedBy      string     `json:"deployed_by"`
	ExceptionReason *string    `json:"exception_reason"`
	RolledBackAt    *time.Time `json:"rolled_back_at"`
	RolledBackBy    *string    `json:"rolled_back_by"`
	RollbackReason  *string    `json:"rollback_reason"`
}

// RollbackResult reports what a rollback changed and what it could not undo.
type RollbackResult struct {
	RolledBack      *Deployment `json:"rolled_back"`
	Restored        *Deployment `json:"restored"`
	DecisionsMade   int         `json:"decisions_made_with_rolled_back_version"`
	RemediationNote string      `json:"remediation_note"`
	Reconciliation  string      `json:"reconciliation"`
}

type Hotfix struct {
	PackRef               string     `json:"pack_ref"`
	Version               string     `json:"version"`
	Severity              string     `json:"severity"`
	ScopeSummary          string     `json:"scope_summary"`
	IncidentRef           string     `json:"incident_ref"`
	IncidentCommander     string     `json:"incident_commander"`
	RollbackTargetVersion string     `json:"rollback_target_version"`
	DeclaredBy            string     `json:"declared_by"`
	DeclaredAt            time.Time  `json:"declared_at"`
	RetroDueAt            time.Time  `json:"retro_due_at"`
	RetroCompletedAt      *time.Time `json:"retro_completed_at"`
	RetroCompletedBy      *string    `json:"retro_completed_by"`
	RetroNote             *string    `json:"retro_note"`
}

var (
	errInvalidLifecycle = domain.ErrInvalidLifecycle
)

type opsVersion struct {
	id, status, author string
}

func (s *PgStore) lockVersion(ctx context.Context, tx pgx.Tx, ref, version string) (opsVersion, error) {
	id, status, author, err := s.versionForUpdate(ctx, tx, ref, version, true)
	return opsVersion{id, status, author}, err
}

// verifyForOps loads a version's artifact and certification and applies the
// single trust definition shared with the runtime resolver.
func (s *PgStore) verifyForOps(ctx context.Context, versionID string) ([]string, error) {
	lp, err := s.LoadPack(ctx, versionID)
	if err != nil {
		return nil, err
	}
	if lp.ArtifactJSON == "" {
		return []string{"not_compiled"}, nil
	}
	return domain.VerifyLoadedPack(lp), nil
}

// packDependencies returns the registered-pack dependencies of a version.
func (s *PgStore) packDependencies(ctx context.Context, q querier, versionID string) ([]struct{ Ref, Version, Status string }, error) {
	rows, err := q.Query(ctx, `SELECT d.dependency_ref, d.dependency_version, COALESCE(dv.status, '')
		FROM pack_dependencies d
		JOIN jurisdiction_packs dp ON dp.pack_ref = d.dependency_ref
		LEFT JOIN jurisdiction_pack_versions dv ON dv.pack_id = dp.pack_id AND dv.version = d.dependency_version
		WHERE d.pack_version_id::text=$1 ORDER BY 1, 2`, versionID)
	if err != nil {
		return nil, s.registryFail("packDependencies", err, nil)
	}
	defer rows.Close()
	var out []struct{ Ref, Version, Status string }
	for rows.Next() {
		var x struct{ Ref, Version, Status string }
		if err := rows.Scan(&x.Ref, &x.Version, &x.Status); err != nil {
			return nil, s.registryFail("packDependencies scan", err, nil)
		}
		out = append(out, x)
	}
	return out, s.registryFail("packDependencies rows", rows.Err(), nil)
}

func (s *PgStore) getHotfixTx(ctx context.Context, q querier, versionID string) (*Hotfix, error) {
	var h Hotfix
	err := q.QueryRow(ctx, `SELECT p.pack_ref, v.version, h.severity, h.scope_summary, h.incident_ref, h.incident_commander,
			h.rollback_target_version, h.declared_by, h.declared_at, h.retro_due_at, h.retro_completed_at, h.retro_completed_by, h.retro_note
		FROM pack_hotfixes h JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)
		WHERE h.pack_version_id::text=$1`, versionID).
		Scan(&h.PackRef, &h.Version, &h.Severity, &h.ScopeSummary, &h.IncidentRef, &h.IncidentCommander, &h.RollbackTargetVersion,
			&h.DeclaredBy, &h.DeclaredAt, &h.RetroDueAt, &h.RetroCompletedAt, &h.RetroCompletedBy, &h.RetroNote)
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// transition writes the release event and the status change together; the
// database refuses the status change without the matching event.
func (s *PgStore) transition(ctx context.Context, tx pgx.Tx, v opsVersion, action, to, actor, reason, evidence string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO pack_release_events (pack_version_id, action, from_status, to_status, actor, reason, evidence_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, v.id, action, v.status, to, actor, nullIfEmpty(reason), nullIfEmpty(evidence)); err != nil {
		return s.registryFail("release event", err, nil)
	}
	if _, err := tx.Exec(ctx, `UPDATE jurisdiction_pack_versions SET status=$2, updated_at=NOW(), updated_by_principal_id=$3
		WHERE pack_version_id::text=$1`, v.id, to, actor); err != nil {
		return s.registryFail("release status", err, nil)
	}
	return nil
}

// ── regions and rings ───────────────────────────────────────────────────────

func (s *PgStore) CreateRegion(ctx context.Context, code string, description *string, actor string) (*Region, bool, error) {
	var r Region
	err := s.pool.QueryRow(ctx, `INSERT INTO deployment_regions (region_code, description, created_by_principal_id) VALUES ($1,$2,$3)
		ON CONFLICT (region_code) DO NOTHING RETURNING region_code, description, created_at, created_by_principal_id`, code, description, actor).
		Scan(&r.RegionCode, &r.Description, &r.CreatedAt, &r.CreatedBy)
	if err == nil {
		return &r, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, s.registryFail("CreateRegion", err, nil)
	}
	if err := s.pool.QueryRow(ctx, `SELECT region_code, description, created_at, created_by_principal_id FROM deployment_regions WHERE region_code=$1`, code).
		Scan(&r.RegionCode, &r.Description, &r.CreatedAt, &r.CreatedBy); err != nil {
		return nil, false, s.registryFail("CreateRegion lookup", err, nil)
	}
	return &r, false, nil
}

func (s *PgStore) ListRegions(ctx context.Context) ([]*Region, error) {
	rows, err := s.pool.Query(ctx, `SELECT region_code, description, created_at, created_by_principal_id FROM deployment_regions ORDER BY region_code`)
	if err != nil {
		return nil, s.registryFail("ListRegions", err, nil)
	}
	defer rows.Close()
	out := []*Region{}
	for rows.Next() {
		var r Region
		if err := rows.Scan(&r.RegionCode, &r.Description, &r.CreatedAt, &r.CreatedBy); err != nil {
			return nil, s.registryFail("ListRegions scan", err, nil)
		}
		out = append(out, &r)
	}
	return out, s.registryFail("ListRegions rows", rows.Err(), nil)
}

// CreateRing defines a rollout ring. Rings are append-only: a replay with the
// same attributes is a no-op, any difference is a conflict.
func (s *PgStore) CreateRing(ctx context.Context, code string, ordinal, soak int, description *string, actor string) (*Ring, bool, error) {
	var r Ring
	err := s.pool.QueryRow(ctx, `INSERT INTO deployment_rings (ring_code, ordinal, min_soak_seconds, description, created_by_principal_id)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (ring_code) DO NOTHING
		RETURNING ring_code, ordinal, min_soak_seconds, description, created_at, created_by_principal_id`, code, ordinal, soak, description, actor).
		Scan(&r.RingCode, &r.Ordinal, &r.MinSoakSeconds, &r.Description, &r.CreatedAt, &r.CreatedBy)
	if err == nil {
		return &r, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, s.registryFail("CreateRing", err, nil)
	}
	if err := s.pool.QueryRow(ctx, `SELECT ring_code, ordinal, min_soak_seconds, description, created_at, created_by_principal_id FROM deployment_rings WHERE ring_code=$1`, code).
		Scan(&r.RingCode, &r.Ordinal, &r.MinSoakSeconds, &r.Description, &r.CreatedAt, &r.CreatedBy); err != nil {
		return nil, false, s.registryFail("CreateRing lookup", err, nil)
	}
	if r.Ordinal != ordinal || r.MinSoakSeconds != soak {
		return nil, false, domain.ErrConflict
	}
	return &r, false, nil
}

func (s *PgStore) ListRings(ctx context.Context) ([]*Ring, error) {
	rows, err := s.pool.Query(ctx, `SELECT ring_code, ordinal, min_soak_seconds, description, created_at, created_by_principal_id FROM deployment_rings ORDER BY ordinal`)
	if err != nil {
		return nil, s.registryFail("ListRings", err, nil)
	}
	defer rows.Close()
	out := []*Ring{}
	for rows.Next() {
		var r Ring
		if err := rows.Scan(&r.RingCode, &r.Ordinal, &r.MinSoakSeconds, &r.Description, &r.CreatedAt, &r.CreatedBy); err != nil {
			return nil, s.registryFail("ListRings scan", err, nil)
		}
		out = append(out, &r)
	}
	return out, s.registryFail("ListRings rows", rows.Err(), nil)
}

// ── release lifecycle ───────────────────────────────────────────────────────

// PublishPackVersion moves CERTIFIED to RELEASED. It re-verifies the artifact
// and certification (never trusting that an earlier check still holds), and
// requires every registered-pack dependency to be RELEASED itself.
func (s *PgStore) PublishPackVersion(ctx context.Context, ref, version, actor, evidenceRef string) (*ReleaseResult, error) {
	tx, err := s.begin(ctx, "PublishPackVersion")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockVersion(ctx, tx, ref, version)
	if err != nil {
		return nil, err
	}
	res := &ReleaseResult{PackRef: ref, Version: version, FromStatus: v.status, ToStatus: "RELEASED"}
	if v.status == "RELEASED" {
		return res, nil
	}
	if v.status != "CERTIFIED" {
		return nil, fmt.Errorf("%w: only a CERTIFIED version can be released, this one is %s", errInvalidLifecycle, v.status)
	}

	var reasons []string
	rs, err := s.verifyForOps(ctx, v.id)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		reasons = append(reasons, "artifact_or_certification_unverified: "+r)
	}
	deps, err := s.packDependencies(ctx, tx, v.id)
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		if d.Status != "RELEASED" {
			reasons = append(reasons, fmt.Sprintf("dependency_not_released: %s@%s is %q", d.Ref, d.Version, d.Status))
		}
	}
	if hf, herr := s.getHotfixTx(ctx, tx, v.id); herr == nil {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM jurisdiction_pack_versions rv JOIN jurisdiction_packs rp USING (pack_id)
			JOIN jurisdiction_packs mp ON mp.pack_id = rp.pack_id WHERE mp.pack_ref=$1 AND rv.version=$2 AND rv.status='RELEASED'`,
			ref, hf.RollbackTargetVersion).Scan(&n); err != nil {
			return nil, s.registryFail("PublishPackVersion rollback target", err, nil)
		}
		if n == 0 {
			reasons = append(reasons, fmt.Sprintf("rollback_target_not_released: %s@%s must be a RELEASED version before the hotfix is published", ref, hf.RollbackTargetVersion))
		}
	} else if !errors.Is(herr, pgx.ErrNoRows) {
		return nil, s.registryFail("PublishPackVersion hotfix", herr, nil)
	}
	if len(reasons) > 0 {
		return nil, &OpsBlocked{Reasons: reasons}
	}
	if err := s.transition(ctx, tx, v, "RELEASE", "RELEASED", actor, "", evidenceRef); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("PublishPackVersion commit", err, nil)
	}
	res.Changed = true
	return res, nil
}

// WithdrawPackVersion prevents any new use of a version (s23, s25); the
// artifact and every past decision stay resolvable for reconstruction. A
// version another RELEASED version depends on cannot be withdrawn. Its ACTIVE
// deployments are rolled back with the reason.
func (s *PgStore) WithdrawPackVersion(ctx context.Context, ref, version, reason, evidenceRef, actor string) (*ReleaseResult, error) {
	tx, err := s.begin(ctx, "WithdrawPackVersion")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockVersion(ctx, tx, ref, version)
	if err != nil {
		return nil, err
	}
	res := &ReleaseResult{PackRef: ref, Version: version, FromStatus: v.status, ToStatus: "WITHDRAWN"}
	if v.status == "WITHDRAWN" {
		return res, nil
	}
	switch v.status {
	case "CERTIFIED", "RELEASED", "EMERGENCY_BLOCKED", "SUPERSEDED":
	default:
		return nil, fmt.Errorf("%w: a %s version cannot be withdrawn", errInvalidLifecycle, v.status)
	}
	rows, err := tx.Query(ctx, `SELECT p2.pack_ref, v2.version FROM pack_dependencies d
		JOIN jurisdiction_pack_versions v2 USING (pack_version_id) JOIN jurisdiction_packs p2 USING (pack_id)
		WHERE d.dependency_ref=$1 AND d.dependency_version=$2 AND v2.status='RELEASED'`, ref, version)
	if err != nil {
		return nil, s.registryFail("WithdrawPackVersion dependents", err, nil)
	}
	var reasons []string
	for rows.Next() {
		var r, ver string
		if err := rows.Scan(&r, &ver); err != nil {
			rows.Close()
			return nil, s.registryFail("WithdrawPackVersion scan", err, nil)
		}
		reasons = append(reasons, fmt.Sprintf("in_use_by_released_version: %s@%s depends on it; withdraw or replace the dependent first", r, ver))
	}
	rows.Close()
	if len(reasons) > 0 {
		return nil, &OpsBlocked{Reasons: reasons}
	}
	if _, err := tx.Exec(ctx, `UPDATE pack_deployments SET status='ROLLED_BACK', rolled_back_at=NOW(), rolled_back_by=$2, rollback_reason=$3
		WHERE pack_version_id::text=$1 AND status='ACTIVE'`, v.id, actor, "version withdrawn: "+reason); err != nil {
		return nil, s.registryFail("WithdrawPackVersion deployments", err, nil)
	}
	if err := s.transition(ctx, tx, v, "WITHDRAW", "WITHDRAWN", actor, reason, evidenceRef); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("WithdrawPackVersion commit", err, nil)
	}
	res.Changed = true
	return res, nil
}

// BlockPackVersion is the operational emergency stop: RELEASED to
// EMERGENCY_BLOCKED pending incident review. Deployments stay recorded but
// the resolver stops using the version immediately (after its cache TTL).
func (s *PgStore) BlockPackVersion(ctx context.Context, ref, version, reason, actor string) (*ReleaseResult, error) {
	tx, err := s.begin(ctx, "BlockPackVersion")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockVersion(ctx, tx, ref, version)
	if err != nil {
		return nil, err
	}
	res := &ReleaseResult{PackRef: ref, Version: version, FromStatus: v.status, ToStatus: "EMERGENCY_BLOCKED"}
	if v.status == "EMERGENCY_BLOCKED" {
		return res, nil
	}
	if v.status != "RELEASED" {
		return nil, fmt.Errorf("%w: only a RELEASED version can be blocked, this one is %s", errInvalidLifecycle, v.status)
	}
	if err := s.transition(ctx, tx, v, "BLOCK", "EMERGENCY_BLOCKED", actor, reason, ""); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("BlockPackVersion commit", err, nil)
	}
	res.Changed = true
	return res, nil
}

// UnblockPackVersion returns a blocked version to RELEASED after incident
// review, re-verifying it first.
func (s *PgStore) UnblockPackVersion(ctx context.Context, ref, version, reason, actor string) (*ReleaseResult, error) {
	tx, err := s.begin(ctx, "UnblockPackVersion")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockVersion(ctx, tx, ref, version)
	if err != nil {
		return nil, err
	}
	res := &ReleaseResult{PackRef: ref, Version: version, FromStatus: v.status, ToStatus: "RELEASED"}
	if v.status == "RELEASED" {
		return res, nil
	}
	if v.status != "EMERGENCY_BLOCKED" {
		return nil, fmt.Errorf("%w: only a blocked version can be unblocked, this one is %s", errInvalidLifecycle, v.status)
	}
	rs, err := s.verifyForOps(ctx, v.id)
	if err != nil {
		return nil, err
	}
	if len(rs) > 0 {
		return nil, &OpsBlocked{Reasons: prefixAll("artifact_or_certification_unverified: ", rs)}
	}
	if err := s.transition(ctx, tx, v, "UNBLOCK", "RELEASED", actor, reason, ""); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("UnblockPackVersion commit", err, nil)
	}
	res.Changed = true
	return res, nil
}

func prefixAll(p string, in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = p + s
	}
	return out
}

// ── deployment ──────────────────────────────────────────────────────────────

const deploymentCols = `d.deployment_id::text, p.pack_ref, v.version, d.ring_code, d.region_code, d.status, d.deployed_at, d.deployed_by,
	d.exception_reason, d.rolled_back_at, d.rolled_back_by, d.rollback_reason`
const deploymentFrom = ` FROM pack_deployments d JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)`

func scanDeployment(r pgx.Row) (*Deployment, error) {
	var d Deployment
	err := r.Scan(&d.DeploymentID, &d.PackRef, &d.Version, &d.RingCode, &d.RegionCode, &d.Status, &d.DeployedAt, &d.DeployedBy,
		&d.ExceptionReason, &d.RolledBackAt, &d.RolledBackBy, &d.RollbackReason)
	return &d, err
}

// DeployPackVersion deploys a RELEASED version to a ring and region.
//
// Gates (all reported together): the version re-verifies; the PREVIOUS ring
// has this version active in the same region, for at least that ring's soak
// time, with no runtime verification failure since (ring order is the
// canary: JUR-NEG-14 context, s24); every registered-pack dependency is ACTIVE
// in the same ring and region (JUR-NEG-15); a hotfix has its rollback target
// active here and two independent approving reviewers.
//
// exceptionReason skips the ring-order gates and is accepted ONLY for a
// declared hotfix and ONLY from its incident commander (s24); it is recorded.
func (s *PgStore) DeployPackVersion(ctx context.Context, ref, version, ring, region, actor, exceptionReason string) (*Deployment, bool, error) {
	tx, err := s.begin(ctx, "DeployPackVersion")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockVersion(ctx, tx, ref, version)
	if err != nil {
		return nil, false, err
	}
	if v.status != "RELEASED" {
		return nil, false, fmt.Errorf("%w: only a RELEASED version can be deployed, this one is %s", errInvalidLifecycle, v.status)
	}
	var ordinal int
	if err := tx.QueryRow(ctx, `SELECT ordinal FROM deployment_rings WHERE ring_code=$1`, ring).Scan(&ordinal); err != nil {
		return nil, false, s.registryFail("DeployPackVersion ring", err, domain.ErrRingNotFound)
	}
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM deployment_regions WHERE region_code=$1`, region).Scan(&one); err != nil {
		return nil, false, s.registryFail("DeployPackVersion region", err, domain.ErrRegionNotFound)
	}
	if existing, derr := scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentCols+deploymentFrom+`
		WHERE d.pack_version_id::text=$1 AND d.ring_code=$2 AND d.region_code=$3 AND d.status='ACTIVE'`, v.id, ring, region)); derr == nil {
		return existing, false, nil
	} else if !errors.Is(derr, pgx.ErrNoRows) {
		return nil, false, s.registryFail("DeployPackVersion existing", derr, nil)
	}

	var reasons []string
	block := func(format string, a ...any) { reasons = append(reasons, fmt.Sprintf(format, a...)) }

	rs, err := s.verifyForOps(ctx, v.id)
	if err != nil {
		return nil, false, err
	}
	for _, r := range rs {
		block("artifact_or_certification_unverified: %s", r)
	}

	hf, herr := s.getHotfixTx(ctx, tx, v.id)
	isHotfix := herr == nil
	if herr != nil && !errors.Is(herr, pgx.ErrNoRows) {
		return nil, false, s.registryFail("DeployPackVersion hotfix", herr, nil)
	}

	skipOrder := false
	if strings.TrimSpace(exceptionReason) != "" {
		switch {
		case !isHotfix:
			block("exception_not_allowed: ring order can only be skipped for a declared emergency hotfix")
		case hf.IncidentCommander != actor:
			block("exception_requires_incident_commander: only %s may document the exception", hf.IncidentCommander)
		default:
			skipOrder = true
		}
	}

	if !skipOrder {
		var prevRing string
		var prevSoak int
		perr := tx.QueryRow(ctx, `SELECT ring_code, min_soak_seconds FROM deployment_rings WHERE ordinal < $1 ORDER BY ordinal DESC LIMIT 1`, ordinal).Scan(&prevRing, &prevSoak)
		switch {
		case errors.Is(perr, pgx.ErrNoRows): // first ring: nothing before it
		case perr != nil:
			return nil, false, s.registryFail("DeployPackVersion prev ring", perr, nil)
		default:
			var deployedAt time.Time
			var elapsed float64
			derr := tx.QueryRow(ctx, `SELECT deployed_at, EXTRACT(EPOCH FROM (NOW() - deployed_at)) FROM pack_deployments
				WHERE pack_version_id::text=$1 AND ring_code=$2 AND region_code=$3 AND status='ACTIVE'`, v.id, prevRing, region).Scan(&deployedAt, &elapsed)
			switch {
			case errors.Is(derr, pgx.ErrNoRows):
				block("previous_ring_not_deployed: deploy to ring %q in region %q first", prevRing, region)
			case derr != nil:
				return nil, false, s.registryFail("DeployPackVersion prev deployment", derr, nil)
			default:
				if int(elapsed) < prevSoak {
					block("soak_not_elapsed: ring %q requires %ds of soak, %ds elapsed", prevRing, prevSoak, int(elapsed))
				}
				var failures int
				if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM pack_verification_failures WHERE pack_version_id::text=$1 AND ring_code=$2
					AND region_code=$3 AND detected_at >= $4`, v.id, prevRing, region, deployedAt).Scan(&failures); err != nil {
					return nil, false, s.registryFail("DeployPackVersion failures", err, nil)
				}
				if failures > 0 {
					block("verification_failures_in_previous_ring: %d failure(s) in ring %q since it was deployed there", failures, prevRing)
				}
			}
		}
	}

	deps, err := s.packDependencies(ctx, tx, v.id)
	if err != nil {
		return nil, false, err
	}
	for _, d := range deps {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM pack_deployments dd JOIN jurisdiction_pack_versions dv USING (pack_version_id)
			JOIN jurisdiction_packs dp USING (pack_id) WHERE dp.pack_ref=$1 AND dv.version=$2 AND dd.ring_code=$3 AND dd.region_code=$4 AND dd.status='ACTIVE'`,
			d.Ref, d.Version, ring, region).Scan(&n); err != nil {
			return nil, false, s.registryFail("DeployPackVersion dependency", err, nil)
		}
		if n == 0 {
			block("dependency_not_deployed: %s@%s is not active in ring %q, region %q (JUR-NEG-15)", d.Ref, d.Version, ring, region)
		}
	}

	if isHotfix {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM pack_deployments dd JOIN jurisdiction_pack_versions dv USING (pack_version_id)
			JOIN jurisdiction_packs dp USING (pack_id) WHERE dp.pack_ref=$1 AND dv.version=$2 AND dd.ring_code=$3 AND dd.region_code=$4 AND dd.status='ACTIVE'`,
			ref, hf.RollbackTargetVersion, ring, region).Scan(&n); err != nil {
			return nil, false, s.registryFail("DeployPackVersion rollback target", err, nil)
		}
		if n == 0 {
			block("rollback_target_not_deployed: %s@%s must be active in ring %q, region %q before the hotfix goes there", ref, hf.RollbackTargetVersion, ring, region)
		}
		var approvers int
		if err := tx.QueryRow(ctx, `SELECT COUNT(DISTINCT reviewer) FROM (
				SELECT DISTINCT ON (reviewer, role) reviewer, decision FROM pack_reviews WHERE pack_version_id::text=$1
				ORDER BY reviewer, role, created_at DESC, review_id DESC) x WHERE decision='APPROVE'`, v.id).Scan(&approvers); err != nil {
			return nil, false, s.registryFail("DeployPackVersion reviewers", err, nil)
		}
		if approvers < 2 {
			block("hotfix_needs_two_reviewers: %d independent approving reviewer(s), 2 required (JUR-NEG-14)", approvers)
		}
	}

	if len(reasons) > 0 {
		return nil, false, &OpsBlocked{Reasons: reasons}
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO pack_deployments (pack_version_id, ring_code, region_code, deployed_by, exception_reason)
		VALUES ($1,$2,$3,$4,$5) RETURNING deployment_id::text`, v.id, ring, region, actor, nullIfEmpty(strings.TrimSpace(exceptionReason))).Scan(&id); err != nil {
		return nil, false, s.registryFail("DeployPackVersion insert", err, nil)
	}
	d, err := scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentCols+deploymentFrom+` WHERE d.deployment_id::text=$1`, id))
	if err != nil {
		return nil, false, s.registryFail("DeployPackVersion read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("DeployPackVersion commit", err, nil)
	}
	return d, true, nil
}

// RollbackDeployment takes a version out of one ring and region and restores
// a named earlier release there (s23 "restore prior eligible version").
//
// Compatibility gate: the restore version belongs to the same pack, is
// RELEASED, is strictly older, re-verifies, and was deployed in this scope
// before. Rollback affects FUTURE resolution only (JUR-NEG-19): the result
// counts the decisions already made with the rolled-back version here and
// states that remediating them is a separate workflow that does not exist.
// There is no reconciliation signal in this service, and the result says so.
func (s *PgStore) RollbackDeployment(ctx context.Context, ref, version, ring, region, restoreVersion, reason, actor string) (*RollbackResult, error) {
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(restoreVersion) == "" {
		return nil, domain.ErrInvalidReference
	}
	tx, err := s.begin(ctx, "RollbackDeployment")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockVersion(ctx, tx, ref, version)
	if err != nil {
		return nil, err
	}
	cur, err := scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentCols+deploymentFrom+`
		WHERE d.pack_version_id::text=$1 AND d.ring_code=$2 AND d.region_code=$3 AND d.status='ACTIVE' FOR UPDATE OF d`, v.id, ring, region))
	if err != nil {
		return nil, s.registryFail("RollbackDeployment current", err, domain.ErrDeploymentNotFound)
	}

	var reasons []string
	var restoreID, restoreStatus string
	rerr := tx.QueryRow(ctx, `SELECT v.pack_version_id::text, v.status FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
		WHERE p.pack_ref=$1 AND v.version=$2`, ref, restoreVersion).Scan(&restoreID, &restoreStatus)
	switch {
	case errors.Is(rerr, pgx.ErrNoRows):
		reasons = append(reasons, fmt.Sprintf("restore_version_not_found: %s@%s", ref, restoreVersion))
	case rerr != nil:
		return nil, s.registryFail("RollbackDeployment restore", rerr, nil)
	default:
		if domain.CompareVersions(restoreVersion, version) >= 0 {
			reasons = append(reasons, fmt.Sprintf("restore_not_older: %s is not older than %s", restoreVersion, version))
		}
		if restoreStatus != "RELEASED" {
			reasons = append(reasons, fmt.Sprintf("restore_version_not_released: %s is %s", restoreVersion, restoreStatus))
		} else {
			rs, verr := s.verifyForOps(ctx, restoreID)
			if verr != nil {
				return nil, verr
			}
			for _, r := range rs {
				reasons = append(reasons, "restore_version_unverified: "+r)
			}
		}
		var everHere int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM pack_deployments WHERE pack_version_id::text=$1 AND ring_code=$2 AND region_code=$3`,
			restoreID, ring, region).Scan(&everHere); err != nil {
			return nil, s.registryFail("RollbackDeployment history", err, nil)
		}
		if everHere == 0 {
			reasons = append(reasons, fmt.Sprintf("restore_version_never_deployed_here: %s was never active in ring %q, region %q", restoreVersion, ring, region))
		}
	}
	if len(reasons) > 0 {
		return nil, &OpsBlocked{Reasons: reasons}
	}

	var decisions int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM rule_decision_evidence WHERE pack_version_id::text=$1 AND resolver_ring=$2 AND resolver_region=$3
		AND created_at >= $4`, v.id, ring, region, cur.DeployedAt).Scan(&decisions); err != nil {
		return nil, s.registryFail("RollbackDeployment decisions", err, nil)
	}

	if _, err := tx.Exec(ctx, `UPDATE pack_deployments SET status='ROLLED_BACK', rolled_back_at=NOW(), rolled_back_by=$2, rollback_reason=$3
		WHERE deployment_id::text=$1`, cur.DeploymentID, actor, reason); err != nil {
		return nil, s.registryFail("RollbackDeployment update", err, nil)
	}
	var restored *Deployment
	if d, derr := scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentCols+deploymentFrom+`
		WHERE d.pack_version_id::text=$1 AND d.ring_code=$2 AND d.region_code=$3 AND d.status='ACTIVE'`, restoreID, ring, region)); derr == nil {
		restored = d
	} else if errors.Is(derr, pgx.ErrNoRows) {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO pack_deployments (pack_version_id, ring_code, region_code, deployed_by)
			VALUES ($1,$2,$3,$4) RETURNING deployment_id::text`, restoreID, ring, region, actor).Scan(&id); err != nil {
			return nil, s.registryFail("RollbackDeployment restore insert", err, nil)
		}
		if restored, err = scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentCols+deploymentFrom+` WHERE d.deployment_id::text=$1`, id)); err != nil {
			return nil, s.registryFail("RollbackDeployment restore read", err, nil)
		}
	} else {
		return nil, s.registryFail("RollbackDeployment restored", derr, nil)
	}
	rolled, err := scanDeployment(tx.QueryRow(ctx, `SELECT `+deploymentCols+deploymentFrom+` WHERE d.deployment_id::text=$1`, cur.DeploymentID))
	if err != nil {
		return nil, s.registryFail("RollbackDeployment read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("RollbackDeployment commit", err, nil)
	}
	out := &RollbackResult{RolledBack: rolled, Restored: restored, DecisionsMade: decisions,
		Reconciliation: "not performed: no reconciliation signal exists in this service"}
	if decisions > 0 {
		out.RemediationNote = fmt.Sprintf("%d decision(s) were made with %s@%s in this ring and region. Rollback changes future resolution only; "+
			"those outcomes are unchanged and need a remediation workflow (not built).", decisions, ref, version)
	}
	return out, nil
}

// ListDeployments lists deployments of a pack version (history included).
func (s *PgStore) ListDeployments(ctx context.Context, ref, version string) ([]*Deployment, error) {
	if _, err := s.GetPackVersion(ctx, ref, version); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+deploymentCols+deploymentFrom+` WHERE p.pack_ref=$1 AND v.version=$2 ORDER BY d.deployed_at, d.deployment_id`, ref, version)
	if err != nil {
		return nil, s.registryFail("ListDeployments", err, nil)
	}
	defer rows.Close()
	out := []*Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, s.registryFail("ListDeployments scan", err, nil)
		}
		out = append(out, d)
	}
	return out, s.registryFail("ListDeployments rows", rows.Err(), nil)
}

// ── hotfix ──────────────────────────────────────────────────────────────────

// DeclareHotfix marks a version as an emergency hotfix BEFORE it is certified,
// so the enhanced review (two independent approvers) always applies. It names
// the known prior release to roll back to, the severity, the incident and its
// commander, and starts the retrospective clock.
func (s *PgStore) DeclareHotfix(ctx context.Context, ref, version, severity, scope, incident, commander, rollbackTarget string, retroSLA time.Duration, actor string) (*Hotfix, bool, error) {
	tx, err := s.begin(ctx, "DeclareHotfix")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockVersion(ctx, tx, ref, version)
	if err != nil {
		return nil, false, err
	}
	if existing, herr := s.getHotfixTx(ctx, tx, v.id); herr == nil {
		if existing.Severity == severity && existing.IncidentRef == incident && existing.RollbackTargetVersion == rollbackTarget {
			return existing, false, nil
		}
		return nil, false, domain.ErrConflict
	}
	if v.status != "DRAFT" && v.status != "REVIEW" {
		return nil, false, fmt.Errorf("%w: a hotfix must be declared while the version is DRAFT or REVIEW, this one is %s", errInvalidLifecycle, v.status)
	}
	var tstatus string
	if err := tx.QueryRow(ctx, `SELECT v.status FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
		WHERE p.pack_ref=$1 AND v.version=$2`, ref, rollbackTarget).Scan(&tstatus); err != nil {
		return nil, false, s.registryFail("DeclareHotfix target", err, domain.ErrInvalidReference)
	}
	if tstatus != "RELEASED" || domain.CompareVersions(rollbackTarget, version) >= 0 {
		return nil, false, &OpsBlocked{Reasons: []string{fmt.Sprintf(
			"rollback_target_invalid: %s@%s must be an older RELEASED version (it is %s)", ref, rollbackTarget, tstatus)}}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pack_hotfixes (pack_version_id, severity, scope_summary, incident_ref, incident_commander,
			rollback_target_version, declared_by, retro_due_at) VALUES ($1,$2,$3,$4,$5,$6,$7, NOW() + make_interval(secs => $8))`,
		v.id, severity, scope, incident, commander, rollbackTarget, actor, retroSLA.Seconds()); err != nil {
		return nil, false, s.registryFail("DeclareHotfix insert", err, nil)
	}
	h, err := s.getHotfixTx(ctx, tx, v.id)
	if err != nil {
		return nil, false, s.registryFail("DeclareHotfix read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("DeclareHotfix commit", err, nil)
	}
	return h, true, nil
}

func (s *PgStore) GetHotfix(ctx context.Context, ref, version string) (*Hotfix, error) {
	v, err := s.GetPackVersion(ctx, ref, version)
	if err != nil {
		return nil, err
	}
	h, err := s.getHotfixTx(ctx, s.pool, v.PackVersionID)
	if err != nil {
		return nil, s.registryFail("GetHotfix", err, domain.ErrHotfixNotFound)
	}
	return h, nil
}

// CompleteRetrospective records the mandatory post-incident review, once.
func (s *PgStore) CompleteRetrospective(ctx context.Context, ref, version, note, actor string) (*Hotfix, bool, error) {
	cur, err := s.GetHotfix(ctx, ref, version)
	if err != nil {
		return nil, false, err
	}
	if cur.RetroCompletedAt != nil {
		return cur, false, nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE pack_hotfixes SET retro_completed_at=NOW(), retro_completed_by=$2, retro_note=$3
		WHERE pack_version_id IN (SELECT v.pack_version_id FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
			WHERE p.pack_ref=$1 AND v.version=$4) AND retro_completed_at IS NULL`, ref, actor, note, version); err != nil {
		return nil, false, s.registryFail("CompleteRetrospective", err, nil)
	}
	h, err := s.GetHotfix(ctx, ref, version)
	return h, true, err
}

// RecordVerificationFailure appends a runtime verification failure (JUR-NEG-04);
// it feeds the promotion health gate and the operations metrics.
func (s *PgStore) RecordVerificationFailure(ctx context.Context, ref, version, ring, region string, reasons []string) error {
	if reasons == nil {
		reasons = []string{}
	}
	rawBytes, merr := json.Marshal(reasons)
	if merr != nil {
		return merr
	}
	raw := string(rawBytes)
	_, err := s.pool.Exec(ctx, `INSERT INTO pack_verification_failures (pack_version_id, ring_code, region_code, reasons)
		SELECT v.pack_version_id, $3, $4, $5::jsonb FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
		WHERE p.pack_ref=$1 AND v.version=$2`, ref, version, nullIfEmpty(ring), nullIfEmpty(region), raw)
	return s.registryFail("RecordVerificationFailure", err, nil)
}
