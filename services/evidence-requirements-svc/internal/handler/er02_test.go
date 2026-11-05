package handler_test

// ER-2: an artifact counts only when its owner confirms it exists in the
// caller's tenant and legal entity. Review focus #1 (SATISFIED from evidence
// that was never proven) and #3 (an owner outage written down as a fact).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcenvelope "zoiko.io/evidence-requirements-svc/internal/envelope"
	"zoiko.io/evidence-requirements-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
)

const (
	manifestA = "aaaaaaaa-0000-4000-8000-000000000001"
	envelopeA = "bbbbbbbb-0000-4000-8000-000000000001"
	documentA = "cccccccc-0000-4000-8000-000000000001"
)

// recordingMetrics is a handler.Recorder that remembers what it was told.
type recordingMetrics struct {
	outcomes []string
	owners   []string
}

func (m *recordingMetrics) EvaluationRecorded(o string) { m.outcomes = append(m.outcomes, o) }
func (m *recordingMetrics) OwnerUnavailable(o string)   { m.owners = append(m.owners, o) }

// er02Router is completionRouter with a metrics recorder.
func er02Router(st handler.Store, docs handler.DocumentVaultClient, manifests handler.ManifestClient,
	esignatures handler.EsignatureClient, m handler.Recorder) http.Handler {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	h := handler.New(st, &recordingAuthz{}, docs, zap.NewNop()).WithRegistry(stubRegistry{}).
		WithManifestClient(manifests).WithEsignatureClient(esignatures).WithMetrics(m)
	handler.RegisterRoutes(r, h)
	return r
}

func unmetReasons(resp domain.EvaluateResponse) string {
	var b strings.Builder
	for _, u := range resp.Unmet {
		b.WriteString(u.Reason)
		b.WriteString("\n")
	}
	return b.String()
}

func TestER02_ManifestOutsideTenantDoesNotCount(t *testing.T) {
	st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement(domain.EvidenceTypeEvidenceManifest, domain.RequirementSpec{MinimumCount: 1}),
	}}
	manifests := &stubManifest{err: domain.ErrManifestMismatch}
	h := er02Router(st, &stubDocs{}, manifests, &stubEsignature{}, &recordingMetrics{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeEvidenceManifest, ReferenceID: manifestA},
	), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeMissing, resp.Outcome, "a manifest of another tenant or entity must not satisfy the requirement")
	assert.Contains(t, unmetReasons(resp), "different tenant or legal entity")
	assert.Equal(t, 1, manifests.calls)
}

func TestER02_ManifestUnknownIsUnmet(t *testing.T) {
	st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement(domain.EvidenceTypeEvidenceManifest, domain.RequirementSpec{MinimumCount: 1}),
	}}
	h := er02Router(st, &stubDocs{}, &stubManifest{err: domain.ErrManifestNotFound}, &stubEsignature{}, &recordingMetrics{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeEvidenceManifest, ReferenceID: manifestA},
	), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeMissing, resp.Outcome)
	assert.Contains(t, unmetReasons(resp), "does not exist in evidence-manifest-svc")
}

func TestER02_ManifestServiceDownIs503NoRow(t *testing.T) {
	st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement(domain.EvidenceTypeEvidenceManifest, domain.RequirementSpec{MinimumCount: 1}),
	}}
	m := &recordingMetrics{}
	h := er02Router(st, &stubDocs{}, &stubManifest{err: domain.ErrManifestServiceUnavailable}, &stubEsignature{}, m)

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeEvidenceManifest, ReferenceID: manifestA},
	), testTenant, testPrincipal)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "evidence_manifest_unavailable", problemOf(t, rec)["code"])
	assert.Empty(t, st.recordedEvals, "an owner outage must not be written into the ledger as a determination")
	assert.Equal(t, []string{"evidence-manifest-svc"}, m.owners)
	assert.Empty(t, m.outcomes)
}

func TestER02_SignatureUnknownIsUnmet(t *testing.T) {
	st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement(domain.EvidenceTypeSignature, domain.RequirementSpec{MinimumCount: 1}),
	}}
	esig := &stubEsignature{err: domain.ErrEnvelopeNotFound}
	h := er02Router(st, &stubDocs{}, &stubManifest{}, esig, &recordingMetrics{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeSignature, ReferenceID: envelopeA},
	), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeMissing, resp.Outcome, "a signature envelope that does not exist must not count")
	assert.Contains(t, unmetReasons(resp), "does not exist in esignature-integration-svc")
	assert.Equal(t, 1, esig.calls)
}

func TestER02_SignatureServiceDownIs503NoRow(t *testing.T) {
	st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement(domain.EvidenceTypeSignature, domain.RequirementSpec{MinimumCount: 1}),
	}}
	m := &recordingMetrics{}
	h := er02Router(st, &stubDocs{}, &stubManifest{}, &stubEsignature{err: domain.ErrEnvelopeServiceUnavailable}, m)

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeSignature, ReferenceID: envelopeA},
	), testTenant, testPrincipal)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "esignature_unavailable", problemOf(t, rec)["code"])
	assert.Empty(t, st.recordedEvals)
	assert.Equal(t, []string{"esignature-integration-svc"}, m.owners)
}

