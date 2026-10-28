package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/handler"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
	"zoiko.io/payment-initiation-adapter-svc/internal/provideradapter"
)

// recordingProvider is the real stub adapter, keeping what it was asked to send.
type recordingProvider struct {
	*provideradapter.StubProviderAdapter
	sent []provideradapter.SubmitRequest
}

func (p *recordingProvider) Submit(ctx context.Context, req provideradapter.SubmitRequest) (*provideradapter.SubmitResult, error) {
	p.sent = append(p.sent, req)
	return p.StubProviderAdapter.Submit(ctx, req)
}

func newRouterWithProvider(st *stubStore, provider provideradapter.Client) chi.Router {
	h := handler.New(st, &stubAuthz{}, provider, &stubTreasury{}, &stubAuthorization{}, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

// callerPrepareBody is the prepare body every current caller builds:
// payment-run-svc's prepareRequestBody, accounts-receivable-svc's
// prepareWire, treasury-svc's bnk06 request and payroll-run-svc's map all
// carry Amount as a Go float64 under the PascalCase key, so encoding/json
// writes it as a bare JSON number.
type callerPrepareBody struct {
	LegalEntityID        string
	SourceReference      string
	PayerAccountRef      string
	PayeeRef             string
	Amount               float64
	Currency             string
	ExecutionDate        time.Time
	PaymentReference     string
	PayerAccountVerified bool
	IdempotencyKey       string
}

// A6: callers that send Amount as a float64 keep working unchanged. The
// number they send is read as decimal text, stored, and handed to the
// provider as that exact text, and a caller that reads the attempt back into
// a float64 still can.
func TestPrepareAttempt_FloatSendingCallersKeepWorking(t *testing.T) {
	cases := []struct {
		amount   float64
		wire     string // what encoding/json writes for the caller's float64
		provider string // what the provider is sent: the column's text
	}{
		{1234.5, "1234.5", "1234.50"},
		{500, "500", "500.00"},
		{0.1, "0.1", "0.10"},
		{99999.99, "99999.99", "99999.99"},
	}
	for i, c := range cases {
		st := newStubStore()
		provider := &recordingProvider{StubProviderAdapter: provideradapter.NewStubProviderAdapter()}
		r := newRouterWithProvider(st, provider)

		body := callerPrepareBody{
			LegalEntityID: testLegalEntity, SourceReference: "run-instruction-float", PayerAccountRef: "payer-acct-1",
			PayeeRef: "payee-1", Amount: c.amount, Currency: "USD", ExecutionDate: time.Now().UTC().Add(24 * time.Hour),
			PaymentReference: "invoice-payment", PayerAccountVerified: true, IdempotencyKey: "idem-float-" + c.wire,
		}
		raw, _ := json.Marshal(body)
		if !strings.Contains(string(raw), `"Amount":`+c.wire+`,`) {
			t.Fatalf("case %d: the caller's float64 is not on the wire as a bare number: %s", i, raw)
		}

		w := doRequest(r, http.MethodPost, "/bnk06/attempts/", body, testTenant)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: prepare %d %s", c.wire, w.Code, w.Body)
		}
		var asCaller struct {
			AttemptID string
			Amount    float64
		}
		if err := json.Unmarshal(w.Body.Bytes(), &asCaller); err != nil || asCaller.Amount != c.amount {
			t.Fatalf("%s: a float64 reader sees %v %v", c.wire, asCaller.Amount, err)
		}
		if got := st.attempts[asCaller.AttemptID].Amount; got != domain.Decimal(c.provider) {
			t.Fatalf("%s: stored %q, want %q", c.wire, got, c.provider)
		}

		if w := doRequest(r, http.MethodPost, "/bnk06/attempts/"+asCaller.AttemptID+"/submit", nil, testTenant); w.Code != http.StatusOK {
			t.Fatalf("%s: submit %d %s", c.wire, w.Code, w.Body)
		}
		if len(provider.sent) != 1 || provider.sent[0].Amount != c.provider {
			t.Fatalf("%s: provider was sent %+v, want amount %q", c.wire, provider.sent, c.provider)
		}
	}
}

// A6: an amount the NUMERIC(18,2) column would round is refused before
// anything is stored, whether it arrives as a number or a string. Before,
// 10.005 was decoded into a float64, passed "amount > 0", and was stored as
// 10.01 while the adapter was sent 10.005.
func TestPrepareAttempt_AmountIsExactOrRefused(t *testing.T) {
	post := func(st *stubStore, key, amount string) int {
		r := newTestRouter(st, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
		body := map[string]any{
			"LegalEntityID": testLegalEntity, "SourceReference": "run-" + key, "PayerAccountRef": "payer-acct-1",
			"PayeeRef": "payee-1", "Amount": json.RawMessage(amount), "Currency": "USD",
			"ExecutionDate": time.Now().UTC().Add(24 * time.Hour), "PayerAccountVerified": true, "IdempotencyKey": key,
		}
		return doRequest(r, http.MethodPost, "/bnk06/attempts/", body, testTenant).Code
	}

	for name, amount := range map[string]string{
		"three places as a number":     `10.005`,
		"three places as a string":     `"10.005"`,
		"zero":                         `0`,
		"negative":                     `-5.00`,
		"seventeen integer digits":     `10000000000000000`,
		"not a number":                 `"ten"`,
		"a fraction":                   `"1/3"`,
		"empty":                        `""`,
		"missing":                      `null`,
		"exponent rounding below cent": `1e-3`,
	} {
		st := newStubStore()
		if code := post(st, "idem-"+strings.ReplaceAll(name, " ", "-"), amount); code != http.StatusBadRequest {
			t.Errorf("%s (%s): %d, want 400", name, amount, code)
		}
		if len(st.attempts) != 0 {
			t.Errorf("%s (%s): an attempt was stored", name, amount)
		}
	}

	for amount, stored := range map[string]string{
		`"10.25"`:             "10.25",
		`10.10`:               "10.10",
		`1e2`:                 "100.00",
		`9999999999999999.99`: "9999999999999999.99",
	} {
		st := newStubStore()
		if code := post(st, "idem-ok-"+amount, amount); code != http.StatusCreated {
			t.Fatalf("%s: %d, want 201", amount, code)
		}
		for _, a := range st.attempts {
			if a.Amount != domain.Decimal(stored) {
				t.Errorf("%s: stored %q, want %q", amount, a.Amount, stored)
			}
		}
	}
}
