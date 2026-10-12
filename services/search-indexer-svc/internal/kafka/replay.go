package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"

	"zoiko.io/search-indexer-svc/internal/projection"
)

// Replayer reads a topic from its earliest retained message to the end offset
// captured when the replay starts. It satisfies indexer.Replayer, and is how a
// new generation is backfilled "from source" (§8.3).
//
// No consumer group: a replay must not move the live consumer's committed
// offsets, and a group reader would share partitions with it. One partition
// reader at a time, from FirstOffset to the snapshot end — messages published
// during the replay reach the candidate through the live consumer's dual
// write instead.
//
// Bounded by retention. Events older than the topic keeps are not replayed;
// a record whose only event has aged out is missing from the candidate, and
// completeness validation against the ledger refuses the generation READY
// (NP-18) rather than activating an index with a hole in it.
type Replayer struct {
	brokers []string
	client  *kafka.Client
}

func NewReplayer(brokers []string) *Replayer {
	return &Replayer{brokers: brokers, client: &kafka.Client{Addr: kafka.TCP(brokers...), Timeout: 10 * time.Second}}
}

// Replay hands every decodable event on topic to fn, in partition order, and
// returns how many it read. fn returning an error aborts the replay.
func (r *Replayer) Replay(ctx context.Context, topic string, fn func(projection.Event) error) (int, error) {
	md, err := r.client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return 0, fmt.Errorf("metadata %s: %w", topic, err)
	}
	var partitions []int
	for _, t := range md.Topics {
		if t.Name != topic {
			continue
		}
		if t.Error != nil {
			if errors.Is(t.Error, kafka.UnknownTopicOrPartition) {
				return 0, nil // nothing was ever published: nothing to replay
			}
			return 0, fmt.Errorf("metadata %s: %w", topic, t.Error)
		}
		for _, p := range t.Partitions {
			partitions = append(partitions, p.ID)
		}
	}
	if len(partitions) == 0 {
		return 0, nil
	}

	reqs := make([]kafka.OffsetRequest, 0, 2*len(partitions))
	for _, id := range partitions {
		reqs = append(reqs, kafka.FirstOffsetOf(id), kafka.LastOffsetOf(id))
	}
	offsets, err := r.client.ListOffsets(ctx, &kafka.ListOffsetsRequest{Topics: map[string][]kafka.OffsetRequest{topic: reqs}})
	if err != nil {
		return 0, fmt.Errorf("list offsets %s: %w", topic, err)
	}

	read := 0
	for _, po := range offsets.Topics[topic] {
		if po.Error != nil {
			return read, fmt.Errorf("list offsets %s/%d: %w", topic, po.Partition, po.Error)
		}
		if po.LastOffset <= po.FirstOffset {
			continue
		}
		n, err := r.replayPartition(ctx, topic, po.Partition, po.FirstOffset, po.LastOffset, fn)
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

func (r *Replayer) replayPartition(ctx context.Context, topic string, partition int, first, end int64, fn func(projection.Event) error) (int, error) {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: r.brokers, Topic: topic, Partition: partition,
		MinBytes: 1, MaxBytes: 10 << 20, MaxWait: 500 * time.Millisecond,
	})
	defer func() { _ = reader.Close() }()
	if err := reader.SetOffset(first); err != nil {
		return 0, fmt.Errorf("seek %s/%d: %w", topic, partition, err)
	}

	read := 0
	for {
		fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		msg, err := reader.FetchMessage(fetchCtx)
		cancel()
		if err != nil {
			return read, fmt.Errorf("replay %s/%d at %d of %d: %w", topic, partition, first+int64(read), end, err)
		}
		var e projection.Event
		if json.Unmarshal(msg.Value, &e) == nil {
			if e.EventID == "" {
				e.EventID = extractEventID(msg)
			}
			if err := fn(e); err != nil {
				return read, err
			}
		}
		read++
		if msg.Offset >= end-1 {
			return read, nil
		}
	}
}
