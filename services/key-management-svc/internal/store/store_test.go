package store_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/key-management-svc/internal/domain"
	"zoiko.io/key-management-svc/internal/store"
	"zoiko.io/key-management-svc/internal/testpg"
)

const testDB = "kms_store_test"

// storeVault is what PgStore and MemoryStore both implement.
type storeVault interface {
	store.Store
	store.Vault
}

// backends is every Store/Vault implementation the conformance tests run
// against. MemoryStore always; PgStore when TEST_DATABASE_URL is set. The
// handler tests that predate Postgres still run on MemoryStore, so these
// tests are what shows the two behave the same.
func backends(*testing.T) map[string]func(t *testing.T) storeVault {
	return map[string]func(t *testing.T) storeVault{
		"memory":   func(*testing.T) storeVault { return store.NewMemoryStore() },
		"postgres": func(t *testing.T) storeVault { return store.NewPgStore(testpg.Open(t, testDB).App) },
	}
}

func newKey(le, alias string) *domain.CustomerKey {
	return &domain.CustomerKey{LegalEntityID: le, KeyAlias: alias, KeyModel: domain.ModelBYOK,
		KeyProvider: domain.ProviderAWSKMS, ExternalKeyARN: "arn:aws:kms:eu-west-2:111122223333:key/" + alias}
}

// The CustomerKey lifecycle the five /v1/keys routes depend on.
func TestConformance_KeyLifecycle(t *testing.T) {
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s, ctx, tenant := open(t), context.Background(), uuid.NewString()
			k := newKey("le-1", "finance")
			if err := s.CreateKey(ctx, tenant, k); err != nil {
				t.Fatal(err)
			}
			if _, err := uuid.Parse(k.ID); err != nil || k.TenantID != tenant || k.KeyVersion != 1 ||
				k.State != domain.StateEnabled || k.RotationCount != 0 || k.CreatedAt.IsZero() {
				t.Fatalf("created key: %+v", k)
			}
			got, err := s.GetKeyByID(ctx, tenant, k.ID)
			if err != nil || got.KeyAlias != "finance" || got.ExternalKeyARN != k.ExternalKeyARN || got.KeyModel != domain.ModelBYOK {
				t.Fatalf("get = %+v, %v", got, err)
			}
			rotated, err := s.RotateKey(ctx, tenant, k.ID)
			if err != nil || rotated.KeyVersion != 2 || rotated.RotationCount != 1 || rotated.LastRotatedAt == nil {
				t.Fatalf("rotate = %+v, %v", rotated, err)
			}
			if err := s.DisableKey(ctx, tenant, k.ID); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.GetKeyByID(ctx, tenant, k.ID); got.State != domain.StateDisabled || got.KeyVersion != 2 {
				t.Fatalf("after disable: %+v", got)
			}
		})
	}
}

// Every by-id method is tenant-scoped, and the listing filters by tenant and
// legal entity — the isolation tenant_scope_test.go asserts at the handler.
func TestConformance_KeysAreTenantScoped(t *testing.T) {
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s, ctx := open(t), context.Background()
			a, b := uuid.NewString(), uuid.NewString()
			ka := newKey("le-1", "a-key")
			if err := s.CreateKey(ctx, a, ka); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateKey(ctx, a, newKey("le-2", "a-other-entity")); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateKey(ctx, b, newKey("le-1", "b-key")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetKeyByID(ctx, b, ka.ID); err == nil {
				t.Fatal("tenant b read tenant a's key")
			}
			if _, err := s.RotateKey(ctx, b, ka.ID); err == nil {
				t.Fatal("tenant b rotated tenant a's key")
			}
			if err := s.DisableKey(ctx, b, ka.ID); err == nil {
				t.Fatal("tenant b disabled tenant a's key")
			}
			if got, _ := s.GetKeyByID(ctx, a, ka.ID); got.State != domain.StateEnabled || got.KeyVersion != 1 {
				t.Fatalf("tenant a's key changed by tenant b's refused calls: %+v", got)
			}
			keys, err := s.ListKeys(ctx, a, "le-1")
			if err != nil || len(keys) != 1 || keys[0].KeyAlias != "a-key" {
				t.Fatalf("list a/le-1 = %+v, %v", keys, err)
			}
			if keys, _ := s.ListKeys(ctx, b, "le-1"); len(keys) != 1 || keys[0].KeyAlias != "b-key" {
				t.Fatalf("list b/le-1 = %+v", keys)
			}
			if _, err := s.GetKeyByID(ctx, a, "does-not-exist"); err == nil {
				t.Fatal("a malformed id was found")
			}
		})
	}
}

