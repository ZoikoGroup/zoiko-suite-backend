// Package credential hashes and verifies human password material.
//
// argon2id is used rather than bcrypt: bcrypt silently truncates at 72 bytes
// (a password manager's output is routinely longer, and the truncation is
// invisible — two different passphrases sharing a 72-byte prefix both
// authenticate), and its cost factor tunes CPU only, so a GPU or ASIC attacker
// scales past it cheaply. argon2id's memory cost is what makes parallel
// hardware expensive, and it is the OWASP first choice for new work.
//
// Nothing in this package logs, and no exported function returns the plaintext
// or the derived key. Verify's only outputs are a boolean and an error.
package credential

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Errors returned by this package. None of them distinguishes "no such
// credential" from "wrong password" — that distinction is the caller's to
// make, and deliberately not one it should surface to a client.
var (
	// ErrMismatch means the supplied password did not derive the stored key.
	ErrMismatch = errors.New("credential does not match")
	// ErrMalformedHash means the stored digest is not a parseable PHC string.
	// Treated as a hard failure, never as a mismatch: a digest we cannot parse
	// is an operational fault, and reporting it as a wrong password would send
	// an operator hunting a user error that does not exist.
	ErrMalformedHash = errors.New("stored credential digest is malformed")
	// ErrUnsupportedAlgorithm means the digest names an algorithm this build
	// cannot compute. Fails closed for the same reason as ErrMalformedHash.
	ErrUnsupportedAlgorithm = errors.New("stored credential uses an unsupported algorithm")
)

// Params are the argon2id cost factors.
//
// MemoryKiB is the dominant term for both security and resource use: each
// concurrent hash allocates MemoryKiB of RAM for its lifetime.
// identity-context-svc runs under a 256 MiB container limit
// (deployments/docker-compose.yml), so MemoryKiB multiplied by the Hasher's
// concurrency cap has to stay well inside that. The defaults below
// (19 MiB x 4 concurrent = 76 MiB peak) do.
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultParams is OWASP's first-listed argon2id configuration: m=19456
// (19 MiB), t=2, p=1. The alternatives OWASP lists alongside it trade memory
// against iterations at equivalent strength, which is the knob to reach for if
// the container limit ever changes — rather than weakening either term alone.
func DefaultParams() Params {
	return Params{
		MemoryKiB:   19456,
		Iterations:  2,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// Validate rejects parameters that would produce a weaker hash than the
// algorithm's own floor. Called on construction so a misconfigured environment
// variable fails at startup rather than silently downgrading every password
// hashed for the life of the process.
func (p Params) Validate() error {
	// argon2's own minimum is 8*Parallelism KiB; these floors sit far above it
	// and are chosen so a typo (m=19 rather than m=19456) cannot pass.
	if p.MemoryKiB < 8192 {
		return fmt.Errorf("argon2id memory must be at least 8192 KiB, got %d", p.MemoryKiB)
	}
	if p.Iterations < 1 {
		return fmt.Errorf("argon2id iterations must be at least 1, got %d", p.Iterations)
	}
	if p.Parallelism < 1 {
		return fmt.Errorf("argon2id parallelism must be at least 1, got %d", p.Parallelism)
	}
	if p.SaltLength < 16 {
		return fmt.Errorf("argon2id salt must be at least 16 bytes, got %d", p.SaltLength)
	}
	if p.KeyLength < 32 {
		return fmt.Errorf("argon2id key length must be at least 32 bytes, got %d", p.KeyLength)
	}
	return nil
}

// Hasher derives and verifies argon2id digests under a bounded concurrency
// budget.
//
// The bound is not throttling for its own sake. Each in-flight hash holds
// Params.MemoryKiB until it completes, so an unbounded handler would let a
// burst of login attempts — the one endpoint on this service reachable without
// credentials — allocate until the container is OOM-killed. That turns a
// password-guessing attempt into an outage of a Tier 0 service every other
// service depends on. The semaphore caps the exposure at a number chosen
// against the memory limit.
type Hasher struct {
	params Params
	// sem admits at most cap(sem) concurrent derivations.
	sem chan struct{}
	// decoy is a real digest over a random secret, verified whenever no stored
	// credential exists. See Verify.
	decoy string
}

// NewHasher validates params and precomputes the decoy digest.
//
// Precomputing costs one derivation at startup, which is the point: the decoy
// has to be produced under the same parameters as real credentials, and doing
// it lazily on the first unknown-principal request would make that request
// measurably slower than every one after it.
func NewHasher(params Params, maxConcurrent int) (*Hasher, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	if maxConcurrent < 1 {
		return nil, fmt.Errorf("maxConcurrent must be at least 1, got %d", maxConcurrent)
	}
	h := &Hasher{
		params: params,
		sem:    make(chan struct{}, maxConcurrent),
	}

	// A random secret nobody holds, so the decoy can never be made to match.
	filler := make([]byte, 32)
	if _, err := rand.Read(filler); err != nil {
		return nil, fmt.Errorf("generate decoy secret: %w", err)
	}
	decoy, err := h.Hash(string(filler))
	if err != nil {
		return nil, fmt.Errorf("precompute decoy digest: %w", err)
	}
	h.decoy = decoy
	return h, nil
}

// Params returns the hasher's cost factors.
func (h *Hasher) Params() Params { return h.params }

// Hash derives a PHC-encoded argon2id digest of password under a fresh random
// salt. Two calls with the same password return different strings; that is the
// salt doing its job, not nondeterminism to be engineered away.
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	h.acquire()
	key := argon2.IDKey(
		[]byte(password), salt,
		h.params.Iterations, h.params.MemoryKiB, h.params.Parallelism, h.params.KeyLength,
	)
	h.release()

	return encode(h.params, salt, key), nil
}

// Verify reports whether password derives the key stored in encodedHash.
//
// An empty encodedHash — the caller found no credential row — is not a fast
// "no". It verifies against the precomputed decoy and returns ErrMismatch, so
// an unknown email costs the same wall-clock time as a known one with the
// wrong password. Without that, response latency alone enumerates every valid
// account on the platform, and an attacker who can list addresses can then
// concentrate guessing on the ones known to exist.
//
// needsRehash is true when the digest verified but was produced under weaker
// parameters than this Hasher now uses, so the caller can transparently
// upgrade it while it still holds the plaintext. It is only ever true
// alongside a nil error.
func (h *Hasher) Verify(password, encodedHash string) (needsRehash bool, err error) {
	if encodedHash == "" {
		_, _ = h.matches(password, h.decoy)
		return false, ErrMismatch
	}

	params, salt, want, err := decode(encodedHash)
	if err != nil {
		return false, err
	}

	h.acquire()
	got := argon2.IDKey([]byte(password), salt,
		params.Iterations, params.MemoryKiB, params.Parallelism, uint32(len(want)))
	h.release()

	// Constant-time: a byte-wise early return leaks how much of the derived
	// key matched, which is enough to reconstruct it one byte at a time.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, ErrMismatch
	}
	return h.weakerThanCurrent(params), nil
}

