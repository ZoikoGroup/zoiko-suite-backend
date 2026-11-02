package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/decision-support-svc/internal/domain"
)

func TestStateMachine(t *testing.T) {
	from := domain.StatusReviewRequired
	for _, to := range []domain.Status{domain.StatusAccepted, domain.StatusRejected, domain.StatusExpired, domain.StatusSuperseded} {
		assert.True(t, domain.CanTransition(from, to), "%s -> %s", from, to)
		assert.True(t, to.IsTerminal(), "%s is terminal", to)
		assert.False(t, domain.CanTransition(to, domain.StatusReviewRequired))
		assert.False(t, domain.CanTransition(to, domain.StatusAccepted))
	}
	assert.False(t, domain.CanTransition(from, from))
	assert.False(t, from.IsTerminal())
}

func TestParseStatus(t *testing.T) {
	status, ok := domain.ParseStatus("accepted")
	assert.True(t, ok)
	assert.Equal(t, domain.StatusAccepted, status)
	_, ok = domain.ParseStatus("APPROVED")
	assert.False(t, ok)
}

func TestOversightClass(t *testing.T) {
	class, ok := domain.ParseOversightClass("")
	require.True(t, ok)
	assert.Equal(t, domain.OversightQualifiedReviewer, class)

	assert.False(t, domain.OversightUserReview.RequiresIndependentReviewer())
	assert.True(t, domain.OversightQualifiedReviewer.RequiresIndependentReviewer())
	assert.True(t, domain.OversightDualControlReview.RequiresIndependentReviewer())
	assert.True(t, domain.OversightDualControlReview.RequiresApprovalReference())
	assert.False(t, domain.OversightQualifiedReviewer.RequiresApprovalReference())

	class, ok = domain.ParseOversightClass("o4")
	assert.True(t, ok)
	assert.Equal(t, domain.OversightAIProhibited, class)
	_, ok = domain.ParseOversightClass("O5")
	assert.False(t, ok)
}

func TestETagRoundTrip(t *testing.T) {
	assert.Equal(t, `"3"`, domain.ETagForVersion(3))
	for _, value := range []string{`"3"`, `W/"3"`, `3`, ` "3" `} {
		version, ok := domain.ParseETag(value)
		assert.True(t, ok, value)
		assert.Equal(t, 3, version, value)
	}
	for _, value := range []string{"", `"abc"`, `"0"`, `"-1"`, "*"} {
		_, ok := domain.ParseETag(value)
		assert.False(t, ok, value)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	rec := domain.Recommendation{RecommendationID: uuid.NewString(), CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)}
	cursor, ok := domain.DecodeCursor(domain.EncodeCursor(rec))
	require.True(t, ok)
	assert.Equal(t, rec.RecommendationID, cursor.RecommendationID)
	assert.True(t, rec.CreatedAt.Equal(cursor.CreatedAt))

	for _, bad := range []string{"!!!", "bm8tc2VwYXJhdG9y", "MjAyNi0wMS0wMnxub3QtYS11dWlk"} {
		_, ok := domain.DecodeCursor(bad)
		assert.False(t, ok, bad)
	}
}

func TestTransitionErrorUnwrapsToSentinel(t *testing.T) {
	err := &domain.TransitionError{From: domain.StatusAccepted, To: domain.StatusRejected}
	assert.ErrorIs(t, err, domain.ErrIllegalTransition)
	assert.Contains(t, err.Error(), "ACCEPTED")
}
