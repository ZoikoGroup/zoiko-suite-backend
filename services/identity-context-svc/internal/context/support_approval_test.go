package context_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/domain"
)

// S1-1 / R-2: break-glass approval is the approver's own act. The audit's
// re-check, case by case.

// Naming an approver who never acts grants nothing, whoever is named.
func TestSupportRequestNamingAnApproverGrantsNothing(t *testing.T) {
	f := newSupportFixture()
	req := validAttachRequest()
	req.ApproverPrincipalID = "principal-who-does-not-exist"
	sc, err := f.build().Attach(context.Background(), req, requesterID)
	require.NoError(t, err)
	assert.Equal(t, domain.SupportPendingApproval, sc.ApprovalStatus)
	assert.False(t, sc.Live(time.Now()), "a pending request is not a grant")
	live, err := f.store.FindLiveSupportContext(context.Background(), req.SupportPrincipalID, req.TenantID, time.Now())
	require.NoError(t, err)
	assert.Nil(t, live)
	assert.Empty(t, f.store.events, "nothing is announced as attached before approval")
}

// The caller naming themselves as approver is refused at the request.
func TestSupportRequestCannotNameTheRequesterAsApprover(t *testing.T) {
	f := newSupportFixture()
	_, err := f.build().Attach(context.Background(), validAttachRequest(), "support-lead-9")
	require.ErrorIs(t, err, domain.ErrSupportSelfApproval)
	assert.Empty(t, f.store.contexts)
}

func TestSupportApprovalRoute(t *testing.T) {
	h := newGov01Harness(t, &permittingAuthz{})
	w := do(h.router, http.MethodPost, "/v1/context/support", "tenant-support", requesterID, validAttachRequest())
	require.Equal(t, http.StatusAccepted, w.Code)
	var created domain.AttachSupportContextResponse
	decodeBody(t, w, &created)

	// Refusal cases carry no If-Match: the refusal (403) must be heard before
	// the version precondition (428). Refusal precedes the precondition,
	// never the other way around.
	refuse := func(caller string, wantCode string) {
		w := do(h.router, http.MethodPost, "/v1/context/support/"+created.SupportContextID+"/approve", "tenant-a", caller, nil)
		require.Equal(t, http.StatusForbidden, w.Code)
		var body map[string]any
		decodeBody(t, w, &body)
		assert.Equal(t, wantCode, body["code"])
	}

	// The requester approving their own request: 403 SOD_CONFLICT.
	refuse(requesterID, domain.ErrCodeSoDConflict)
	// The grantee approving their own elevation: the same.
	refuse("support-eng-1", domain.ErrCodeSoDConflict)
	// Somebody other than the named approver: 403 AUTHORIZATION_DENIED.
	refuse("someone-else", domain.ErrCodeAuthorizationDenied)
	assert.Equal(t, domain.SupportPendingApproval, h.support.contexts[created.SupportContextID].ApprovalStatus)

	// The named approver: granted. A mutation carries If-Match (R6), so the
	// happy path supplies it; the version is 1 right after attach.
	approve := func(caller, ifMatch string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/v1/context/support/"+created.SupportContextID+"/approve", nil)
		req.Header.Set("X-Tenant-Id", "tenant-a")
		req.Header.Set("X-Principal-Id", caller)
		req.Header.Set("X-Correlation-ID", "test-correlation")
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		var body map[string]any
		decodeBody(t, w, &body)
		return w.Code, body
	}

	code, _ := approve("support-lead-9", `"v1"`)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, h.support.contexts[created.SupportContextID].Live(time.Now()))

	// A second approve is a conflict: the request is no longer pending. After
	// the approve the record version is 2.
	code, _ = approve("support-lead-9", `"v2"`)
	assert.Equal(t, http.StatusConflict, code)
}
