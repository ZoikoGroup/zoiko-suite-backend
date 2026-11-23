package store

// Console summary (tracker JR-FE-1): the counts behind the /admin/jurisdictions
// KPI row, the pack pipeline and the attention callout. Every number is a live
// count of platform reference data; none is invented and none reads tenant data.

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// JurisdictionSummary is the console summary.
type JurisdictionSummary struct {
	GeneratedAt time.Time        `json:"generated_at"`
	KPIs        SummaryKPIs      `json:"kpis"`
	Pipeline    SummaryPipeline  `json:"pack_pipeline"`
	Attention   SummaryAttention `json:"attention"`
}

// SummaryKPIs is the KPI row.
type SummaryKPIs struct {
	ActiveJurisdictions int `json:"active_jurisdictions"`
	RulesDraft          int `json:"rules_draft"`
	RulesUnderReview    int `json:"rules_under_review"`
	PacksReleased       int `json:"pack_versions_released"`
	RolloutsInProgress  int `json:"rollouts_in_progress"`
	OpenDriftAndCases   int `json:"open_drift_and_remediation"`
}

// SummaryPipeline counts pack versions by stage. Signed counts versions with a
// signed artifact still before release; Deployed counts released versions with
// an active deployment.
type SummaryPipeline struct {
	Draft     int `json:"draft"`
	Review    int `json:"review"`
	Certified int `json:"certified"`
	Signed    int `json:"signed"`
	Released  int `json:"released"`
	Deployed  int `json:"deployed"`
	Withdrawn int `json:"withdrawn"`
	Blocked   int `json:"emergency_blocked"`
}

// SummaryAttention is what needs a person.
type SummaryAttention struct {
	DriftedRules            int `json:"drifted_rules"`
	OpenSourceNotices       int `json:"open_source_change_notices"`
	OpenRemediationCases    int `json:"open_remediation_cases"`
	ProposedImports         int `json:"proposed_imports"`
	ParkedOutboxEvents      int `json:"parked_outbox_events"`
	HotfixRetrosOutstanding int `json:"hotfix_retrospectives_outstanding"`
}

// JurisdictionSummary reads every count in one statement (one snapshot).
func (s *PgStore) JurisdictionSummary(ctx context.Context) (*JurisdictionSummary, error) {
	var x JurisdictionSummary
	err := s.db(ctx).QueryRow(ctx, `
		SELECT NOW(),
		  (SELECT count(*) FROM jurisdictions WHERE active_flag AND (effective_to IS NULL OR effective_to > NOW())),
		  (SELECT count(*) FROM jurisdiction_rules WHERE rule_status = 'DRAFT'),
		  (SELECT count(*) FROM jurisdiction_rules WHERE rule_status = 'ACTIVE' AND legal_drift_state = 'UNDER_REVIEW'),
		  (SELECT count(*) FROM jurisdiction_pack_versions WHERE status = 'RELEASED'),
		  (SELECT count(*) FROM jurisdiction_rollouts WHERE status IN ('AUTHORING', 'READY')),
		  (SELECT count(*) FROM jurisdiction_rules WHERE rule_status = 'ACTIVE' AND legal_drift_state IN ('DRIFTED', 'UNDER_REVIEW')),
		  (SELECT count(*) FROM remediation_cases WHERE status = 'OPEN'),
		  (SELECT count(*) FROM jurisdiction_pack_versions WHERE status = 'DRAFT'),
		  (SELECT count(*) FROM jurisdiction_pack_versions WHERE status = 'REVIEW'),
		  (SELECT count(*) FROM jurisdiction_pack_versions WHERE status = 'CERTIFIED'),
		  (SELECT count(*) FROM jurisdiction_pack_versions v JOIN pack_artifacts a USING (pack_version_id)
		    WHERE a.signature IS NOT NULL AND v.status IN ('REVIEW', 'CERTIFIED')),
		  (SELECT count(DISTINCT v.pack_version_id) FROM jurisdiction_pack_versions v JOIN pack_deployments d USING (pack_version_id)
		    WHERE v.status = 'RELEASED' AND d.status = 'ACTIVE'),
		  (SELECT count(*) FROM jurisdiction_pack_versions WHERE status = 'WITHDRAWN'),
		  (SELECT count(*) FROM jurisdiction_pack_versions WHERE status = 'EMERGENCY_BLOCKED'),
		  (SELECT count(*) FROM source_change_notices WHERE status <> 'CLOSED'),
		  (SELECT count(*) FROM jurisdiction_import_batches WHERE status = 'PROPOSED'),
		  (SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND publish_attempts >= $1),
		  (SELECT count(*) FROM pack_hotfixes WHERE retro_completed_at IS NULL)`, MaxOutboxAttempts).Scan(
		&x.GeneratedAt,
		&x.KPIs.ActiveJurisdictions, &x.KPIs.RulesDraft, &x.KPIs.RulesUnderReview, &x.KPIs.PacksReleased, &x.KPIs.RolloutsInProgress,
		&x.Attention.DriftedRules, &x.Attention.OpenRemediationCases,
		&x.Pipeline.Draft, &x.Pipeline.Review, &x.Pipeline.Certified, &x.Pipeline.Signed, &x.Pipeline.Deployed,
		&x.Pipeline.Withdrawn, &x.Pipeline.Blocked,
		&x.Attention.OpenSourceNotices, &x.Attention.ProposedImports, &x.Attention.ParkedOutboxEvents, &x.Attention.HotfixRetrosOutstanding)
	if err != nil {
		return nil, fmt.Errorf("%w: summary: %v", domain.ErrStoreUnavailable, err)
	}
	x.Pipeline.Released = x.KPIs.PacksReleased
	x.KPIs.OpenDriftAndCases = x.Attention.DriftedRules + x.Attention.OpenRemediationCases
	return &x, nil
}
