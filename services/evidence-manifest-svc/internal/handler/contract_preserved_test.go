package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/evidence-manifest-svc/internal/domain"
)

// The freeze quarantine fix moved where the mismatch error is raised (after
// commit); the HTTP contract must be unchanged: 422 control_total_mismatch
// carrying the QUARANTINED population.
func TestFreezePopulation_Mismatch_Keeps422ContractAndQuarantinedBody(t *testing.T) {
	st := newStubStore()
	st.population = &domain.AuditPopulation{PopulationID: "pop-1", LegalEntityID: "e1", Status: domain.PopulationQuarantined}
	st.popErr = domain.ErrPopulationControlTotalMismatch
	r, _ := principalRouterWith(t, st, &recordingAuthz{})

	req := httptest.NewRequest(http.MethodPost, "/v1/audit-populations/pop-1/freeze", nil)
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-1")
	req.Header.Set("X-Correlation-ID", "corr-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	var body struct {
		Error      string                 `json:"error"`
		Population domain.AuditPopulation `json:"population"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "control_total_mismatch", body.Error)
	assert.Equal(t, domain.PopulationQuarantined, body.Population.Status)
}

// A store failure while persisting a generated manifest is a 503, never a 201
// for something that was not stored.
func TestGenerateManifest_PersistFailure_Is503_NotCreated(t *testing.T) {
	st := newStubStore()
	st.createErr = errors.New("store down")
	r, _ := principalRouterWith(t, st, &recordingAuthz{})
	w := postGenerate(r, generateBody(t, "t1"), map[string]string{"X-Principal-Id": "principal-1"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, st.manifests)
	assert.Empty(t, st.outboxEvents)
}

func TestManifestChecksum_DependsOnOrderAndContent(t *testing.T) {
	a := domain.ManifestRecord{SourceType: domain.SourceGovernanceDecision, SourceRecordID: "1", RecordSnapshot: []byte(`{"a":1}`)}
	b := domain.ManifestRecord{SourceType: domain.SourceAccessDecision, SourceRecordID: "2", RecordSnapshot: []byte(`{"b":2}`)}
	ab := domain.ManifestChecksum([]domain.ManifestRecord{a, b})
	assert.Equal(t, ab, domain.ManifestChecksum([]domain.ManifestRecord{a, b}))
	assert.NotEqual(t, ab, domain.ManifestChecksum([]domain.ManifestRecord{b, a}))
	assert.NotEqual(t, ab, domain.ManifestChecksum([]domain.ManifestRecord{a}))
	assert.Len(t, ab, 64)
}
