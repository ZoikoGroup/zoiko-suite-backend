// Package policy implements the canonical 8-level preference precedence engine
// and pre-render policy resolver for ZoikoSuite communications per ZS-COMMS-EMAIL-001 §4, §5.
package policy

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/ledger"
)

// SuppressionChecker evaluates whether an email recipient is suppressed for a given stream and class.
type SuppressionChecker interface {
	IsEmailSuppressed(
		ctx context.Context,
		tenantID string,
		recipientEmail string,
		stream ledger.SenderStream,
		commClass ledger.CommunicationClass,
	) (bool, string, error)
}

// PrecedenceEngine implements the 8-level precedence evaluation.
type PrecedenceEngine struct {
	suppressions SuppressionChecker
	log          *zap.Logger
}

// NewPrecedenceEngine constructs the communications policy precedence engine.
func NewPrecedenceEngine(suppressions SuppressionChecker, log *zap.Logger) *PrecedenceEngine {
	if log == nil {
		log = zap.NewNop()
	}
	return &PrecedenceEngine{
		suppressions: suppressions,
		log:          log,
	}
}

// Evaluate applies the canonical 8-level precedence order:
//  1. Legal or regulatory obligation
//  2. Security protection (S0)
//  3. Requested service and transaction necessity (T0)
//  4. Assigned-role workflow obligation
//  5. Tenant policy
//  6. User operational preference (A1)
//  7. Lifecycle preference (L1)
//  8. Marketing consent and suppression (M1)
//
// Invariants per ZS-COMMS-EMAIL-001 §4:
//   - A lower level cannot disable a higher one.
//   - Unsubscribe cannot disable S0 (Security) or T0 (Transactional) communications.
//   - Physical delivery barriers (HARD_BOUNCE, COMPLAINT) halt delivery for any class.
func (e *PrecedenceEngine) Evaluate(
	ctx context.Context,
	intent *ledger.MessageIntent,
	stream ledger.SenderStream,
) (ledger.PolicyDecision, error) {
	if intent == nil {
		return ledger.PolicyDecision{Allowed: false, Reason: "nil intent"}, fmt.Errorf("intent cannot be nil")
	}

	// Step 1: Check deliverability suppression list
	if e.suppressions != nil {
		suppressed, reason, err := e.suppressions.IsEmailSuppressed(
			ctx,
			intent.TenantID,
			intent.RecipientEmail,
			stream,
			intent.CommunicationClass,
		)
		if err != nil {
			return ledger.PolicyDecision{Allowed: false, Reason: "suppression lookup failure"}, fmt.Errorf("suppression check: %w", err)
		}

		if suppressed {
			e.log.Info("communications policy: delivery suppressed",
				zap.String("tenant_id", intent.TenantID),
				zap.String("recipient_email", intent.RecipientEmail),
				zap.String("class", string(intent.CommunicationClass)),
				zap.String("reason", reason),
			)
			return ledger.PolicyDecision{
				Allowed:         false,
				PrecedenceLevel: precedenceLevelForClass(intent.CommunicationClass),
				RuleName:        "SUPPRESSION_ENFORCED",
				Reason:          fmt.Sprintf("address suppressed due to %s", reason),
			}, nil
		}
	}

	// Step 2: Evaluate 8-level precedence rules by communication class
	switch intent.CommunicationClass {
	case ledger.ClassS0:
		// Level 2: Security protection — cannot be disabled by user preferences or tenant settings
		return ledger.PolicyDecision{
			Allowed:         true,
			PrecedenceLevel: 2,
			RuleName:        "SECURITY_PROTECTION_PRECEDENCE",
			Reason:          "S0 critical security notices bypass preference opt-outs",
		}, nil

	case ledger.ClassT0:
		// Level 3: Service & transaction necessity — cannot be disabled while transaction is requested
		return ledger.PolicyDecision{
			Allowed:         true,
			PrecedenceLevel: 3,
			RuleName:        "TRANSACTIONAL_NECESSITY_PRECEDENCE",
			Reason:          "T0 core transactional messages bypass general opt-outs",
		}, nil

	case ledger.ClassA1:
		// Level 6: User operational preference
		return ledger.PolicyDecision{
			Allowed:         true,
			PrecedenceLevel: 6,
			RuleName:        "OPERATIONAL_PREFERENCE_PERMITTED",
			Reason:          "A1 operational notice permitted by default preference",
		}, nil

	case ledger.ClassL1:
		// Level 7: Lifecycle / educational preference
		return ledger.PolicyDecision{
			Allowed:         true,
			PrecedenceLevel: 7,
			RuleName:        "LIFECYCLE_PREFERENCE_PERMITTED",
			Reason:          "L1 lifecycle notice permitted by default preference",
		}, nil

	case ledger.ClassM1:
		// Level 8: Commercial / marketing consent
		return ledger.PolicyDecision{
			Allowed:         true,
			PrecedenceLevel: 8,
			RuleName:        "MARKETING_CONSENT_PERMITTED",
			Reason:          "M1 marketing communication permitted (no active suppression)",
		}, nil

	default:
		// Fail closed on unknown class per §5
		return ledger.PolicyDecision{
			Allowed:         false,
			PrecedenceLevel: 0,
			RuleName:        "UNKNOWN_COMMUNICATION_CLASS",
			Reason:          fmt.Sprintf("unknown communication class: %q", intent.CommunicationClass),
		}, nil
	}
}

func precedenceLevelForClass(class ledger.CommunicationClass) int {
	switch class {
	case ledger.ClassS0:
		return 2
	case ledger.ClassT0:
		return 3
	case ledger.ClassA1:
		return 6
	case ledger.ClassL1:
		return 7
	case ledger.ClassM1:
		return 8
	default:
		return 0
	}
}
