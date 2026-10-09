package kafkaout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"zoiko.io/eventing/outbox"
)

func TestNew_EnforcesAcknowledgedWrites(t *testing.T) {
	w, err := New(Config{Brokers: []string{"localhost:9092"}, Topic: "zoiko.test.events"})
	if err != nil {
		t.Fatal(err)
	}
	if w.w.RequiredAcks != kafka.RequireAll {
		t.Fatalf("RequiredAcks = %v; fire-and-forget would let the outbox mark unstored events published", w.w.RequiredAcks)
	}
	if _, ok := w.w.Balancer.(*kafka.Hash); !ok {
		t.Fatal("balancer must hash the key so one aggregate stays on one partition")
	}
	if _, err := New(Config{Topic: "t"}); err == nil {
		t.Fatal("missing brokers accepted")
	}
	if _, err := New(Config{Brokers: []string{"b:9092"}}); err == nil {
		t.Fatal("missing topic accepted")
	}
}

func TestIsPermanent(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{kafka.MessageSizeTooLarge, true},
		{kafka.InvalidRecord, true},
		{fmt.Errorf("produce: %w", kafka.InvalidMessage), true},
		{kafka.WriteErrors{kafka.RecordListTooLarge}, true},
		{kafka.WriteErrors{nil, kafka.NotLeaderForPartition}, false},
		{kafka.NotLeaderForPartition, false},
		{kafka.RequestTimedOut, false},
		{errors.New("dial tcp: connection refused"), false},
	}
	for _, c := range cases {
		if got := isPermanent(c.err); got != c.want {
			t.Errorf("isPermanent(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestProbe_UnreachableBrokerFailsWithinTimeout(t *testing.T) {
	w, err := New(Config{Brokers: []string{"127.0.0.1:1"}, Topic: "zoiko.test.events", ProbeTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := w.Probe(context.Background()); err == nil {
		t.Fatal("probe of an unreachable broker succeeded")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("probe did not respect its timeout")
	}
}

func TestWriteMessages_SingleRejectedRecordIsPermanent(t *testing.T) {
	// Exercise the classification seam without a broker: the wrapper is what
	// the relay keys its immediate quarantine on.
	err := outbox.Permanent(kafka.MessageSizeTooLarge)
	if !errors.Is(err, outbox.ErrPermanent) || !errors.Is(err, kafka.MessageSizeTooLarge) {
		t.Fatal("permanent wrapper must keep both the marker and the cause")
	}
}

// TestRealBroker runs against a live broker when TEST_KAFKA_BROKERS is set
// (e.g. localhost:9092). It proves what the fakes cannot: an acknowledged
// write lands on the topic with its key and event-id header, and Probe tells
// a healthy topic from a missing one.
func TestRealBroker(t *testing.T) {
	broker := os.Getenv("TEST_KAFKA_BROKERS")
	if broker == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	topic := fmt.Sprintf("zoiko.eventing-test.%d", time.Now().UnixNano())

	w, err := New(Config{Brokers: []string{broker}, Topic: topic, AllowAutoTopicCreation: true})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Probe(ctx); err == nil {
		t.Fatal("probe of a topic that does not exist yet must fail")
	}

	msg := outbox.Message{
		Key:     []byte("partition-key"),
		Value:   []byte(`{"id":"evt-1"}`),
		Headers: []outbox.Header{{Key: outbox.EventIDHeader, Value: []byte("evt-1")}},
	}
	// The first write may race the auto-creation; the outbox would simply
	// retry, and so does this test.
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = w.WriteMessages(ctx, msg)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("acknowledged write failed: %v", err)
	}
	if err := w.Probe(ctx); err != nil {
		t.Fatalf("probe of a healthy topic failed: %v", err)
	}

	r := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{broker}, Topic: topic, MaxWait: time.Second})
	defer r.Close()
	got, err := r.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got.Key) != "partition-key" || string(got.Value) != `{"id":"evt-1"}` {
		t.Fatalf("read back key %q value %q", got.Key, got.Value)
	}
	if len(got.Headers) != 1 || got.Headers[0].Key != outbox.EventIDHeader || string(got.Headers[0].Value) != "evt-1" {
		t.Fatalf("event id header not delivered: %+v", got.Headers)
	}
}

// A broker that accepts the connection but never answers — what a frozen or
// overloaded broker looks like from the client — must fail the probe within
// its timeout, not hang the relay.
func TestProbe_UnresponsiveBrokerFailsWithinTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, say nothing
		}
	}()
	w, err := New(Config{Brokers: []string{ln.Addr().String()}, Topic: "zoiko.test.events", ProbeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := w.Probe(context.Background()); err == nil {
		t.Fatal("probe of an unresponsive broker succeeded")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("probe did not respect its timeout")
	}
}

// cutProxy forwards TCP to target until cut, then drops every connection and
// refuses new ones.
type cutProxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln}
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, in, out)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(out, in) }()
			go func() { _, _ = io.Copy(in, out) }()
		}
	}()
	return p
}

func (p *cutProxy) cut() {
	p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
}

// Regression: Probe once answered from kafka-go's metadata cache, so after
// one success a broker that had gone away still probed healthy and a whole
// outage was charged to the events' retry budgets. Found by pausing a real
// broker under a running general-ledger-svc.
func TestRealBroker_ProbeObservesBrokerLoss(t *testing.T) {
	broker := os.Getenv("TEST_KAFKA_BROKERS")
	if broker == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	topic := fmt.Sprintf("zoiko.eventing-probe-test.%d", time.Now().UnixNano())

	// Create the topic through the real broker.
	direct, err := New(Config{Brokers: []string{broker}, Topic: topic, AllowAutoTopicCreation: true})
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = direct.WriteMessages(ctx, outbox.Message{Key: []byte("k"), Value: []byte(`{}`)})
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}

	proxy := newCutProxy(t, broker)
	viaProxy, err := New(Config{Brokers: []string{proxy.ln.Addr().String()}, Topic: topic, ProbeTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := viaProxy.Probe(ctx); err != nil {
		t.Fatalf("probe through a live proxy failed: %v", err)
	}
	proxy.cut()
	if err := viaProxy.Probe(ctx); err == nil {
		t.Fatal("probe reported a broker it can no longer reach as healthy (answered from cache?)")
	}
}
