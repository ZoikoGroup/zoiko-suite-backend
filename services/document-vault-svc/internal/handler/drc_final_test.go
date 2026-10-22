package handler_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/domain"
)

// DRC-004 naming, row_version/document_state on the wire, effective window and
// as_of on the retention routes.

func TestDRC004_TenantMismatchInBody_NamesTheCode(t *testing.T) {
	st := newCounting(t)
	r := fullRouter(newStubStore(), st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	body, err := json.Marshal(domain.CreateDocumentRequest{
		TenantID: "some-other-tenant", LegalEntityID: "entity-1", Title: "Contract",
		Classification: domain.ClassificationConfidential, ContentType: "text/plain",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("x"))})
	require.NoError(t, err)
	w := call(r, http.MethodPost, "/v1/documents", string(body), nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	code, detail := errorCode(t, w)
	assert.Equal(t, "tenant_mismatch", code)
	assert.Contains(t, detail, "DRC-004: TENANT_SCOPE_MISMATCH")
	assert.Zero(t, st.puts, "nothing is stored for a cross-tenant request")
}

func TestGetDocument_ExposesRowVersionAndDocumentState(t *testing.T) {
	s := newStubStore()
	s.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-1", CurrentVersion: 1, Status: domain.StatusActive, DocumentState: "ACTIVE", RowVersion: 7}
	r := fullRouter(s, newCounting(t), &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	w := call(r, http.MethodGet, "/v1/documents/doc-1", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	assert.EqualValues(t, 7, m["row_version"])
	assert.Equal(t, "ACTIVE", m["document_state"])
}

func TestEvaluate_PassesAsOfAndActor(t *testing.T) {
	st := &retStub{entities: map[string]string{"rec-1": "ent-A"}}
	az := &entityActionAuthz{}
	r := retRouter(t, az, st, true)
	when := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	w := call(r, http.MethodPost, "/v1/retention:evaluate", `{"record_ids":["rec-1"],"as_of":"`+when.Format(time.RFC3339)+`"}`, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, st.evalAsOf, 1)
	assert.True(t, st.evalAsOf[0].Equal(when))
	assert.Equal(t, []string{testPrincipal}, st.evalActors)

	w = call(r, http.MethodPost, "/v1/retention:evaluate", `{"record_ids":["rec-1"]}`, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.WithinDuration(t, time.Now(), st.evalAsOf[1], time.Minute, "no as_of means now")
}

func TestGetDocument_CurrentVersionIDAndClassificationID(t *testing.T) {
	s := newStubStore()
	s.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-1", CurrentVersion: 2, Status: domain.StatusActive}
	s.versions["doc-1"] = []domain.DocumentVersion{
		{DocumentVersionID: "ver-1", DocumentID: "doc-1", Version: 1},
		{DocumentVersionID: "ver-2", DocumentID: "doc-1", Version: 2},
	}
	r := fullRouter(s, newCounting(t), &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	get := func() map[string]any {
		w := call(r, http.MethodGet, "/v1/documents/doc-1", "", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var m map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
		return m
	}

	m := get()
	assert.Equal(t, "ver-2", m["current_version_id"], "the version whose number is current_version, not the first or the latest by id")
	assert.NotContains(t, m, "classification_id", "no classification at all")

	s.classifications = append(s.classifications, &domain.RecordClassification{
		ClassificationID: "cls-1", DocumentID: "doc-1", Status: domain.ClassificationStatusCandidate, ProposedAt: time.Now()})
	assert.NotContains(t, get(), "classification_id", "a CANDIDATE is not an approved classification")

	s.classifications[0].Status = domain.ClassificationStatusConfirmed
	assert.Equal(t, "cls-1", get()["classification_id"])

	s.docs["doc-1"].CurrentVersion = 1
	assert.Equal(t, "ver-1", get()["current_version_id"], "follows the document's current version")
}

func TestDeclareRecord_RequiresBusinessContextAndReason_DRC005(t *testing.T) {
	r, rec := recordsRouter(t, &scopeAuthz{}, &stubResidency{}, nil)
	base := `"document_id":"doc-1","document_version_id":"ver-1","record_class":"LEGAL_CONTRACT","jurisdiction_scope":"US-DE"`

	w := call(r, http.MethodPost, "/v1/records:declare", `{`+base+`,"declaration_reason":"executed"}`, nil)
	require.Equal(t, http.StatusBadRequest, w.Code)
	_, detail := errorCode(t, w)
	assert.Equal(t, "DRC-005: RECORD_DECLARATION_INCOMPLETE: business_context", detail)

	w = call(r, http.MethodPost, "/v1/records", `{`+base+`,"business_context":"   ","declaration_reason":"executed"}`, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, "whitespace is not a business context")

	w = call(r, http.MethodPost, "/v1/records:declare", `{`+base+`,"business_context":"msa"}`, nil)
	require.Equal(t, http.StatusBadRequest, w.Code)
	_, detail = errorCode(t, w)
	assert.Equal(t, "DRC-005: RECORD_DECLARATION_INCOMPLETE: declaration_reason", detail)
	assert.Empty(t, rec.declared, "an incomplete declaration is refused, never defaulted")

	w = call(r, http.MethodPost, "/v1/records:declare", `{`+base+`,"business_context":"msa","declaration_reason":"executed"}`, nil)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}
