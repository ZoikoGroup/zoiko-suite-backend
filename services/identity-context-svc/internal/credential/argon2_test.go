package credential

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testParams keeps the derivations in this file fast. They are below
// DefaultParams and deliberately still above Validate's floors, so the tests
// exercise the same code path production does rather than a bypass.
func testParams() Params {
	p := DefaultParams()
	p.MemoryKiB = 8192
	p.Iterations = 1
	return p
}

func newTestHasher(t *testing.T) *Hasher {
	t.Helper()
	h, err := NewHasher(testParams(), 2)
	require.NoError(t, err)
	return h
}

func TestHashVerify_RoundTrip(t *testing.T) {
	h := newTestHasher(t)

	hash, err := h.Hash("correct horse battery staple")
	require.NoError(t, err)

	needsRehash, err := h.Verify("correct horse battery staple", hash)
	require.NoError(t, err)
	require.False(t, needsRehash, "digest was produced at the current parameters")
}

func TestVerify_WrongPasswordIsMismatch(t *testing.T) {
	h := newTestHasher(t)
	hash, err := h.Hash("correct horse battery staple")
	require.NoError(t, err)

	_, err = h.Verify("Correct horse battery staple", hash)
	require.ErrorIs(t, err, ErrMismatch)
}

// A password longer than bcrypt's 72-byte truncation point must not collide
// with one sharing its first 72 bytes. This is the specific failure mode that
// motivated choosing argon2id, so it is asserted rather than assumed.
func TestVerify_NoTruncationAtSeventyTwoBytes(t *testing.T) {
	h := newTestHasher(t)

	prefix := strings.Repeat("a", 72)
	hash, err := h.Hash(prefix + "SUFFIX-ONE")
	require.NoError(t, err)

	_, err = h.Verify(prefix+"SUFFIX-TWO", hash)
	require.ErrorIs(t, err, ErrMismatch,
		"two passwords sharing a 72-byte prefix must not both verify")

	_, err = h.Verify(prefix+"SUFFIX-ONE", hash)
	require.NoError(t, err)
}

func TestHash_SaltIsPerCall(t *testing.T) {
	h := newTestHasher(t)

	first, err := h.Hash("same password")
	require.NoError(t, err)
	second, err := h.Hash("same password")
	require.NoError(t, err)

	require.NotEqual(t, first, second, "each hash must carry a fresh random salt")

	// Both must still verify — different salts, same password.
	_, err = h.Verify("same password", first)
	require.NoError(t, err)
	_, err = h.Verify("same password", second)
	require.NoError(t, err)
}

// An absent stored digest must not short-circuit. It routes to the decoy and
// still answers ErrMismatch, which is what keeps an unknown email from being
// distinguishable by response time.
func TestVerify_EmptyHashUsesDecoyAndMismatches(t *testing.T) {
	h := newTestHasher(t)

	_, err := h.Verify("anything at all", "")
	require.ErrorIs(t, err, ErrMismatch)

	// The decoy must be a real, parseable digest — a malformed one would
	// return ErrMalformedHash from the inner call and skip the derivation,
	// silently reintroducing the timing gap this defends against.
	require.True(t, strings.HasPrefix(h.decoy, "$argon2id$v=19$"))
	_, _, _, decodeErr := decode(h.decoy)
	require.NoError(t, decodeErr)
}

// A digest produced under weaker parameters must verify AND report that it
// needs upgrading, so the caller can rehash while it still holds the plaintext.
func TestVerify_ReportsRehashWhenParametersRaised(t *testing.T) {
	weakParams := testParams()
	weak, err := NewHasher(weakParams, 1)
	require.NoError(t, err)

	oldHash, err := weak.Hash("passphrase")
	require.NoError(t, err)

	strongParams := weakParams
	strongParams.MemoryKiB = weakParams.MemoryKiB * 2
	strongParams.Iterations = weakParams.Iterations + 1
	strong, err := NewHasher(strongParams, 1)
	require.NoError(t, err)

	needsRehash, err := strong.Verify("passphrase", oldHash)
	require.NoError(t, err, "an old digest must still verify at its own cost")
	require.True(t, needsRehash)

	// And the reverse must not ask for a pointless downgrade.
	newHash, err := strong.Hash("passphrase")
	require.NoError(t, err)
	needsRehash, err = strong.Verify("passphrase", newHash)
	require.NoError(t, err)
	require.False(t, needsRehash)
}

