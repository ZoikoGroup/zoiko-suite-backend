package events_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/events"
)

// The event-contract half of DoD gate 1.
//
// The OpenAPI contract test walks the router; this one walks the event type
// constants. Both directions matter for the same reason: an event the code
// publishes but the contract omits cannot be consumed by anyone who built from
// the document, and an event the contract promises but the code never emits is
// one a consumer will wait for forever.

type asyncAPIDoc struct {
	Components struct {
		Messages map[string]struct {
			Name string `yaml:"name"`
		} `yaml:"messages"`
	} `yaml:"components"`
	Channels map[string]struct {
		Publish struct {
			Message struct {
				OneOf []struct {
					Ref string `yaml:"$ref"`
				} `yaml:"oneOf"`
			} `yaml:"message"`
		} `yaml:"publish"`
		Subscribe struct {
			Message struct {
				OneOf []struct {
					Ref string `yaml:"$ref"`
				} `yaml:"oneOf"`
			} `yaml:"message"`
		} `yaml:"subscribe"`
	} `yaml:"channels"`
}

func loadAsyncAPI(t *testing.T) asyncAPIDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "asyncapi.yaml"))
	require.NoError(t, err, "asyncapi.yaml must be present — it is the published event contract")

	var doc asyncAPIDoc
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Components.Messages)
	return doc
}

// publishedNames returns the `name:` of every message the document declares.
func publishedNames(t *testing.T) map[string]bool {
	t.Helper()
	doc := loadAsyncAPI(t)
	out := map[string]bool{}
	for _, m := range doc.Components.Messages {
		if m.Name != "" {
			out[m.Name] = true
		}
	}
	return out
}

// producedEventTypes is every event type this service emits.
//
// Listed explicitly rather than reflected out of the package, because the
// point is to fail when somebody adds a publisher method and forgets the
// contract — and a reflective list would grow silently along with the code.
func producedEventTypes() []string {
	return []string{
		events.EventContextResolved,
		events.EventResolutionFailed,
		events.EventSessionRevoked,
		events.EventRiskSignalUnavailable,
		// The status pair, emitted together by PUT /v1/principals/{id}/status
		// since 19 Oct 2026 (internal/lifecycle.SetStatus).
		events.EventPrincipalStatusChanged,
		events.EventSubjectDisabled,
		events.EventSubjectEnabled,
		events.EventAuthenticationSucceeded,
		events.EventAuthenticationFailed,
		events.EventMFAFactorEnrolled,
		events.EventMFAFactorRevoked,
		events.EventMFAFactorReset,
		events.EventStepUpSucceeded,
		events.EventStepUpFailed,
		// The OIDC provider's (internal/oidc), wired 19 Oct 2026.
		events.EventLoginSessionStarted,
		events.EventLoginSessionEnded,
		events.EventRefreshReuseDetected,
		events.EventAuthCodeReplayed,
		events.EventLoginThrottled,
		events.EventTokenExchanged,
		events.EventTenantContextCacheInvalidated,
		events.EventTenantContextInvalidated,
		events.EventSupportContextAttached,
		events.EventSupportContextRevoked,
		events.EventSupportContextReviewed,
		events.EventDispositionExecuted,
		events.EventDispositionBlocked,
	}
}

