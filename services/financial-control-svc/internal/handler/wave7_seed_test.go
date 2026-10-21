package handler_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
)

func TestSeedCatalogue_Wave7SeedsTheWorkforcePayrollControls(t *testing.T) {
	s := &fakeStore{}
	rr := do(newServerExec(s, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", `{"wave":7,"effective_from":"2026-10-01"}`, auth)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var got struct {
		Controls []struct {
			ControlCode string `json:"control_code"`
		} `json:"controls"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	codes := []string{}
	for _, c := range got.Controls {
		codes = append(codes, c.ControlCode)
	}
	assert.ElementsMatch(t, []string{"FIN-CTRL-005", "PAY-CTRL-004", "FIN-CTRL-006"}, codes)

	assert.Equal(t, 400, do(newServerExec(&fakeStore{}, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", `{"wave":7}`, auth).Code, "effective_from is required")
	denied := &fakeStore{}
	assert.Equal(t, 403, do(newServerExec(denied, &fakeAuthz{err: domain.ErrAuthorizationDenied}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed",
		`{"wave":7,"effective_from":"2026-10-01"}`, auth).Code)
	assert.NotContains(t, denied.calls, "CreateDefinition")
}
