package store

import (
	"context"
	"fmt"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 s10 and s23: the store side of the cross-pack conflict check at release and of the supersede command.
// The decisions themselves are pure functions in domain/pack_consistency.go.

// packWithDoc reads a version's window, scope and verified-shape artifact content. Doc is nil when the version has no artifact yet or its
// artifact cannot be read: such a version cannot be served either, so it cannot take part in a conflict.
func (s *PgStore) packWithDoc(ctx context.Context, ref, version string) (domain.PackWithDoc, *domain.EligiblePack, error) {
	ep, err := s.FindPackVersion(ctx, ref, version)
	if err != nil {
		return domain.PackWithDoc{}, nil, err
	}
	return s.packWithDocFor(ctx, ep)
}

func (s *PgStore) packWithDocFor(ctx context.Context, ep *domain.EligiblePack) (domain.PackWithDoc, *domain.EligiblePack, error) {
	out := domain.PackWithDoc{Ref: ep.PackRef, Version: ep.Version, From: ep.EffectiveFrom, To: ep.EffectiveTo}
	lp, err := s.LoadPack(ctx, ep.PackVersionID)
	if err != nil {
		return out, ep, err
	}
	if lp.ArtifactJSON != "" {
		if doc, perr := domain.ParseArtifact([]byte(lp.ArtifactJSON)); perr == nil {
			out.Doc = doc
		}
	}
	return out, ep, nil
}

// crossPackReasons lists the release blockers caused by rules of OTHER released packs that this version would collide with (s10).
func (s *PgStore) crossPackReasons(ctx context.Context, ref, version string) ([]string, error) {
	self, _, err := s.packWithDoc(ctx, ref, version)
	if err != nil {
		return nil, err
	}
	if self.Doc == nil {
		return nil, nil
	}
	released, err := s.ListEligiblePacks(ctx, []string{"RELEASED"}, "", "")
	if err != nil {
		return nil, err
	}
	var others []domain.PackWithDoc
	for i := range released {
		if released[i].PackRef == ref {
			continue
		}
		o, _, err := s.packWithDocFor(ctx, &released[i])
		if err != nil {
			return nil, err
		}
		if o.Doc != nil {
			others = append(others, o)
		}
	}
	var reasons []string
	for _, c := range domain.CrossPackConflicts(self, others) {
		reasons = append(reasons, "cross_pack_conflict: "+c)
	}
	return reasons, nil
}

func (s *PgStore) activeDeployments(ctx context.Context, versionID string) ([]domain.RingRegion, error) {
	rows, err := s.pool.Query(ctx, `SELECT ring_code, region_code FROM pack_deployments WHERE pack_version_id::text=$1 AND status='ACTIVE'`, versionID)
	if err != nil {
		return nil, s.registryFail("activeDeployments", err, nil)
	}
	defer rows.Close()
	var out []domain.RingRegion
	for rows.Next() {
		var d domain.RingRegion
		if err := rows.Scan(&d.Ring, &d.Region); err != nil {
			return nil, s.registryFail("activeDeployments scan", err, nil)
		}
		out = append(out, d)
	}
	return out, s.registryFail("activeDeployments rows", rows.Err(), nil)
}

// SupersedePackVersion marks a RELEASED version SUPERSEDED by a newer RELEASED version of the same pack (s23: historical resolution stays
// permitted for pinned replay). A superseded version is no longer eligible for NEW decisions, so it is refused, with every reason at once,
// unless nothing it served is left without a servant: the newer version must cover its period, jurisdictions, rules and obligations, and be
// ACTIVE in every ring and region where the older one is. A repeat of an already superseded version changes nothing.
func (s *PgStore) SupersedePackVersion(ctx context.Context, ref, version, byVersion, reason, evidenceRef, actor string) (*ReleaseResult, error) {
	tx, err := s.begin(ctx, "SupersedePackVersion")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := s.lockVersion(ctx, tx, ref, version)
	if err != nil {
		return nil, err
	}
	res := &ReleaseResult{PackRef: ref, Version: version, FromStatus: v.status, ToStatus: "SUPERSEDED"}
	if v.status == "SUPERSEDED" {
		return res, nil
	}
	if v.status != "RELEASED" {
		return nil, fmt.Errorf("%w: only a RELEASED version can be superseded, this one is %s", errInvalidLifecycle, v.status)
	}
	old, oldEP, err := s.packWithDoc(ctx, ref, version)
	if err != nil {
		return nil, err
	}
	nw, newEP, err := s.packWithDoc(ctx, ref, byVersion)
	if err != nil {
		return nil, err
	}
	oldDeploys, err := s.activeDeployments(ctx, oldEP.PackVersionID)
	if err != nil {
		return nil, err
	}
	newDeploys, err := s.activeDeployments(ctx, newEP.PackVersionID)
	if err != nil {
		return nil, err
	}
	reasons := domain.SupersessionBlockers(domain.SupersessionInput{Old: old, New: nw, OldStatus: oldEP.Status, NewStatus: newEP.Status,
		OldScope: oldEP.ScopeCodes, NewScope: newEP.ScopeCodes, OldDeploys: oldDeploys, NewDeploys: newDeploys})
	if old.Doc == nil || nw.Doc == nil {
		reasons = append(reasons, "artifact_unavailable: both versions need a compiled artifact so that what is carried forward can be checked")
	}
	if len(reasons) > 0 {
		return nil, &OpsBlocked{Reasons: reasons}
	}
	if err := s.transition(ctx, tx, v, "SUPERSEDE", "SUPERSEDED", actor, "superseded by "+byVersion+": "+reason, evidenceRef); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("SupersedePackVersion commit", err, nil)
	}
	res.Changed = true
	return res, nil
}
