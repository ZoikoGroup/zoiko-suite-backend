package federation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newKM(idp *testIdP) (*KeyManager, *memRepo, *fakeFetcher, *clock) {
	repo := newMemRepo()
	fetch := &fakeFetcher{doc: idp.jwks()}
	km := NewKeyManager(repo, fetch, repo, nil)
	clk := &clock{t: time.Now()}
	km.SetClock(clk.now)
	return km, repo, fetch, clk
}

func resolve(km *KeyManager, cfg Config, kid string) (*ResolvedKey, error) {
	return km.Resolve(context.Background(), testConnection(), cfg, kid, "RS256")
}

func TestKeysAreFetchedOnceAndCached(t *testing.T) {
	idp := newTestIdP(t)
	km, _, fetch, _ := newKM(idp)
	for i := 0; i < 5; i++ {
		if _, err := resolve(km, testConfig(), idp.kid); err != nil {
			t.Fatal(err)
		}
	}
	if fetch.count() != 1 {
		t.Fatalf("a fresh key set is reused: %d fetches", fetch.count())
	}
}

func TestKeysRefreshAfterTheirLife(t *testing.T) {
	idp := newTestIdP(t)
	km, _, fetch, clk := newKM(idp)
	_, _ = resolve(km, testConfig(), idp.kid)
	clk.advance(km.RefreshAfter + time.Second)
	if _, err := resolve(km, testConfig(), idp.kid); err != nil {
		t.Fatal(err)
	}
	if fetch.count() != 2 {
		t.Fatalf("an old key set is refreshed: %d fetches", fetch.count())
	}
}

func TestRotatedOutKeyStopsWorkingAfterRefresh(t *testing.T) {
	idp := newTestIdP(t)
	km, _, fetch, clk := newKM(idp)
	if _, err := resolve(km, testConfig(), idp.kid); err != nil {
		t.Fatal(err)
	}
	// the IdP rotates: the old key is withdrawn and a new one published
	next := newTestIdP(t)
	next.kid = "rsa-2"
	fetch.set(next.jwks(), nil)
	clk.advance(km.RefreshAfter + time.Second)
	if _, err := resolve(km, testConfig(), idp.kid); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a withdrawn key must no longer verify: %v", err)
	}
	if _, err := resolve(km, testConfig(), "rsa-2"); err != nil {
		t.Fatalf("the new key verifies: %v", err)
	}
}

func TestUnknownKidTriggersAThrottledRefresh(t *testing.T) {
	idp := newTestIdP(t)
	km, _, fetch, clk := newKM(idp)
	_, _ = resolve(km, testConfig(), idp.kid)
	clk.advance(km.MinRefresh + time.Second)

	// the IdP publishes a new key between our refreshes: an unknown kid finds it
	next := newTestIdP(t)
	next.kid = "rsa-new"
	fetch.set(jwksDoc(rsaJWK("rsa-new", &next.rsaKey.PublicKey)), nil)
	if _, err := resolve(km, testConfig(), "rsa-new"); err != nil {
		t.Fatalf("unknown kid refresh: %v", err)
	}
	if fetch.count() != 2 {
		t.Fatalf("fetches: %d", fetch.count())
	}

	// a forged kid must not make this service hammer the IdP
	for i := 0; i < 20; i++ {
		_, _ = resolve(km, testConfig(), "forged")
	}
	if fetch.count() != 2 {
		t.Fatalf("forged key ids caused %d extra fetches", fetch.count()-2)
	}
	clk.advance(km.MinRefresh + time.Second)
	_, _ = resolve(km, testConfig(), "forged")
	if fetch.count() != 3 {
		t.Fatalf("after the throttle window one more look is allowed: %d", fetch.count())
	}
}

func TestOutageFailsClosedByDefault(t *testing.T) {
	idp := newTestIdP(t)
	km, repo, fetch, clk := newKM(idp)
	cfg := testConfig()
	if _, err := resolve(km, cfg, idp.kid); err != nil {
		t.Fatal(err)
	}
	fetch.set(nil, errBoom)
	clk.advance(km.RefreshAfter + time.Second)
	_, err := resolve(km, cfg, idp.kid)
	if !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("FAIL_CLOSED with the IdP down and the cache stale: %v", err)
	}
	if repo.lastHealth() != "degraded" {
		t.Fatalf("the connection is reported degraded, got %q", repo.lastHealth())
	}
}

