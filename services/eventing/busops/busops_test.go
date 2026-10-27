package busops

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
)

func TestComputeLag(t *testing.T) {
	l := ComputeLag("t", "g", []PartitionOffsets{
		{Partition: 1, First: 0, Latest: 100, Committed: 90}, // 10 behind
		{Partition: 0, First: 0, Latest: 50, Committed: -1},  // never committed: all 50
		{Partition: 2, First: 40, Latest: 60, Committed: 10}, // commit fell off retention: measured from First -> 20
		{Partition: 3, First: 0, Latest: 5, Committed: 9},    // committed ahead of latest: never negative
		{Partition: 4, First: 30, Latest: 30, Committed: -1}, // empty partition
	})
	want := map[int]int64{0: 50, 1: 10, 2: 20, 3: 0, 4: 0}
	for _, p := range l.Partitions {
		if p.Lag != want[p.Partition] {
			t.Errorf("partition %d lag = %d, want %d", p.Partition, p.Lag, want[p.Partition])
		}
	}
	if l.Total != 80 {
		t.Errorf("total = %d, want 80", l.Total)
	}
	for i := 1; i < len(l.Partitions); i++ {
		if l.Partitions[i].Partition < l.Partitions[i-1].Partition {
			t.Fatal("partitions must be ordered")
		}
	}
}

func TestEvaluateAlerts(t *testing.T) {
	healthy := TopicHealth{Topic: "t", Partitions: 3}
	if a := Evaluate(Lag{Total: 5}, healthy, 100); len(a) != 0 {
		t.Fatalf("healthy: %v", a)
	}
	a := Evaluate(Lag{Group: "g", Topic: "t", Total: 500}, TopicHealth{Topic: "t", Partitions: 3, Unavailable: []int{2}}, 100)
	if len(a) != 2 || a[0].Event != "consumer_lag_exceeded" || a[1].Event != "partition_unavailable" || !strings.Contains(a[1].Detail, "2") {
		t.Fatalf("alerts = %+v", a)
	}
	if a := Evaluate(Lag{Total: 1 << 40}, healthy, 0); len(a) != 0 {
		t.Fatal("zero maxLag means unchecked")
	}
}

func TestHealthy(t *testing.T) {
	if !(TopicHealth{Partitions: 2}).Healthy() {
		t.Error("two clean partitions are healthy")
	}
	for name, h := range map[string]TopicHealth{
		"none":             {},
		"unavailable":      {Partitions: 2, Unavailable: []int{1}},
		"under-replicated": {Partitions: 2, UnderReplicated: []int{0}},
	} {
		if h.Healthy() {
			t.Errorf("%s reported healthy", name)
		}
	}
}

// ---- against a real broker ---------------------------------------------------

func broker(t *testing.T) string {
	t.Helper()
	b := os.Getenv("KAFKA_TEST_BROKER")
	if b == "" {
		// The shared CI job provides Postgres but no broker, so CI alone must not
		// fail this; a job that does run a broker opts in to strictness explicitly.
		if os.Getenv("REQUIRE_KAFKA_TESTS") != "" {
			t.Fatal("KAFKA_TEST_BROKER must be set when REQUIRE_KAFKA_TESTS is set")
		}
		t.Skip("KAFKA_TEST_BROKER not set")
	}
	return b
}

func newTopic(t *testing.T, addr string, partitions int) string {
	t.Helper()
	topic := "busops-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	c := &kafka.Client{Addr: kafka.TCP(addr), Timeout: 10 * time.Second}
	resp, err := c.CreateTopics(context.Background(), &kafka.CreateTopicsRequest{
		Topics: []kafka.TopicConfig{{Topic: topic, NumPartitions: partitions, ReplicationFactor: 1}},
	})
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}
	if e := resp.Errors[topic]; e != nil {
		t.Fatalf("create topic: %v", e)
	}
	return topic
}

