package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// callLog records the order of register and transport calls, because the order is
// the safety property: the row exists, is linked and is marked BEFORE the provider
// is called, and the outcome is recorded after.
type callLog struct{ calls []string }

func (c *callLog) add(s string) { c.calls = append(c.calls, s) }

type fakeRegister struct {
	log       *callLog
	created   []*domain.Notification
	createErr error
	createdOK bool // set false to simulate "already exists"
	linkErr   error
	beginErr  error
	completed []completion
	unknown   []string
	linked    [][2]string
}

type completion struct{ status, failure, response string }

func (f *fakeRegister) CreateNotification(_ context.Context, n *domain.Notification) (bool, error) {
	f.log.add("create")
	if f.createErr != nil {
		return false, f.createErr
	}
	f.created = append(f.created, n)
	return f.createdOK, nil
}
func (f *fakeRegister) LinkIntentToNotification(_ context.Context, intentID, notificationID string) error {
	f.log.add("link")
	f.linked = append(f.linked, [2]string{intentID, notificationID})
	return f.linkErr
}
func (f *fakeRegister) BeginSubmission(_ context.Context, _, _ string, _ time.Time) error {
	f.log.add("begin")
	return f.beginErr
}
func (f *fakeRegister) CompleteDelivery(_ context.Context, _, status, failure, response string, _ *time.Time, _ string, _ domain.AttemptMeta) error {
	f.log.add("complete:" + status)
	f.completed = append(f.completed, completion{status, failure, response})
	return nil
}
func (f *fakeRegister) MarkOutcomeUnknown(_ context.Context, _, _, reason string, _ time.Time, _ string, _ domain.AttemptMeta) error {
	f.log.add("unknown")
	f.unknown = append(f.unknown, reason)
	return nil
}

type loggingDeliverer struct {
	log     *callLog
	outcome domain.DeliveryOutcome
	got     domain.Notification
}

func (d *loggingDeliverer) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	d.log.add("deliver")
	d.got = n
	return d.outcome
}

func registered(t *testing.T, outcome domain.DeliveryOutcome, tweak func(*fakeRegister)) (*ledger.Orchestrator, *mockLedgerStore, *fakeRegister, *loggingDeliverer, *callLog) {
	t.Helper()
	log := &callLog{}
	store := newMockLedgerStore()
	compiler := ledger.NewCompiler()
	for _, seed := range ledger.DefaultSeedDefinitions() {
		if err := compiler.Register(seed); err != nil {
			t.Fatal(err)
		}
	}
	reg := &fakeRegister{log: log, createdOK: true}
	if tweak != nil {
		tweak(reg)
	}
	del := &loggingDeliverer{log: log, outcome: outcome}
	orc := ledger.NewOrchestrator(store, compiler, ledger.NewKillSwitchManager(zap.NewNop()), del,
		&mockRecipientResolver{email: "resolved@example.com"}, zap.NewNop()).WithRegister(reg)
	return orc, store, reg, del, log
}

func regReq() ledger.EventIngestRequest {
	return ledger.EventIngestRequest{
		EventID: "evt-reg-1", EventType: "identity.password_reset_requested", RecipientPrincipalID: "usr-001",
		LegalEntityID: "entity-001", TemplateKey: "ZS-IA-001", CorrelationID: "corr-reg-1",
		Variables: map[string]string{
			"recipient.first_name": "Alice", "recipient.email_masked": "a***@example.com",
			"links.action_url": "https://auth.zoiko.com/verify?token=xyz", "security.link_expires_at_local": "15 minutes",
			"message.reference": "REF-1",
		},
	}
}

func ctxFor(tenant string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenant)
}

