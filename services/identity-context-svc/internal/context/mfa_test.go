package context_test

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/mfa"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeMFAStore struct {
	mu         sync.Mutex
	principals map[string]*domain.Principal
	sessions   map[string]*domain.SessionContext
	factors    map[string]*domain.TOTPFactorSecret
	created    []string // factor ids in creation order
	decisions  []string // action:outcome:basis
	events     []outbox.Record
}

func newFakeMFAStore() *fakeMFAStore {
	return &fakeMFAStore{
		principals: map[string]*domain.Principal{},
		sessions:   map[string]*domain.SessionContext{},
		factors:    map[string]*domain.TOTPFactorSecret{},
	}
}

func (f *fakeMFAStore) FindByID(_ context.Context, id, tenant string) (*domain.Principal, error) {
	p := f.principals[id]
	if p == nil || p.TenantID != tenant {
		return nil, nil
	}
	return p, nil
}

func (f *fakeMFAStore) FindSessionContext(_ context.Context, id, tenant string) (*domain.SessionContext, error) {
	s := f.sessions[id]
	if s == nil || s.TenantID != tenant {
		return nil, nil
	}
	return s, nil
}

func (f *fakeMFAStore) HasActiveTOTP(_ context.Context, pid, tid string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.factors {
		if x.PrincipalID == pid && x.TenantID == tid && x.Status == domain.CredentialStatusActive {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeMFAStore) CreatePendingTOTP(_ context.Context, n domain.TOTPFactorSecret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.factors {
		if x.PrincipalID == n.PrincipalID && x.Status == domain.CredentialStatusPending {
			x.Status = domain.CredentialStatusRetired
		}
	}
	c := n
	f.factors[n.FactorID] = &c
	f.created = append(f.created, n.FactorID)
	return nil
}

func (f *fakeMFAStore) FindTOTPFactor(_ context.Context, id, pid, tid string) (*domain.TOTPFactorSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x := f.factors[id]
	if x == nil || x.PrincipalID != pid || x.TenantID != tid {
		return nil, domain.ErrCredentialNotFound
	}
	c := *x
	return &c, nil
}

func (f *fakeMFAStore) FindActiveTOTP(_ context.Context, pid, tid string) (*domain.TOTPFactorSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.factors {
		if x.PrincipalID == pid && x.TenantID == tid && x.Status == domain.CredentialStatusActive {
			c := *x
			return &c, nil
		}
	}
	return nil, domain.ErrCredentialNotFound
}

// AcceptTOTPCode mirrors the SQL compare-and-set.
func (f *fakeMFAStore) AcceptTOTPCode(_ context.Context, a domain.TOTPAcceptance, rec outbox.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	x := f.factors[a.FactorID]
	from := domain.CredentialStatusActive
	if a.Activate {
		from = domain.CredentialStatusPending
	}
	now := time.Now()
	if x == nil || x.PrincipalID != a.PrincipalID || x.Status != from ||
		(x.LockedUntil != nil && x.LockedUntil.After(now)) ||
		(x.LastUsedStep != nil && *x.LastUsedStep >= a.Step) {
		return domain.ErrTOTPReplayed
	}
	if a.Activate {
		for _, y := range f.factors {
			if y.PrincipalID == a.PrincipalID && y.Status == domain.CredentialStatusActive {
				y.Status = domain.CredentialStatusRetired
			}
		}
	}
	step := a.Step
	x.LastUsedStep, x.Status, x.FailedAttempts, x.LockedUntil = &step, domain.CredentialStatusActive, 0, nil
	f.decisions = append(f.decisions, a.Action+":ALLOWED:totp_verified")
	if rec.EventID != "" {
		f.events = append(f.events, rec)
	}
	return nil
}

func (f *fakeMFAStore) RecordTOTPFailure(_ context.Context, id, _, _, action, _ string, max int, lock time.Duration) (bool, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x := f.factors[id]
	x.FailedAttempts++
	locked := x.FailedAttempts >= max
	if locked {
		t := time.Now().Add(lock)
		x.LockedUntil = &t
	}
	f.decisions = append(f.decisions, action+":DENIED:totp_mismatch")
	return locked, x.FailedAttempts, nil
}

func (f *fakeMFAStore) RecordMFADecision(_ context.Context, _, _, action, outcome, basis, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, action+":"+outcome+":"+basis)
	return nil
}

func (f *fakeMFAStore) RetireTOTPFactors(_ context.Context, pid, tid, id, reason, _, action, _ string, rec outbox.Record) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, x := range f.factors {
		if x.PrincipalID == pid && x.TenantID == tid && (id == "" || x.FactorID == id) &&
			(x.Status == domain.CredentialStatusActive || x.Status == domain.CredentialStatusPending) {
			x.Status = domain.CredentialStatusRetired
			n++
		}
	}
	if n > 0 {
		f.decisions = append(f.decisions, action+":ALLOWED:totp_retired:"+reason)
		if rec.EventID != "" {
			f.events = append(f.events, rec)
		}
	}
	return n, nil
}

