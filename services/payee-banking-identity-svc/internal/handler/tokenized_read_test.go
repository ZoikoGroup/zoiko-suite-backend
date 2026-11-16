package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/handler"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
	"zoiko.io/payee-banking-identity-svc/internal/tokenization"
)

const rawIBAN = "GB29NWBK60161331926819"

// detokRecorder wraps the Fake, recording detokenize scopes; detokFail makes
// every detokenize fail with that error.
type detokRecorder struct {
	*tokenization.Fake
	mu        sync.Mutex
	scopes    []tokenization.Scope
	detokFail error
}

func (d *detokRecorder) Detokenize(ctx context.Context, scope tokenization.Scope, token string) (string, error) {
	d.mu.Lock()
	d.scopes = append(d.scopes, scope)
	d.mu.Unlock()
	if d.detokFail != nil {
		return "", d.detokFail
	}
	return d.Fake.Detokenize(ctx, scope, token)
}

type tokReadFixture struct {
	router                chi.Router
	tok                   *detokRecorder
	az                    *stubAuthz
	tokenizedID, legacyID string
	token                 string
}

// newTokReadFixture seeds an ACTIVE tokenized destination (party
// "party-tok"; account_identifier holds the token and a token row exists, as
// an enforced propose or the backfill leaves it) and an ACTIVE legacy raw one
// (party "party-raw").
func newTokReadFixture(t *testing.T, withTokenizer bool) *tokReadFixture {
	t.Helper()
	f := &tokReadFixture{tok: &detokRecorder{Fake: &tokenization.Fake{}}, az: &stubAuthz{}}
	token, err := f.tok.Tokenize(context.Background(), tokenization.Scope{TenantID: testTenant, PrincipalID: "writer", LegalEntityID: testLegalEntity},
		tokenization.ClassificationRestricted, rawIBAN)
	if err != nil {
		t.Fatal(err)
	}
	f.token = token
	st := newStubStore()
	for _, d := range []*domain.PayeeDestination{
		{DestinationID: "dest-tok", TenantID: strp(testTenant), LegalEntityID: testLegalEntity, PartyRef: "party-tok", Scope: "DEFAULT",
			AccountIdentifier: token, AccountIdentifierToken: token, AccountLast4: domain.Last4(rawIBAN), Status: domain.StatusActive},
		{DestinationID: "dest-raw", TenantID: strp(testTenant), LegalEntityID: testLegalEntity, PartyRef: "party-raw", Scope: "DEFAULT",
			AccountIdentifier: "DE89370400440532013000", AccountLast4: "3000", Status: domain.StatusActive},
	} {
		st.destinations[d.DestinationID] = d
	}
	f.tokenizedID, f.legacyID = "dest-tok", "dest-raw"

	h := handler.New(st, f.az, newStubParty(), zap.NewNop())
	if withTokenizer {
		h.WithTokenizer(f.tok)
	}
	f.router = chi.NewRouter()
	f.router.Use(middleware.TenantContext())
	handler.RegisterRoutes(f.router, h)
	return f
}

func (f *tokReadFixture) read(path, principal string) (*domain.PayeeDestination, http.Header, string, int) {
	w := doRequestAsWithHeaders(f.router, http.MethodGet, path, nil, testTenant, principal, map[string]string{"X-Correlation-ID": "corr-read"})
	var d domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	return &d, w.Header(), w.Body.String(), w.Code
}

