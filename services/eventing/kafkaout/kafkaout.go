// Package kafkaout adapts segmentio/kafka-go to outbox.Writer.
//
// It builds the kafka.Writer itself rather than accepting one, because the
// setting that matters most is the one every hand-built writer in this repo
// got wrong: kafka-go's RequiredAcks defaults to RequireNone, fire-and-forget.
// With that default WriteMessages returns before any broker has stored the
// record, the outbox marks the event published, and a broker crash loses it —
// exactly the loss the outbox exists to prevent. This writer always uses
// RequireAll (the full in-sync replica set has the record).
package kafkaout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"

	"zoiko.io/eventing/outbox"
)

// Config describes the target topic.
type Config struct {
	Brokers []string
	Topic   string
	// AllowAutoTopicCreation is for local stacks only; production topics are
	// provisioned. kafka-go never asks the broker to auto-create unless this
	// is set, whatever the broker's own setting.
	AllowAutoTopicCreation bool
	// WriteTimeout bounds one produce request. Keep it well under the relay's
	// lease (outbox.Config.Lease) so a lease cannot expire mid-write.
	// Default 10s.
	WriteTimeout time.Duration
	// ProbeTimeout bounds the metadata request Probe sends. Default 5s.
	ProbeTimeout time.Duration
}

// Writer is an outbox.Writer over kafka-go.
type Writer struct {
	w            *kafka.Writer
	brokers      []string
	dialer       *kafka.Dialer
	topic        string
	probeTimeout time.Duration
}

var _ outbox.Writer = (*Writer)(nil)

// New builds the writer. It connects lazily: an unreachable broker does not
// fail startup, it shows up as probe failures while events wait in Postgres.
func New(cfg Config) (*Writer, error) {
	if len(cfg.Brokers) == 0 || cfg.Topic == "" {
		return nil, errors.New("kafkaout: brokers and topic are required")
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 5 * time.Second
	}
	addr := kafka.TCP(cfg.Brokers...)
	return &Writer{
		w: &kafka.Writer{
			Addr:     addr,
			Topic:    cfg.Topic,
			Balancer: &kafka.Hash{}, // same key → same partition (ZS-EVENT-001 §5.3)
			// The relay hands over a whole claimed batch per call, so a short
			// timeout costs nothing and a long one adds latency to every
			// partially-filled batch (kafka-go's default is 1s).
			BatchTimeout:           10 * time.Millisecond,
			RequiredAcks:           kafka.RequireAll,
			WriteTimeout:           cfg.WriteTimeout,
			AllowAutoTopicCreation: cfg.AllowAutoTopicCreation,
			// The outbox is the retry loop, with backoff and quarantine; a
			// second hidden retry loop inside the client would only stretch
			// one attempt past the lease.
			MaxAttempts: 1,
		},
		brokers:      cfg.Brokers,
		dialer:       &kafka.Dialer{Timeout: cfg.ProbeTimeout},
		topic:        cfg.Topic,
		probeTimeout: cfg.ProbeTimeout,
	}, nil
}

// WriteMessages sends msgs and waits for RequireAll acknowledgement.
func (k *Writer) WriteMessages(ctx context.Context, msgs ...outbox.Message) error {
	km := make([]kafka.Message, len(msgs))
	for i, m := range msgs {
		headers := make([]kafka.Header, len(m.Headers))
		for j, h := range m.Headers {
			headers[j] = kafka.Header{Key: h.Key, Value: h.Value}
		}
		km[i] = kafka.Message{Key: m.Key, Value: m.Value, Headers: headers}
	}
	err := k.w.WriteMessages(ctx, km...)
	if err == nil {
		return nil
	}
	// A single-message write that the broker rejected for the record itself
	// will fail the same way forever: let the relay quarantine it at once.
	if len(msgs) == 1 && isPermanent(err) {
		return outbox.Permanent(err)
	}
	return err
}

// permanentCodes are broker verdicts about the record, not the broker.
var permanentCodes = []kafka.Error{
	kafka.InvalidMessage,
	kafka.InvalidMessageSize,
	kafka.MessageSizeTooLarge,
	kafka.RecordListTooLarge,
	kafka.InvalidRecord,
}

func isPermanent(err error) bool {
	var we kafka.WriteErrors
	if errors.As(err, &we) {
		for _, e := range we {
			if e != nil && isPermanent(e) {
				return true
			}
		}
		return false
	}
	for _, code := range permanentCodes {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}

// Probe dials a broker and asks it, over that fresh connection, for the
// topic's partitions; every partition must have a leader, because a produce
// request can only succeed on a partition that has one.
//
// It deliberately does NOT use kafka.Client.Metadata. kafka-go's Transport
// answers metadata requests from its own cache, so after one successful probe
// every later probe "succeeds" without touching the network — a paused broker
// read as healthy, and the relay charged a whole outage to the events'
// retry budgets. A new connection per probe is the cost of a probe that
// actually observes the broker; probes only run after a failed write.
func (k *Writer) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, k.probeTimeout)
	defer cancel()
	var lastErr error
	for _, broker := range k.brokers {
		if lastErr = k.probeBroker(ctx, broker); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (k *Writer) probeBroker(ctx context.Context, broker string) error {
	conn, err := k.dialer.DialContext(ctx, "tcp", broker)
	if err != nil {
		return fmt.Errorf("kafkaout: dial %s: %w", broker, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	partitions, err := conn.ReadPartitions(k.topic)
	if err != nil {
		return fmt.Errorf("kafkaout: topic %s metadata from %s: %w", k.topic, broker, err)
	}
	if len(partitions) == 0 {
		return fmt.Errorf("kafkaout: topic %s has no partitions", k.topic)
	}
	for _, p := range partitions {
		if p.Error != nil {
			return fmt.Errorf("kafkaout: topic %s partition %d: %w", k.topic, p.ID, p.Error)
		}
		if p.Leader.ID < 0 {
			return fmt.Errorf("kafkaout: topic %s partition %d has no leader", k.topic, p.ID)
		}
	}
	return nil
}

// Close flushes and closes the producer.
func (k *Writer) Close() error { return k.w.Close() }