func TestConformance_VaultIsTenantScopedAndIdempotent(t *testing.T) {
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			v, ctx := open(t), context.Background()
			a, b := uuid.NewString(), uuid.NewString()
			row := &domain.TokenizedValue{TokenID: uuid.NewString(), LegalEntityID: "le-1", EncryptedValue: []byte{1, 2, 3},
				Classification: domain.ClassRestricted, CreatedByPrincipalID: "p1", IdempotencyKey: "idem-1"}
			if existing, created, err := v.CreateToken(ctx, a, row); err != nil || !created || existing != nil {
				t.Fatalf("create = %v, %v, %v", existing, created, err)
			}
			got, err := v.GetToken(ctx, a, row.TokenID)
			if err != nil || got.TenantID != a || !bytes.Equal(got.EncryptedValue, []byte{1, 2, 3}) ||
				got.LegalEntityID != "le-1" || got.Classification != domain.ClassRestricted || got.CreatedByPrincipalID != "p1" {
				t.Fatalf("get = %+v, %v", got, err)
			}
			if _, err := v.GetToken(ctx, b, row.TokenID); !errors.Is(err, domain.ErrTokenNotFound) {
				t.Fatalf("tenant b read tenant a's token: %v", err)
			}
			if _, err := v.GetToken(ctx, a, uuid.NewString()); !errors.Is(err, domain.ErrTokenNotFound) {
				t.Fatalf("unknown token: %v", err)
			}

			// Same idempotency key in the same tenant: nothing new, the
			// original row back.
			retry := *row
			retry.TokenID = uuid.NewString()
			existing, created, err := v.CreateToken(ctx, a, &retry)
			if err != nil || created || existing == nil || existing.TokenID != row.TokenID {
				t.Fatalf("replay = %+v, %v, %v", existing, created, err)
			}
			if _, err := v.GetToken(ctx, a, retry.TokenID); !errors.Is(err, domain.ErrTokenNotFound) {
				t.Fatal("a replay inserted a second row")
			}
			// The same key is free in another tenant.
			other := *row
			other.TokenID = uuid.NewString()
			if _, created, err := v.CreateToken(ctx, b, &other); err != nil || !created {
				t.Fatalf("same key, other tenant: created=%v err=%v", created, err)
			}
		})
	}
}

func pg(t *testing.T) (testpg.Pools, *store.PgStore) {
	p := testpg.Open(t, testDB)
	return p, store.NewPgStore(p.App)
}

// Durability is the reason this store exists: a second store on a new pool —
// a restarted process — still has the keys and tokens.
func TestPg_SurvivesARestart(t *testing.T) {
	p, s := pg(t)
	ctx, tenant := context.Background(), uuid.NewString()
	k := newKey("le-1", "durable")
	if err := s.CreateKey(ctx, tenant, k); err != nil {
		t.Fatal(err)
	}
	tok := &domain.TokenizedValue{TokenID: uuid.NewString(), LegalEntityID: "le-1", EncryptedValue: []byte("ct"),
		Classification: domain.ClassRestricted, CreatedByPrincipalID: "p1", IdempotencyKey: "durable"}
	if _, _, err := s.CreateToken(ctx, tenant, tok); err != nil {
		t.Fatal(err)
	}
	p.App.Close()

	restarted := store.NewPgStore(testpg.Open(t, testDB).App)
	if got, err := restarted.GetKeyByID(ctx, tenant, k.ID); err != nil || got.KeyAlias != "durable" {
		t.Fatalf("key after restart = %+v, %v", got, err)
	}
	if got, err := restarted.GetToken(ctx, tenant, tok.TokenID); err != nil || string(got.EncryptedValue) != "ct" {
		t.Fatalf("token after restart = %+v, %v", got, err)
	}
}

