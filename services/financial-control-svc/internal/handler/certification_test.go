package handler_test

// SPEC_DEVIATIONS A2/B7: certification records its authority, and the
// evidence read serves the ControlCertification directly.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/handler"
)

// Certify authorizes live (Decide, not the cached CheckAllowed) and hands that
// decision to the store as the certification's authority snapshot.
func TestCertify_RecordsTheLiveAuthorizationDecision(t *testing.T) {
	s := &fakeStore{run: sampleRun()}
	a := &fakeAuthz{}
	rr := do(newServer(s, a), "POST", "/controls/v1/runs/r1/certification", `{"decision":"CERTIFY"}`,
		with(map[string]string{"If-Match": `"3"`, "Idempotency-Key": "k-1"}))
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Equal(t, []string{handler.ActionCertify}, a.decided, "the certify check is live")
	require.NotNil(t, s.w5.certAuth)
	assert.Equal(t, "decision-1", s.w5.certAuth.AccessDecisionID)
	assert.Equal(t, "e1", s.w5.certAuth.LegalEntityID)
	assert.Equal(t, "FINCTRL_CERTIFY", s.w5.certAuth.ActionType)
}

func TestGetEvidence_IncludesTheCertification(t *testing.T) {
	ok := true
	s := &fakeStore{run: sampleRun()}
	s.w1.evidence = &domain.EvidencePackage{PackageID: "p1", RunID: "r1", Content: json.RawMessage(`{}`),
		Digest: "sha256:00", Verified: &ok}
	h := newServer(s, &fakeAuthz{})

	// Not certified: the package alone, no certification field.
	rr := do(h, "GET", "/controls/v1/runs/r1/evidence", "", auth)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "p1", body["package_id"], "the package fields are unchanged")
	assert.NotContains(t, body, "certification")

	s.w5.cert = &domain.ControlCertification{CertificationID: "c1", RunID: "r1", Certifier: "checker",
		Authority: domain.AuthoritySnapshot{AccessDecisionID: "decision-1"}, Result: domain.ResultPass,
		UnresolvedExceptions: []domain.CertifiedException{}, CertifiedAt: time.Now(), EvidencePackageID: "p1"}
	rr = do(h, "GET", "/controls/v1/runs/r1/evidence", "", auth)
	require.Equal(t, 200, rr.Code)
	body = nil
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "p1", body["package_id"])
	cert, okc := body["certification"].(map[string]any)
	require.True(t, okc, "certification is served")
	assert.Equal(t, "checker", cert["certifier"])
	assert.Equal(t, "decision-1", cert["authority_snapshot"].(map[string]any)["access_decision_id"])
}

// The export carries the certification as a direct read, inside the digest.
func TestEvidenceExport_IncludesTheCertification(t *testing.T) {
	ok := true
	s := &fakeStore{run: sampleRun(), def: &domain.ControlDefinition{ControlDefinitionID: "d1"},
		w1: wave1State{evidence: &domain.EvidencePackage{RunID: "r1", Digest: "sha256:x", Verified: &ok}}}
	s.w5.cert = &domain.ControlCertification{CertificationID: "c1", RunID: "r1", Certifier: "checker",
		Authority: domain.AuthoritySnapshot{AccessDecisionID: "decision-1"}, Result: domain.ResultPass}
	rr := do(newServer(s, &fakeAuthz{}), "GET", "/controls/v1/runs/r1/evidence-export", "", auth)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var b struct {
		Certification *domain.ControlCertification `json:"certification"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	require.NotNil(t, b.Certification)
	assert.Equal(t, "checker", b.Certification.Certifier)
	assert.Equal(t, "decision-1", b.Certification.Authority.AccessDecisionID)

	// An uncertified run exports certification: null.
	s.w5.cert = nil
	rr = do(newServer(s, &fakeAuthz{}), "GET", "/controls/v1/runs/r1/evidence-export", "", auth)
	require.Equal(t, 200, rr.Code)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	assert.Equal(t, "null", string(raw["certification"]))
}
