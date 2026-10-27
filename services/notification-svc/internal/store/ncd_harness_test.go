package store_test

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ncd"
	"zoiko.io/notification-svc/internal/store"
)

// The NCD suite runs as an unprivileged role. A superuser bypasses row-level
// security entirely — even FORCEd — so a suite connected as one cannot see an
// RLS defect; configuration-feature-flag-svc shipped a 503 on every tenant
// write for exactly that reason.
const ncdAppRole = "zoiko_ncd_app"

var (
	ncdOnce    sync.Once
	ncdAppPool *pgxpool.Pool
	ncdAdmin   *pgxpool.Pool
	ncdErr     error
)

func ncdPools(t *testing.T) (app, admin *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping NCD integration test: TEST_DATABASE_URL not set")
	}
	requireThrowawayDatabase(t, dsn)
	ncdOnce.Do(func() {
		ctx := context.Background()
		ncdAdmin, ncdErr = pgxpool.New(ctx, dsn)
		if ncdErr != nil {
			return
		}
		if _, ncdErr = ncdAdmin.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); ncdErr != nil {
			return
		}
		_, file, _, _ := runtime.Caller(0)
		names, _ := filepath.Glob(filepath.Join(filepath.Dir(file), "../../deployments/migrations/*.up.sql"))
		sort.Strings(names)
		for _, n := range names {
			raw, err := os.ReadFile(n)
			if err != nil {
				ncdErr = err
				return
			}
			if _, err := ncdAdmin.Exec(ctx, string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))); err != nil {
				ncdErr = &migrationErr{name: filepath.Base(n), err: err}
				return
			}
		}
		if _, ncdErr = ncdAdmin.Exec(ctx, `DO $do$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+ncdAppRole+`') THEN
				CREATE ROLE `+ncdAppRole+` LOGIN PASSWORD 'ncd' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
			END IF;
		END $do$;
		GRANT USAGE ON SCHEMA public TO `+ncdAppRole+`;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+ncdAppRole+`;
		GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO `+ncdAppRole+`;`); ncdErr != nil {
			return
		}
		u, _ := url.Parse(dsn)
		u.User = url.UserPassword(ncdAppRole, "ncd")
		ncdAppPool, ncdErr = pgxpool.New(ctx, u.String())
	})
	if ncdErr != nil {
		t.Fatalf("NCD test setup: %v", ncdErr)
	}
	return ncdAppPool, ncdAdmin
}

type migrationErr struct {
	name string
	err  error
}

func (m *migrationErr) Error() string { return m.name + ": " + m.err.Error() }

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeResolver struct{ emails map[string]string }

func (f *fakeResolver) ResolveEmail(_ context.Context, _, _, recipient string) (string, error) {
	switch e, ok := f.emails[recipient]; {
	case !ok:
		return "", domain.ErrPrincipalNotFound
	case e == "":
		return "", domain.ErrPrincipalHasNoAddress
	default:
		return e, nil
	}
}

// fakeEmail scripts the SMTP router's answers.
type fakeEmail struct {
	mu      sync.Mutex
	sent    []domain.Notification
	outcome func(n domain.Notification) domain.DeliveryOutcome
}

func (f *fakeEmail) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, n)
	if f.outcome != nil {
		return f.outcome(n)
	}
	return domain.DeliveryOutcome{Delivered: true, ProviderName: "smtp",
		ProviderResponse: "smtp test accepted; message-id=<" + n.NotificationID + "@test>"}
}

func (f *fakeEmail) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// harness is one test's view: its own tenant, service and fakes.
type harness struct {
	t        *testing.T
	ctx      context.Context
	svc      *ncd.Service
	store    *store.NCDStore
	admin    *pgxpool.Pool
	email    *fakeEmail
	resolver *fakeResolver
	clock    *clock
	tenant   string
	entity   string
	author   ncd.Actor
	approver ncd.Actor
}

func newHarness(t *testing.T) *harness {
	app, admin := ncdPools(t)
	st := store.NewNCD(store.New(app))
	em := &fakeEmail{}
	rs := &fakeResolver{emails: map[string]string{}}
	svc := ncd.NewService(st, rs, ncd.RouterTransport{Email: em, Inbox: st}, ncd.DefaultLimits(), nil)
	clk := &clock{t: time.Now().UTC()}
	svc.SetClock(clk.now)
	tenant := "tenant-" + uuid.NewString()[:8]
	h := &harness{t: t, ctx: context.Background(), svc: svc, store: st, admin: admin, email: em, resolver: rs, clock: clk,
		tenant: tenant, entity: "le-" + uuid.NewString()[:8]}
	h.author = ncd.Actor{TenantID: tenant, PrincipalID: "alice", CorrelationID: "corr-a"}
	h.approver = ncd.Actor{TenantID: tenant, PrincipalID: "bob", CorrelationID: "corr-b"}
	return h
}

func (h *harness) as(principal string) ncd.Actor {
	return ncd.Actor{TenantID: h.tenant, PrincipalID: principal, CorrelationID: "corr-" + principal}
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatalf("unexpected error: %v", err)
	}
}

