package context_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
)

// DoD gate 6: "Historical version/as-of reconstruction demonstrated", and the
// ExplainContextResolution query GOV-01's contract names.
//
// The property under test throughout is that the answer is RECONSTRUCTED from
// the recorded decision, never RE-DERIVED. Re-deriving would read today's risk
// signals, role assignments and entity state against a session issued last
// week, and would confidently report a decision that was never made.

func issuedSession() *domain.SessionContext {
	issued := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	return &domain.SessionContext{
		SessionContextID:      "sc-1",
		PrincipalID:           "p-1",
		TenantID:              "tenant-a",
		LegalEntityID:         "entity-7",
		CorrelationID:         "corr-1",
		TrustPosture:          domain.TrustPostureMFAVerified,
		MFAVerified:           true,
		AdaptiveRiskScore:     12,
		RiskSignalSource:      "INTELLIGENCE_PLANE",
		EnvelopeJWTJTI:        "jti-1",
		IssuedAt:              issued,
		ExpiresAt:             issued.Add(5 * time.Minute),
		DataResidencyPolicyID: "residency-eu",
		IngressSource:         "tenant-a.zoiko.io",
		Environment:           domain.EnvironmentProduction,
		EvidenceID:            "ev-1",
		RetentionClass:        domain.RetentionClassSessionEvidence,
	}
}

func TestExplainReportsEverySixDimension(t *testing.T) {
	sc := issuedSession()
	ex := identityctx.Explain(sc, sc.IssuedAt.Add(time.Minute))

	require.Len(t, ex.Dimensions, 6, "all six dimensions must be accounted for")
	for i, d := range ex.Dimensions {
		assert.Equal(t, i+1, d.Dimension)
		assert.NotEmpty(t, d.Name)
		assert.NotEmpty(t, d.Source, "an auditor asking 'who asserted this' needs the source named")
	}
}

func TestExplainCarriesTheDecisionsIdentifiers(t *testing.T) {
	sc := issuedSession()
	ex := identityctx.Explain(sc, sc.IssuedAt)

	assert.Equal(t, "sc-1", ex.SessionContextID)
	assert.Equal(t, "ev-1", ex.EvidenceID)
	assert.Equal(t, "tenant-a.zoiko.io", ex.IngressSource,
		"ingress is part of TenantContextDecision and had no column before")
	assert.Equal(t, domain.EnvironmentProduction, ex.Environment)
	assert.Contains(t, ex.ReconstructedFrom, "append-only",
		"the answer must say it came from the record, not from a live re-derivation")
}

// ── as-of ────────────────────────────────────────────────────────────────────

func TestExplainAsOfBeforeExpiryReportsResolved(t *testing.T) {
	sc := issuedSession()
	ex := identityctx.Explain(sc, sc.IssuedAt.Add(time.Minute))

	assert.Equal(t, "RESOLVED", ex.Outcome)
	assert.Equal(t, sc.IssuedAt.Add(time.Minute), ex.AsOf)
}

func TestExplainAsOfAfterExpiryReportsExpired(t *testing.T) {
	sc := issuedSession()
	ex := identityctx.Explain(sc, sc.ExpiresAt.Add(time.Second))

	assert.Equal(t, "EXPIRED", ex.Outcome)
}

// TestExplainAsOfBeforeInvalidationStillReportsResolved is the point of the
// whole feature.
//
// "Was this session live at 14:05?" must be answerable with the state as it
// stood at 14:05, not with the state now. A session invalidated at 14:30 WAS
// live at 14:05, and an explain that reported INVALIDATED for every as_of
// would make the parameter decorative.
func TestExplainAsOfBeforeInvalidationStillReportsResolved(t *testing.T) {
	sc := issuedSession()
	invalidatedAt := sc.IssuedAt.Add(2 * time.Minute)
	reason := domain.InvalidationReasonAdminRevoke
	sc.InvalidatedAt = &invalidatedAt
	sc.InvalidationReason = &reason

	// One minute in: live.
	before := identityctx.Explain(sc, sc.IssuedAt.Add(time.Minute))
	assert.Equal(t, "RESOLVED", before.Outcome,
		"a session later revoked was still live before the revocation")

	// Three minutes in: revoked.
	after := identityctx.Explain(sc, sc.IssuedAt.Add(3*time.Minute))
	assert.Equal(t, "INVALIDATED", after.Outcome)
	require.NotNil(t, after.InvalidationReason)
	assert.Equal(t, string(domain.InvalidationReasonAdminRevoke), *after.InvalidationReason)
}

func TestExplainAtTheInvalidationInstantReportsInvalidated(t *testing.T) {
	sc := issuedSession()
	invalidatedAt := sc.IssuedAt.Add(2 * time.Minute)
	reason := domain.InvalidationReasonLogout
	sc.InvalidatedAt = &invalidatedAt
	sc.InvalidationReason = &reason

	// The boundary is inclusive: a session is revoked AT its invalidation
	// instant, not one nanosecond later.
	ex := identityctx.Explain(sc, invalidatedAt)
	assert.Equal(t, "INVALIDATED", ex.Outcome)
}

// ── Disposition ──────────────────────────────────────────────────────────────

// TestExplainSaysSoWhenTheEvidenceWasDisposed.
//
// The skeleton of the decision survives disposition; the personal data does
// not. Presenting redacted fields as though they were the original values
// would be worse than saying nothing — an auditor would read 'DISPOSED' as a
// real ingress hostname.
func TestExplainSaysSoWhenTheEvidenceWasDisposed(t *testing.T) {
	sc := issuedSession()
	disposedAt := sc.IssuedAt.Add(400 * 24 * time.Hour)
	sc.DisposedAt = &disposedAt

	ex := identityctx.Explain(sc, disposedAt.Add(time.Hour))
	assert.Equal(t, "RESOLVED_EVIDENCE_DISPOSED", ex.Outcome)
}

func TestExplainReportsSupportContextWhenOneApplied(t *testing.T) {
	sc := issuedSession()
	supportID := "sup-1"
	sc.SupportContextID = &supportID

	ex := identityctx.Explain(sc, sc.IssuedAt)

	require.NotNil(t, ex.SupportContextID)
	assert.Equal(t, "sup-1", *ex.SupportContextID,
		"every action taken during support must be attributable to the grant that allowed it")
}

func TestExplainNamesTheResidencyPolicyThatGoverned(t *testing.T) {
	sc := issuedSession()
	ex := identityctx.Explain(sc, sc.IssuedAt)

	assert.Contains(t, ex.Dimensions[2].Detail, "residency-eu")
}

func TestExplainHandlesAMissingResidencyPolicyReadably(t *testing.T) {
	sc := issuedSession()
	sc.DataResidencyPolicyID = ""

	ex := identityctx.Explain(sc, sc.IssuedAt)
	assert.Contains(t, ex.Dimensions[2].Detail, "none recorded",
		"an absent value must read as absent, not as an empty string an auditor squints at")
}

func TestExplainReportsTheRiskSourceNotJustTheScore(t *testing.T) {
	sc := issuedSession()
	sc.RiskSignalSource = "UNAVAILABLE"
	sc.AdaptiveRiskScore = 0

	ex := identityctx.Explain(sc, sc.IssuedAt)

	// A session resolved with no risk signal is one whose posture was
	// DEFAULTED rather than measured, and the two must be distinguishable long
	// after the fact.
	assert.Equal(t, "UNAVAILABLE", ex.Dimensions[5].Source)
	assert.Contains(t, ex.Dimensions[5].Detail, "adaptive_risk_score=0")
}
