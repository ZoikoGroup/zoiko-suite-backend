package context_test

import (
	"context"
	"net/http"
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
	approve := func(caller string) (int, map[string]any) {
		w := do(h.router, http.MethodPost, "/v1/context/support/"+created.SupportContextID+"/approve", "tenant-a", caller, nil)
		var body map[string]any
		decodeBody(t, w, &body)
		return w.Code, body
	}

	// The requester approving their own request: 403 SOD_CONFLICT.
	code, body := approve(requesterID)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, domain.ErrCodeSoDConflict, body["error_code"])
	// The grantee approving their own elevation: the same.
	code, _ = approve("support-eng-1")
	assert.Equal(t, http.StatusForbidden, code)
	// Somebody other than the named approver: 403 AUTHORIZATION_DENIED.
	code, body = approve("someone-else")
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, domain.ErrCodeAuthorizationDenied, body["error_code"])
	assert.Equal(t, domain.SupportPendingApproval, h.support.contexts[created.SupportContextID].ApprovalStatus)

	// The named approver: granted, and only once.
	code, _ = approve("support-lead-9")
	require.Equal(t, http.StatusOK, code)
	assert.True(t, h.support.contexts[created.SupportContextID].Live(time.Now()))
	code, _ = approve("support-lead-9")
	assert.Equal(t, http.StatusConflict, code)
}
