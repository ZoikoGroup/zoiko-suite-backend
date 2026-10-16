package paydeps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func server(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func deadURL() string {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func TestPayeeMasterPinsTheActiveDestinationByVersion(t *testing.T) {
	var path string
	var hdr http.Header
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		path, hdr = r.URL.RequestURI(), r.Header.Clone()
		_, _ = w.Write([]byte(`{"DestinationID":"dest-1","LegalEntityID":"le-1","Status":"ACTIVE","Currency":"GBP","Fingerprint":"bank-fp",
			"UpdatedAt":"2026-03-01T10:00:00.5Z","ProposedByPrincipalID":"p-a","VerifiedByPrincipalID":"p-b","ApprovedByPrincipalID":"p-c","AccountIdentifier":"12345678"}`))
	})
	d, err := NewPayeeMaster(url, nil).ActiveDestination(context.Background(), "t-1", "p-1", "le-1", "payee-1")
	require.NoError(t, err)
	assert.Equal(t, "/org10/parties/payee-1/active?scope=DEFAULT", path)
	assert.Equal(t, "t-1", hdr.Get("X-Tenant-Id"))
	assert.Equal(t, "p-1", hdr.Get("X-Principal-Id"), "payee-banking-identity-svc requires the caller's principal")
	assert.Equal(t, "dest-1", d.DestinationID)
	assert.Equal(t, "2026-03-01T10:00:00.5Z", d.Version, "the destination's updated_at is its version")
	assert.Equal(t, "bank-fp", d.Fingerprint)
	assert.True(t, d.ChangedByPrincipal("p-b"))
	b, _ := json.Marshal(d)
	assert.NotContains(t, string(b), "12345678", "bank details are never carried")

	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"no destination": {404, ``, domain.ErrNoActivePayee},
		"suspended":      {200, `{"DestinationID":"d","Status":"SUSPENDED","UpdatedAt":"2026-03-01T10:00:00Z"}`, domain.ErrNoActivePayee},
		"no version":     {200, `{"DestinationID":"d","Status":"ACTIVE"}`, domain.ErrNoActivePayee},
		"server error":   {500, ``, domain.ErrPayeeUnavailable},
		"garbage":        {200, `nope`, domain.ErrPayeeUnavailable},
	} {
		_, err := NewPayeeMaster(server(t, reply(tc.status, tc.body)), nil).ActiveDestination(context.Background(), "t", "p", "le-1", "x")
		assert.ErrorIs(t, err, tc.want, name)
	}
	_, err = NewPayeeMaster(deadURL(), nil).ActiveDestination(context.Background(), "t", "p", "le-1", "x")
	assert.ErrorIs(t, err, domain.ErrPayeeUnavailable, "unknown is not valid")
}

func TestWorkerPayeeNeedsOneVerifiedReference(t *testing.T) {
	ask := func(body string, status int) (string, error) {
		return NewWorkerPayee(server(t, reply(status, body)), nil).PayeeRefFor(context.Background(), "t", "p", "le-1", "w-1", "2026-09-30")
	}
	ref, err := ask(`{"profiles":[{"legal_entity_id":"le-1","payee_ref":"payee-1","payee_ref_status":"VERIFIED"}]}`, 200)
	require.NoError(t, err)
	assert.Equal(t, "payee-1", ref)
	ref, err = ask(`{"profiles":[{"legal_entity_id":"le-1","payee_ref":"payee-1","payee_ref_status":"VERIFIED"},{"legal_entity_id":"le-1","payee_ref":"payee-1","payee_ref_status":"VERIFIED"}]}`, 200)
	require.NoError(t, err, "two assignments with the same payee are fine")
	assert.Equal(t, "payee-1", ref)
	ref, err = ask(`{"profiles":[{"legal_entity_id":"le-2","payee_ref":"other","payee_ref_status":"VERIFIED"},{"legal_entity_id":"le-1","payee_ref":"payee-1","payee_ref_status":"VERIFIED"}]}`, 200)
	require.NoError(t, err, "a profile of another entity is ignored")
	assert.Equal(t, "payee-1", ref)

	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"unverified":           {200, `{"profiles":[{"legal_entity_id":"le-1","payee_ref":"payee-1","payee_ref_status":"UNVERIFIED"}]}`, domain.ErrPayeeMissing},
		"none":                 {200, `{"profiles":[{"legal_entity_id":"le-1","payee_ref_status":"NONE"}]}`, domain.ErrPayeeMissing},
		"blank":                {200, `{"profiles":[{"legal_entity_id":"le-1","payee_ref":" ","payee_ref_status":"VERIFIED"}]}`, domain.ErrPayeeMissing},
		"conflicting payees":   {200, `{"profiles":[{"legal_entity_id":"le-1","payee_ref":"a","payee_ref_status":"VERIFIED"},{"legal_entity_id":"le-1","payee_ref":"b","payee_ref_status":"VERIFIED"}]}`, domain.ErrPayeeMissing},
		"no profile in entity": {200, `{"profiles":[{"legal_entity_id":"le-2","payee_ref":"a","payee_ref_status":"VERIFIED"}]}`, domain.ErrPayeeMissing},
		"no profile at all":    {404, ``, domain.ErrPayeeMissing},
		"server error":         {503, ``, domain.ErrWorkerProfileUnavailable},
		"garbage":              {200, `nope`, domain.ErrWorkerProfileUnavailable},
	} {
		_, err := ask(tc.body, tc.status)
		assert.ErrorIs(t, err, tc.want, name)
	}
	_, err = NewWorkerPayee(deadURL(), nil).PayeeRefFor(context.Background(), "t", "p", "le-1", "w", "2026-09-30")
	assert.ErrorIs(t, err, domain.ErrWorkerProfileUnavailable)
}