// refusal asserts err is a coded refusal with code.
func (h *harness) refusal(err error, code ncd.ReasonCode) {
	h.t.Helper()
	if err == nil {
		h.t.Fatalf("expected refusal %s, got success", code)
	}
	e := ncd.AsError(err)
	if e.Refusal == nil || e.Refusal.Code != code {
		h.t.Fatalf("expected refusal %s, got %v", code, err)
	}
}

func (h *harness) kind(err error, k ncd.Kind, code string) {
	h.t.Helper()
	if err == nil {
		h.t.Fatalf("expected %s, got success", code)
	}
	e := ncd.AsError(err)
	if e.Kind != k || (code != "" && e.Code != code) {
		h.t.Fatalf("expected kind %d code %s, got %v (kind %d)", k, code, err, e.Kind)
	}
}

// intent creates and activates an intent (author ≠ approver) and publishes
// the given templates. Each template is "CHANNEL|locale|subject|body".
func (h *harness) intent(mod func(*ncd.IntentInput), templates ...[4]string) *ncd.Intent {
	h.t.Helper()
	in := ncd.IntentInput{
		LegalEntityID: h.entity, IntentCode: "invoice.issued", DisplayName: "Invoice issued",
		PurposeClass: ncd.PurposeTransactional, DomainOwner: "billing", Sensitivity: "S1", Urgency: "U1",
		EvidenceClass: "E1", AllowedChannels: []string{"EMAIL", "IN_APP"}, FallbackAllowed: true,
		VariableContract: []ncd.VariableSpec{
			{Name: "invoice_no", Type: "string", Required: true, Sensitivity: "S1"},
			{Name: "amount", Type: "money", Required: false, Sensitivity: "S2", FallbackText: "see your account"},
		},
	}
	if mod != nil {
		mod(&in)
	}
	i, err := h.svc.CreateIntent(h.ctx, h.author, in)
	h.must(err)
	i, err = h.svc.ActivateIntent(h.ctx, h.approver, i.IntentID, 1, nil)
	h.must(err)
	for _, tp := range templates {
		h.publish(i.IntentID, tp[0], tp[1], tp[2], tp[3], nil)
	}
	return i
}

func (h *harness) publish(intentID, channel, locale, subject, body string, compat []string) *ncd.TemplateVersion {
	h.t.Helper()
	tv, err := h.svc.CreateTemplate(h.ctx, h.author, ncd.TemplateInput{IntentID: intentID, Channel: channel, Locale: locale,
		Subject: subject, Body: body, CompatibleLocales: compat})
	h.must(err)
	tv, err = h.svc.ValidateTemplate(h.ctx, h.author, tv.TemplateVersionID)
	h.must(err)
	if tv.Status != ncd.TemplateReview {
		h.t.Fatalf("template did not pass validation: %+v", tv.ValidationReport)
	}
	tv, err = h.svc.ApproveTemplate(h.ctx, h.approver, tv.TemplateVersionID)
	h.must(err)
	tv, err = h.svc.PublishTemplate(h.ctx, h.approver, tv.TemplateVersionID, nil)
	h.must(err)
	return tv
}

var (
	emailInvoice = [4]string{"EMAIL", "en-GB", "Invoice {{invoice_no}} issued", "<p>Your invoice {{invoice_no}} is ready. Sign in to see the amount.</p>"}
	inAppInvoice = [4]string{"IN_APP", "en-GB", "Invoice {{invoice_no}}", "<p>Invoice {{invoice_no}} for {{amount}}</p>"}
)

// comm creates a communication for recipient with default variables.
func (h *harness) comm(intentID, recipient string, mod func(*ncd.CommunicationInput)) *ncd.Communication {
	h.t.Helper()
	in := ncd.CommunicationInput{IntentID: intentID, LegalEntityID: h.entity, RecipientPrincipalID: recipient,
		Locale: "en-GB", Variables: map[string]string{"invoice_no": "INV-1"}, SourceEventID: "evt-" + uuid.NewString()}
	if mod != nil {
		mod(&in)
	}
	c, _, err := h.svc.CreateCommunication(h.ctx, h.author, in)
	h.must(err)
	return c
}

// send prepares and dispatches, then runs the worker once.
func (h *harness) send(c *ncd.Communication) *ncd.CommunicationView {
	h.t.Helper()
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal != nil {
		h.t.Fatalf("prepare refused: %+v", p.Refusal)
	}
	_, _, err = h.svc.Dispatch(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	h.svc.RunOnce(h.ctx)
	return h.view(c.CommunicationID)
}

func (h *harness) view(id string) *ncd.CommunicationView {
	h.t.Helper()
	v, err := h.svc.GetCommunication(h.ctx, h.author, id)
	h.must(err)
	return v
}

func (h *harness) recipient(email string) string {
	id := "rcpt-" + uuid.NewString()[:8]
	h.resolver.emails[id] = email
	return id
}
