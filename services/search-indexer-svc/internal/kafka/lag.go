package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"

	"zoiko.io/search-indexer-svc/internal/indexer"
)

// LagProbe measures a topic's consumer lag for this service's consumer group,
// at the broker. It satisfies indexer.LagProbe.
//
// Measured, not inferred. The checkpoint sweep used to stamp time.Now() as the
// watermark and compute lag against time.Now(), so lag was always zero and a
// consumer that had stopped hours ago reported CURRENT. §5.3 asks for the
// "highest committed source position represented in the index" and the
// "current source-to-index lag", and both are facts the broker holds: the
// group's committed offset per partition, the log-end offset per partition,
// and the timestamp of the oldest message between them.
type LagProbe struct {
	client  *kafka.Client
	groupID string
}

func NewLagProbe(brokers []string, groupID string) *LagProbe {
	return &LagProbe{
		client:  &kafka.Client{Addr: kafka.TCP(brokers...), Timeout: 5 * time.Second},
		groupID: groupID,
	}
}

// TopicLag reads the group's position on topic from the broker.
//
// An error means the position could not be established, and the caller must
// record freshness UNKNOWN rather than any earlier value — §2.2, "UNKNOWN is
// never represented as CURRENT".
func (p *LagProbe) TopicLag(ctx context.Context, topic string) (indexer.SourceLag, error) {
	md, err := p.client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return indexer.SourceLag{}, fmt.Errorf("metadata %s: %w", topic, err)
	}
	var partitions []int
	for _, t := range md.Topics {
		if t.Name != topic {
			continue
		}
		if t.Error != nil {
			if errors.Is(t.Error, kafka.UnknownTopicOrPartition) {
				// Nothing has ever been published to it. There is nothing to be
				// behind on, which is a known state — not an unknown one.
				return indexer.SourceLag{}, nil
			}
			return indexer.SourceLag{}, fmt.Errorf("metadata %s: %w", topic, t.Error)
		}
		for _, part := range t.Partitions {
			partitions = append(partitions, part.ID)
		}
	}
	if len(partitions) == 0 {
		return indexer.SourceLag{}, nil
	}

	offsetReqs := make([]kafka.OffsetRequest, 0, 2*len(partitions))
	for _, id := range partitions {
		offsetReqs = append(offsetReqs, kafka.FirstOffsetOf(id), kafka.LastOffsetOf(id))
	}
	ends, err := p.client.ListOffsets(ctx, &kafka.ListOffsetsRequest{
		Topics: map[string][]kafka.OffsetRequest{topic: offsetReqs},
	})
	if err != nil {
		return indexer.SourceLag{}, fmt.Errorf("list offsets %s: %w", topic, err)
	}
	committed, err := p.client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{
		GroupID: p.groupID,
		Topics:  map[string][]int{topic: partitions},
	})
	if err != nil {
		return indexer.SourceLag{}, fmt.Errorf("offset fetch %s: %w", topic, err)
	}
	if committed.Error != nil {
		return indexer.SourceLag{}, fmt.Errorf("offset fetch %s: %w", topic, committed.Error)
	}

	committedBy := map[int]int64{}
	for _, c := range committed.Topics[topic] {
		if c.Error != nil {
			return indexer.SourceLag{}, fmt.Errorf("offset fetch %s/%d: %w", topic, c.Partition, c.Error)
		}
		committedBy[c.Partition] = c.CommittedOffset
	}

	out := indexer.SourceLag{Partitions: len(partitions)}
	for _, po := range ends.Topics[topic] {
		if po.Error != nil {
			return indexer.SourceLag{}, fmt.Errorf("list offsets %s/%d: %w", topic, po.Partition, po.Error)
		}
		position, ok := committedBy[po.Partition]
		// No commit yet means the group starts at the earliest retained
		// message (the reader's StartOffset is FirstOffset); a commit older
		// than retention likewise resumes at the earliest retained one.
		if !ok || position < po.FirstOffset {
			position = po.FirstOffset
		}
		if position > 0 {
			out.Watermark += position
		}
		pending := po.LastOffset - position
		if pending <= 0 {
			continue
		}
		out.Backlog += pending

		at, err := p.messageTime(ctx, topic, po.Partition, position)
		if err != nil {
			return indexer.SourceLag{}, err
		}
		if out.OldestPending.IsZero() || at.Before(out.OldestPending) {
			out.OldestPending = at
		}
	}
	return out, nil
}

// messageTime reads the broker timestamp of the message at offset.
//
// A fetch returns a whole record batch, which can begin before the requested
// offset, so records are skipped until the one asked for.
func (p *LagProbe) messageTime(ctx context.Context, topic string, partition int, offset int64) (time.Time, error) {
	resp, err := p.client.Fetch(ctx, &kafka.FetchRequest{
		Topic: topic, Partition: partition, Offset: offset,
		MinBytes: 1, MaxBytes: 1 << 20, MaxWait: 500 * time.Millisecond,
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("fetch %s/%d@%d: %w", topic, partition, offset, err)
	}
	if resp.Error != nil {
		return time.Time{}, fmt.Errorf("fetch %s/%d@%d: %w", topic, partition, offset, resp.Error)
	}
	if resp.Records == nil {
		return time.Time{}, fmt.Errorf("fetch %s/%d@%d: no records", topic, partition, offset)
	}
	for {
		rec, err := resp.Records.ReadRecord()
		if err != nil {
			return time.Time{}, fmt.Errorf("fetch %s/%d@%d: %w", topic, partition, offset, err)
		}
		if rec.Key != nil {
			_ = rec.Key.Close()
		}
		if rec.Value != nil {
			_ = rec.Value.Close()
		}
		if rec.Offset >= offset {
			return rec.Time, nil
		}
	}
}
