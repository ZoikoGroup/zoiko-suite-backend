package handler_test

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/domain"
)

// §11.1 defines the failure CODES and the required behaviour; it defines no
// response structure, so the service-wide {error, detail} body is unchanged and the
// canonical code appears in `detail`, as DRC-002, DRC-008 and DRC-011 already did.
// These pin the two codes that previously were missing from that position.

func TestDRC003_QuarantinedUpload_NamesTheCode_OnBothDirectRoutes(t *testing.T) {
	sc := newStubScanner()
	sc.clean, sc.reason = false, "Eicar-Test-Signature"

	// POST /v1/documents
	s, st := newStubStore(), newCounting(t)
	r := fullRouter(s, st, &stubAuthz{}, sc, &stubResidency{}, nil)
	rec := doReq(r, newCreateReq(t, "text/plain", []byte("x"), ""))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	code, detail := errorCode(t, rec)
	assert.Equal(t, "upload_quarantined", code)
	assert.Contains(t, detail, "DRC-003: MALWARE_QUARANTINED")
	assert.Contains(t, detail, "Eicar-Test-Signature")
	assert.Zero(t, st.puts)
	assert.Zero(t, s.createCalls)

	// POST /v1/documents/{id}/versions
	s2, st2 := newStubStore(), newCounting(t)
	s2.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-1", CurrentVersion: 1, Status: domain.StatusActive}
	r2 := fullRouter(s2, st2, &stubAuthz{}, sc, &stubResidency{}, nil)
	body := `{"content_type":"text/plain","content_base64":"` + base64.StdEncoding.EncodeToString([]byte("x")) + `"}`
	w := call(r2, http.MethodPost, "/v1/documents/doc-1/versions", body, nil)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	code, detail = errorCode(t, w)
	assert.Equal(t, "upload_quarantined", code)
	assert.Contains(t, detail, "DRC-003: MALWARE_QUARANTINED")
	assert.Zero(t, st2.puts)
	assert.Zero(t, s2.addVersionCalls)
}

func TestDRC005_IncompleteRecordDeclaration_NamesTheCode(t *testing.T) {
	r, rec := recordsRouter(t, &scopeAuthz{}, &stubResidency{}, nil)

	w := call(r, http.MethodPost, "/v1/records", `{"document_id":"doc-1","document_version_id":"ver-1","jurisdiction_scope":"US-DE"}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	code, detail := errorCode(t, w)
	assert.Equal(t, "missing_field", code)
	assert.Equal(t, "DRC-005: RECORD_DECLARATION_INCOMPLETE: record_class", detail)

	w = call(r, http.MethodPost, "/v1/records:declare", `{"document_id":"doc-1","document_version_id":"ver-1","record_class":"LEGAL_CONTRACT"}`, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	code, detail = errorCode(t, w)
	assert.Equal(t, "missing_field", code)
	assert.Equal(t, "DRC-005: RECORD_DECLARATION_INCOMPLETE: jurisdiction_scope", detail)
	assert.Empty(t, rec.declared, "an incomplete declaration is refused, not defaulted")
}

// The detected content type is persisted on the direct version route too (§3.3
// detected_mime_type).
func TestAddVersion_PersistsTheDetectedContentType(t *testing.T) {
	s, st := newStubStore(), newCounting(t)
	s.docs["doc-1"] = &domain.Document{DocumentID: "doc-1", LegalEntityID: "entity-1", CurrentVersion: 1, Status: domain.StatusActive}
	r := fullRouter(s, st, &stubAuthz{}, newStubScanner(), &stubResidency{}, nil)
	body := `{"content_type":"application/pdf","content_base64":"` + base64.StdEncoding.EncodeToString(pdfBytes) + `"}`
	w := call(r, http.MethodPost, "/v1/documents/doc-1/versions", body, nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.NotEmpty(t, s.versions["doc-1"])
	last := s.versions["doc-1"][len(s.versions["doc-1"])-1]
	assert.Equal(t, "application/pdf", last.DetectedContentType)
	assert.Equal(t, "application/pdf", last.ContentType)
}