func TestOutageCachedKeysWithinAndBeyondGrace(t *testing.T) {
	idp := newTestIdP(t)
	km, repo, fetch, clk := newKM(idp)
	cfg := testConfig()
	cfg.OutageBehavior, cfg.KeyGraceSeconds = OutageCachedKeys, 3600
	if _, err := resolve(km, cfg, idp.kid); err != nil {
		t.Fatal(err)
	}
	fetch.set(nil, errBoom)

	clk.advance(km.RefreshAfter + time.Minute) // stale, but inside the grace
	k, err := resolve(km, cfg, idp.kid)
	if err != nil || !k.Degraded {
		t.Fatalf("within grace the cached keys serve, flagged degraded: %+v %v", k, err)
	}
	if repo.lastHealth() != "degraded" {
		t.Fatalf("health: %q", repo.lastHealth())
	}

	clk.advance(2 * time.Hour) // past the grace
	if _, err := resolve(km, cfg, idp.kid); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("beyond the grace there is no insecure fallback: %v", err)
	}

	// the IdP returns: service resumes and the connection recovers
	fetch.set(idp.jwks(), nil)
	clk.advance(km.MinRefresh + time.Second)
	k, err = resolve(km, cfg, idp.kid)
	if err != nil || k.Degraded {
		t.Fatalf("recovered: %+v %v", k, err)
	}
	if repo.lastHealth() != "ok" {
		t.Fatalf("health after recovery: %q", repo.lastHealth())
	}
}

func TestAnEmptyKeySetNeverReplacesAGoodOne(t *testing.T) {
	idp := newTestIdP(t)
	km, repo, fetch, clk := newKM(idp)
	cfg := testConfig()
	_, _ = resolve(km, cfg, idp.kid)
	fetch.set(jwksDoc(), nil) // a "successful" fetch that publishes nothing usable
	clk.advance(km.RefreshAfter + time.Second)
	if _, err := resolve(km, cfg, idp.kid); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("an empty key set is a failed fetch: %v", err)
	}
	if repo.keySet.KeyCount != 2 {
		t.Fatalf("the good key set was overwritten: %d keys", repo.keySet.KeyCount)
	}
}

func TestChangedLocationInvalidatesTheCache(t *testing.T) {
	idp := newTestIdP(t)
	km, _, fetch, _ := newKM(idp)
	cfg := testConfig()
	_, _ = resolve(km, cfg, idp.kid)
	cfg.JWKSURI = "https://idp.example.com/jwks-v2"
	if _, err := resolve(km, cfg, idp.kid); err != nil {
		t.Fatal(err)
	}
	if fetch.count() != 2 {
		t.Fatalf("keys fetched from the old location must not serve the new one: %d fetches", fetch.count())
	}
	// and if the new location is unreachable, the old keys are NOT a fallback, whatever the outage policy says
	cfg.JWKSURI = "https://idp.example.com/jwks-v3"
	cfg.OutageBehavior, cfg.KeyGraceSeconds = OutageCachedKeys, 3600
	fetch.set(nil, errBoom)
	if _, err := resolve(km, cfg, idp.kid); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("keys from another location are never a fallback: %v", err)
	}
}

func TestConcurrentLoginsShareOneFetch(t *testing.T) {
	idp := newTestIdP(t)
	km, _, fetch, _ := newKM(idp)
	fetch.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := resolve(km, testConfig(), idp.kid); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if fetch.count() > 2 {
		t.Fatalf("20 concurrent logins caused %d fetches", fetch.count())
	}
}

func TestAFailedFirstFetchIsNotRetriedInAStorm(t *testing.T) {
	idp := newTestIdP(t)
	km, _, fetch, clk := newKM(idp)
	fetch.set(nil, errBoom)
	for i := 0; i < 30; i++ {
		_, _ = resolve(km, testConfig(), idp.kid)
	}
	if fetch.count() != 1 {
		t.Fatalf("an unreachable IdP was asked %d times", fetch.count())
	}
	clk.advance(km.MinRefresh + time.Second)
	_, _ = resolve(km, testConfig(), idp.kid)
	if fetch.count() != 2 {
		t.Fatalf("one more attempt after the window: %d", fetch.count())
	}
}

func TestOperatorRefreshIsNotThrottledByAnEarlierFailure(t *testing.T) {
	idp := newTestIdP(t)
	km, _, fetch, _ := newKM(idp)
	fetch.set(nil, errBoom)
	_, _ = resolve(km, testConfig(), idp.kid)
	fetch.set(idp.jwks(), nil)
	n, err := km.Refresh(context.Background(), testConnection(), testConfig())
	if err != nil || n != 2 {
		t.Fatalf("refresh: %d %v", n, err)
	}
}
