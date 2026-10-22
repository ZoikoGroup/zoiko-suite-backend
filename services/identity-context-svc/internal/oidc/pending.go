package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Pending is an authorization request in flight: validated at /authorize,
// carried through the login steps, consumed when the code is issued.
type Pending struct {
	ID                  string    `json:"id"`
	ClientID            string    `json:"client_id"`
	RedirectURI         string    `json:"redirect_uri"`
	Scope               string    `json:"scope"`
	State               string    `json:"state,omitempty"`
	Nonce               string    `json:"nonce,omitempty"`
	CodeChallenge       string    `json:"code_challenge"`
	CodeChallengeMethod string    `json:"code_challenge_method"`
	DPoPJKT             string    `json:"dpop_jkt,omitempty"`
	TenantHint          string    `json:"tenant_hint,omitempty"`
	LoginHint           string    `json:"login_hint,omitempty"`
	Prompt              string    `json:"prompt,omitempty"`
	MaxAge              int       `json:"max_age,omitempty"`
	ACRValues           string    `json:"acr_values,omitempty"`
	CSRFHash            string    `json:"csrf_hash"`
	CreatedAt           time.Time `json:"created_at"`

	// Set as the login progresses.
	TenantID      string   `json:"tenant_id,omitempty"`
	PrincipalID   string   `json:"principal_id,omitempty"`
	AMR           []string `json:"amr,omitempty"`
	IdPID         string   `json:"idp_id,omitempty"`
	FailedSecond  int      `json:"failed_second,omitempty"`
	PasswordOK    bool     `json:"password_ok,omitempty"`
	SecondFactors []string `json:"second_factors,omitempty"`
}

// PendingStore keeps in-flight requests for a few minutes.
type PendingStore interface {
	Put(ctx context.Context, p *Pending, ttl time.Duration) error
	Get(ctx context.Context, id string) (*Pending, error)
	Delete(ctx context.Context, id string) error
	// Seen records a one-time value (a DPoP proof, a federation state) and
	// reports whether it was already recorded.
	Seen(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// ErrPendingNotFound is an unknown or expired request.
var ErrPendingNotFound = errors.New("authorization request expired or unknown")

// RedisPending is the production store, shared across replicas.
type RedisPending struct{ rdb *redis.Client }

// NewRedisPending builds one.
func NewRedisPending(rdb *redis.Client) *RedisPending { return &RedisPending{rdb: rdb} }

func (s *RedisPending) Put(ctx context.Context, p *Pending, ttl time.Duration) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, "oidc:pending:"+p.ID, b, ttl).Err()
}

func (s *RedisPending) Get(ctx context.Context, id string) (*Pending, error) {
	b, err := s.rdb.Get(ctx, "oidc:pending:"+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrPendingNotFound
	}
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *RedisPending) Delete(ctx context.Context, id string) error {
	return s.rdb.Del(ctx, "oidc:pending:"+id).Err()
}

func (s *RedisPending) Seen(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := s.rdb.SetNX(ctx, "oidc:seen:"+key, 1, ttl).Result()
	if err != nil {
		return false, err
	}
	return !ok, nil
}

// MemoryPending is an in-process store (tests, single-replica local runs).
type MemoryPending struct {
	mu   sync.Mutex
	m    map[string]memPending
	seen map[string]time.Time
}

type memPending struct {
	b     []byte
	until time.Time
}

// NewMemoryPending builds one.
func NewMemoryPending() *MemoryPending {
	return &MemoryPending{m: map[string]memPending{}, seen: map[string]time.Time{}}
}

func (s *MemoryPending) Put(_ context.Context, p *Pending, ttl time.Duration) error {
	b, _ := json.Marshal(p)
	s.mu.Lock()
	s.m[p.ID] = memPending{b: b, until: time.Now().Add(ttl)}
	s.mu.Unlock()
	return nil
}

func (s *MemoryPending) Get(_ context.Context, id string) (*Pending, error) {
	s.mu.Lock()
	e, ok := s.m[id]
	s.mu.Unlock()
	if !ok || time.Now().After(e.until) {
		return nil, ErrPendingNotFound
	}
	var p Pending
	_ = json.Unmarshal(e.b, &p)
	return &p, nil
}

func (s *MemoryPending) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
	return nil
}

func (s *MemoryPending) Seen(_ context.Context, key string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until, ok := s.seen[key]; ok && time.Now().Before(until) {
		return true, nil
	}
	s.seen[key] = time.Now().Add(ttl)
	return false, nil
}

// replayCache adapts PendingStore.Seen to idenv.ReplayCache for DPoP proofs.
type replayCache struct{ store PendingStore }

func (r replayCache) Seen(jti string, until time.Time) bool {
	seen, err := r.store.Seen(context.Background(), "dpop:"+jti, time.Until(until))
	if err != nil {
		// Fail closed: an unanswerable replay check treats the proof as seen.
		return true
	}
	return seen
}

// RandomToken returns n random bytes, base64url.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is the stored form of a code, refresh token or link.
func HashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}
