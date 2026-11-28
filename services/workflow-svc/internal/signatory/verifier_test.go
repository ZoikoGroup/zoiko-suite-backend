package signatory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/identity-envelope/govdecision"
)

const (
	tenant = "11111111-1111-4111-8111-111111111111"
	entity = "22222222-2222-4222-8222-222222222222"
	decID  = "33333333-3333-4333-8333-333333333333"
	hash   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func decision(now time.Time) *govdecision.Decision {
	return &govdecision.Decision{
		ContractVersion: "1.0", DecisionID: decID, RequestID: "r1", CorrelationID: "c1",
		Outcome: govdecision.SignatureRequired, OwningService: "policy-svc", PolicyFamily: "SIGNATORY_MATRIX",
		RuleID: "pv#band:B2", PolicyVersion: "1", PolicyVersionID: "44444444-4444-4444-8444-444444444444",
		TenantID: tenant, LegalEntityID: entity, ActorPrincipalID: "preparer", Action: "SIGN_CONTRACT",
		Resource: govdecision.Resource{Type: "contract", ID: "C-1", Version: "3"}, EvaluatedAt: now,
		ValidUntil: now.Add(15 * time.Minute), InputFingerprint: "sha256:f",
		ReasonCodes: []string{govdecision.ReasonSignatureQuorumIncomplete},
		RequiredActions: []govdecision.RequiredAction{{Kind: govdecision.ActionKindSignatureQuorum, MinDistinctSigners: 2,
			RequiredOffices: []govdecision.OfficeRequirement{{Office: "DIRECTOR", MinCount: 1}}, DocumentHash: hash}},
		EvidenceRef: "policy-svc:policy-evaluations/" + decID, UpstreamRefs: []string{},
	}
}

// policyServer serves one signed decision and the key set.
func policyServer(t *testing.T, raw []byte, s *govdecision.Signer, status int, seen *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/policy-decision-keys":
			_ = json.NewEncoder(w).Encode(govdecision.JWKS{Keys: []govdecision.JWK{s.JWK()}})
		case "/internal/v1/policy/decisions/" + decID:
			if seen != nil {
				*seen = r.Header.Clone()
			}
			w.WriteHeader(status)
			_, _ = w.Write(raw)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestVerify_TrustedDecisionWithEnvelopeHeaders(t *testing.T) {
	s, _ := govdecision.NewRandomSigner()
	raw, _ := s.Sign(decision(time.Now().UTC()))
	var seen http.Header
	srv := policyServer(t, raw, s, http.StatusOK, &seen)
	defer srv.Close()
	d, err := NewHTTPVerifier(srv.URL, nil).Verify(context.Background(), Request{
		TenantID: tenant, LegalEntityID: entity, DecisionID: decID, PrincipalID: "user-1", CorrelationID: "flow-1", RequireValid: true,
	})
	if err != nil || d.Outcome != govdecision.SignatureRequired {
		t.Fatalf("%+v %v", d, err)
	}
	if seen.Get("X-Tenant-Id") != tenant || seen.Get("X-Principal-Id") != "user-1" || seen.Get("X-Request-Id") == "" ||
		seen.Get("X-Correlation-ID") != "flow-1" || seen.Get("X-Source-Channel") == "" {
		t.Fatalf("policy-svc's input contract headers missing: %v", seen)
	}
}

// Doc 02 §2.3: anything untrustworthy is refused, whatever it says.
func TestVerify_UntrustedDecisionsRefused(t *testing.T) {
	now := time.Now().UTC()
	s, _ := govdecision.NewRandomSigner()
	other, _ := govdecision.NewRandomSigner()
	good, _ := s.Sign(decision(now))
	cases := map[string]struct {
		raw []byte
		req Request
	}{
		"tampered":       {raw: []byte(strings.Replace(string(good), `"SIGNATURE_REQUIRED"`, `"PERMIT"`, 1))},
		"other key":      {raw: func() []byte { r, _ := other.Sign(decision(now)); return r }()},
		"other entity":   {raw: good, req: Request{LegalEntityID: "55555555-5555-4555-8555-555555555555"}},
		"other resource": {raw: good, req: Request{Resource: govdecision.Resource{Type: "contract", ID: "C-1", Version: "4"}}},
		"other family":   {raw: func() []byte { d := decision(now); d.PolicyFamily = "SPEND_CONTROL"; r, _ := s.Sign(d); return r }()},
		"expired": {raw: func() []byte {
			d := decision(now.Add(-time.Hour))
			d.ValidUntil = now.Add(-time.Minute)
			r, _ := s.Sign(d)
			return r
		}()},
	}
	for name, c := range cases {
		srv := policyServer(t, c.raw, s, http.StatusOK, nil)
		r := c.req
		r.TenantID, r.DecisionID, r.RequireValid = tenant, decID, true
		if r.LegalEntityID == "" {
			r.LegalEntityID = entity
		}
		_, err := NewHTTPVerifier(srv.URL, nil).Verify(context.Background(), r)
		var u *UntrustedError
		if !errors.As(err, &u) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
		srv.Close()
	}
	// A release re-check may read a decision past its validity window.
	d := decision(now.Add(-time.Hour))
	d.ValidUntil = now.Add(-time.Minute)
	raw, _ := s.Sign(d)
	srv := policyServer(t, raw, s, http.StatusOK, nil)
	defer srv.Close()
	if _, err := NewHTTPVerifier(srv.URL, nil).Verify(context.Background(), Request{TenantID: tenant, LegalEntityID: entity, DecisionID: decID}); err != nil {
		t.Fatalf("expired decision without RequireValid: %v", err)
	}
}

func TestVerify_FailsClosed(t *testing.T) {
	s, _ := govdecision.NewRandomSigner()
	for status, want := range map[int]error{http.StatusNotFound: ErrNotFound, http.StatusForbidden: ErrUnavailable, http.StatusBadGateway: ErrUnavailable} {
		srv := policyServer(t, []byte(`{}`), s, status, nil)
		_, err := NewHTTPVerifier(srv.URL, nil).Verify(context.Background(), Request{TenantID: tenant, LegalEntityID: entity, DecisionID: decID})
		if !errors.Is(err, want) {
			t.Errorf("HTTP %d: %v, want %v", status, err, want)
		}
		srv.Close()
	}
	if _, err := NewHTTPVerifier("http://127.0.0.1:1", nil).Verify(context.Background(), Request{TenantID: tenant, DecisionID: decID}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable policy-svc: %v", err)
	}
}
