#!/usr/bin/env bash
# Push the canonical input contract (ZS-ARCH-SVC-001 v2.0 §4) into every service.
#
# services/ has no shared Go module — each service is its own zoiko.io/<svc> with
# no replace directives — so the package is vendored rather than imported. That is
# the pattern the repo already uses for tenant scope (internal/middleware/tenant.go
# is duplicated across 73 services). This script is what keeps the copies identical:
# edit services/_contract/envelope/, then re-run it. Never edit a vendored copy.
#
#   ./rollout.sh            vendor into every service and regenerate contract.go
#   ./rollout.sh <svc>...   only the named services
#
# Wiring the middleware into each router is deliberately NOT done here — main.go
# differs enough between services that a scripted edit would be guesswork. Run
# rollout.sh, then wire each service's router with the two lines printed at the end.

set -euo pipefail

cd "$(dirname "$0")"
SRC="$PWD/envelope"
SERVICES_DIR="$PWD/.."

# Entity-scoped services: their records identify an authoritative legal entity, so
# §4 makes legal_entity_id mandatory for them (INV-02). Derived from which services
# already carry legal_entity_id in internal/domain — this reflects the platform as
# built, not an aspiration.
ENTITY_SCOPED="access-control-svc accounts-payable-svc accounts-receivable-svc \
anomaly-detection-svc authorization-svc bank-reconciliation-svc banking-connector-svc \
benefits-svc board-resolutions-svc carta-svc clause-template-svc commercial-account-svc \
compensation-svc compliance-risk-scoring-svc compliance-status-svc \
connectivity-api-bridge-svc consolidation-svc contract-lifecycle-svc \
corporate-actions-svc corporate-tax-svc counterparty-management-svc decision-support-svc \
delegated-authority-svc document-vault-svc employee-master-svc employment-contracts-svc \
esignature-integration-svc evidence-manifest-svc evidence-requirements-svc \
exception-escalation-svc external-data-feed-svc filing-preparation-svc filing-tracker-svc \
financial-close-svc forecasting-svc general-ledger-svc governance-decision-log-svc \
hris-connector-svc identity-context-svc invoice-approval-svc key-management-svc \
leave-absence-svc migration-integrity-svc mtls-management-svc notification-svc \
obligation-tracking-svc obligations-svc offboarding-severance-svc org-structure-svc \
payroll-run-svc payroll-tax-svc performance-review-svc policy-svc procurement-workflow-svc \
purchase-order-svc purchase-request-svc reconciliation-intelligence-svc \
reporting-orchestration-svc siem-integration-svc spend-controls-svc \
tax-authority-interface-svc tax-determination-svc tenant-entity-registry-svc treasury-svc \
vat-gst-svc vendor-due-diligence-svc withholding-tax-svc workflow-svc workforce-compliance-svc"

# Services whose reads touch personal, bank, tax, payroll or privileged content.
# §4 requires purpose_context "for governed sensitive access", and §15 (INV-15)
# forbids emitting that content into telemetry — capturing WHY it was accessed is
# what makes the access reviewable afterwards.
SENSITIVE="document-vault-svc employee-master-svc compensation-svc benefits-svc \
payroll-run-svc payroll-tax-svc payroll-exceptions-svc offboarding-severance-svc \
leave-absence-svc performance-review-svc employment-contracts-svc carta-svc \
key-management-svc secret-vault-integration-svc mtls-management-svc \
counterparty-management-svc vendor-due-diligence-svc banking-connector-svc \
treasury-svc corporate-tax-svc vat-gst-svc withholding-tax-svc tax-determination-svc \
tax-authority-interface-svc evidence-manifest-svc governance-decision-log-svc \
audit-event-store-svc hris-connector-svc"

