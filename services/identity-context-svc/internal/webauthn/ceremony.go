package webauthn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Ceremonies keeps the server half of a WebAuthn ceremony (the challenge and
// what it was issued for) between its begin and finish calls. Take is
// single-use: a challenge answered once can never be answered again, which is
// what makes a captured assertion worthless.
type Ceremonies interface {
	Put(ctx context.Context, key string, data []byte, ttl time.Duration) error
	// Take returns and deletes the entry; errCeremonyNotFound when absent or
	// expired.
	Take(ctx context.Context, key string) ([]byte, error)
}

var errCeremonyNotFound = errors.New("webauthn ceremony not found")

// ceremonyKey stores a ceremony under the hash of its handle, so a dump of
// the store does not hand out live handles.
func ceremonyKey(handle string) string {
	sum := sha256.Sum256([]byte(handle))
	return "webauthn:ceremony:" + hex.EncodeToString(sum[:])
}

// RedisCeremonies is the production store, shared across replicas: a begin
// answered by one replica can be finished on another.
type RedisCeremonies struct{ rdb *redis.Client }

// NewRedisCeremonies builds one.
func NewRedisCeremonies(rdb *redis.Client) *RedisCeremonies { return &RedisCeremonies{rdb: rdb} }

func (s *RedisCeremonies) Put(ctx context.Context, key string, data []byte, ttl time.Duration) error {
	return s.rdb.Set(ctx, ceremonyKey(key), data, ttl).Err()
}

func (s *RedisCeremonies) Take(ctx context.Context, key string) ([]byte, error) {
	b, err := s.rdb.GetDel(ctx, ceremonyKey(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, errCeremonyNotFound
	}
	return b, err
}

// MemoryCeremonies is an in-process store (tests, single-replica local runs).
type MemoryCeremonies struct {
	mu sync.Mutex
	m  map[string]memCeremony
}

type memCeremony struct {
	b     []byte
	until time.Time
}

// NewMemoryCeremonies builds one.
func NewMemoryCeremonies() *MemoryCeremonies {
	return &MemoryCeremonies{m: map[string]memCeremony{}}
}

func (s *MemoryCeremonies) Put(_ context.Context, key string, data []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[ceremonyKey(key)] = memCeremony{b: append([]byte(nil), data...), until: time.Now().Add(ttl)}
	return nil
}

func (s *MemoryCeremonies) Take(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ceremonyKey(key)
	e, ok := s.m[k]
	delete(s.m, k)
	if !ok || time.Now().After(e.until) {
		return nil, errCeremonyNotFound
	}
	return e.b, nil
}