func TestPayerAccountMustBeActiveVerifiedOfTheEntityInTheCurrency(t *testing.T) {
	good := `{"bank_account_id":"acct-1","tenant_id":"t-1","legal_entity_id":"le-1","currency_code":"GBP","account_status":"ACTIVE","is_ownership_verified":true}`
	require.NoError(t, NewPayerAccounts(server(t, reply(200, good)), nil).Verify(context.Background(), "t-1", "p", "le-1", "acct-1", "GBP"))
	for name, body := range map[string]string{
		"other tenant":   `{"bank_account_id":"acct-1","tenant_id":"t-2","legal_entity_id":"le-1","currency_code":"GBP","account_status":"ACTIVE","is_ownership_verified":true}`,
		"other entity":   `{"bank_account_id":"acct-1","tenant_id":"t-1","legal_entity_id":"le-2","currency_code":"GBP","account_status":"ACTIVE","is_ownership_verified":true}`,
		"other currency": `{"bank_account_id":"acct-1","tenant_id":"t-1","legal_entity_id":"le-1","currency_code":"USD","account_status":"ACTIVE","is_ownership_verified":true}`,
		"suspended":      `{"bank_account_id":"acct-1","tenant_id":"t-1","legal_entity_id":"le-1","currency_code":"GBP","account_status":"SUSPENDED","is_ownership_verified":true}`,
		"not verified":   `{"bank_account_id":"acct-1","tenant_id":"t-1","legal_entity_id":"le-1","currency_code":"GBP","account_status":"ACTIVE","is_ownership_verified":false}`,
		"different acct": `{"bank_account_id":"acct-9","tenant_id":"t-1","legal_entity_id":"le-1","currency_code":"GBP","account_status":"ACTIVE","is_ownership_verified":true}`,
	} {
		err := NewPayerAccounts(server(t, reply(200, body)), nil).Verify(context.Background(), "t-1", "p", "le-1", "acct-1", "GBP")
		assert.ErrorIs(t, err, domain.ErrPayerAccountInvalid, name)
	}
	assert.ErrorIs(t, NewPayerAccounts(server(t, reply(404, ``)), nil).Verify(context.Background(), "t-1", "p", "le-1", "acct-1", "GBP"), domain.ErrPayerAccountInvalid)
	assert.ErrorIs(t, NewPayerAccounts(server(t, reply(403, ``)), nil).Verify(context.Background(), "t-1", "p", "le-1", "acct-1", "GBP"), domain.ErrPayerUnavailable)
	assert.ErrorIs(t, NewPayerAccounts(deadURL(), nil).Verify(context.Background(), "t-1", "p", "le-1", "acct-1", "GBP"), domain.ErrPayerUnavailable)
}