# Services that post to, or report from, an accounting book (INV-03).
#
# Their generated policy sets BookID to NotRequired with a pointer to the gap,
# NOT to Required. §4 does make book_id mandatory for accounting actions, but
# REF-06 Accounting Book / Ledger Basis does not exist in services/, so there is
# nothing that can issue or validate a book_id. Requiring one would force every
# caller to invent an identifier no service can check — a posting claiming a basis
# nobody decided, which is precisely what INV-03 exists to prevent. The envelope
# carries book_id today so callers can begin sending it; flipping these to
# Required is a one-line change per service once REF-06 ships.
ACCOUNTING="general-ledger-svc accounts-payable-svc accounts-receivable-svc \
consolidation-svc intercompany-accounting-svc financial-close-svc migration-integrity-svc \
reporting-orchestration-svc metric-registry-svc corporate-tax-svc vat-gst-svc \
withholding-tax-svc tax-determination-svc payroll-tax-svc treasury-svc \
bank-reconciliation-svc reconciliation-intelligence-svc"

in_list() {
	local needle="$1" hay="$2" item
	for item in $hay; do [ "$item" = "$needle" ] && return 0; done
	return 1
}

targets=()
if [ "$#" -gt 0 ]; then
	targets=("$@")
else
	for d in "$SERVICES_DIR"/*/; do
		name="$(basename "$d")"
		[ "$name" = "_contract" ] && continue
		# search-client is a library module with no HTTP server — there is no
		# router to wire and nothing to gate.
		[ "$name" = "search-client" ] && continue
		[ -f "$d/go.mod" ] || continue
		targets+=("$name")
	done
fi

for svc in "${targets[@]}"; do
	dest="$SERVICES_DIR/$svc/internal/envelope"
	if [ ! -f "$SERVICES_DIR/$svc/go.mod" ]; then
		echo "skip $svc (no go.mod)" >&2
		continue
	fi
	mkdir -p "$dest"
	cp "$SRC/envelope.go" "$SRC/policy.go" "$SRC/middleware.go" "$SRC/resolver.go" "$SRC/reporter.go" "$dest/"

	legal_entity="NotRequired"
	in_list "$svc" "$ENTITY_SCOPED" && legal_entity="RequiredOnWrite"

	purpose="NotRequired"
	in_list "$svc" "$SENSITIVE" && purpose="RequiredOnWrite"

	book_note="// This service does not post to an accounting book."
	if in_list "$svc" "$ACCOUNTING"; then
		book_note="// BookID stays NotRequired despite §4 marking book_id mandatory for
	// accounting actions: REF-06 Accounting Book / Ledger Basis does not exist in
	// services/, so no book_id can be issued or validated. See rollout.sh.
	// Flip to Required once REF-06 ships."
	fi

	cat > "$dest/contract.go" <<CONTRACT
// Code generated by services/_contract/rollout.sh. DO NOT EDIT.
//
// Regenerate with: services/_contract/rollout.sh $svc

package envelope

// ServicePolicy is this service's §4 conditional-field policy.
//
// The unconditionally mandatory fields — tenant_id, actor_subject_id,
// request_id, correlation_id, source_channel — and idempotency_key on material
// writes are enforced for every service and are not expressible here.
func ServicePolicy() Policy {
	return Policy{
		ServiceName: "$svc",

		// §4: mandatory for entity-specific records (INV-02).
		LegalEntityID: $legal_entity,

		// §4: required for governed sensitive access.
		PurposeContext: $purpose,

		$book_note
		BookID: NotRequired,
	}
}
CONTRACT

	(cd "$SERVICES_DIR/$svc" && gofmt -w ./internal/envelope/)
	echo "vendored -> services/$svc/internal/envelope/"
done

cat <<'NEXT'

Vendored. Now wire each service's router (cmd/server/main.go), after the
existing chi middleware and before the routes:

    import svcenvelope "zoiko.io/<svc>/internal/envelope"

    r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(),
        func(req *http.Request, e svcenvelope.Envelope, err *svcenvelope.ValidationError) {
            logger.Warn("canonical input contract violated",
                zap.String("operation", e.Operation),
                zap.String("correlation_id", e.CorrelationID),
                zap.Strings("missing", svcenvelope.Fields(err)))
        }))

Enforcement mode comes from ZS_ENVELOPE_ENFORCEMENT (strict | write-strict |
observe), defaulting to write-strict.
NEXT
