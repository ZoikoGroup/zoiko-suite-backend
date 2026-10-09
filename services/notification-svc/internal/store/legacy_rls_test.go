package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/quota"
	"zoiko.io/notification-svc/internal/store"
)

// TestLegacyTables_RLSIsolatesTenantsAsTheAppRole proves row-level security on
// every legacy tenant table, read directly as the unprivileged role (audit gap
// G-14, ZS-SVC-Y-001 §11.3).
//
// The store tests could not prove this even once they stopped running as a
// superuser: every legacy store query also filters on tenant_id in its own
// WHERE clause, so with each tenant policy rewritten to USING (true) the whole
// suite still passed. RLS was only ever exercised by the outbox test. Here the
// table list comes from the catalog — every non-NCD table with a tenant_id
// column — so a table added later is covered without anyone remembering to
// add it, and the reads bypass the store's own filter entirely.
func TestLegacyTables_RLSIsolatesTenantsAsTheAppRole(t *testing.T) {
	app, admin := openTestPools(t)
	s := store.New(app)
	ctx := context.Background()
	tenantA, tenantB := "tenant-a-"+uuid.NewString()[:8], "tenant-b-"+uuid.NewString()[:8]
	actx := tenantCtx(tenantA)
	now := time.Now().UTC()

	// ── Seed tenant A through the real write paths, as the app role ──────────
	n := seedNotification(t, s, tenantA, "corr-rls-"+uuid.NewString())
	must(t, s.CompleteDelivery(actx, n.NotificationID, "SENT", "", "accepted", &now, "corr-rls", domain.AttemptMeta{}))

	intentID, renderID, attemptID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, _, err := s.CreateMessageIntent(actx, &ledger.MessageIntent{
		MessageIntentID: intentID, TenantID: tenantA, LegalEntityID: "entity-1", RecipientPrincipalID: "usr-1",
		RecipientEmail: "rls@example.com", Channel: "EMAIL", CommunicationClass: ledger.ClassS0, TemplateKey: "ZS-IA-001",
		EventID: "evt-rls", SourceEventType: "identity.password_reset_requested", DeduplicationKey: "dedup-rls",
		CorrelationID: "corr-rls", Status: ledger.IntentStatusDispatched, CreatedAt: now, UpdatedAt: now,
	})
	must(t, err)
	must(t, s.RecordMessageRender(actx, &ledger.MessageRender{
		RenderID: renderID, MessageIntentID: intentID, TenantID: tenantA, TemplateKey: "ZS-IA-001",
		TemplateVersion: "1.0.0", Locale: "en-US", ContentHash: "hash-rls", Subject: "s", BodyHTML: "<p>b</p>",
		BodyText: "b", RenderedAt: now,
	}))
	must(t, s.RecordDeliveryAttempt(actx, &ledger.DeliveryAttempt{
		ProviderAttemptID: attemptID, MessageIntentID: intentID, TenantID: tenantA, RenderID: renderID,
		SenderStream: ledger.StreamCritical, FromAddress: "security@security.zoikosuite.com", ToAddress: "rls@example.com",
		ProviderName: "smtp", AttemptNumber: 1, Status: ledger.AttemptStatusAccepted, AttemptedAt: now,
	}))
	must(t, s.RecordDeliveryEvent(actx, &ledger.DeliveryEvent{
		DeliveryEventID: uuid.NewString(), MessageIntentID: intentID, TenantID: tenantA, ProviderAttemptID: attemptID,
		EventType: ledger.DeliveryEventDelivered, RawPayload: json.RawMessage(`{"code":250}`), OccurredAt: now,
	}))
	must(t, s.AddSuppression(actx, &ledger.EmailSuppression{
		SuppressionID: uuid.NewString(), TenantID: tenantA, RecipientEmail: "rls@example.com",
		Reason: ledger.SuppressionReasonUnsubscribe, SourceStream: "ALL", CreatedAt: now,
	}))
	must(t, s.CreateActionToken(actx, &ledger.ActionToken{
		TokenID: uuid.NewString(), TokenHash: "hash-rls-" + uuid.NewString(), MessageIntentID: intentID, TenantID: tenantA,
		RecipientPrincipalID: "usr-1", Purpose: "VERIFY_EMAIL", TargetActionURL: "https://app.zoiko.com/verify",
		TargetMethod: "POST", Payload: json.RawMessage(`{}`), Status: ledger.ActionTokenStatusActive,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}))
	tmpl := newTestTemplate(t, s, actx, "owner-1")
	_, err = s.CreateVersion(actx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>hi</p>", CreatedByPrincipalID: "owner-1",
	})
	must(t, err)
	// No store method writes these two outside an HTTP request, so insert
	// them as the app role under tenant A — which also exercises each
	// policy's WITH CHECK.
	asTenant(t, app, tenantA, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (tenant_id, endpoint, idempotency_key, request_fingerprint)
			VALUES ($1, 'POST /v1/notifications', $2, 'fp')`, tenantA, uuid.NewString()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO webhook_dlq (dlq_id, tenant_id, provider_name, raw_payload, error_reason)
			VALUES ($1, $2, 'smtp', '{}', 'rls test')`, uuid.NewString(), tenantA)
		return err
	})

	// The register-side tables merged from main on 7 Oct (intent registry,
	// regulated notices and their events and acknowledgements, delivery
	// evidence, preferences, send quotas), seeded through their real write
	// paths as the app role.
	iv := regulatedIntentVersion(t, s, tenantA, "legal.rls_"+uuid.NewString()[:8])
	notice, err := s.CreateNotice(actx, noticeParams(iv, domain.AckReceipt, now.Add(72*time.Hour)))
	must(t, err)
	_, noticeAttempt := deliverNotice(t, s, tenantA, notice, "<rls-"+uuid.NewString()+"@example.com>")
	mailboxAccepted(t, s, tenantA, noticeAttempt, domain.EvidenceMailboxAccepted)
	_, err = s.RefreshNotice(actx, notice.NoticeID, now)
	must(t, err)
	_, _, err = s.RecordNoticeAck(actx, notice.NoticeID, noticeRecipient, domain.ActionAcknowledge, "rls", time.Now())
	must(t, err)
	quietStart, quietEnd := "22:00", "07:00"
	_, err = s.SetPreferences(actx, domain.SetPreferencesParams{PrincipalID: "usr-1", TimeZone: "Europe/London",
		QuietStart: &quietStart, QuietEnd: &quietEnd, MutedChannels: []string{}, UpdatedBy: "usr-1"})
	must(t, err)
	// Counters are written only when quotas are on, so count through a quota-enabled store.
	must(t, send(store.New(app).WithQuota(quota.Limits{RecipientPerHour: 100}), tenantA, "corr-rls-quota-"+uuid.NewString(), "usr-quota", "T0"))

	// ── Every legacy tenant table, from the catalog ──────────────────────────
	tables := legacyTenantTables(t, admin)
	if len(tables) < 13 {
		t.Fatalf("found %d legacy tenant tables, want at least the 13 known ones: %v", len(tables), tables)
	}
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			var enabled, forced bool
			if err := admin.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class
				WHERE oid = $1::regclass`, table).Scan(&enabled, &forced); err != nil {
				t.Fatal(err)
			}
			if !enabled || !forced {
				t.Fatalf("row-level security enabled=%v forced=%v, want both", enabled, forced)
			}
			// Ground truth, read past RLS.
			var truth int
			if err := admin.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+
				` WHERE tenant_id = $1`, tenantA).Scan(&truth); err != nil {
				t.Fatal(err)
			}
			if truth == 0 {
				t.Fatalf("no tenant-A row was seeded into %s, so isolation would pass vacuously; seed it above", table)
			}
			count := func(setting, value string) int {
				t.Helper()
				var c int
				err := asSetting(t, app, setting, value, func(tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+
						` WHERE tenant_id = $1`, tenantA).Scan(&c)
				})
				// A session with no tenant may be refused outright rather
				// than shown nothing; either way it sees no row.
				if err != nil && value != "" {
					t.Fatalf("read as %s=%q: %v", setting, value, err)
				}
				return c
			}
			if got := count("app.tenant_id", tenantB); got != 0 {
				t.Errorf("tenant B sees %d of tenant A's rows", got)
			}
			if got := count("app.tenant_id", ""); got != 0 {
				t.Errorf("a session with no tenant sees %d of tenant A's rows", got)
			}
			if got := count("app.tenant_id", tenantA); got != truth {
				t.Errorf("tenant A sees %d of its own %d rows", got, truth)
			}
		})
	}
}

// legacyTenantTables lists every table outside the NCD plane that carries a
// tenant_id. The NCD tables have their own isolation tests.
func legacyTenantTables(t *testing.T, admin *pgxpool.Pool) []string {
	t.Helper()
	rows, err := admin.Query(context.Background(), `
		SELECT c.table_name FROM information_schema.columns c
		JOIN information_schema.tables tb ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
		WHERE c.table_schema = 'public' AND c.column_name = 'tenant_id' AND tb.table_type = 'BASE TABLE'
		  AND c.table_name NOT LIKE 'ncd\_%'
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return tables
}

// asSetting runs fn in a transaction with one GUC set locally, so nothing
// leaks to the next use of the pooled connection, and returns fn's error.
func asSetting(t *testing.T, pool *pgxpool.Pool, setting, value string, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", setting, value); err != nil {
		t.Fatal(err)
	}
	return fn(tx)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