func (f *fakeMFAStore) ListMFAFactors(_ context.Context, pid, tid string) ([]domain.MFAFactor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []domain.MFAFactor{}
	for _, id := range f.created {
		x := f.factors[id]
		if x.PrincipalID == pid && x.TenantID == tid {
			out = append(out, domain.MFAFactor{FactorID: x.FactorID, PrincipalID: pid, Type: "TOTP", Status: x.Status})
		}
	}
	return out, nil
}

func (f *fakeMFAStore) eventTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.events {
		out = append(out, e.EventType)
	}
	return out
}

// fakeEnvelopes maps a bearer string to the envelope it "verifies" to.
type fakeEnvelopes map[string]*domain.IdentityContextEnvelope

func (f fakeEnvelopes) Verify(token string) (*domain.IdentityContextEnvelope, error) {
	if e, ok := f[token]; ok {
		c := *e
		return &c, nil
	}
	return nil, errors.New("bad signature")
}

type recordingStepUpEvents struct {
	mu      sync.Mutex
	reasons []string
}

func (r *recordingStepUpEvents) PublishStepUpFailed(_ context.Context, _, _, _, _, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
	return nil
}

// ── fixture ──────────────────────────────────────────────────────────────────

const (
	mfaTenant = "11111111-1111-1111-1111-111111111111"
	alice     = "P-ALICE"
	admin     = "P-ADMIN"
)

type mfaFixture struct {
	store  *fakeMFAStore
	envs   fakeEnvelopes
	issuer *mockTokenIssuer
	events *recordingStepUpEvents
	svc    *identityctx.MFAService
}

func newMFAFixture(t *testing.T) *mfaFixture {
	t.Helper()
	st := newFakeMFAStore()
	for _, id := range []string{alice, admin} {
		st.principals[id] = &domain.Principal{PrincipalID: id, TenantID: mfaTenant, PrincipalType: domain.PrincipalTypeHuman,
			IdentityProviderSubject: "idp|" + id, Email: strings.ToLower(id) + "@example.com", Status: domain.PrincipalStatusActive}
	}
	st.principals["P-SVC"] = &domain.Principal{PrincipalID: "P-SVC", TenantID: mfaTenant, PrincipalType: domain.PrincipalTypeServiceAccount, Status: domain.PrincipalStatusActive}
	kr, err := mfa.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	fx := &mfaFixture{store: st, envs: fakeEnvelopes{}, issuer: &mockTokenIssuer{}, events: &recordingStepUpEvents{}}
	fx.svc = identityctx.NewMFAService(zap.NewNop(), st, kr, fx.envs, fx.issuer, fx.events, nil, identityctx.MFAPolicy{
		MaxFailedAttempts: 3, LockDuration: time.Minute, RecentAuth: 15 * time.Minute, SkewSteps: 1,
	})
	return fx
}

