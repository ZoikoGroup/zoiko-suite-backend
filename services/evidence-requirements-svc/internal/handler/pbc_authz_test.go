package handler_test

// D-ER-29 (Governance row 21): every PBC route is authorized at the request's
// own legal entity. GET /v1/pbc/requests/{id}, view, notes and acknowledge
// used to check the tenant only — or nothing — so any principal in the tenant
// could read any request, move it to VIEWED (and so make the manager's next
// command 412), write AUDIT_ONLY notes and acknowledge any response.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

const (
	pbcManager  = "principal-manager"
	pbcAssignee = "principal-assignee"
	pbcOutsider = "principal-outsider"
)

// principalAuthz grants each principal a set of actions; everything else is
// denied. down makes every call fail as if authorization-svc were unreachable.
type principalAuthz struct {
	grants map[string][]string
	down   bool
	asked  []string // "principal:action@scope"
}

func (a *principalAuthz) CheckAllowed(_ context.Context, principalID, scope, action string) error {
	a.asked = append(a.asked, principalID+":"+action+"@"+scope)
	if a.down {
		return errors.New("authorization-svc unreachable")
	}
	for _, granted := range a.grants[principalID] {
		if granted == action {
			return nil
		}
	}
	return domain.ErrAuthorizationDenied
}

// pbcRoles: the manager holds EVIDENCE_REQUEST_MANAGE, the assignee and the
// outsider each hold EVIDENCE_REQUEST_SUBMIT_RESPONSE — the outsider is simply
// not the request's assignee.
func pbcRoles() *principalAuthz {
	return &principalAuthz{grants: map[string][]string{
		pbcManager:  {"EVIDENCE_REQUEST_MANAGE"},
		pbcAssignee: {"EVIDENCE_REQUEST_SUBMIT_RESPONSE"},
		pbcOutsider: {"EVIDENCE_REQUEST_SUBMIT_RESPONSE"},
	}}
}

func TestPBCAuthz_ReadIsForTheManagerOrTheAssignee(t *testing.T) {
	path := "/v1/pbc/requests/" + pbcRequestID
	for _, c := range []struct {
		principal string
		want      int
		asked     string
	}{
		{pbcManager, http.StatusOK, pbcManager + ":EVIDENCE_REQUEST_MANAGE@" + testEntity},
		{pbcAssignee, http.StatusOK, pbcAssignee + ":EVIDENCE_REQUEST_SUBMIT_RESPONSE@" + testEntity},
		{pbcOutsider, http.StatusForbidden, pbcOutsider + ":EVIDENCE_REQUEST_MANAGE@" + testEntity},
	} {
		t.Run(c.principal, func(t *testing.T) {
			az := pbcRoles()
			rec := do(t, pbcRouter(&stubPBC{version: 2, assignee: pbcAssignee}, az), http.MethodGet, path, nil, testTenant, c.principal)
			require.Equal(t, c.want, rec.Code, rec.Body.String())
			assert.Equal(t, []string{c.asked}, az.asked, "one check, at the request's entity")
			if c.want == http.StatusOK {
				assert.Equal(t, `"v2"`, rec.Header().Get("ETag"))
			} else {
				assert.Empty(t, rec.Header().Get("ETag"), "a refusal does not disclose the version")
			}
		})
	}
}

func TestPBCAuthz_OnlyTheAssigneeRecordsAView(t *testing.T) {
	path := "/v1/pbc/requests/" + pbcRequestID + "/view"

	t.Run("the assignee", func(t *testing.T) {
		st := &stubPBC{version: 2, assignee: pbcAssignee}
		rec := do(t, pbcRouter(st, pbcRoles()), http.MethodPost, path, nil, testTenant, pbcAssignee)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, 1, st.writes)
		assert.EqualValues(t, 3, st.version)
	})
	for _, principal := range []string{pbcManager, pbcOutsider} {
		t.Run(principal+" is refused and the version is unchanged", func(t *testing.T) {
			st := &stubPBC{version: 2, assignee: pbcAssignee}
			az := pbcRoles()
			rec := do(t, pbcRouter(st, az), http.MethodPost, path, nil, testTenant, principal)
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Equal(t, 0, st.writes, "the store is never called")
			assert.EqualValues(t, 2, st.version, "a manager's ETag stays current")
			assert.Empty(t, az.asked, "refused before any grant is consulted")
		})
	}
	t.Run("the assignee without SUBMIT_RESPONSE at the entity", func(t *testing.T) {
		st := &stubPBC{version: 2, assignee: pbcAssignee}
		az := &principalAuthz{}
		rec := do(t, pbcRouter(st, az), http.MethodPost, path, nil, testTenant, pbcAssignee)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, 0, st.writes)
		assert.Equal(t, []string{pbcAssignee + ":EVIDENCE_REQUEST_SUBMIT_RESPONSE@" + testEntity}, az.asked)
	})
}