func TestApprovalPolicyFailsClosedButNoPolicyIsWithinThreshold(t *testing.T) {
	var got map[string]any
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"result":"APPROVAL_REQUIRED","policy_version_id":"pol-9"}`))
	})
	res, ver, err := NewApprovalPolicy(url, nil).Evaluate(context.Background(), "t-1", "p-1", "le-1", 12345.5)
	require.NoError(t, err)
	assert.Equal(t, "APPROVAL_REQUIRED", res)
	assert.Equal(t, "pol-9", ver)
	assert.Equal(t, "APPROVAL_THRESHOLD", got["policy_type"])
	assert.Equal(t, "le-1", got["legal_entity_id"])
	assert.JSONEq(t, `{"amount":12345.5}`, string(mustJSON(got["action_context"])))

	res, _, err = NewApprovalPolicy(server(t, reply(404, ``)), nil).Evaluate(context.Background(), "t", "p", "le", 1)
	require.NoError(t, err)
	assert.Equal(t, "WITHIN_THRESHOLD", res, "policy-svc's own stance on an unconfigured policy")
	for name, h := range map[string]http.HandlerFunc{"500": reply(500, ``), "garbage": reply(200, `nope`), "empty result": reply(200, `{}`)} {
		_, _, err := NewApprovalPolicy(server(t, h), nil).Evaluate(context.Background(), "t", "p", "le", 1)
		assert.ErrorIs(t, err, domain.ErrPolicyUnavailable, name)
	}
	_, _, err = NewApprovalPolicy(deadURL(), nil).Evaluate(context.Background(), "t", "p", "le", 1)
	assert.ErrorIs(t, err, domain.ErrPolicyUnavailable)
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func handoffReq() HandoffRequest {
	return HandoffRequest{
		TenantID: "t-1", PrincipalID: "p-9", CorrelationID: "corr", InstructionID: "i-1", LegalEntityID: "le-1", Fingerprint: "sha256:approved",
		PayerAccountRef: "acct-1", PayeeRef: "payee-1", Amount: 2400.4, Currency: "GBP", ValueDate: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), Reference: "Payroll 2026-09",
	}
}

func TestHandoffPreparesIdempotentlyAndSubmits(t *testing.T) {
	var prepared map[string]any
	var submitted bool
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bnk06/attempts":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &prepared)
			_, _ = w.Write([]byte(`{"AttemptID":"att-1","Status":"PREPARED"}`))
		case "/bnk06/attempts/att-1/submit":
			submitted = true
			_, _ = w.Write([]byte(`{"AttemptID":"att-1","Status":"SUBMITTED"}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	a, err := NewBankingHandoff(url, nil).Handoff(context.Background(), handoffReq())
	require.NoError(t, err)
	assert.Equal(t, "att-1", a.AttemptID)
	assert.Equal(t, "SUBMITTED", a.Status)
	assert.True(t, submitted)
	assert.Equal(t, "PAYROLL_RUN_SVC", prepared["AuthorizationSource"])
	assert.Equal(t, "i-1", prepared["AuthorizationID"])
	assert.Equal(t, "sha256:approved", prepared["AuthorizationFingerprint"], "the APPROVED fingerprint is what Banking re-verifies")
	assert.Equal(t, "wfp-payroll-instruction:i-1", prepared["IdempotencyKey"], "deterministic: a retry reuses the attempt")
	assert.Equal(t, "i-1", prepared["SourceReference"], "Banking keeps the instruction as the payment's source reference")
	assert.Equal(t, 2400.4, prepared["Amount"])
	assert.Equal(t, "payee-1", prepared["PayeeRef"])
	assert.NotContains(t, prepared, "AccountIdentifier")
}

func TestHandoffOutcomes(t *testing.T) {
	// Banking already has it submitted (a retry): no second submit.
	calls := 0
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"AttemptID":"att-1","Status":"SUBMITTED"}`))
	})
	a, err := NewBankingHandoff(url, nil).Handoff(context.Background(), handoffReq())
	require.NoError(t, err)
	assert.Equal(t, "SUBMITTED", a.Status)
	assert.Equal(t, 1, calls, "an attempt that is already submitted is not submitted again")

	// The submit call fails after the attempt exists: the attempt is returned, still PREPARED (unconfirmed).
	url = server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bnk06/attempts" {
			_, _ = w.Write([]byte(`{"AttemptID":"att-1","Status":"PREPARED"}`))
			return
		}
		w.WriteHeader(504)
	})
	a, err = NewBankingHandoff(url, nil).Handoff(context.Background(), handoffReq())
	require.NoError(t, err)
	assert.Equal(t, "PREPARED", a.Status, "unconfirmed is not handed off")

	// Banking refuses before any attempt exists.
	_, err = NewBankingHandoff(server(t, reply(422, `{"error":"payer account is not ACTIVE"}`)), nil).Handoff(context.Background(), handoffReq())
	var refused *ErrRefused
	require.True(t, errors.As(err, &refused))
	assert.Contains(t, refused.Reason, "payer account")

	// Unreachable / overloaded / garbage: unknown, never failed.
	for name, u := range map[string]string{"dead": deadURL(), "503": server(t, reply(503, ``)), "429": server(t, reply(429, ``)), "garbage": server(t, reply(200, `nope`)), "no id": server(t, reply(200, `{"Status":"PREPARED"}`))} {
		_, err := NewBankingHandoff(u, nil).Handoff(context.Background(), handoffReq())
		assert.ErrorIs(t, err, domain.ErrHandoffUnavailable, name)
	}
}