// session registers a live session for principal and returns its bearer.
func (fx *mfaFixture) session(principal string, authAge time.Duration, mfaVerified bool) string {
	sid := "S-" + principal + "-" + authAge.String() + "-" + map[bool]string{true: "mfa", false: "pwd"}[mfaVerified]
	fx.store.sessions[sid] = &domain.SessionContext{SessionContextID: sid, PrincipalID: principal, TenantID: mfaTenant}
	env := &domain.IdentityContextEnvelope{
		Principal: domain.PrincipalClaims{PrincipalID: principal, TenantID: mfaTenant}, TenantID: mfaTenant,
		SessionTrustPosture: domain.SessionTrustClaims{SessionContextID: sid, MFAVerified: mfaVerified},
		AuthorizedParty:     "zoiko-console",
	}
	if authAge >= 0 {
		env.AuthTime = time.Now().Add(-authAge).Unix()
	}
	token := "bearer-" + sid
	fx.envs[token] = env
	return token
}

func (fx *mfaFixture) caller(t *testing.T, bearer string) *identityctx.MFACaller {
	t.Helper()
	c, err := fx.svc.Authenticate(context.Background(), bearer)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return c
}

func secretOf(t *testing.T, e *domain.TOTPEnrollment) []byte {
	t.Helper()
	s, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(e.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// enrolAndActivate gives principal an ACTIVE factor and returns its secret.
// The activation code is the previous window's, so a later step-up with the
// current window's code is not a replay.
func (fx *mfaFixture) enrolAndActivate(t *testing.T, principal string) []byte {
	t.Helper()
	c := fx.caller(t, fx.session(principal, time.Minute, false))
	e, err := fx.svc.Enrol(context.Background(), c, "corr-enrol")
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	sec := secretOf(t, e)
	if _, err := fx.svc.Activate(context.Background(), c, e.FactorID, mfa.Code(sec, mfa.Step(time.Now())-1), "corr-act"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	return sec
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestMFA_AuthenticateRefusesWhatIsNotALiveHumanSession(t *testing.T) {
	fx := newMFAFixture(t)
	ctx := context.Background()
	if _, err := fx.svc.Authenticate(ctx, "forged"); !errors.Is(err, identityctx.ErrMFASessionInvalid) {
		t.Fatalf("forged bearer: %v", err)
	}
	b := fx.session(alice, time.Minute, false)
	now := time.Now()
	fx.store.sessions["S-"+alice+"-1m0s-pwd"].InvalidatedAt = &now
	if _, err := fx.svc.Authenticate(ctx, b); !errors.Is(err, identityctx.ErrMFASessionInvalid) {
		t.Fatalf("invalidated session: %v", err)
	}
	b2 := fx.session(alice, 2*time.Minute, false)
	fx.store.principals[alice].Status = domain.PrincipalStatusSuspended
	if _, err := fx.svc.Authenticate(ctx, b2); !errors.Is(err, identityctx.ErrMFASessionInvalid) {
		t.Fatalf("suspended principal: %v", err)
	}
	if _, err := fx.svc.Authenticate(ctx, fx.session("P-SVC", time.Minute, false)); !errors.Is(err, identityctx.ErrMFASessionInvalid) {
		t.Fatalf("service account: %v", err)
	}
}

func TestMFA_FirstEnrolmentNeedsRecentLogin(t *testing.T) {
	fx := newMFAFixture(t)
	ctx := context.Background()
	if _, err := fx.svc.Enrol(ctx, fx.caller(t, fx.session(alice, time.Hour, false)), "c"); !errors.Is(err, identityctx.ErrReauthRequired) {
		t.Fatalf("stale login: %v", err)
	}
	// An envelope minted before auth_time existed is never fresh.
	if _, err := fx.svc.Enrol(ctx, fx.caller(t, fx.session(alice, -1, false)), "c"); !errors.Is(err, identityctx.ErrReauthRequired) {
		t.Fatalf("no auth_time: %v", err)
	}
	e, err := fx.svc.Enrol(ctx, fx.caller(t, fx.session(alice, time.Minute, false)), "c")
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != domain.CredentialStatusPending || e.Digits != 6 || e.Period != 30 || !strings.HasPrefix(e.OTPAuthURI, "otpauth://totp/") {
		t.Fatalf("enrolment: %+v", e)
	}
	stored := fx.store.factors[e.FactorID]
	if stored.Ciphertext == "" || strings.Contains(stored.Ciphertext, e.Secret) || stored.KeyID != "k1" {
		t.Fatal("secret not stored encrypted")
	}
}

func TestMFA_ActivateNeedsACodeFromThePendingFactor(t *testing.T) {
	fx := newMFAFixture(t)
	ctx := context.Background()
	c := fx.caller(t, fx.session(alice, time.Minute, false))
	e, _ := fx.svc.Enrol(ctx, c, "c")
	sec := secretOf(t, e)

	if _, err := fx.svc.Activate(ctx, c, e.FactorID, "000000", "c"); !errors.Is(err, identityctx.ErrInvalidCode) && mfa.Code(sec, mfa.Step(time.Now())) != "000000" {
		t.Fatalf("wrong code: %v", err)
	}
	if _, err := fx.svc.Activate(ctx, c, e.FactorID, "12ab56", "c"); !errors.Is(err, identityctx.ErrRequestInvalid) {
		t.Fatalf("malformed code: %v", err)
	}
	if _, err := fx.svc.Activate(ctx, c, "no-such-factor", mfa.Code(sec, mfa.Step(time.Now())), "c"); !errors.Is(err, identityctx.ErrMFAFactorNotFound) {
		t.Fatalf("unknown factor: %v", err)
	}
	f, err := fx.svc.Activate(ctx, c, e.FactorID, mfa.Code(sec, mfa.Step(time.Now())), "c")
	if err != nil || f.Status != domain.CredentialStatusActive {
		t.Fatalf("activate: %v %+v", err, f)
	}
	if got := fx.store.eventTypes(); len(got) != 1 || got[0] != "identity.mfa_factor.enrolled" {
		t.Fatalf("events: %v", got)
	}
	// A second activation of the same (now ACTIVE) factor is refused.
	if _, err := fx.svc.Activate(ctx, c, e.FactorID, mfa.Code(sec, mfa.Step(time.Now())), "c"); !errors.Is(err, identityctx.ErrMFAFactorNotFound) {
		t.Fatalf("re-activation: %v", err)
	}
}

func TestMFA_ReplacingAFactorNeedsAStepUp(t *testing.T) {
	fx := newMFAFixture(t)
	ctx := context.Background()
	fx.enrolAndActivate(t, alice)
	if _, err := fx.svc.Enrol(ctx, fx.caller(t, fx.session(alice, time.Minute, false)), "c"); !errors.Is(err, identityctx.ErrMFAStepUpRequired) {
		t.Fatalf("password-only replacement: %v", err)
	}
	if _, err := fx.svc.Enrol(ctx, fx.caller(t, fx.session(alice, time.Minute, true)), "c"); err != nil {
		t.Fatalf("replacement after step-up: %v", err)
	}
}

func TestMFA_StepUpMintsAnMFATokenOnceAndCarriesTheClient(t *testing.T) {
	fx := newMFAFixture(t)
	ctx := context.Background()
	c := fx.caller(t, fx.session(alice, time.Minute, false))

	if _, err := fx.svc.StepUp(ctx, c, "123456", "c"); !errors.Is(err, identityctx.ErrMFANotEnrolled) {
		t.Fatalf("not enrolled: %v", err)
	}
	sec := fx.enrolAndActivate(t, alice)
	code := mfa.Code(sec, mfa.Step(time.Now()))

	resp, err := fx.svc.StepUp(ctx, c, code, "c")
	if err != nil {
		t.Fatalf("step-up: %v", err)
	}
	last := fx.issuer.issued[len(fx.issuer.issued)-1]
	if !last.mfaDone || last.subject != "idp|"+alice || last.clientID != "zoiko-console" ||
		strings.Join(last.amr, ",") != "pwd,otp" || time.Since(last.authTime) > 5*time.Second {
		t.Fatalf("minted grant: %+v", last)
	}
	if !resp.MFAEnrolled || resp.AuthTime == 0 || strings.Join(resp.AMR, ",") != "pwd,otp" {
		t.Fatalf("response: %+v", resp)
	}
	// The same code again is a replay.
	if _, err := fx.svc.StepUp(ctx, c, code, "c"); !errors.Is(err, identityctx.ErrInvalidCode) {
		t.Fatalf("replayed code: %v", err)
	}
	if !contains(fx.store.decisions, "MFA_STEP_UP:DENIED:totp_replayed") {
		t.Fatalf("replay not evidenced: %v", fx.store.decisions)
	}
	if !contains(fx.store.eventTypes(), "identity.step_up.succeeded") {
		t.Fatal("step-up success event not written with the acceptance")
	}
}

func TestMFA_LockoutRefusesEvenTheRightCode(t *testing.T) {
	fx := newMFAFixture(t)
	ctx := context.Background()
	sec := fx.enrolAndActivate(t, alice)
	c := fx.caller(t, fx.session(alice, time.Minute, false))
	good := mfa.Code(sec, mfa.Step(time.Now()))
	bad := "000000"
	if bad == good {
		bad = "111111"
	}
	for i := 0; i < 3; i++ {
		if _, err := fx.svc.StepUp(ctx, c, bad, "c"); !errors.Is(err, identityctx.ErrInvalidCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := fx.svc.StepUp(ctx, c, good, "c"); !errors.Is(err, identityctx.ErrInvalidCode) {
		t.Fatalf("locked factor accepted the right code: %v", err)
	}
	if !contains(fx.store.decisions, "MFA_STEP_UP:DENIED:totp_factor_locked") {
		t.Fatalf("lock not evidenced: %v", fx.store.decisions)
	}
}

func TestMFA_RevokeAndResetNeedARecentStepUp(t *testing.T) {
	fx := newMFAFixture(t)
	ctx := context.Background()
	fx.enrolAndActivate(t, alice)
	factors, _ := fx.svc.List(ctx, fx.caller(t, fx.session(alice, time.Minute, false)))
	var active string
	for _, f := range factors {
		if f.Status == domain.CredentialStatusActive {
			active = f.FactorID
		}
	}
	if err := fx.svc.Revoke(ctx, fx.caller(t, fx.session(alice, time.Minute, false)), active, "lost", "c"); !errors.Is(err, identityctx.ErrMFAStepUpRequired) {
		t.Fatalf("password-only revoke: %v", err)
	}
	if err := fx.svc.Revoke(ctx, fx.caller(t, fx.session(alice, time.Hour, true)), active, "lost", "c"); !errors.Is(err, identityctx.ErrMFAStepUpRequired) {
		t.Fatalf("stale step-up revoke: %v", err)
	}
	if err := fx.svc.Revoke(ctx, fx.caller(t, fx.session(alice, time.Minute, true)), active, "lost", "c"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := fx.svc.Revoke(ctx, fx.caller(t, fx.session(alice, 2*time.Minute, true)), active, "lost", "c"); !errors.Is(err, identityctx.ErrMFAFactorNotFound) {
		t.Fatalf("second revoke: %v", err)
	}

	fx.enrolAndActivate(t, alice)
	if _, err := fx.svc.ResetFactors(ctx, fx.caller(t, fx.session(admin, time.Minute, false)), alice, "LOST_DEVICE", "", "c"); !errors.Is(err, identityctx.ErrMFAStepUpRequired) {
		t.Fatalf("admin reset without step-up: %v", err)
	}
	n, err := fx.svc.ResetFactors(ctx, fx.caller(t, fx.session(admin, time.Minute, true)), alice, "LOST_DEVICE", "ticket 42", "c")
	if err != nil || n != 1 {
		t.Fatalf("admin reset: n=%d %v", n, err)
	}
	if !contains(fx.store.eventTypes(), "identity.mfa_factor.reset") {
		t.Fatal("reset event missing")
	}
}

func TestMFA_UnconfiguredKeyIsUnavailableNotPermissive(t *testing.T) {
	fx := newMFAFixture(t)
	svc := identityctx.NewMFAService(zap.NewNop(), fx.store, nil, fx.envs, fx.issuer, nil, nil, identityctx.MFAPolicy{RecentAuth: time.Hour})
	c, err := svc.Authenticate(context.Background(), fx.session(alice, time.Minute, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enrol(context.Background(), c, "c"); !errors.Is(err, identityctx.ErrMFAUnavailable) {
		t.Fatalf("enrol: %v", err)
	}
	if _, err := svc.StepUp(context.Background(), c, "123456", "c"); !errors.Is(err, identityctx.ErrMFAUnavailable) {
		t.Fatalf("step-up: %v", err)
	}
}

// ── handler ──────────────────────────────────────────────────────────────────

func mfaRouter(fx *mfaFixture) chi.Router {
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, identityctx.NewHandler(nil, nil, nil, nil, allowAll{}, zap.NewNop()).WithMFA(fx.svc))
	return r
}

type allowAll struct{}

func (allowAll) CheckAllowed(context.Context, string, string, string, string) error { return nil }

func mfaDo(r chi.Router, method, path, bearer string, hdr map[string]string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMFAHandler_IdentityComesFromTheEnvelopeNotTheHeader(t *testing.T) {
	fx := newMFAFixture(t)
	r := mfaRouter(fx)
	b := fx.session(alice, time.Minute, false)

	if w := mfaDo(r, http.MethodPost, "/v1/mfa/totp/enroll", "", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", w.Code)
	}
	if w := mfaDo(r, http.MethodPost, "/v1/mfa/totp/enroll", b, map[string]string{"X-Principal-Id": admin}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("header naming someone else: %d %s", w.Code, w.Body)
	}
	if w := mfaDo(r, http.MethodPost, "/v1/mfa/totp/enroll", b, map[string]string{"X-Tenant-Id": "22222222-2222-2222-2222-222222222222"}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("foreign tenant header: %d", w.Code)
	}
	w := mfaDo(r, http.MethodPost, "/v1/mfa/totp/enroll", b, map[string]string{"X-Principal-Id": alice, "X-Tenant-Id": mfaTenant}, nil)
	if w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("enrol: %d cache=%q %s", w.Code, w.Header().Get("Cache-Control"), w.Body)
	}
}

func TestMFAHandler_StatusCodes(t *testing.T) {
	fx := newMFAFixture(t)
	r := mfaRouter(fx)
	b := fx.session(alice, time.Minute, false)

	if w := mfaDo(r, http.MethodPost, "/v1/authenticate/step-up", b, nil, map[string]string{"code": "123456"}); w.Code != http.StatusConflict {
		t.Fatalf("step-up unenrolled: %d", w.Code)
	}
	sec := fx.enrolAndActivate(t, alice)
	w := mfaDo(r, http.MethodPost, "/v1/authenticate/step-up", b, nil, map[string]string{"code": mfa.Code(sec, mfa.Step(time.Now()))})
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"amr":["pwd","otp"]`) {
		t.Fatalf("step-up: %d %s", w.Code, w.Body)
	}
	if w := mfaDo(r, http.MethodPost, "/v1/authenticate/step-up", b, nil, map[string]string{"code": "99999"}); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed code: %d", w.Code)
	}
	if w := mfaDo(r, http.MethodPost, "/v1/principals/"+admin+"/mfa/reset", fx.session(admin, time.Minute, true), nil, map[string]string{"reason_code": "X"}); w.Code != http.StatusForbidden {
		t.Fatalf("self reset: %d", w.Code)
	}
	if w := mfaDo(r, http.MethodPost, "/v1/principals/"+alice+"/mfa/reset", fx.session(admin, 3*time.Minute, true), nil, map[string]string{}); w.Code != http.StatusBadRequest {
		t.Fatalf("reset without reason: %d", w.Code)
	}
	if w := mfaDo(r, http.MethodPost, "/v1/principals/"+alice+"/mfa/reset", fx.session(admin, 4*time.Minute, true), nil, map[string]string{"reason_code": "LOST_DEVICE"}); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if w := mfaDo(r, http.MethodGet, "/v1/mfa/factors", b, nil, nil); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "ciphertext") {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