// RLS is enforced for the app role on its own, without the store's explicit
// tenant filters: no tenant set sees nothing, and one tenant cannot write a
// row claiming another.
func TestPg_RowLevelSecurityIsForced(t *testing.T) {
	p, s := pg(t)
	ctx, tenant := context.Background(), uuid.NewString()
	if err := s.CreateKey(ctx, tenant, newKey("le-1", "rls")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateToken(ctx, tenant, &domain.TokenizedValue{TokenID: uuid.NewString(), LegalEntityID: "le-1",
		EncryptedValue: []byte("ct"), Classification: domain.ClassRestricted, CreatedByPrincipalID: "p1", IdempotencyKey: "rls"}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"customer_keys", "tokenized_values"} {
		var total int
		if err := p.Admin.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&total); err != nil || total == 0 {
			t.Fatalf("%s has no rows to hide (%d, %v) — the check below would prove nothing", table, total, err)
		}
		var n int
		if err := p.App.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s visible to the app role with no tenant set: %d rows, %v", table, n, err)
		}
		var forced bool
		if err := p.Admin.QueryRow(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&forced); err != nil || !forced {
			t.Fatalf("%s: FORCE ROW LEVEL SECURITY = %v, %v", table, forced, err)
		}
	}
	tx, err := p.App.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	_, err = tx.Exec(ctx, `INSERT INTO tokenized_values (token_id, tenant_id, legal_entity_id, encrypted_value, classification,
		created_by_principal_id, idempotency_key) VALUES ($1, $2, 'le', '\x00', 'S3_RESTRICTED', 'p', 'k')`,
		uuid.NewString(), uuid.NewString())
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("insert of another tenant's token row: %v; want a row-level security refusal", err)
	}
}

func TestPg_RefusesANonUUIDTenant(t *testing.T) {
	_, s := pg(t)
	if err := s.CreateKey(context.Background(), "t1", newKey("le-1", "x")); !errors.Is(err, store.ErrInvalidTenant) {
		t.Fatalf("CreateKey with tenant t1 = %v, want ErrInvalidTenant", err)
	}
	if _, err := s.GetToken(context.Background(), "t1", uuid.NewString()); !errors.Is(err, store.ErrInvalidTenant) {
		t.Fatalf("GetToken with tenant t1 = %v, want ErrInvalidTenant", err)
	}
}

// A token is a fact. Even the table owner cannot repoint or remove one.
func TestPg_TokensAreImmutable(t *testing.T) {
	p, s := pg(t)
	ctx, tenant := context.Background(), uuid.NewString()
	tok := &domain.TokenizedValue{TokenID: uuid.NewString(), LegalEntityID: "le-1", EncryptedValue: []byte("ct"),
		Classification: domain.ClassRestricted, CreatedByPrincipalID: "p1", IdempotencyKey: "imm"}
	if _, _, err := s.CreateToken(ctx, tenant, tok); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE tokenized_values SET encrypted_value = '\x01' WHERE token_id = $1`,
		`DELETE FROM tokenized_values WHERE token_id = $1`,
	} {
		if _, err := p.Admin.Exec(ctx, stmt, tok.TokenID); err == nil {
			t.Fatalf("%q succeeded", stmt)
		}
	}
	if got, _ := s.GetToken(ctx, tenant, tok.TokenID); got == nil || string(got.EncryptedValue) != "ct" {
		t.Fatalf("token changed: %+v", got)
	}
}

// Concurrent tokenize calls with one idempotency key create exactly one row,
// and every caller gets that row's token.
func TestPg_ConcurrentIdempotentCreatesOneRow(t *testing.T) {
	p, s := pg(t)
	ctx, tenant := context.Background(), uuid.NewString()
	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			row := &domain.TokenizedValue{TokenID: uuid.NewString(), LegalEntityID: "le-1", EncryptedValue: []byte("ct"),
				Classification: domain.ClassRestricted, CreatedByPrincipalID: "p1", IdempotencyKey: "race"}
			existing, created, err := s.CreateToken(ctx, tenant, row)
			errs[i] = err
			if created {
				ids[i] = row.TokenID
			} else if existing != nil {
				ids[i] = existing.TokenID
			}
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("caller %d: token %q err %v; want all %q", i, ids[i], errs[i], ids[0])
		}
	}
	var rows int
	_ = p.Admin.QueryRow(ctx, `SELECT count(*) FROM tokenized_values WHERE tenant_id = $1`, tenant).Scan(&rows)
	if rows != 1 {
		t.Fatalf("%d rows for one idempotency key", rows)
	}
}
