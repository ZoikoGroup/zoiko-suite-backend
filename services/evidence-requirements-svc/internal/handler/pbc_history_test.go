package handler_test

// ER-S6 (ZS-STATE ST-2): GET /v1/pbc/requests/{id}/transitions and
// /available-actions, under the same manager-or-assignee rule as the read.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

func actionsOf(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var got struct {
		Actions []domain.PBCAction `json:"actions"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	out := map[string]string{}
	for _, a := range got.Actions {
		out[a.Command] = a.Role
	}
	return out
}

func TestERS6_AvailableActions_FollowTheCallersRoleAndTheStatus(t *testing.T) {
	path := "/v1/pbc/requests/" + pbcRequestID + "/available-actions"
	// stubPBC's request is SENT.
	rec := do(t, pbcRouter(&stubPBC{version: 2, assignee: pbcAssignee}, pbcRoles()), http.MethodGet, path, nil, testTenant, pbcAssignee)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := actionsOf(t, rec.Body.Bytes())
	assert.Equal(t, map[string]string{"view": "ASSIGNEE", "responses": "ASSIGNEE", "notes": "ASSIGNEE"}, got,
		"the assignee of a SENT request may view it, respond and add notes, and nothing a manager does")

	rec = do(t, pbcRouter(&stubPBC{version: 2, assignee: pbcAssignee}, pbcRoles()), http.MethodGet, path, nil, testTenant, pbcManager)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = actionsOf(t, rec.Body.Bytes())
	for _, cmd := range []string{"reassign", "extend-due-date", "notes", "close"} {
		assert.Equal(t, "MANAGER", got[cmd], cmd)
	}
	for _, cmd := range []string{"send", "view", "responses", "clarify", "mark-satisfied"} {
		_, has := got[cmd]
		assert.False(t, has, "%s is not available to the manager of a SENT request", cmd)
	}

	rec = do(t, pbcRouter(&stubPBC{version: 2, assignee: pbcAssignee}, pbcRoles()), http.MethodGet, path, nil, testTenant, pbcOutsider)
	assert.Equal(t, http.StatusForbidden, rec.Code, "an outsider learns nothing about the request")
}

func TestERS6_Transitions_ReadByTheManagerOrTheAssigneeOnly(t *testing.T) {
	path := "/v1/pbc/requests/" + pbcRequestID + "/transitions"
	from := "DRAFT"
	st := &stubPBC{version: 2, assignee: pbcAssignee, transitions: []*domain.EvidenceRequestTransition{
		{RequestID: pbcRequestID, ToStatus: "DRAFT", Command: "create", Actor: pbcManager, RecordVersion: 1},
		{RequestID: pbcRequestID, FromStatus: &from, ToStatus: "SENT", Command: "send", Actor: pbcManager, RecordVersion: 2},
	}}
	for _, p := range []string{pbcManager, pbcAssignee} {
		rec := do(t, pbcRouter(st, pbcRoles()), http.MethodGet, path, nil, testTenant, p)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got []domain.EvidenceRequestTransition
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 2)
		assert.Equal(t, "send", got[1].Command)
	}
	rec := do(t, pbcRouter(st, pbcRoles()), http.MethodGet, path, nil, testTenant, pbcOutsider)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestERS6_PBCAvailableActions_ByStatus(t *testing.T) {
	r := &domain.EvidenceRequest{RequestID: "r-1"}
	cases := map[domain.EvidenceRequestStatus][]string{
		domain.EvidenceRequestDraft:            {"send", "reassign", "extend-due-date", "notes", "close"},
		domain.EvidenceRequestResponseReceived: {"reassign", "extend-due-date", "notes", "clarify", "mark-satisfied", "close"},
		domain.EvidenceRequestUnderEvaluation:  {"reassign", "extend-due-date", "notes", "mark-satisfied", "close"},
		domain.EvidenceRequestClosed:           {},
	}
	for status, want := range cases {
		r.Status = status
		var got []string
		for _, a := range domain.PBCAvailableActions(r, true, false) {
			got = append(got, a.Command)
		}
		if got == nil {
			got = []string{}
		}
		assert.Equal(t, want, got, string(status))
	}
	r.Status = domain.EvidenceRequestClarificationRequired
	var assignee []string
	for _, a := range domain.PBCAvailableActions(r, false, true) {
		assignee = append(assignee, a.Command)
	}
	assert.Equal(t, []string{"responses", "notes"}, assignee, "after a clarification request the assignee responds again")
}