// Every privileged read path — by id, and /org10/parties/{party}/active, the
// route payment-authorization-svc calls — returns the REAL identifier of a
// tokenized destination, detokenized under the reader's own principal.
func TestTokenizedRead_PrivilegedReaderGetsTheRealIdentifier(t *testing.T) {
	f := newTokReadFixture(t, true)
	for _, path := range []string{"/org10/destinations/" + f.tokenizedID, "/org10/parties/party-tok/active"} {
		d, hdr, body, code := f.read(path, "bank-detail-reader")
		if code != http.StatusOK || d.AccountIdentifier != rawIBAN || hdr.Get(handler.HeaderIdentifierWithheld) != "" {
			t.Fatalf("%s: %d %s; want the raw identifier", path, code, body)
		}
		if strings.Contains(body, f.token) {
			t.Fatalf("%s returned the token: %s", path, body)
		}
	}
	for _, s := range f.tok.scopes {
		if s.PrincipalID != "bank-detail-reader" || s.TenantID != testTenant || s.LegalEntityID != testLegalEntity || s.CorrelationID != "corr-read" {
			t.Fatalf("detokenized under %+v; want the reader's own principal, tenant and the destination's legal entity", s)
		}
	}
	if len(f.tok.scopes) != 2 {
		t.Fatalf("%d detokenize calls, want 2", len(f.tok.scopes))
	}
}

// The versions list reveals each tokenized version the same way.
func TestTokenizedRead_VersionsListRevealsForPrivileged(t *testing.T) {
	f := newTokReadFixture(t, true)
	w := doRequestAsWithHeaders(f.router, http.MethodGet, "/org10/parties/party-tok/versions", nil, testTenant, "bank-detail-reader", nil)
	var got struct{ Data []domain.PayeeDestination }
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || len(got.Data) != 1 || got.Data[0].AccountIdentifier != rawIBAN || strings.Contains(w.Body.String(), f.token) {
		t.Fatalf("versions: %d %s", w.Code, w.Body.String())
	}
}

// Not privileged: blank, exactly as before — no vault call, no withheld
// header, and never the token.
func TestTokenizedRead_UnprivilegedSeesNoIdentifier(t *testing.T) {
	f := newTokReadFixture(t, true)
	f.az.denyPrivileged = true
	d, hdr, body, code := f.read("/org10/destinations/"+f.tokenizedID, "ap-clerk")
	if code != http.StatusOK || d.AccountIdentifier != "" || d.AccountLast4 != "6819" || hdr.Get(handler.HeaderIdentifierWithheld) != "" ||
		strings.Contains(body, f.token) || len(f.tok.scopes) != 0 {
		t.Fatalf("%d %s (vault calls %d)", code, body, len(f.tok.scopes))
	}
}

// Privileged but the identifier cannot be revealed: the read still succeeds
// (payment-authorization-svc's eligibility check must not fail on a vault
// outage), the identifier is withheld with a reason, and the token never
// leaks.
func TestTokenizedRead_WithheldNotFailedWhenDetokenizeFails(t *testing.T) {
	cases := map[string]struct {
		withTokenizer bool
		fail          error
		reason        string
	}{
		"no TOKEN_DETOKENIZE at the vault": {true, tokenization.ErrDetokenizationDenied, "detokenize_denied"},
		"vault unavailable":                {true, tokenization.ErrTokenizationServiceUnavailable, "tokenization_unavailable"},
		"no tokenizer configured":          {false, nil, "tokenization_not_configured"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTokReadFixture(t, tc.withTokenizer)
			f.tok.detokFail = tc.fail
			for _, path := range []string{"/org10/destinations/" + f.tokenizedID, "/org10/parties/party-tok/active"} {
				d, hdr, body, code := f.read(path, "half-privileged")
				if code != http.StatusOK || d.AccountIdentifier != "" || d.Status != domain.StatusActive ||
					hdr.Get(handler.HeaderIdentifierWithheld) != tc.reason || strings.Contains(body, f.token) {
					t.Fatalf("%s: %d %s (withheld=%q); want 200, blank identifier, reason %s",
						path, code, body, hdr.Get(handler.HeaderIdentifierWithheld), tc.reason)
				}
			}
		})
	}
}

// A legacy raw row is unchanged for a privileged reader, without a vault call.
func TestTokenizedRead_LegacyRowUnchanged(t *testing.T) {
	f := newTokReadFixture(t, true)
	d, _, body, code := f.read("/org10/destinations/"+f.legacyID, "bank-detail-reader")
	if code != http.StatusOK || d.AccountIdentifier != "DE89370400440532013000" || len(f.tok.scopes) != 0 {
		t.Fatalf("%d %s (vault calls %d)", code, body, len(f.tok.scopes))
	}
}
