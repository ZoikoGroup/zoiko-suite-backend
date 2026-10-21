package paymentstatus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func serve(t *testing.T, status int, body string, path *string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path != nil {
			*path = r.URL.Path
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, nil)
}

func TestVerifySettled(t *testing.T) {
	var path string
	c := serve(t, 200, `{"PaymentID":"pay-1","LegalEntityID":"le-1","Status":"SETTLED","FinalitySource":"PROVIDER_CALLBACK","HasOpenConflict":false}`, &path)
	ev, err := c.VerifySettled(context.Background(), "t-1", "le-1", "pay-1")
	require.NoError(t, err)
	assert.Equal(t, "/bnk07/payments/pay-1", path)
	assert.Equal(t, "SETTLED", ev.Status)
	assert.Equal(t, "PROVIDER_CALLBACK", ev.FinalitySource)
}

func TestOnlyASettledPaymentWithNoConflictInTheSameEntityIsEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"pending":       {200, `{"PaymentID":"pay-1","LegalEntityID":"le-1","Status":"PENDING"}`, domain.ErrPaymentNotSettled},
		"submitted":     {200, `{"PaymentID":"pay-1","LegalEntityID":"le-1","Status":"SUBMITTED"}`, domain.ErrPaymentNotSettled},
		"returned":      {200, `{"PaymentID":"pay-1","LegalEntityID":"le-1","Status":"RETURNED"}`, domain.ErrPaymentNotSettled},
		"conflict":      {200, `{"PaymentID":"pay-1","LegalEntityID":"le-1","Status":"SETTLED","HasOpenConflict":true}`, domain.ErrPaymentInConflict},
		"other entity":  {200, `{"PaymentID":"pay-1","LegalEntityID":"le-2","Status":"SETTLED"}`, domain.ErrPaymentEntityMismatch},
		"other payment": {200, `{"PaymentID":"pay-9","LegalEntityID":"le-1","Status":"SETTLED"}`, domain.ErrPaymentNotFound},
		"not found":     {404, ``, domain.ErrPaymentNotFound},
		"server error":  {503, ``, domain.ErrBankingUnavailable},
		"garbage":       {200, `nope`, domain.ErrBankingUnavailable},
	} {
		_, err := serve(t, tc.status, tc.body, nil).VerifySettled(context.Background(), "t-1", "le-1", "pay-1")
		assert.ErrorIs(t, err, tc.want, name)
	}
}

func TestUnreachableBankingIsUnknownNeverSettled(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := NewClient(url, nil).VerifySettled(context.Background(), "t", "le-1", "pay-1")
	assert.ErrorIs(t, err, domain.ErrBankingUnavailable)
}
