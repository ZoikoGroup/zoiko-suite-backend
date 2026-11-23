// Package session issues and verifies anonymous public chat sessions
// (Doc 05 §08, Doc 06 §11). A session grant is a random opaque bearer value;
// only its SHA-256 hash is stored and it is never logged.
package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"zoiko.io/public-ai-svc/internal/store"
)

var (
	ErrExpired  = errors.New("session expired or unknown")
	ErrMismatch = errors.New("idempotency key reused with a different request")
)

// Service manages sessions.
type Service struct {
	store store.Store
	ttl   time.Duration
	idle  time.Duration // 0: no idle expiry
	// idem is how long a session Idempotency-Key may be replayed
	// (PUBLIC_AI_IDEMPOTENCY_TTL).
	idem time.Duration
	now  func() time.Time
	// ReleaseID, when set, names the active KB release; it is stamped on
	// each new session (Doc 04 §16).
	ReleaseID func() string
}

// WithIdleTTL ends a session that has had no completed turn for d, before
// its fixed maximum (Doc 06 §17).
func (s *Service) WithIdleTTL(d time.Duration) *Service {
	s.idle = d
	return s
}

// DefaultIdempotencyTTL matches the config default for PUBLIC_AI_IDEMPOTENCY_TTL.
const DefaultIdempotencyTTL = 24 * time.Hour

// WithIdempotencyTTL bounds how long a create-session Idempotency-Key can be
// replayed (Doc 05 §12). d <= 0 keeps the default.
func (s *Service) WithIdempotencyTTL(d time.Duration) *Service {
	if d > 0 {
		s.idem = d
	}
	return s
}

func New(s store.Store, ttl time.Duration) *Service {
	return &Service{store: s, ttl: ttl, idem: DefaultIdempotencyTTL, now: func() time.Time { return time.Now().UTC() }}
}

// Issued is a newly created (or replayed) session and its plaintext grant.
type Issued struct {
	Session store.Session
	Grant   string
}

// HashGrant returns the stored form of a grant.
func HashGrant(grant string) []byte {
	sum := sha256.Sum256([]byte(grant))
	return sum[:]
}

func random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return b
}

func newGrant() string { return "pgr_" + base64.RawURLEncoding.EncodeToString(random(32)) }

// Create issues a session. Replaying the same idempotency key with the same
// request returns the same session_id and expiry with a freshly rotated grant:
// the original grant is never stored, so it cannot be returned again, and the
// rotation invalidates it (dev default recorded in ADR-005).
func (s *Service) Create(ctx context.Context, key string, requestHash []byte, locale, market, page, origin string) (Issued, error) {
	if prev, hash, err := s.store.SessionByIdempotencyKey(ctx, key); err == nil {
		if !bytes.Equal(hash, requestHash) {
			return Issued{}, ErrMismatch
		}
		// Doc 05 §12: key reuse is not a credential. Only the same site
		// origin, within the idempotency window, gets the replay; anyone
		// else holding the key cannot mint a grant for this session.
		if prev.Origin != origin || !s.now().Before(prev.CreatedAt.Add(s.idem)) {
			return Issued{}, ErrMismatch
		}
		if prev.DeletedAt != nil || !s.now().Before(prev.ExpiresAt) {
			return Issued{}, ErrExpired
		}
		grant := newGrant()
		if err := s.store.RotateGrant(ctx, prev.SessionID, HashGrant(grant)); err != nil {
			return Issued{}, err
		}
		return Issued{Session: prev, Grant: grant}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return Issued{}, err
	}

	now := s.now()
	grant := newGrant()
	sess := store.Session{
		SessionID:   "pub_s_" + hex.EncodeToString(random(12)),
		GrantHash:   HashGrant(grant),
		CreatedAt:   now,
		ExpiresAt:   now.Add(s.ttl),
		Locale:      locale,
		MarketCode:  market,
		PageContext: page,
		Origin:      origin,
	}
	if s.ReleaseID != nil {
		sess.ReleaseID = s.ReleaseID()
	}
	if err := s.store.CreateSession(ctx, sess, key, requestHash); err != nil {
		return Issued{}, err
	}
	return Issued{Session: sess, Grant: grant}, nil
}

// Authenticate resolves a grant to a live session.
func (s *Service) Authenticate(ctx context.Context, grant string) (store.Session, error) {
	sess, err := s.lookup(ctx, grant)
	if err != nil {
		return store.Session{}, err
	}
	if sess.DeletedAt != nil {
		return store.Session{}, ErrExpired
	}
	return sess, nil
}

func (s *Service) lookup(ctx context.Context, grant string) (store.Session, error) {
	if grant == "" || len(grant) > 256 {
		return store.Session{}, ErrExpired
	}
	sess, err := s.store.SessionByGrant(ctx, HashGrant(grant))
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, ErrExpired
	}
	if err != nil {
		return store.Session{}, err
	}
	if !s.now().Before(sess.ExpiresAt) {
		return store.Session{}, ErrExpired
	}
	if s.idle > 0 {
		last := sess.LastSeenAt
		if last.IsZero() {
			last = sess.CreatedAt
		}
		if !s.now().Before(last.Add(s.idle)) {
			return store.Session{}, ErrExpired
		}
	}
	return sess, nil
}

// FromOrigin reports whether a request from origin may use sess: a grant
// works only on the site origin that created it (Doc 05 §08). Sessions
// created before origins were recorded carry none and are not restricted.
func FromOrigin(sess store.Session, origin string) bool {
	return sess.Origin == "" || sess.Origin == origin
}

// Delete ends the session identified by grant if it owns sessionID. Deleting
// an already-deleted own session succeeds (Doc 05 §07). It reports
// ErrExpired for unknown/expired grants and store.ErrNotFound when the grant
// belongs to a different session (no existence leak).
func (s *Service) Delete(ctx context.Context, grant, sessionID string) error {
	sess, err := s.lookup(ctx, grant)
	if err != nil {
		return err
	}
	if sess.SessionID != sessionID {
		return store.ErrNotFound
	}
	if sess.DeletedAt != nil {
		return nil
	}
	return s.store.DeleteSession(ctx, sessionID, s.now())
}