func produce(t *testing.T, addr, topic string, n int) {
	t.Helper()
	w := &kafka.Writer{Addr: kafka.TCP(addr), Topic: topic, Balancer: &kafka.Hash{}, RequiredAcks: kafka.RequireAll,
		BatchTimeout: 10 * time.Millisecond, AllowAutoTopicCreation: false}
	defer w.Close()
	msgs := make([]kafka.Message, n)
	for i := range msgs {
		msgs[i] = kafka.Message{Key: []byte(fmt.Sprintf("k%d", i)), Value: []byte("v")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for deadline := time.Now().Add(15 * time.Second); ; {
		err := w.WriteMessages(ctx, msgs...)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("produce: %v", err)
		}
		time.Sleep(300 * time.Millisecond) // topic leaders may still be electing
	}
}

func consume(t *testing.T, addr, topic, group string, n int) {
	t.Helper()
	r := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{addr}, Topic: topic, GroupID: group,
		MinBytes: 1, MaxBytes: 1 << 20, StartOffset: kafka.FirstOffset, MaxWait: 500 * time.Millisecond})
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < n; i++ {
		if _, err := r.ReadMessage(ctx); err != nil { // commits synchronously (no CommitInterval)
			t.Fatalf("consume %d: %v", i, err)
		}
	}
}

func TestConsumerLagAgainstRealBroker(t *testing.T) {
	addr := broker(t)
	topic := newTopic(t, addr, 3)
	group := "g-" + uuid.NewString()[:8]
	c := &kafka.Client{Addr: kafka.TCP(addr), Timeout: 10 * time.Second}
	ctx := context.Background()

	produce(t, addr, topic, 30)

	l, err := ConsumerLag(ctx, c, topic, group)
	if err != nil {
		t.Fatal(err)
	}
	if l.Total != 30 || len(l.Partitions) != 3 {
		t.Fatalf("a group that never consumed is behind by everything: %+v", l)
	}

	consume(t, addr, topic, group, 12)
	l, err = ConsumerLag(ctx, c, topic, group)
	if err != nil {
		t.Fatal(err)
	}
	if l.Total != 18 {
		t.Fatalf("after reading 12 of 30 the lag must be 18, got %d (%+v)", l.Total, l)
	}

	produce(t, addr, topic, 5)
	if l, _ = ConsumerLag(ctx, c, topic, group); l.Total != 23 {
		t.Fatalf("new messages must add to lag: %d", l.Total)
	}

	other, _ := ConsumerLag(ctx, c, topic, "other-"+uuid.NewString()[:8])
	if other.Total != 35 {
		t.Fatalf("groups are independent: %d", other.Total)
	}
}

func TestHealthAgainstRealBroker(t *testing.T) {
	addr := broker(t)
	topic := newTopic(t, addr, 4)
	c := &kafka.Client{Addr: kafka.TCP(addr), Timeout: 10 * time.Second}
	var h TopicHealth
	var err error
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if h, err = Health(context.Background(), c, topic); err == nil && h.Healthy() {
			break
		}
	}
	if err != nil || !h.Healthy() || h.Partitions != 4 {
		t.Fatalf("health = %+v err=%v", h, err)
	}
	if _, err := Health(context.Background(), c, "no-such-topic-"+uuid.NewString()[:6]); err == nil {
		t.Fatal("an unknown topic must be an error, not 'healthy'")
	}
	if _, err := ConsumerLag(context.Background(), c, "no-such-topic-"+uuid.NewString()[:6], "g"); err == nil {
		t.Fatal("lag on an unknown topic must be an error")
	}
}

func TestUnreachableBrokerFailsClosed(t *testing.T) {
	c := &kafka.Client{Addr: kafka.TCP("127.0.0.1:1"), Timeout: 500 * time.Millisecond}
	if _, err := ConsumerLag(context.Background(), c, "t", "g"); err == nil {
		t.Fatal("an unreachable broker must be an error, never zero lag")
	}
	if _, err := Health(context.Background(), c, "t"); err == nil {
		t.Fatal("an unreachable broker must be an error, never healthy")
	}
}