// One manifest offered twice — in different case and with whitespace —
// against minimum_count: 2 is one artifact, and the owner is asked once.
func TestER02_SameManifestTwiceCountsOnce(t *testing.T) {
	st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement(domain.EvidenceTypeEvidenceManifest, domain.RequirementSpec{MinimumCount: 2}),
	}}
	manifests := &stubManifest{}
	h := er02Router(st, &stubDocs{}, manifests, &stubEsignature{}, &recordingMetrics{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeEvidenceManifest, ReferenceID: manifestA},
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeEvidenceManifest, ReferenceID: " " + strings.ToUpper(manifestA) + " "},
	), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeMissing, resp.Outcome, "one manifest named twice must not satisfy minimum_count: 2")
	assert.Contains(t, unmetReasons(resp), "requires 2 matching artifact(s), 1 present")
	assert.Equal(t, 1, manifests.calls)
}

// With no client configured for an owner (EVIDENCE_MANIFEST_SERVICE_URL or
// ESIGNATURE_SERVICE_URL unset, local only), a reference of that type is
// verified as unavailable: 503 and nothing recorded. It never counts on the
// caller's word.
func TestER02_UnconfiguredOwnerNeverCounts(t *testing.T) {
	for _, tc := range []struct {
		evidenceType, ref, code, owner string
	}{
		{domain.EvidenceTypeEvidenceManifest, manifestA, "evidence_manifest_unavailable", "evidence-manifest-svc"},
		{domain.EvidenceTypeSignature, envelopeA, "esignature_unavailable", "esignature-integration-svc"},
	} {
		t.Run(tc.evidenceType, func(t *testing.T) {
			st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
				requirement(tc.evidenceType, domain.RequirementSpec{MinimumCount: 1}),
			}}
			m := &recordingMetrics{}
			h := er02Router(st, &stubDocs{}, nil, nil, m)

			rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
				domain.PresentArtifact{EvidenceType: tc.evidenceType, ReferenceID: tc.ref},
			), testTenant, testPrincipal)
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, problemOf(t, rec)["code"])
			assert.Empty(t, st.recordedEvals)
			assert.Equal(t, []string{tc.owner}, m.owners)
		})
	}
}

// Review focus #1, fourth case: a document of another legal entity.
func TestER02_DocumentOfAnotherEntityDoesNotCount(t *testing.T) {
	st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement(domain.EvidenceTypeSupportingDocument, domain.RequirementSpec{MinimumCount: 1}),
	}}
	h := er02Router(st, &stubDocs{err: domain.ErrDocumentMismatch}, &stubManifest{}, &stubEsignature{}, &recordingMetrics{})

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeSupportingDocument, ReferenceID: documentA},
	), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp := decodeEvaluate(t, rec)
	assert.Equal(t, domain.OutcomeMissing, resp.Outcome)
	assert.Contains(t, unmetReasons(resp), "different tenant or legal entity")
}

// Review focus #1, fifth case: an empty reference never counts, for any
// evidence type, and no owner is asked about it.
func TestER02_EmptyReferenceNeverCounts(t *testing.T) {
	for _, et := range []string{domain.EvidenceTypeSupportingDocument, domain.EvidenceTypeEvidenceManifest,
		domain.EvidenceTypeSignature, "APPROVAL_RECORD"} {
		t.Run(et, func(t *testing.T) {
			st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
				requirement(et, domain.RequirementSpec{MinimumCount: 1}),
			}}
			docs, manifests, esig := &stubDocs{}, &stubManifest{}, &stubEsignature{}
			h := er02Router(st, docs, manifests, esig, &recordingMetrics{})

			rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
				domain.PresentArtifact{EvidenceType: et, ReferenceID: "   "},
			), testTenant, testPrincipal)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, domain.OutcomeMissing, decodeEvaluate(t, rec).Outcome)
			assert.Zero(t, docs.calls+manifests.calls+esig.calls)
		})
	}
}

// Every recorded determination is counted once by outcome; a replay is not.
func TestER05_EvaluationsAreCountedByOutcome(t *testing.T) {
	m := &recordingMetrics{}
	st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement(domain.EvidenceTypeEvidenceManifest, domain.RequirementSpec{MinimumCount: 1}),
	}}
	h := er02Router(st, &stubDocs{}, &stubManifest{}, &stubEsignature{}, m)
	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeEvidenceManifest, ReferenceID: manifestA},
	), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"SATISFIED"}, m.outcomes)

	st.evalCreated = false
	st.evaluation = &st.recordedEvals[0]
	rec = do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeEvidenceManifest, ReferenceID: manifestA},
	), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"SATISFIED"}, m.outcomes, "a replay is not a new determination")
}
