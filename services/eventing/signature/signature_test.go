package signature

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/eventing/integration"
)

var (
	ctx   = context.Background()
	k1    = []byte("key-one-key-one-key-one-key-one!")
	k2    = []byte("key-two-key-two-key-two-key-two!")
	t0    = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	body  = []byte(`{"invoice_id":"inv-1","amount":"10.00"}`)
	tol   = 5 * time.Minute
	evtID = "evt-123"
)

func sign(t *testing.T, ts time.Time, id string, b []byte, signers ...Signer) string {
	t.Helper()
	h, err := Sign(ctx, ts, id, b, signers...)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSignedMessageVerifies(t *testing.T) {
	h := sign(t, t0, evtID, body, LocalSigner(k1))
	if !strings.HasPrefix(h, "t=") || !strings.Contains(h, ",v1=") {
		t.Fatalf("header shape: %s", h)
	}
	if err := Verify(h, evtID, body, [][]byte{k1}, t0.Add(time.Second), tol); err != nil {
		t.Fatalf("a correctly signed message must verify: %v", err)
	}
}

// INT-NP-014: an invalid signature is rejected and carries INT-SIG-009.
func TestInvalidSignatureIsRejected(t *testing.T) {
	good := sign(t, t0, evtID, body, LocalSigner(k1))
	cases := map[string]struct {
		header, id string
		body       []byte
		keys       [][]byte
	}{
		"wrong key":         {good, evtID, body, [][]byte{k2}},
		"tampered body":     {good, evtID, []byte(`{"invoice_id":"inv-1","amount":"99999"}`), [][]byte{k1}},
		"relabelled event":  {good, "evt-999", body, [][]byte{k1}},
		"no keys":           {good, evtID, body, nil},
		"redated header":    {strings.Replace(good, "t="+itoa(t0.Unix()), "t="+itoa(t0.Unix()+1), 1), evtID, body, [][]byte{k1}},
		"signature flipped": {good[:len(good)-1] + flip(good[len(good)-1]), evtID, body, [][]byte{k1}},
	}
	for name, c := range cases {
		err := Verify(c.header, c.id, c.body, c.keys, t0, tol)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
		if Code(err) != integration.CodeSignatureInvalid {
			t.Errorf("%s: code %s", name, Code(err))
		}
	}
}

func TestMalformedHeadersAreRejected(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	for name, h := range map[string]string{
		"empty":           "",
		"no timestamp":    "v1=" + hex64,
		"no signature":    "t=100",
		"bad timestamp":   "t=abc,v1=" + hex64,
		"zero timestamp":  "t=0,v1=" + hex64,
		"short signature": "t=100,v1=abcd",
		"non hex":         "t=100,v1=" + strings.Repeat("z", 64),
		"duplicate t":     "t=100,t=101,v1=" + hex64,
		"no equals":       "garbage",
		"too many sigs":   "t=100," + strings.Repeat("v1="+hex64+",", 5) + "x=1",
	} {
		if _, err := Parse(h); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
		if err := Verify(h, evtID, body, [][]byte{k1}, t0, tol); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	if _, err := Parse("t=100,v2=whatever,v1=" + hex64); err != nil {
		t.Errorf("an unknown parameter must be ignored, not fatal: %v", err)
	}
}

// INT-NP-015: outside the window is refused as a callback conflict.
func TestTimestampWindow(t *testing.T) {
	h := sign(t, t0, evtID, body, LocalSigner(k1))
	for name, now := range map[string]time.Time{
		"just inside":       t0.Add(tol),
		"just inside early": t0.Add(-tol),
	} {
		if err := Verify(h, evtID, body, [][]byte{k1}, now, tol); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, now := range map[string]time.Time{
		"too old":   t0.Add(tol + time.Second),
		"too early": t0.Add(-tol - time.Second),
	} {
		err := Verify(h, evtID, body, [][]byte{k1}, now, tol)
		if !errors.Is(err, ErrStale) || Code(err) != integration.CodeCallbackConflict {
			t.Errorf("%s: %v (%s)", name, err, Code(err))
		}
	}
}

func TestForgedMessageIsInvalidNotStale(t *testing.T) {
	// The signature is judged first, so an attacker cannot use the error to learn
	// whether a guessed timestamp was acceptable.
	h := sign(t, t0, evtID, body, LocalSigner(k2))
	err := Verify(h, evtID, body, [][]byte{k1}, t0.Add(24*time.Hour), tol)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
}

func TestKeyRotationAcceptsEitherKey(t *testing.T) {
	both := sign(t, t0, evtID, body, LocalSigner(k1), LocalSigner(k2)) // current + previous
	for name, keys := range map[string][][]byte{
		"recipient still on the old key": {k2},
		"recipient moved to the new key": {k1},
		"recipient holds both":           {k1, k2},
	} {
		if err := Verify(both, evtID, body, keys, t0, tol); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := Verify(both, evtID, body, [][]byte{[]byte("a-third-key-a-third-key-a-third-k")}, t0, tol); !errors.Is(err, ErrInvalid) {
		t.Errorf("a third key must not verify: %v", err)
	}
}

func TestSignRefusesBadInput(t *testing.T) {
	if _, err := Sign(ctx, t0, "", body, LocalSigner(k1)); err == nil {
		t.Error("an empty event id was signed")
	}
	for _, id := range []string{"a.b", "a,b", "a=b", "a b", "a\nb"} {
		if _, err := Sign(ctx, t0, id, body, LocalSigner(k1)); err == nil {
			t.Errorf("event id %q was signed: it could be re-split on verification", id)
		}
	}
	if _, err := Sign(ctx, t0, evtID, body); err == nil {
		t.Error("signing with no signer succeeded")
	}
	boom := errors.New("vault down")
	if _, err := Sign(ctx, t0, evtID, body, LocalSigner(k1), func(context.Context, []byte) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Errorf("a failing signer must fail the whole signature (never a half-signed header): %v", err)
	}
}

func TestLocalSignerCopiesItsKey(t *testing.T) {
	key := append([]byte(nil), k1...)
	s := LocalSigner(key)
	for i := range key {
		key[i] = 0
	}
	h, _ := Sign(ctx, t0, evtID, body, s)
	if err := Verify(h, evtID, body, [][]byte{k1}, t0, tol); err != nil {
		t.Fatalf("zeroing the caller's slice must not change the signer: %v", err)
	}
}

// INT-NP-015: the same message is accepted once.
func TestReplayIsRejected(t *testing.T) {
	g := NewMemoryGuard(100, func() time.Time { return t0 })
	h := sign(t, t0, evtID, body, LocalSigner(k1))
	keys := [][]byte{k1}
	if err := VerifyOnce(ctx, g, "tenant-a|bank-x", h, evtID, body, keys, t0, tol); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	err := VerifyOnce(ctx, g, "tenant-a|bank-x", h, evtID, body, keys, t0.Add(time.Minute), tol)
	if !errors.Is(err, ErrReplay) || Code(err) != integration.CodeCallbackConflict {
		t.Fatalf("replay inside the window: %v", err)
	}
	// the same bytes under another tenant or source are a different message scope
	if err := VerifyOnce(ctx, g, "tenant-b|bank-x", h, evtID, body, keys, t0, tol); err != nil {
		t.Fatalf("another scope: %v", err)
	}
	// and after the window the timestamp rule takes over
	if err := VerifyOnce(ctx, g, "tenant-a|bank-x", h, evtID, body, keys, t0.Add(3*tol), tol); !errors.Is(err, ErrStale) {
		t.Fatalf("replay outside the window: %v", err)
	}
}

func TestInvalidMessageDoesNotConsumeTheReplaySlot(t *testing.T) {
	g := NewMemoryGuard(100, func() time.Time { return t0 })
	good := sign(t, t0, evtID, body, LocalSigner(k1))
	forged := sign(t, t0, evtID, body, LocalSigner(k2))
	if err := VerifyOnce(ctx, g, "s", forged, evtID, body, [][]byte{k1}, t0, tol); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := VerifyOnce(ctx, g, "s", good, evtID, body, [][]byte{k1}, t0, tol); err != nil {
		t.Fatalf("a forged attempt must not block the genuine message: %v", err)
	}
}

func TestMemoryGuardIsBoundedAndForgetsOnlyOldEntries(t *testing.T) {
	now := t0
	g := NewMemoryGuard(3, func() time.Time { return now })
	exp := t0.Add(time.Hour)
	for _, k := range []string{"a", "b", "c", "d"} {
		if first, _ := g.FirstSeen(ctx, k, exp); !first {
			t.Fatalf("%s should be new", k)
		}
	}
	if len(g.entries) != 3 {
		t.Fatalf("size %d exceeds the bound", len(g.entries))
	}
	if first, _ := g.FirstSeen(ctx, "d", exp); first {
		t.Error("the newest entry must still be remembered")
	}
	// expired entries are reusable
	now = t0.Add(2 * time.Hour)
	if first, _ := g.FirstSeen(ctx, "d", now.Add(time.Hour)); !first {
		t.Error("an expired entry must be accepted again")
	}
	if g2 := NewMemoryGuard(0, nil); g2.max != 1 {
		t.Errorf("minimum size: %d", g2.max)
	}
}

func TestMemoryGuardConcurrentDuplicatesAcceptOnce(t *testing.T) {
	g := NewMemoryGuard(1000, func() time.Time { return t0 })
	var accepted int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if first, _ := g.FirstSeen(ctx, "same", t0.Add(time.Hour)); first {
				atomic.AddInt32(&accepted, 1)
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("%d concurrent duplicates were accepted, want exactly 1", accepted)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func flip(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}
