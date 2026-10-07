package handler_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
)

func TestEvidenceExport_UsesItsOwnActionAndCarriesAVerifiableManifest(t *testing.T) {
	ok := true
	s := &fakeStore{run: sampleRun(), def: &domain.ControlDefinition{ControlDefinitionID: "d1", ControlCode: "FIN-CTRL-001"},
		w1: wave1State{evidence: &domain.EvidencePackage{RunID: "r1", Digest: "sha256:x", Verified: &ok}}}
	a := &fakeAuthz{}
	rr := do(newServer(s, a), "GET", "/controls/v1/runs/r1/evidence-export", "", auth)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Equal(t, []string{"FINCTRL_EVIDENCE_EXPORT"}, a.actions, "an export leaves the platform: it has its own action")
	assert.Equal(t, []string{"e1"}, a.entity)
	assert.Contains(t, rr.Header().Get("Content-Disposition"), "control-run-r1")

	var b struct {
		Manifest struct {
			Format           string `json:"format"`
			Digest           string `json:"digest"`
			EvidenceVerified bool   `json:"evidence_integrity_verified"`
			ExportedBy       string `json:"exported_by"`
		} `json:"manifest"`
		Exceptions []any `json:"exceptions"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	assert.Equal(t, "zoiko.financial-control.run-evidence/v1", b.Manifest.Format)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, b.Manifest.Digest)
	assert.True(t, b.Manifest.EvidenceVerified)
	assert.Equal(t, "alice", b.Manifest.ExportedBy)
	assert.NotNil(t, b.Exceptions)
}

func TestEvidenceExport_DeniedNeverReadsTheRun(t *testing.T) {
	s := &fakeStore{run: sampleRun(), def: &domain.ControlDefinition{}}
	rr := do(newServer(s, &fakeAuthz{err: domain.ErrAuthorizationDenied}), "GET", "/controls/v1/runs/r1/evidence-export", "", auth)
	assert.Equal(t, 403, rr.Code)
	assert.NotContains(t, s.calls, "GetDefinition")
}

func TestMonitoring_ValidatesAndUsesReadAction(t *testing.T) {
	s := &fakeStore{}
	a := &fakeAuthz{}
	h := newServer(s, a)
	assert.Equal(t, 400, do(h, "GET", "/controls/v1/monitoring/metrics", "", auth).Code)
	assert.Equal(t, 400, do(h, "GET", "/controls/v1/monitoring/metrics?legal_entity_id=nope", "", auth).Code)
	rr := do(h, "GET", "/controls/v1/monitoring/metrics?legal_entity_id="+entityUUID, "", auth)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Equal(t, []string{"FINCTRL_READ"}, a.actions)
	assert.Contains(t, s.calls, "Monitoring")
	var m map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &m))
	assert.Contains(t, m, "unavailable")
	assert.Contains(t, m, "rates")
}