// matches is Verify's inner half, used for the decoy path where the result is
// discarded but the work must still happen.
func (h *Hasher) matches(password, encodedHash string) (bool, error) {
	params, salt, want, err := decode(encodedHash)
	if err != nil {
		return false, err
	}
	h.acquire()
	got := argon2.IDKey([]byte(password), salt,
		params.Iterations, params.MemoryKiB, params.Parallelism, uint32(len(want)))
	h.release()
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// weakerThanCurrent reports whether a digest's parameters fall below the
// hasher's. Salt and key length are included: shortening either weakens the
// digest as surely as lowering the memory cost does.
func (h *Hasher) weakerThanCurrent(p Params) bool {
	return p.MemoryKiB < h.params.MemoryKiB ||
		p.Iterations < h.params.Iterations ||
		p.SaltLength < h.params.SaltLength ||
		p.KeyLength < h.params.KeyLength
}

func (h *Hasher) acquire() { h.sem <- struct{}{} }
func (h *Hasher) release() { <-h.sem }

// encode renders the PHC string format argon2's reference implementation uses,
// so a digest written here is readable by any standard argon2 tool:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>
//
// Base64 is the unpadded standard alphabet, which is what PHC specifies.
func encode(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// decode parses a PHC string back into parameters, salt and expected key.
//
// Every failure is an error, never a silent default. A digest that decodes
// "mostly" is a digest that verifies against parameters the writer did not
// choose, which is indistinguishable from an attacker downgrading a stored
// credential to m=8,t=1 and brute-forcing it offline in seconds.
func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// The leading "$" makes parts[0] the empty string, giving
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, key].
	if len(parts) != 6 || parts[0] != "" {
		return Params{}, nil, nil, ErrMalformedHash
	}
	if parts[1] != "argon2id" {
		return Params{}, nil, nil, fmt.Errorf("%w: %s", ErrUnsupportedAlgorithm, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("%w: argon2 version %d", ErrUnsupportedAlgorithm, version)
	}

	p, err := decodeParams(parts[3])
	if err != nil {
		return Params{}, nil, nil, err
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return Params{}, nil, nil, ErrMalformedHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return Params{}, nil, nil, ErrMalformedHash
	}

	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))
	return p, salt, key, nil
}

// decodeParams parses the "m=<n>,t=<n>,p=<n>" segment. Written out rather than
// handed to Sscanf so that a segment with the fields reordered, repeated, or
// carrying trailing junk is rejected instead of partially consumed.
func decodeParams(segment string) (Params, error) {
	fields := strings.Split(segment, ",")
	if len(fields) != 3 {
		return Params{}, ErrMalformedHash
	}
	var (
		p    Params
		seen = map[string]bool{}
	)
	for _, f := range fields {
		name, value, ok := strings.Cut(f, "=")
		if !ok || seen[name] {
			return Params{}, ErrMalformedHash
		}
		seen[name] = true

		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return Params{}, ErrMalformedHash
		}
		switch name {
		case "m":
			p.MemoryKiB = uint32(n)
		case "t":
			p.Iterations = uint32(n)
		case "p":
			if n > 255 {
				return Params{}, ErrMalformedHash
			}
			p.Parallelism = uint8(n)
		default:
			return Params{}, ErrMalformedHash
		}
	}
	if p.MemoryKiB == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		return Params{}, ErrMalformedHash
	}
	return p, nil
}