// Every malformed digest must be an error, never a mismatch and never a
// silently-defaulted parameter set. A digest that decodes "mostly" is one an
// attacker can downgrade to trivially-crackable parameters.
func TestVerify_MalformedDigestsAreRejected(t *testing.T) {
	h := newTestHasher(t)
	valid, err := h.Hash("passphrase")
	require.NoError(t, err)
	parts := strings.Split(valid, "$")

	cases := map[string]struct {
		hash    string
		wantErr error
	}{
		"empty segments":        {"$argon2id$v=19$m=8192,t=1,p=1$$", ErrMalformedHash},
		"too few segments":      {"$argon2id$v=19$m=8192,t=1,p=1", ErrMalformedHash},
		"no leading dollar":     {"argon2id$v=19$m=8192,t=1,p=1$c2FsdA$a2V5", ErrMalformedHash},
		"missing t parameter":   {"$argon2id$v=19$m=8192,p=1$c2FsdA$a2V5", ErrMalformedHash},
		"duplicate parameter":   {"$argon2id$v=19$m=8192,m=1,p=1$c2FsdA$a2V5", ErrMalformedHash},
		"unknown parameter":     {"$argon2id$v=19$m=8192,t=1,x=1$c2FsdA$a2V5", ErrMalformedHash},
		"zero memory":           {"$argon2id$v=19$m=0,t=1,p=1$c2FsdA$a2V5", ErrMalformedHash},
		"parallelism overflows": {"$argon2id$v=19$m=8192,t=1,p=256$c2FsdA$a2V5", ErrMalformedHash},
		"non-numeric memory":    {"$argon2id$v=19$m=lots,t=1,p=1$c2FsdA$a2V5", ErrMalformedHash},
		"bad base64 salt":       {"$argon2id$v=19$m=8192,t=1,p=1$!!!!$a2V5", ErrMalformedHash},
		"wrong algorithm":       {"$argon2i$v=19$m=8192,t=1,p=1$c2FsdA$a2V5", ErrUnsupportedAlgorithm},
		"wrong version": {
			"$argon2id$v=16$" + parts[3] + "$" + parts[4] + "$" + parts[5],
			ErrUnsupportedAlgorithm,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := h.Verify("passphrase", tc.hash)
			require.ErrorIs(t, err, tc.wantErr)
			require.NotErrorIs(t, err, ErrMismatch,
				"an unusable digest is an operational fault, not a wrong password")
		})
	}
}

func TestEncode_ProducesStandardPHCFormat(t *testing.T) {
	h := newTestHasher(t)
	hash, err := h.Hash("passphrase")
	require.NoError(t, err)

	require.True(t, strings.HasPrefix(hash, "$argon2id$v=19$m=8192,t=1,p=1$"),
		"got %q", hash)

	params, salt, key, err := decode(hash)
	require.NoError(t, err)
	require.Equal(t, uint32(8192), params.MemoryKiB)
	require.Equal(t, uint32(1), params.Iterations)
	require.Equal(t, uint8(1), params.Parallelism)
	require.Len(t, salt, int(testParams().SaltLength))
	require.Len(t, key, int(testParams().KeyLength))
}

func TestNewHasher_RejectsWeakParameters(t *testing.T) {
	cases := map[string]func(Params) Params{
		"memory below floor": func(p Params) Params { p.MemoryKiB = 19; return p },
		"zero iterations":    func(p Params) Params { p.Iterations = 0; return p },
		"zero parallelism":   func(p Params) Params { p.Parallelism = 0; return p },
		"short salt":         func(p Params) Params { p.SaltLength = 8; return p },
		"short key":          func(p Params) Params { p.KeyLength = 16; return p },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewHasher(mutate(testParams()), 1)
			require.Error(t, err, "a misconfigured cost factor must fail at construction")
		})
	}

	t.Run("zero concurrency", func(t *testing.T) {
		_, err := NewHasher(testParams(), 0)
		require.Error(t, err)
	})
}

func TestDefaultParams_MeetOWASPFloor(t *testing.T) {
	p := DefaultParams()
	require.NoError(t, p.Validate())
	// OWASP's first-listed argon2id configuration. Asserted so a well-meaning
	// tuning change that weakens the shipped default is caught here rather
	// than in production.
	require.GreaterOrEqual(t, p.MemoryKiB, uint32(19456))
	require.GreaterOrEqual(t, p.Iterations, uint32(2))
}

// The concurrency cap is a memory bound, not a correctness one, so this only
// asserts that hashing stays correct when more callers contend than the
// semaphore admits.
func TestHasher_ConcurrentUseIsCorrect(t *testing.T) {
	h, err := NewHasher(testParams(), 2)
	require.NoError(t, err)

	hash, err := h.Hash("shared passphrase")
	require.NoError(t, err)

	const callers = 8
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, verifyErr := h.Verify("shared passphrase", hash)
			errs <- verifyErr
		}()
	}
	for i := 0; i < callers; i++ {
		require.NoError(t, <-errs)
	}
}

func TestErrors_AreDistinguishable(t *testing.T) {
	// The authenticator branches on these, so they must not collapse.
	require.False(t, errors.Is(ErrMalformedHash, ErrMismatch))
	require.False(t, errors.Is(ErrUnsupportedAlgorithm, ErrMismatch))
}
