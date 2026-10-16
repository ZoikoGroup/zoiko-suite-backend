// Package busops reads the operational signals of the event bus that
// ZS-SVC-M-001 INT-02 names: consumer lag and topic/partition health
// (GetConsumerLag, GetTopicHealth, ListPartitions/Offsets).
//
// It only reads. It never commits offsets, creates topics or moves a consumer:
// replay is done from the outbox under an approved repair (outbox.Relay.Replay),
// so every redelivery keeps its event id and is absorbed by consumer inboxes.
package busops

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/segmentio/kafka-go"
)

// PartitionOffsets are the raw offsets of one partition.
type PartitionOffsets struct {
	Partition int
	First     int64 // earliest retained offset
	Latest    int64 // next offset to be written
	Committed int64 // the group's committed offset, or -1 when it has none
}

// PartitionLag is the lag of one partition.
type PartitionLag struct {
	Partition int
	Committed int64
	Latest    int64
	Lag       int64
}

// Lag is a consumer group's lag on a topic.
type Lag struct {
	Topic      string
	Group      string
	Partitions []PartitionLag
	Total      int64
}

// ComputeLag derives lag from offsets. A group with no commit has consumed
// nothing, so its lag is everything still retained. A committed offset that
// has fallen behind retention (earlier than First) is measured from First:
// those messages are gone, they are not lag. Lag is never negative.
func ComputeLag(topic, group string, parts []PartitionOffsets) Lag {
	l := Lag{Topic: topic, Group: group}
	for _, p := range parts {
		from := p.Committed
		if from < p.First {
			from = p.First
		}
		lag := p.Latest - from
		if lag < 0 {
			lag = 0
		}
		l.Partitions = append(l.Partitions, PartitionLag{Partition: p.Partition, Committed: p.Committed, Latest: p.Latest, Lag: lag})
		l.Total += lag
	}
	sort.Slice(l.Partitions, func(i, j int) bool { return l.Partitions[i].Partition < l.Partitions[j].Partition })
	return l
}

// ConsumerLag reads a group's lag on a topic from the broker.
func ConsumerLag(ctx context.Context, c *kafka.Client, topic, group string) (Lag, error) {
	meta, err := c.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return Lag{}, fmt.Errorf("busops: metadata: %w", err)
	}
	ids, err := partitionIDs(meta, topic)
	if err != nil {
		return Lag{}, err
	}
	first := map[string][]kafka.OffsetRequest{topic: nil}
	latest := map[string][]kafka.OffsetRequest{topic: nil}
	for _, id := range ids {
		first[topic] = append(first[topic], kafka.FirstOffsetOf(id))
		latest[topic] = append(latest[topic], kafka.LastOffsetOf(id))
	}
	fo, err := c.ListOffsets(ctx, &kafka.ListOffsetsRequest{Topics: first})
	if err != nil {
		return Lag{}, fmt.Errorf("busops: list first offsets: %w", err)
	}
	lo, err := c.ListOffsets(ctx, &kafka.ListOffsetsRequest{Topics: latest})
	if err != nil {
		return Lag{}, fmt.Errorf("busops: list latest offsets: %w", err)
	}
	of, err := c.OffsetFetch(ctx, &kafka.OffsetFetchRequest{GroupID: group, Topics: map[string][]int{topic: ids}})
	if err != nil {
		return Lag{}, fmt.Errorf("busops: offset fetch: %w", err)
	}

	byPart := map[int]*PartitionOffsets{}
	for _, id := range ids {
		byPart[id] = &PartitionOffsets{Partition: id, Committed: -1}
	}
	for _, p := range fo.Topics[topic] {
		if p.Error != nil {
			return Lag{}, fmt.Errorf("busops: partition %d first offset: %w", p.Partition, p.Error)
		}
		byPart[p.Partition].First = p.FirstOffset
	}
	for _, p := range lo.Topics[topic] {
		if p.Error != nil {
			return Lag{}, fmt.Errorf("busops: partition %d latest offset: %w", p.Partition, p.Error)
		}
		byPart[p.Partition].Latest = p.LastOffset
	}
	for _, p := range of.Topics[topic] {
		if p.Error != nil {
			return Lag{}, fmt.Errorf("busops: partition %d committed offset: %w", p.Partition, p.Error)
		}
		byPart[p.Partition].Committed = p.CommittedOffset
	}
	parts := make([]PartitionOffsets, 0, len(byPart))
	for _, p := range byPart {
		parts = append(parts, *p)
	}
	return ComputeLag(topic, group, parts), nil
}

// TopicHealth is the availability of a topic's partitions.
type TopicHealth struct {
	Topic           string
	Partitions      int
	Unavailable     []int // no leader or reporting an error
	UnderReplicated []int // fewer in-sync replicas than replicas
}

// Healthy reports whether every partition is available and fully replicated.
func (h TopicHealth) Healthy() bool {
	return h.Partitions > 0 && len(h.Unavailable) == 0 && len(h.UnderReplicated) == 0
}

// Health reads a topic's partition health.
func Health(ctx context.Context, c *kafka.Client, topic string) (TopicHealth, error) {
	meta, err := c.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return TopicHealth{}, fmt.Errorf("busops: metadata: %w", err)
	}
	for _, t := range meta.Topics {
		if t.Name != topic {
			continue
		}
		if t.Error != nil {
			return TopicHealth{}, fmt.Errorf("busops: topic %s: %w", topic, t.Error)
		}
		h := TopicHealth{Topic: topic, Partitions: len(t.Partitions)}
		for _, p := range t.Partitions {
			if p.Error != nil || p.Leader.ID < 0 {
				h.Unavailable = append(h.Unavailable, p.ID)
				continue
			}
			if len(p.Isr) < len(p.Replicas) {
				h.UnderReplicated = append(h.UnderReplicated, p.ID)
			}
		}
		sort.Ints(h.Unavailable)
		sort.Ints(h.UnderReplicated)
		return h, nil
	}
	return TopicHealth{}, fmt.Errorf("busops: topic %s not found", topic)
}

func partitionIDs(meta *kafka.MetadataResponse, topic string) ([]int, error) {
	for _, t := range meta.Topics {
		if t.Name != topic {
			continue
		}
		if t.Error != nil {
			return nil, fmt.Errorf("busops: topic %s: %w", topic, t.Error)
		}
		if len(t.Partitions) == 0 {
			return nil, errors.New("busops: topic has no partitions")
		}
		ids := make([]int, 0, len(t.Partitions))
		for _, p := range t.Partitions {
			ids = append(ids, p.ID)
		}
		sort.Ints(ids)
		return ids, nil
	}
	return nil, fmt.Errorf("busops: topic %s not found", topic)
}

// Alert is a canonical integration event the service should emit.
type Alert struct {
	Event  string // ZS-SVC-M-001 §10 event name
	Detail string
}

// Evaluate turns lag and health into the §10 alerts: consumer_lag_exceeded when
// total lag passes maxLag (0 = unchecked), partition_unavailable for any
// partition without a leader.
func Evaluate(l Lag, h TopicHealth, maxLag int64) []Alert {
	var out []Alert
	if maxLag > 0 && l.Total > maxLag {
		out = append(out, Alert{"consumer_lag_exceeded",
			fmt.Sprintf("group %s is %d messages behind on %s", l.Group, l.Total, l.Topic)})
	}
	if len(h.Unavailable) > 0 {
		out = append(out, Alert{"partition_unavailable",
			fmt.Sprintf("%s partitions without a leader: %v", h.Topic, h.Unavailable)})
	}
	return out
}
