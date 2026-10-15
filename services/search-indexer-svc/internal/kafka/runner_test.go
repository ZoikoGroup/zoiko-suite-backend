package kafka

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// A group reader created before its topic exists never recovered (found live,
// 30 Sep 2026). Subscribe now creates a reader only for a topic that exists,
// and picks the topic up on a later Subscribe once it does.
func TestSubscribe_WaitsForTheTopicToExist(t *testing.T) {
	r := NewRunner([]string{"localhost:1"}, "g", nil, nil, zap.NewNop())
	exists := false
	r.topicExists = func(context.Context, string) (bool, error) { return exists, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); r.Close() }()

	r.Subscribe(ctx, []string{"not.yet"})
	assert.Empty(t, r.readers, "no reader for a topic that does not exist")

	exists = true
	r.Subscribe(ctx, []string{"not.yet"})
	assert.Contains(t, r.readers, "not.yet", "subscribed once the topic appears")
}
