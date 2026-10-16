package paymentstatus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func TestSnapshotReportsAnyStateWithoutJudgingItAndFailsClosed(t *testing.T) {
	var path string
	c := serve(t, 200, `{"PaymentID":"pay-1","LegalEntityID":"le-1","Status":"RETURNED","SourceReference":"instr-1","HasOpenConflict":true}`, &path)
	s, err := c.Snapshot(context.Background(), "t-1", "le-1", "pay-1")
	require.NoError(t, err)
	assert.Equal(t, "/bnk07/payments/pay-1", path)
	assert.Equal(t, "RETURNED", s.Status)
	assert.Equal(t, "instr-1", s.SourceReference)
	assert.True(t, s.HasOpenConflict, "the caller decides what a conflict means")

	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"not found":     {404, ``, domain.ErrPaymentNotFound},
		"other payment": {200, `{"PaymentID":"pay-9","LegalEntityID":"le-1","Status":"SETTLED"}`, domain.ErrPaymentNotFound},
		"server error":  {500, ``, domain.ErrBankingUnavailable},
		"garbage":       {200, `nope`, domain.ErrBankingUnavailable},
	} {
		_, err := serve(t, tc.status, tc.body, nil).Snapshot(context.Background(), "t-1", "le-1", "pay-1")
		assert.ErrorIs(t, err, tc.want, name)
	}
	_, err = NewClient("http://127.0.0.1:1", nil).Snapshot(context.Background(), "t-1", "le-1", "pay-1")
	assert.ErrorIs(t, err, domain.ErrBankingUnavailable)
}