func TestRegister_SuccessfulDeliveryIsRegisteredLinkedMarkedThenConcluded(t *testing.T) {
	orc, _, reg, del, log := registered(t, domain.DeliveryOutcome{Delivered: true, ProviderResponse: "smtp h accepted; message-id=<a@b>", ProviderName: "p"}, nil)
	res, err := orc.IngestEvent(ctxFor("tenant-r"), regReq(), "caller-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"create", "link", "begin", "deliver", "complete:SENT"}
	if strings.Join(log.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", log.calls, want)
	}
	row := reg.created[0]
	if row.IdempotencyKey == "" || !strings.HasPrefix(row.IdempotencyKey, "ledger:") || row.PurposeContext != "ZS-IA-001" {
		t.Errorf("the row must be idempotent per event and name its template: key=%q purpose=%q", row.IdempotencyKey, row.PurposeContext)
	}
	if row.RecipientAddress != "resolved@example.com" || row.RecipientAddressSource != domain.AddressSourceIdentityContext {
		t.Errorf("the address needs its provenance: %q %q", row.RecipientAddress, row.RecipientAddressSource)
	}
	if reg.linked[0][0] != res.MessageIntentID || reg.linked[0][1] != row.NotificationID {
		t.Errorf("the intent and the row must be linked to each other: %v", reg.linked)
	}
	if del.got.NotificationID != row.NotificationID {
		t.Errorf("the transport must be handed the register id, got %q want %q", del.got.NotificationID, row.NotificationID)
	}
	if reg.completed[0].response == "" {
		t.Errorf("the provider receipt belongs on the concluded row: %+v", reg.completed[0])
	}
}

// A failed delivery CONCLUDES the row: the ledger path has no retry, so nothing is
// scheduled for the direct path's retry worker to pick up.
func TestRegister_FailedDeliveryConcludesAndIsNeverScheduled(t *testing.T) {
	orc, _, reg, _, log := registered(t, domain.DeliveryOutcome{Reason: "421 try later", Retryable: true}, nil)
	if _, err := orc.IngestEvent(ctxFor("tenant-r"), regReq(), "caller-1"); err != nil {
		t.Fatal(err)
	}
	if got := log.calls[len(log.calls)-1]; got != "complete:FAILED" {
		t.Fatalf("last call = %q, want complete:FAILED (%v)", got, log.calls)
	}
	if reg.completed[0].failure != "421 try later" {
		t.Errorf("failure reason lost: %+v", reg.completed[0])
	}
}

func TestRegister_AmbiguousOutcomeBecomesUnknownNeverFailed(t *testing.T) {
	orc, _, reg, _, log := registered(t, domain.DeliveryOutcome{Reason: "timeout after DATA", Unknown: true}, nil)
	if _, err := orc.IngestEvent(ctxFor("tenant-r"), regReq(), "caller-1"); err != nil {
		t.Fatal(err)
	}
	if len(reg.unknown) != 1 || len(reg.completed) != 0 {
		t.Fatalf("an ambiguous outcome must be PENDING_UNKNOWN, not concluded: unknown=%v completed=%v", reg.unknown, reg.completed)
	}
	if log.calls[len(log.calls)-1] != "unknown" {
		t.Errorf("calls = %v", log.calls)
	}
}

// If the communication cannot be recorded, nothing is sent.
func TestRegister_FailureBeforeSendMeansNothingIsSent(t *testing.T) {
	cases := map[string]func(*fakeRegister){
		"create fails":      func(f *fakeRegister) { f.createErr = errors.New("db down") },
		"already exists":    func(f *fakeRegister) { f.createdOK = false },
		"link fails":        func(f *fakeRegister) { f.linkErr = errors.New("conflict") },
		"marker not stored": func(f *fakeRegister) { f.beginErr = errors.New("db down") },
	}
	for name, tweak := range cases {
		orc, store, _, del, log := registered(t, domain.DeliveryOutcome{Delivered: true}, tweak)
		_, err := orc.IngestEvent(ctxFor("tenant-r"), regReq(), "caller-1")
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
		for _, c := range log.calls {
			if c == "deliver" {
				t.Errorf("%s: the provider was called although the communication was not recorded (%v)", name, log.calls)
			}
		}
		if del.got.NotificationID != "" {
			t.Errorf("%s: transport received a notification", name)
		}
		for _, in := range store.intents {
			if in.Status != ledger.IntentStatusFailed {
				t.Errorf("%s: the intent should be FAILED, got %s", name, in.Status)
			}
			break
		}
	}
}

// Off by default: without a register the pipeline behaves exactly as before.
func TestRegister_AbsentMeansNoRegisterCalls(t *testing.T) {
	orc, _, deliverer, _ := setupTestOrchestrator(t)
	if _, err := orc.IngestEvent(ctxFor("tenant-r"), regReq(), "caller-1"); err != nil {
		t.Fatal(err)
	}
	if deliverer.deliverCalls != 1 {
		t.Fatalf("delivery calls = %d", deliverer.deliverCalls)
	}
	if deliverer.lastReceived.IdempotencyKey != "" {
		t.Errorf("no register row should be described without a register: %+v", deliverer.lastReceived)
	}
}

// A replayed event never reaches the register.
func TestRegister_ReplayDoesNotRegisterAgain(t *testing.T) {
	orc, _, reg, _, _ := registered(t, domain.DeliveryOutcome{Delivered: true, ProviderResponse: "ok"}, nil)
	if _, err := orc.IngestEvent(ctxFor("tenant-r"), regReq(), "caller-1"); err != nil {
		t.Fatal(err)
	}
	res, err := orc.IngestEvent(ctxFor("tenant-r"), regReq(), "caller-1")
	if err != nil || !res.IsReplay {
		t.Fatalf("replay: %v %+v", err, res)
	}
	if len(reg.created) != 1 {
		t.Errorf("register rows after a replay = %d, want 1", len(reg.created))
	}
}