func TestPBCAuthz_NotesByVisibility(t *testing.T) {
	path := "/v1/pbc/requests/" + pbcRequestID + "/notes"
	for _, c := range []struct {
		name, principal, visibility string
		want                        int
	}{
		{"shared by the assignee", pbcAssignee, "SHARED", http.StatusCreated},
		{"shared by a manager", pbcManager, "SHARED", http.StatusCreated},
		{"shared by an outsider", pbcOutsider, "SHARED", http.StatusForbidden},
		{"audit-only by the assignee", pbcAssignee, "AUDIT_ONLY", http.StatusForbidden},
		{"audit-only by an outsider", pbcOutsider, "AUDIT_ONLY", http.StatusForbidden},
		{"audit-only by a manager", pbcManager, "AUDIT_ONLY", http.StatusCreated},
		{"unknown visibility is 400 before authorization", pbcOutsider, "PUBLIC", http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &stubPBC{version: 2, assignee: pbcAssignee}
			rec := do(t, pbcRouter(st, pbcRoles()), http.MethodPost, path,
				map[string]any{"body": "chasing the bank letter", "visibility": c.visibility}, testTenant, c.principal)
			require.Equal(t, c.want, rec.Code, rec.Body.String())
			if c.want == http.StatusCreated {
				assert.Equal(t, 1, st.writes)
			} else {
				assert.Equal(t, 0, st.writes, "a refused note is never written")
			}
		})
	}
}

func TestPBCAuthz_AcknowledgeNeedsManageAtTheEntity(t *testing.T) {
	path := "/v1/pbc/responses/resp-1/acknowledge"

	az := pbcRoles()
	st := &stubPBC{version: 2, assignee: pbcAssignee}
	rec := do(t, pbcRouter(st, az), http.MethodPost, path, nil, testTenant, pbcManager)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{pbcManager + ":EVIDENCE_REQUEST_MANAGE@" + testEntity}, az.asked)

	for _, principal := range []string{pbcAssignee, pbcOutsider} {
		st := &stubPBC{version: 2, assignee: pbcAssignee}
		rec := do(t, pbcRouter(st, pbcRoles()), http.MethodPost, path, nil, testTenant, principal)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", principal, rec.Body.String())
		assert.Equal(t, 0, st.writes, "%s: no receipt written", principal)
	}
}

// authorization-svc unreachable: every one of the four routes is 503 and
// nothing is written — never admitted on an outage.
func TestPBCAuthz_UnreachableAuthorizationFailsClosed(t *testing.T) {
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/pbc/requests/" + pbcRequestID, nil},
		{http.MethodPost, "/v1/pbc/requests/" + pbcRequestID + "/view", nil},
		{http.MethodPost, "/v1/pbc/requests/" + pbcRequestID + "/notes", map[string]any{"body": "x", "visibility": "SHARED"}},
		{http.MethodPost, "/v1/pbc/requests/" + pbcRequestID + "/notes", map[string]any{"body": "x", "visibility": "AUDIT_ONLY"}},
		{http.MethodPost, "/v1/pbc/responses/resp-1/acknowledge", nil},
	} {
		st := &stubPBC{version: 2, assignee: pbcAssignee}
		rec := do(t, pbcRouter(st, &principalAuthz{down: true}), c.method, c.path, c.body, testTenant, pbcAssignee)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
		assert.Equal(t, 0, st.writes, "%s %s", c.method, c.path)
	}
}
