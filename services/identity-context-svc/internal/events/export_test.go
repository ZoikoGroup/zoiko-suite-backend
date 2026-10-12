package events

import (
	"context"

	"github.com/segmentio/kafka-go"
)

// ApplyForTest exposes apply to the external test package.
func (c *Consumer) ApplyForTest(ctx context.Context, msg kafka.Message) bool {
	return c.apply(ctx, msg)
}