func TestEveryProducedEventIsPublishedInTheContract(t *testing.T) {
	declared := publishedNames(t)

	var missing []string
	for _, name := range producedEventTypes() {
		if !declared[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	assert.Empty(t, missing,
		"these events are emitted but not in asyncapi.yaml — nobody who built from the document can consume them")
}

// TestEveryPublishedContractEventIsProduced is the other direction: a message
// the document says this service publishes must be one it emits. Without it
// the document went on promising session.invalidated after the code stopped
// sending it (Group 1 re-audit, 19 Oct 2026).
func TestEveryPublishedContractEventIsProduced(t *testing.T) {
	doc := loadAsyncAPI(t)
	produced := map[string]bool{}
	for _, name := range producedEventTypes() {
		produced[name] = true
	}
	var promised []string
	for _, ch := range doc.Channels {
		for _, m := range ch.Publish.Message.OneOf {
			key := filepath.Base(m.Ref)
			name := doc.Components.Messages[key].Name
			require.NotEmpty(t, name, "publish ref %s names no message", m.Ref)
			if !produced[name] {
				promised = append(promised, name)
			}
		}
	}
	sort.Strings(promised)
	assert.Empty(t, promised, "asyncapi.yaml promises these but nothing emits them — a consumer would wait forever")
}

// TestEveryConsumedEventIsInTheContract covers the inbound half.
//
// A consumed event that is undocumented is worse than an undocumented
// published one: the producer is a DIFFERENT team, and the only record that
// this service depends on their event shape is the Go switch statement.
func TestEveryConsumedEventIsInTheContract(t *testing.T) {
	declared := publishedNames(t)

	// The names the consumer's dispatch switch matches. Kept in step with
	// Consumer.Handle by hand, deliberately — see producedEventTypes.
	consumed := []string{
		"authority.revoked",
		"authority.expired",
		"authority.delegated",
		"role.updated",
		"iam.assignment.revoked",
		"iam.assignment.granted",
		"entity.updated",
		"session.risk.changed",
		"legal.hold.issued",
		"legal.hold.released",
	}

	// Aliases the document folds into one message, with the reason each is
	// folded: the consumer's response to both names is identical.
	alias := map[string]string{
		"authority.expired":   "authority.revoked",
		"authority.delegated": "authority.revoked",
	}

	var missing []string
	for _, name := range consumed {
		if declared[name] {
			continue
		}
		if a, ok := alias[name]; ok && declared[a] {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)

	assert.Empty(t, missing, "these events are consumed but not documented as dependencies")
}

// TestRiskSignalAlarmIsNotNamedLikeASignal is a regression guard with a
// specific history.
//
// This service published `session.risk.changed` when its risk cache MISSED,
// and subscribed to `session.risk.changed` as the sole writer of that cache.
// It was answering its own alarm: every cache-miss resolution emitted an event
// that came straight back, burned a dedupe key, and logged a cache write that
// UpsertSignal had silently declined to perform.
//
// The rename is the fix. This test stops it being renamed back.
func TestRiskSignalAlarmIsNotNamedLikeASignal(t *testing.T) {
	assert.NotEqual(t, "session.risk.changed", events.EventRiskSignalUnavailable,
		"the unavailability ALARM must not share a name with the risk SIGNAL this service consumes")

	assert.Equal(t, "identity.risk_signal.unavailable", events.EventRiskSignalUnavailable)
}

// TestPublishedEventsAreNamespacedToThisService guards the class rather than
// the instance.
//
// Every event this service produces should be recognisably ours. The one that
// is not — principal.status.changed — predates the convention and is consumed
// under that name by authorization-svc, so it is emitted alongside the
// namespaced identity.subject.* pair rather than renamed. (session.invalidated
// had no consumer and was retired for identity.session.revoked.) It is named
// here so the exception is deliberate rather than an oversight that grows.
func TestPublishedEventsAreNamespacedToThisService(t *testing.T) {
	grandfathered := map[string]bool{
		events.EventPrincipalStatusChanged: true,
	}

	for _, name := range producedEventTypes() {
		if grandfathered[name] {
			continue
		}
		assert.Contains(t, name, "identity.",
			"new events should be namespaced, so a collision with a consumed name is visible at review")
	}
}

func TestSourceServiceNameMatchesTheSelfGuard(t *testing.T) {
	// The consumer drops events whose source_service equals this constant, and
	// the publisher stamps it. If the two ever diverge the guard silently
	// stops working and the service resumes consuming its own events.
	assert.Equal(t, "identity-context-svc", events.SourceServiceName)
}

// The reader's default topics and the document's subscribed channels must be
// the same set. They were once both wrong in the same way — every consumed
// event filed under this service's own topic — so neither caught the other.
func TestConsumedChannelsMatchTheReaderTopics(t *testing.T) {
	doc := loadAsyncAPI(t)
	var documented []string
	for name, ch := range doc.Channels {
		if len(ch.Subscribe.Message.OneOf) > 0 {
			documented = append(documented, name)
		}
	}
	reader := append([]string(nil), config.DefaultConsumeTopics...)
	sort.Strings(documented)
	sort.Strings(reader)
	assert.Equal(t, documented, reader,
		"asyncapi.yaml subscribe channels and config.DefaultConsumeTopics have drifted apart")
}
