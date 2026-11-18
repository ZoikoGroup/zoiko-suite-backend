package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/authz"
	"zoiko.io/document-vault-svc/internal/domain"
)

// BIZ-02 SoD (spec section 4): "security/privacy owner for restricted classes".
// Confirming a RESTRICTED classification, or superseding one, needs the
// additional CONFIRM_RESTRICTED_CLASSIFICATION action on top of the ordinary one.

func confirmReq(classificationID, principal string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+classificationID+"/confirm", nil)
	req.Header.Set("X-Principal-Id", principal)
	return req
}

func TestConfirmClassification_Restricted_NeedsSecurityOwner(t *testing.T) {
	s := newStubStore()
	az := &stubAuthz{denied: map[string]bool{"steward-1|" + authz.ActionConfirmRestrictedClassification: true}}
	r := newRouterAuthz(s, &stubResidency{}, newTestStorage(t), az)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "RESTRICTED", "content"))))
	proposal := s.classifications[0]
	require.Equal(t, domain.ClassificationRestricted, proposal.ClassificationValue)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, confirmReq(proposal.ClassificationID, "steward-1"))
	assert.Equal(t, http.StatusForbidden, rec.Code, "a steward without the security-owner action must not confirm a RESTRICTED classification")
	assert.True(t, az.called("steward-1", authz.ActionConfirmClassification), "the ordinary action is still checked first")
	assert.True(t, az.called("steward-1", authz.ActionConfirmRestrictedClassification))
	assert.Equal(t, domain.ClassificationStatusCandidate, s.classifications[0].Status, "a refused confirm must leave the proposal a CANDIDATE")

	// The security owner (the restricted action is granted to this principal) can.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, confirmReq(proposal.ClassificationID, "security-owner-1"))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, domain.ClassificationStatusConfirmed, s.classifications[0].Status)
}

func TestConfirmClassification_NonRestricted_DoesNotConsultSecurityOwnerAction(t *testing.T) {
	s := newStubStore()
	az := &stubAuthz{}
	r := newRouterAuthz(s, &stubResidency{}, newTestStorage(t), az)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, confirmReq(s.classifications[0].ClassificationID, "steward-1"))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, az.called("steward-1", authz.ActionConfirmRestrictedClassification), "only RESTRICTED needs the extra action")
}

func TestConfirmClassification_Restricted_AuthzUnavailable_FailsClosed(t *testing.T) {
	s := newStubStore()
	az := &stubAuthz{}
	r := newRouterAuthz(s, &stubResidency{}, newTestStorage(t), az)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "RESTRICTED", "content"))))
	az.err = domain.ErrAuthzServiceUnavailable

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, confirmReq(s.classifications[0].ClassificationID, "steward-1"))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, domain.ClassificationStatusCandidate, s.classifications[0].Status)
}

func TestSupersedeClassification_ToRestricted_NeedsSecurityOwner(t *testing.T) {
	s := newStubStore()
	az := &stubAuthz{denied: map[string]bool{"steward-4|" + authz.ActionConfirmRestrictedClassification: true}}
	r := newRouterAuthz(s, &stubResidency{}, newTestStorage(t), az)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/documents", bytes.NewReader(createBody(t, "CONFIDENTIAL", "content"))))
	original := confirmFirstClassification(t, r, s)

	reclassify := httptest.NewRequest(http.MethodPost, "/v1/documents/doc-1/reclassify", bytes.NewReader(classifyBody(t, "RESTRICTED", "HUMAN", nil)))
	reclassify.Header.Set("X-Principal-Id", "steward-3")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, reclassify)
	require.Equal(t, http.StatusCreated, rec.Code)
	var proposed domain.RecordClassification
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &proposed))

	body, err := json.Marshal(map[string]string{"new_classification_id": proposed.ClassificationID})
	require.NoError(t, err)
	supersede := func(principal string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/documents/classifications/"+original.ClassificationID+"/supersede", bytes.NewReader(body))
		req.Header.Set("X-Principal-Id", principal)
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		return out
	}

	denied := supersede("steward-4")
	assert.Equal(t, http.StatusForbidden, denied.Code, "moving a document INTO RESTRICTED needs the security owner")
	assert.True(t, az.called("steward-4", authz.ActionConfirmRestrictedClassification))

	allowed := supersede("security-owner-1")
	assert.Equal(t, http.StatusOK, allowed.Code, allowed.Body.String())
}
