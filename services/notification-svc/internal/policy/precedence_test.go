package policy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/policy"
)

type mockSuppressionChecker struct {
	suppressions map[string]string // key = email+stream -> reason
}

func (m *mockSuppressionChecker) IsEmailSuppressed(
	_ context.Context,
	_ string,
	recipientEmail string,
	stream ledger.SenderStream,
	commClass ledger.CommunicationClass,
) (bool, string, error) {
	key := recipientEmail + ":" + string(stream)
	reason, ok := m.suppressions[key]
	if !ok {
		// try ALL stream
		reason, ok = m.suppressions[recipientEmail+":ALL"]
	}
	if !ok {
		return false, "", nil
	}

	// S0 / T0 ignore UNSUBSCRIBE
	if commClass == ledger.ClassS0 || commClass == ledger.ClassT0 {
		if reason == string(ledger.SuppressionReasonUnsubscribe) {
			return false, "", nil
		}
		return true, reason, nil
	}

	return true, reason, nil
}

func TestPrecedence_S0_BypassesUnsubscribe(t *testing.T) {
	checker := &mockSuppressionChecker{
		suppressions: map[string]string{
			"user@example.com:ALL": string(ledger.SuppressionReasonUnsubscribe),
		},
	}
	engine := policy.NewPrecedenceEngine(checker, zap.NewNop())

	intent := &ledger.MessageIntent{
		TenantID:           "tenant-1",
		RecipientEmail:     "user@example.com",
		CommunicationClass: ledger.ClassS0,
	}

	dec, err := engine.Evaluate(context.Background(), intent, ledger.StreamCritical)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "S0 security notices must bypass unsubscribe")
	assert.Equal(t, 2, dec.PrecedenceLevel)
	assert.Equal(t, "SECURITY_PROTECTION_PRECEDENCE", dec.RuleName)
}

func TestPrecedence_T0_BypassesUnsubscribe(t *testing.T) {
	checker := &mockSuppressionChecker{
		suppressions: map[string]string{
			"billing@example.com:TRANSACTIONAL": string(ledger.SuppressionReasonUnsubscribe),
		},
	}
	engine := policy.NewPrecedenceEngine(checker, zap.NewNop())

	intent := &ledger.MessageIntent{
		TenantID:           "tenant-1",
		RecipientEmail:     "billing@example.com",
		CommunicationClass: ledger.ClassT0,
	}

	dec, err := engine.Evaluate(context.Background(), intent, ledger.StreamTransactional)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "T0 transactional notices must bypass unsubscribe")
	assert.Equal(t, 3, dec.PrecedenceLevel)
	assert.Equal(t, "TRANSACTIONAL_NECESSITY_PRECEDENCE", dec.RuleName)
}

func TestPrecedence_S0_BlockedByHardBounce(t *testing.T) {
	checker := &mockSuppressionChecker{
		suppressions: map[string]string{
			"deadbox@example.com:ALL": string(ledger.SuppressionReasonHardBounce),
		},
	}
	engine := policy.NewPrecedenceEngine(checker, zap.NewNop())

	intent := &ledger.MessageIntent{
		TenantID:           "tenant-1",
		RecipientEmail:     "deadbox@example.com",
		CommunicationClass: ledger.ClassS0,
	}

	dec, err := engine.Evaluate(context.Background(), intent, ledger.StreamCritical)
	require.NoError(t, err)
	assert.False(t, dec.Allowed, "hard bounced address must block S0 delivery")
	assert.Equal(t, "SUPPRESSION_ENFORCED", dec.RuleName)
	assert.Contains(t, dec.Reason, "HARD_BOUNCE")
}

func TestPrecedence_M1_BlockedByUnsubscribe(t *testing.T) {
	checker := &mockSuppressionChecker{
		suppressions: map[string]string{
			"prospect@example.com:MARKETING": string(ledger.SuppressionReasonUnsubscribe),
		},
	}
	engine := policy.NewPrecedenceEngine(checker, zap.NewNop())

	intent := &ledger.MessageIntent{
		TenantID:           "tenant-1",
		RecipientEmail:     "prospect@example.com",
		CommunicationClass: ledger.ClassM1,
	}

	dec, err := engine.Evaluate(context.Background(), intent, ledger.StreamMarketing)
	require.NoError(t, err)
	assert.False(t, dec.Allowed, "unsubscribed address must block M1 marketing")
	assert.Equal(t, "SUPPRESSION_ENFORCED", dec.RuleName)
}

func TestPrecedence_M1_PermittedWhenNoSuppression(t *testing.T) {
	checker := &mockSuppressionChecker{
		suppressions: map[string]string{},
	}
	engine := policy.NewPrecedenceEngine(checker, zap.NewNop())

	intent := &ledger.MessageIntent{
		TenantID:           "tenant-1",
		RecipientEmail:     "active@example.com",
		CommunicationClass: ledger.ClassM1,
	}

	dec, err := engine.Evaluate(context.Background(), intent, ledger.StreamMarketing)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "M1 marketing permitted when no suppression")
	assert.Equal(t, 8, dec.PrecedenceLevel)
	assert.Equal(t, "MARKETING_CONSENT_PERMITTED", dec.RuleName)
}

func TestPrecedence_UnknownClass_FailsClosed(t *testing.T) {
	engine := policy.NewPrecedenceEngine(nil, zap.NewNop())

	intent := &ledger.MessageIntent{
		TenantID:           "tenant-1",
		RecipientEmail:     "test@example.com",
		CommunicationClass: ledger.CommunicationClass("UNKNOWN_X"),
	}

	dec, err := engine.Evaluate(context.Background(), intent, ledger.StreamTransactional)
	require.NoError(t, err)
	assert.False(t, dec.Allowed, "unknown class must fail closed")
	assert.Equal(t, "UNKNOWN_COMMUNICATION_CLASS", dec.RuleName)
}
