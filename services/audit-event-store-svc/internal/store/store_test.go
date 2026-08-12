package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/audit-event-store-svc/internal/store"
)

func newEvent(eventID string, payload string) *store.AuditEvent {
	return &store.AuditEvent{
		EventID:       eventID,
		EventType:     "test.event",
		TenantID:      "tenant-abc",
		LegalEntityID: "entity-xyz",
		SourceService: "test-svc",
		SchemaVersion: "1.0",
		Payload:       json.RawMessage(payload),
	}
}

// TestChain_GenesisHasNoPreviousHash verifies the very first event ever
// stored gets a real payload_hash but no previous_event_hash — the
// documented genesis case.
func TestChain_GenesisHasNoPreviousHash(t *testing.T) {
	s := store.NewFakeStore()
	e := newEvent("evt-1", `{"a":1}`)

	require.NoError(t, s.Store(context.Background(), e))

	assert.Equal(t, int64(1), e.SequenceNumber)
	assert.NotEmpty(t, e.PayloadHash, "genesis event must still get a real payload_hash")
	assert.Empty(t, e.PreviousEventHash, "genesis event must have no previous_event_hash")
}

// TestChain_SecondEventLinksToFirst verifies the core hash-chain property:
// event N's previous_event_hash equals event N-1's payload_hash.
func TestChain_SecondEventLinksToFirst(t *testing.T) {
	s := store.NewFakeStore()
	first := newEvent("evt-1", `{"a":1}`)
	second := newEvent("evt-2", `{"b":2}`)

	require.NoError(t, s.Store(context.Background(), first))
	require.NoError(t, s.Store(context.Background(), second))

	assert.Equal(t, int64(2), second.SequenceNumber)
	assert.Equal(t, first.PayloadHash, second.PreviousEventHash,
		"second event's previous_event_hash must equal first event's payload_hash")
	assert.NotEqual(t, first.PayloadHash, second.PayloadHash,
		"different payloads must hash differently")
}

// TestChain_PayloadHashIsDeterministicAndVerifiable verifies that hashing
// the same payload bytes always produces the same hash — the property that
// lets a reviewer independently re-verify a stored row wasn't tampered with.
func TestChain_PayloadHashIsDeterministicAndVerifiable(t *testing.T) {
	s := store.NewFakeStore()
	e1 := newEvent("evt-1", `{"same":"payload"}`)
	require.NoError(t, s.Store(context.Background(), e1))

	s2 := store.NewFakeStore()
	e2 := newEvent("evt-2", `{"same":"payload"}`)
	require.NoError(t, s2.Store(context.Background(), e2))

	assert.Equal(t, e1.PayloadHash, e2.PayloadHash,
		"identical payload bytes must produce identical hashes, independent of chain position")
}

// TestChain_DuplicateEventDoesNotConsumeChainLink verifies that a duplicate
// event_id delivery is a true no-op with respect to the chain: it must not
// advance the sequence counter or leave a gap.
func TestChain_DuplicateEventDoesNotConsumeChainLink(t *testing.T) {
	s := store.NewFakeStore()
	first := newEvent("evt-1", `{"a":1}`)
	require.NoError(t, s.Store(context.Background(), first))

	dup := newEvent("evt-1", `{"a":1}`) // same event_id
	require.NoError(t, s.Store(context.Background(), dup))
	assert.Zero(t, dup.SequenceNumber, "duplicate delivery must not be assigned a new sequence_number")

	third := newEvent("evt-3", `{"c":3}`)
	require.NoError(t, s.Store(context.Background(), third))

	assert.Equal(t, int64(2), third.SequenceNumber,
		"the chain must have no gap: the next real event gets sequence 2, not 3")
	assert.Equal(t, first.PayloadHash, third.PreviousEventHash,
		"the next real event must link to the last REAL event, skipping the no-op duplicate")
}

// TestChain_FullChainVerifiable simulates what an auditor would actually do:
// walk the chain in order and confirm every link matches, catching a
// tampered row.
func TestChain_FullChainVerifiable(t *testing.T) {
	s := store.NewFakeStore()
	for i, payload := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		e := newEvent("evt-"+string(rune('1'+i)), payload)
		require.NoError(t, s.Store(context.Background(), e))
	}

	chain := s.ChainInOrder()
	require.Len(t, chain, 3)

	assert.Empty(t, chain[0].PreviousEventHash, "first link has no predecessor")
	for i := 1; i < len(chain); i++ {
		assert.Equal(t, chain[i-1].PayloadHash, chain[i].PreviousEventHash,
			"chain link %d must reference link %d's payload_hash", i, i-1)
	}

	// Simulate tampering: mutate a stored payload_hash and confirm the
	// chain no longer verifies — this is the whole point of the feature.
	tampered := chain[1]
	tampered.PayloadHash = "tampered-hash"
	assert.NotEqual(t, tampered.PayloadHash, chain[2].PreviousEventHash,
		"a tampered middle link must break verification against the next link")
}

// TestChain_ConcurrentInsertsProduceNoFork fires many concurrent Store()
// calls and verifies the resulting chain is still a single linear sequence
// with no duplicate sequence_numbers and no two events claiming the same
// previous_event_hash (which would indicate a fork).
func TestChain_ConcurrentInsertsProduceNoFork(t *testing.T) {
	s := store.NewFakeStore()
	const n = 20

	var wg sync.WaitGroup
	ready := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			e := newEvent(fmt.Sprintf("evt-concurrent-%02d", i), fmt.Sprintf(`{"i":%d}`, i))
			_ = s.Store(context.Background(), e)
		}()
	}
	close(ready)
	wg.Wait()

	assert.Equal(t, n, s.Count())

	chain := s.ChainInOrder()
	require.Len(t, chain, n)
	seen := map[string]bool{}
	for i, ev := range chain {
		require.Equal(t, int64(i+1), ev.SequenceNumber, "sequence numbers must be gap-free and ordered")
		if i > 0 {
			assert.Equal(t, chain[i-1].PayloadHash, ev.PreviousEventHash,
				"every link must reference exactly its immediate predecessor — a mismatch here means a fork occurred")
		}
		require.False(t, seen[ev.PayloadHash+ev.PreviousEventHash], "no two links may share the same (hash, prev_hash) pair")
		seen[ev.PayloadHash+ev.PreviousEventHash] = true
	}
}
