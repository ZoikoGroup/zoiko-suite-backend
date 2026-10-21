// Package certification is the INT-domain production-certification ledger (ZS-SVC-M-001 section 11 controls and
// section 12 negative-path scenarios). It is not a service. It holds, as data, which test proves each control and
// scenario, and a checker that refuses a ledger that cites a test that does not exist or does not pass.
package certification

// Prefixes shorten the evidence references. A reference is "<prefix>:<path inside that directory>::<TestName>".
var Prefixes = map[string]string{
	"EV": "eventing",
	"GA": "gateway-auth-svc/internal",
	"NS": "notification-svc/internal",
	"CB": "connectivity-api-bridge-svc/internal",
	"DI": "data-ingestion-svc/internal",
	"PI": "payment-initiation-adapter-svc/internal",
	"PS": "payment-status-svc/internal",
	"TX": "tax-authority-interface-svc/internal",
	"IC": "identity-context-svc/internal",
	"AC": "access-control-svc/internal",
	"SR": "schema-registry-svc/internal",
	"DV": "document-vault-svc/internal",
	// CBC is connectivity-api-bridge-svc's command package, which sits beside internal/ rather than inside it.
	"CBC": "connectivity-api-bridge-svc",
}

// Status of one ledger entry.
const (
	Covered = "COVERED"
	// Partial: a test proves part of it; Note says what is missing.
	Partial = "PARTIAL"
	// Deviation: the design deliberately differs from the specification text, recorded for a named owner to accept.
	Deviation = "DEVIATION"
	// OutOfScope: the specification names it but the product deliberately does not offer it; closed only by an acceptance.
	OutOfScope = "OUT_OF_SCOPE"
)

// Entry is one control or scenario.
type Entry struct {
	ID       string
	Title    string
	Status   string
	Evidence []string
	Note     string
}

// Controls are INT-CTRL-001..036.
var Controls = []Entry{
	{"INT-CTRL-001", "Tenant/context resolved before integration processing", Covered, []string{
		"GA:handler/tenantctx_test.go::TestVerify_ActiveTenantResolvesServerContext",
		"GA:handler/handler_test.go::TestVerify_ResolvedTenantMismatch_Returns403",
		"EV:integration/integration_test.go::TestUnresolvedTenantFailsClosed"}, ""},
	{"INT-CTRL-002", "Downstream protected command authorization re-evaluated", Covered, []string{
		"CB:handler/governance_test.go::TestEveryCommandChecksItsOwnAuthorizationAction",
		"DI:store/import_http_test.go::TestImportAPI_AuthorizationAndIdentity",
		"NS:store/webhook_out_http_test.go::TestWebhookAPI_AuthorizationAndIdentity"}, ""},
	{"INT-CTRL-003", "OpenAPI/API version/runtime conformance", Partial, []string{
		"GA:admission/admission_test.go::TestPolicyVersionAdmission_INT_NP_003",
		"GA:problem/problem_test.go::TestCodesAndTitlesMatchCanonicalTaxonomy",
		"IC:context/contract_test.go::TestEveryRouteIsInThePublishedContract",
		"IC:context/contract_test.go::TestEveryContractPathIsServed"},
		"Router-versus-OpenAPI conformance exists for identity-context-svc only; several integration services have no openapi.yaml."},
	{"INT-CTRL-004", "Gateway request size/rate/abuse control", Covered, []string{
		"GA:admission/admission_test.go::TestLimiterConcurrentRequestsCannotBypassQuota_INT_NP_005",
		"GA:handler/admission_test.go::TestOversizedAndCompressedBodiesRefusedBeforeAuthentication_INT_NP_004",
		"GA:admission/admission_test.go::TestRecorderAbuseDetectionFiresOncePerMinute"}, ""},
	{"INT-CTRL-005", "Event schema/producer authorization", Partial, []string{
		"SR:handler/handler_test.go::TestRegisterVersion_BreakingChange_Returns409WithViolations",
		"SR:handler/handler_test.go::TestRegisterVersion_AuthorizationDenied_Returns403",
		"EV:outbox/controls_test.go::TestGateQuarantinesIncompatiblePayloadBeforePublish_INT_NP_006",
		"CBC:cmd/server/schemagate_test.go::TestTheConnectivityRelayIsGatedByTheRegisteredContract",
		"EV:integration/gateenv_test.go::TestGateFromEnv"},
		"The gate is installed (SCHEMA_REGISTRY_URL, off by default) in the connectivity, payment-status, payment-initiation and tax-authority relays, all of which use the shared relay. Notification, data-ingestion, identity-context and access-control run their own outbox with no quarantine state; they must move onto the shared relay (a migration per service) before they can be gated without blocking or dropping events."},
	{"INT-CTRL-006", "Partition/order policy defined", Covered, []string{
		"EV:envelope/envelope_test.go::TestPartitionKey_StablePerAggregateAndOpaque",
		"EV:outbox/controls_test.go::TestVerifyPartitioning",
		"AC:events/assignment_key_test.go::TestAssignmentEventsShareTheAssignmentKey"}, ""},
	{"INT-CTRL-007", "Transactional outbox atomicity", Covered, []string{
		"EV:outbox/outbox_test.go::TestEnqueue_RolledBackTransactionLeavesNoEvent",
		"CB:store/pg_governance_test.go::TestPg_FailedEffectRollsBackTheConnectorAndItsEvents",
		"CB:store/pg_governance_test.go::TestPg_FullLifecycleWritesEvidenceAndEventsAtomically"}, ""},
	{"INT-CTRL-008", "Consumer inbox/deduplication", Covered, []string{
		"EV:inbox/inbox_test.go::TestProcessOnceThenDuplicate_INT_NP_007",
		"EV:inbox/inbox_test.go::TestConcurrentDuplicatesProduceOneEffect",
		"EV:inbox/inbox_test.go::TestConflictingRedeliveryRejected_INT_IDEM_005",
		"EV:outbox/load_test.go::TestLoad_ConcurrentRedeliveryProducesOneEffectPerEvent"}, ""},
	{"INT-CTRL-009", "Controlled event replay", Covered, []string{
		"EV:outbox/controls_test.go::TestReplayNeedsApprovalAndBoundedBatch",
		"EV:inbox/inbox_test.go::TestRequeueNeedsApprovalAndIsLogged",
		"NS:store/webhook_out_test.go::TestWebhook_ReplayNeedsApprovalAndKeepsIdentity"}, ""},
	{"INT-CTRL-010", "Webhook signature/replay protection", Covered, []string{
		"EV:signature/signature_test.go::TestInvalidSignatureIsRejected",
		"EV:signature/signature_test.go::TestReplayIsRejected",
		"NS:store/webhook_out_test.go::TestWebhook_NeverSentUnsigned"}, ""},
	{"INT-CTRL-011", "Webhook data minimization/egress", Covered, []string{
		"NS:store/webhook_out_test.go::TestWebhook_BodyOutsideScopeIsBlockedBeforeSend",
		"NS:whout/rules_test.go::TestMinimizeCopiesOnlyAllowedFields",
		"EV:egress/egress_test.go::TestDialContextRefusesPrivateAndRebindingTargets"}, ""},
	{"INT-CTRL-012", "Bounded webhook retry/dead-letter", Covered, []string{
		"NS:store/webhook_out_test.go::TestWebhook_BoundedRetryThenDeadLetter",
		"NS:store/webhook_out_test.go::TestWebhook_CircuitBreakerAndAutoDisable",
		"NS:store/webhook_out_test.go::TestWebhook_PerDestinationBulkhead"}, ""},
	{"INT-CTRL-013", "Connector secret externalization", Covered, []string{
		"CB:connector/service_test.go::TestCreateRefusesAnythingThatCouldBeASecret_INT_NP_019",
		"CB:vault/vault_test.go::TestVerifyRejectsNonIDsWithoutCallingTheVault",
		"CB:store/pg_governance_test.go::TestMigration_GrandfathersLegacyRowsAndStripsStoredSecrets"}, ""},
	{"INT-CTRL-014", "Connector scope/purpose approval", Covered, []string{
		"CB:connector/service_test.go::TestIngestEnforcesApprovedScopes_INT_NP_021",
		"CB:connector/service_test.go::TestApprovalNeedsADifferentPerson_SoD",
		"CB:domain/lifecycle_test.go::TestFingerprintCoversEveryProtectedField"}, ""},
	{"INT-CTRL-015", "Connector residency/egress allowlist", Covered, []string{
		"CB:connector/service_test.go::TestSubmitRefusals",
		"CB:connector/service_test.go::TestApprovalRechecksTheWorldAsItIsNow_INT_NP_018",
		"EV:egress/egress_test.go::TestCheckDestinationResolves_INT_NP_018"}, ""},
	{"INT-CTRL-016", "Sync checkpoint integrity", Covered, []string{
		"CB:connector/service_test.go::TestCheckpointAdvanceRules",
		"CB:connector/service_test.go::TestCheckpointResetNeedsASecondPerson",
		"CB:store/pg_governance_test.go::TestPg_ConcurrentCheckpointAdvancesExactlyOneWins"}, ""},
	{"INT-CTRL-017", "Import malware/integrity scan", Covered, []string{
		"EV:scan/scan_test.go::TestBuiltinFlagsEICARAndHidingPlaces",
		"DI:store/import_test.go::TestImport_MalwareIsQuarantinedBeforeAnythingIsParsed",
		"DI:store/import_test.go::TestImport_NoScannerMeansNoImport"}, ""},
	{"INT-CTRL-018", "Import schema/mapping validation", Covered, []string{
		"DI:imports/profile_test.go::TestCSVSchemaMismatchIsAFileLevelError",
		"DI:store/import_test.go::TestImport_SchemaMismatchStopsBeforePreview",
		"DI:store/import_http_test.go::TestImportAPI_ValidationErrorsNameColumnsNeverValues"}, ""},
	{"INT-CTRL-019", "Import preview approval fingerprint", Covered, []string{
		"DI:store/import_test.go::TestImport_ExecutionRefusesAnythingThatDiffersFromTheApprovedPreview",
		"DI:store/import_test.go::TestImport_NonMaterialMayBeApprovedBySomeoneElseOrNot",
		"DI:store/import_test.go::TestImport_TheDatabaseRefusesDirectWrites"}, ""},
	{"INT-CTRL-020", "Import control totals/reconciliation", Covered, []string{
		"DI:store/import_test.go::TestImport_ControlTotals",
		"DI:store/import_test.go::TestImport_MaterialHappyPathToReconciled",
		"DI:store/import_test.go::TestImport_PartialFailureIsExplicitAndNeverReconciled"}, ""},
	{"INT-CTRL-021", "No direct authoritative DB import writes", Covered, []string{
		"DI:imports/architecture_test.go::TestTheImportEngineHasNoDatabaseAccessOfItsOwn",
		"DI:store/import_test.go::TestImport_TheDatabaseRefusesDirectWrites",
		"DI:store/import_test.go::TestImport_ARetriedDeliveryNeverCreatesASecondDomainRecord",
		"DI:store/import_test.go::TestImport_ExecutionIsBoundedAndDeliversEachItemOnce"},
		"The engine cannot import a database driver (build-time guard) and delivers only over HTTP. The database role's grants are a deployment control, not asserted here."},
	{"INT-CTRL-022", "Durable provider attempt before side effect", Covered, []string{
		"PI:store/dispatch_store_test.go::TestDispatch_BeginSendIsACompareAndSet",
		"PI:providers/engine_test.go::TestStaleInFlightIsSweptToUnknownNeverResent",
		"TX:store/authority_store_test.go::TestAuthority_BeginSendIsACompareAndSet"}, ""},
	{"INT-CTRL-023", "Callback authenticity/dedup/order", Covered, []string{
		"PS:callbacks/callbacks_test.go::TestBadSignatureWritesNothingAndLeaksNothing",
		"PS:store/callback_store_test.go::TestCallback_ConcurrentCopiesOfOneEventApplyExactlyOnce",
		"TX:store/callback_store_test.go::TestCallback_NeverMovesAFilingBackwardsOrChangesAFinalOne"}, ""},
	{"INT-CTRL-024", "Pending/Unknown external outcome", Covered, []string{
		"EV:integration/integration_test.go::TestPendingUnknownIsNotReportedAsFailure",
		"PI:providers/engine_test.go::TestTimeoutThenRetryAsksBeforeSending",
		"TX:authority/engine_test.go::TestTimeoutThenRetryAsksTheAuthorityAndNeverFilesTwice"}, ""},
	{"INT-CTRL-025", "Provider/profile version pinning", Covered, []string{
		"PI:providers/engine_test.go::TestPayloadIsPinnedAcrossRetries",
		"PI:store/dispatch_store_test.go::TestDispatch_PrepareBindsTheProviderAndTheExactAmount",
		"TX:store/einvoice_store_test.go::TestEInvoice_ValidationPinsTheProfileVersion",
		"CB:connector/service_test.go::TestProfileWithdrawnDegradesAndRefuses_INT_NP_022",
		"CB:connector/service_test.go::TestDeprecationWindowIsEnforcedBeforeAProfileIsWithdrawn",
		"CB:connector/service_test.go::TestAWithdrawalAfterTheWindowIsAllowed",
		"CB:store/pg_governance_test.go::TestPg_ADeprecationRecordsItsSunset"}, ""},
	{"INT-CTRL-026", "File manifest/hash/acknowledgement", OutOfScope, []string{
		"DI:store/import_test.go::TestImport_HashMismatchQuarantinesAtSeal",
		"DI:store/import_test.go::TestImport_ChunkLevelIntegrity",
		"DI:imports/profile_test.go::TestSpecHashesMoveWithTheirMeaning"},
		"Inbound import manifests are proven. Outbound bank payment-file exchange is out of scope: banks are reached through their APIs only, so there is no file manifest or acknowledgement to certify."},
	{"INT-CTRL-027", "Bank adapter finality separation", Covered, []string{
		"PI:providers/engine_test.go::TestAcknowledgedIsSubmittedNeverSettled",
		"PS:store/callback_store_test.go::TestCallback_TheStateGuardIsTheBackstop",
		"PS:handler/handler_test.go::TestPollPaymentStatus_SamePrincipalAsCreator_AssertingSettled_Forbidden"}, ""},
	{"INT-CTRL-028", "Tax adapter calculation separation", Covered, []string{
		"TX:authority/engine_test.go::TestTheAdapterSendsExactlyTheApprovedReturnAndComputesNothing",
		"TX:authority/engine_test.go::TestTransportSuccessIsReceivedNotAccepted",
		"TX:authority/engine_test.go::TestAChangedPayloadIsNeverSent",
		"EV:adapter/tax_test.go::TestFilingMayCarryNoAmount"},
		""},
	{"INT-CTRL-029", "E-invoice economics separation", Covered, []string{
		"EV:adapter/einvoice_test.go::TestATransformedDocumentThatChangesTheMoneyIsCaught",
		"EV:adapter/einvoice_test.go::TestInconsistentEconomicsAreRefusedNotRepaired",
		"TX:authority/einvoice_test.go::TestRefusalsBlockBeforeSend"}, ""},
	{"INT-CTRL-030", "IdP issuer/subject identity binding", Covered, []string{
		"IC:federation/login_test.go::TestEmailCollisionAcrossIssuersCannotReachAnotherPrincipal",
		"IC:store/pg_federation_test.go::TestFederation_Links",
		"IC:store/pg_federation_test.go::TestFederation_OneLiveConnectionPerIssuerPerTenant"}, ""},
	{"INT-CTRL-031", "Provisioning privilege guard", Covered, []string{
		"IC:store/pg_provisioning_test.go::TestProvisioning_MappingNeedsABaselineRoleAndAnIndependentApprover",
		"AC:handler/federation_test.go::TestFederationOrigin_PrivilegedIsRefusedNotQueued",
		"IC:store/pg_provisioning_test.go::TestProvisioning_CreateGrantsOnlyWhatTheApprovedMappingNames"}, ""},
	{"INT-CTRL-032", "Deprovision latency/reconciliation", Covered, []string{
		"IC:store/pg_provisioning_test.go::TestProvisioning_SessionEvictionFailureIsRetriedAndEscalated",
		"IC:store/pg_provisioning_test.go::TestProvisioning_DeprovisionEndsAccessFirstAndAsksQuestionsAfterwards",
		"IC:store/pg_provisioning_test.go::TestProvisioning_Reconcile"}, ""},
	{"INT-CTRL-033", "Retry budget/backpressure/circuit breaker", Covered, []string{
		"NS:store/webhook_out_test.go::TestWebhook_CircuitBreakerAndAutoDisable",
		"PI:providers/engine_test.go::TestSendingPausesWhenTheProviderIsFailing",
		"GA:handler/admission_test.go::TestOverloadIsShedWithBackpressureNotQueued_INT_NP_044"}, ""},
	{"INT-CTRL-034", "Quarantine/dead-letter ownership", Covered, []string{
		"EV:inbox/inbox_test.go::TestQuarantineAlertsNameAnOwner",
		"EV:outbox/controls_test.go::TestOutboxAlertsNameAnOwner",
		"NS:whout/owner_test.go::TestADeadLetterNamesItsOwner",
		"PS:store/callback_store_test.go::TestCallback_UncorrelatedIsQuarantinedForANamedOwnerOnce",
		"TX:store/callback_store_test.go::TestCallback_UncorrelatedIsQuarantinedForANamedOwnerOnce",
		"DI:store/import_test.go::TestImport_PartialFailureIsExplicitAndNeverReconciled"}, ""},
	{"INT-CTRL-035", "Sensitive telemetry redaction", Covered, []string{
		"EV:integration/integration_test.go::TestRedactNestedSecrets",
		"EV:integration/integration_test.go::TestEvidenceJSONHasNoPayloadBody",
		"EV:inbox/inbox_test.go::TestStoredErrorTextIsScrubbed_INT_NP_047"}, ""},
	{"INT-CTRL-036", "Integration-to-domain reconciliation", Partial, []string{
		"EV:integration/integration_test.go::TestOutcomeCannotHideReconciliationBreak_INT_NP_048",
		"DI:imports/outcome_test.go::TestAnImportIsCertifiedOnlyWhenItReconciled",
		"DI:store/import_test.go::TestImport_MaterialHappyPathToReconciled",
		"PS:handler/handler_test.go::TestLinkStatementConfirmation_Conflict"},
		"Imports reconcile and report through the shared Outcome model. Bank, tax and webhook flows have no population to reconcile inside INT; the bank statement confirmation lives in payment-status."},
}

// Scenarios are INT-NP-001..048.
var Scenarios = []Entry{
	{"INT-NP-001", "Forged tenant header conflicts with authenticated tenant context", Covered, []string{
		"GA:handler/admission_test.go::TestForgedTenantContextRefusedAsProblem_INT_NP_001",
		"EV:integration/integration_test.go::TestForgedTenantHeaderRejected_INT_NP_001"}, ""},
	{"INT-NP-002", "Valid gateway credential lacks downstream permission", Covered, []string{
		"GA:handler/admission_test.go::TestGatewayAdmitsAndTheDomainRefusesWithBothDecisionsRetained_INT_NP_002",
		"GA:handler/admission_test.go::TestValidCredentialIsAdmittedAndCounted_INT_NP_002",
		"CB:handler/governance_test.go::TestEveryCommandChecksItsOwnAuthorizationAction"}, ""},
	{"INT-NP-003", "Unsupported API version requested", Covered, []string{
		"GA:handler/admission_test.go::TestUnsupportedAPIVersionRefusedWithoutFallback_INT_NP_003",
		"GA:admission/admission_test.go::TestPolicyVersionHeaderNeverFallsBack"}, ""},
	{"INT-NP-004", "Oversized/decompression-bomb request", Covered, []string{
		"GA:handler/admission_test.go::TestOversizedAndCompressedBodiesRefusedBeforeAuthentication_INT_NP_004",
		"GA:admission/admission_test.go::TestPolicyRefusesCompressedBodiesByDefault_DecompressionBomb"}, ""},
	{"INT-NP-005", "Concurrent requests attempt rate-limit bypass", Covered, []string{
		"GA:admission/admission_test.go::TestLimiterConcurrentRequestsCannotBypassQuota_INT_NP_005",
		"GA:handler/admission_test.go::TestConcurrentRequestsCannotBypassClientQuota_INT_NP_005"}, ""},
	{"INT-NP-006", "Producer sends event with incompatible schema", Partial, []string{
		"EV:outbox/controls_test.go::TestGateQuarantinesIncompatiblePayloadBeforePublish_INT_NP_006",
		"EV:integration/gate_test.go::TestGateRejectsNonConformingNamingTheContract",
		"CBC:cmd/server/schemagate_test.go::TestTheConnectivityRelayIsGatedByTheRegisteredContract",
		"EV:integration/gateenv_test.go::TestGateFromEnv"},
		"Quarantine-before-publish is proven at the service for connectivity and by the shared relay's own test for the other three shared-relay producers; the four producers with a local outbox (notification, data-ingestion, identity-context, access-control) are still ungated."},
	{"INT-NP-007", "Same event delivered twice", Covered, []string{
		"EV:inbox/inbox_test.go::TestProcessOnceThenDuplicate_INT_NP_007",
		"EV:inbox/inbox_test.go::TestConcurrentDuplicatesProduceOneEffect"}, ""},
	{"INT-NP-008", "Events arrive out of order within protected aggregate", Covered, []string{
		"EV:inbox/inbox_test.go::TestOutOfOrderDoesNotRegressState_INT_NP_008",
		"EV:inbox/inbox_test.go::TestConcurrentVersionsOnlyEverApplyIncreasing",
		"PS:store/callback_store_test.go::TestCallback_NeverMovesAPaymentBackwards"}, ""},
	{"INT-NP-009", "Wrong partition key breaks expected ordering", Covered, []string{
		"EV:outbox/controls_test.go::TestWrongPartitionKeyIsQuarantinedAndTheProducerBlocked_INT_NP_009",
		"EV:outbox/controls_test.go::TestVerifyPartitioning",
		"EV:envelope/envelope_test.go::TestPartitionKey_StablePerAggregateAndOpaque"},
		"The relay quarantines any event whose key is not the envelope's hash, and RequirePartitioning is the startup/CI gate. Mixed keys per aggregate are detectable, not preventable per row."},
	{"INT-NP-010", "Event replay occurs after old consumer side effect", Covered, []string{
		"EV:outbox/compose_test.go::TestReplayAfterTheConsumerActedNeverDoublesTheEffect_INT_NP_010",
		"EV:outbox/controls_test.go::TestReplayRepublishesUnderOriginalID_INT_NP_010",
		"EV:inbox/inbox_test.go::TestProcessOnceThenDuplicate_INT_NP_007"},
		""},
	{"INT-NP-011", "Domain DB commits but broker is unavailable", Covered, []string{
		"EV:outbox/outbox_test.go::TestRelay_BrokerOutageDoesNotSpendAttempts",
		"EV:outbox/outbox_test.go::TestEnqueue_RolledBackTransactionLeavesNoEvent",
		"EV:outbox/load_test.go::TestLoad_BrokerOutageMidBacklogLosesNothingAndBlamesNoEvent",
		"EV:outbox/load_test.go::TestLoad_ConcurrentRelaysDrainABacklogExactlyOnce"}, ""},
	{"INT-NP-012", "Broker publish succeeds but relay crashes before marking published", Covered, []string{
		"EV:outbox/outbox_test.go::TestRelay_ExpiredLeaseIsReclaimedWithOriginalDispatchTime",
		"EV:outbox/outbox_test.go::TestRelay_RetryKeepsTheSameEventID",
		"EV:inbox/inbox_test.go::TestProcessOnceThenDuplicate_INT_NP_007"}, ""},
	{"INT-NP-013", "Consumer crashes after irreversible side effect but before ACK", Covered, []string{
		"EV:inbox/inbox_test.go::TestCrashBeforeCommitLeavesNoEffectAndRetrySucceeds_INT_NP_013",
		"EV:inbox/inbox_test.go::TestIdempotencyKeyIsStableAcrossRedelivery",
		"PI:providers/engine_test.go::TestTimeoutThenRetryAsksBeforeSending"}, ""},
	{"INT-NP-014", "Webhook signature invalid", Covered, []string{
		"EV:signature/signature_test.go::TestInvalidSignatureIsRejected",
		"NS:store/webhook_out_test.go::TestWebhook_SignedMinimizedDeliveryWithEvidence"}, ""},
	{"INT-NP-015", "Webhook is replayed inside/outside replay window", Covered, []string{
		"EV:signature/signature_test.go::TestReplayIsRejected",
		"EV:signature/signature_test.go::TestTimestampWindow",
		"PS:callbacks/callbacks_test.go::TestSharedTimestampedSchemeEnforcesTheReplayWindowAndOnce"}, ""},
	{"INT-NP-016", "Webhook destination returns 500/429 repeatedly", Covered, []string{
		"NS:store/webhook_out_test.go::TestWebhook_BoundedRetryThenDeadLetter",
		"NS:store/webhook_out_test.go::TestWebhook_RateLimitedHonoursRetryAfter",
		"NS:store/webhook_out_test.go::TestWebhook_CircuitBreakerAndAutoDisable"}, ""},
	{"INT-NP-017", "Webhook payload contains field outside approved subscription scope", Covered, []string{
		"NS:store/webhook_out_test.go::TestWebhook_BodyOutsideScopeIsBlockedBeforeSend",
		"NS:whout/rules_test.go::TestVerifyBodyBlocksAnythingOutsideTheScope"}, ""},
	{"INT-NP-018", "Webhook destination resolves/redirects to prohibited region/private endpoint", Covered, []string{
		"EV:egress/egress_test.go::TestCheckDestinationResolves_INT_NP_018",
		"NS:store/webhook_out_test.go::TestWebhook_EgressIsReCheckedOnEverySend"}, ""},
	{"INT-NP-019", "Connector plaintext credential appears in config/log", Covered, []string{
		"CB:connector/service_test.go::TestCreateRefusesAnythingThatCouldBeASecret_INT_NP_019",
		"CB:handler/governance_test.go::TestSecretsAreRefusedAndNeverEchoed_INT_NP_019",
		"CB:domain/lifecycle_test.go::TestEndpointSyntaxRefusesCredentials_INT_NP_019"}, ""},
	{"INT-NP-020", "Credential/consent revoked mid-sync", Covered, []string{
		"CB:connector/service_test.go::TestCredentialRevokedMidSyncSuspendsAndKeepsTheCheckpoint_INT_NP_020",
		"CB:connector/service_test.go::TestConsentExpiryDegradesAndRefusesIngestion_INT_NP_020"}, ""},
	{"INT-NP-021", "Connector scope widened without approval", Covered, []string{
		"CB:connector/service_test.go::TestProtectedChangeStopsALiveConnectorUntilReapproved_INT_NP_021_023",
		"CB:connector/service_test.go::TestIngestEnforcesApprovedScopes_INT_NP_021"}, ""},
	{"INT-NP-022", "Provider API version becomes unsupported", Covered, []string{
		"CB:connector/service_test.go::TestProfileWithdrawnDegradesAndRefuses_INT_NP_022",
		"CB:connector/service_test.go::TestUpgradeProfileIsAProtectedChange"}, ""},
	{"INT-NP-023", "Connector destination changes after approval", Covered, []string{
		"CB:domain/lifecycle_test.go::TestProtectedChangeOnALiveConnectorWithdrawsApproval_INT_NP_021_023",
		"CB:domain/lifecycle_test.go::TestConfigurationChangedBehindTheApprovalCannotGoLive"}, ""},
	{"INT-NP-024", "Inbound import contains malware", Covered, []string{
		"DI:store/import_test.go::TestImport_MalwareIsQuarantinedBeforeAnythingIsParsed",
		"EV:scan/scan_test.go::TestBuiltinFlagsEICARAndHidingPlaces"}, ""},
	{"INT-NP-025", "File content hash differs from manifest", Covered, []string{
		"DI:store/import_test.go::TestImport_HashMismatchQuarantinesAtSeal",
		"DI:store/import_test.go::TestImport_ChunkLevelIntegrity"}, ""},
	{"INT-NP-026", "Import schema/profile mismatch", Covered, []string{
		"DI:store/import_test.go::TestImport_SchemaMismatchStopsBeforePreview",
		"DI:imports/profile_test.go::TestCSVSchemaMismatchIsAFileLevelError"}, ""},
	{"INT-NP-027", "Same import file is uploaded twice", Covered, []string{
		"DI:store/import_test.go::TestImport_DuplicateFileIsRefused",
		"DI:store/import_test.go::TestImport_ARetriedDeliveryNeverCreatesASecondDomainRecord"}, ""},
	{"INT-NP-028", "Approved preview payload changes before execution", Covered, []string{
		"DI:store/import_test.go::TestImport_ExecutionRefusesAnythingThatDiffersFromTheApprovedPreview"}, ""},
	{"INT-NP-029", "Partial import failures are reported as completed", Covered, []string{
		"DI:store/import_test.go::TestImport_PartialFailureIsExplicitAndNeverReconciled"}, ""},
	{"INT-NP-030", "Material financial import lacks counts/control totals", Covered, []string{
		"DI:store/import_test.go::TestImport_ControlTotals"}, ""},
	{"INT-NP-031", "Import worker attempts direct DB write into target domain", Covered, []string{
		"DI:imports/architecture_test.go::TestTheImportEngineHasNoDatabaseAccessOfItsOwn",
		"DI:store/import_test.go::TestImport_TheDatabaseRefusesDirectWrites"}, ""},
	{"INT-NP-032", "Bank provider times out after receiving payment request", Covered, []string{
		"PI:providers/engine_test.go::TestTimeoutThenRetryAsksBeforeSending",
		"PI:handler/handler_test.go::TestSubmitAttempt_Timeout_PendingUnknown",
		"EV:adapter/adapter_test.go::TestTimeoutAfterSendIsUnknown"}, ""},
	{"INT-NP-033", "Duplicate bank callback received", Covered, []string{
		"PS:store/callback_store_test.go::TestCallback_DuplicateIsRecordedAndChangesNothing",
		"PS:store/callback_store_test.go::TestCallback_ConcurrentCopiesOfOneEventApplyExactlyOnce",
		"PS:handler/handler_test.go::TestProcessProviderCallback_Duplicate_NotDoubleApplied"}, ""},
	{"INT-NP-034", "Older provider callback arrives after newer final status", Covered, []string{
		"PS:store/callback_store_test.go::TestCallback_NeverMovesAPaymentBackwards",
		"PS:handler/handler_test.go::TestProcessProviderCallback_RegressionBlocked"}, ""},
	{"INT-NP-035", "Tax authority times out after submission", Covered, []string{
		"TX:authority/engine_test.go::TestTimeoutThenRetryAsksTheAuthorityAndNeverFilesTwice",
		"TX:authority/engine_test.go::TestRetryWithoutIdempotencyGuaranteeNeverResends",
		"TX:authority/engine_test.go::TestOutageNearTheDeadlineIsEscalatedNotLeftToWait"}, ""},
	{"INT-NP-036", "Authority schema changes after tax payload approval", Covered, []string{
		"TX:authority/engine_test.go::TestAChangedPayloadIsNeverSent",
		"TX:authority/engine_test.go::TestUnsupportedSchemaVersionIsBlockedNotSent",
		"TX:authority/einvoice_test.go::TestAProfileChangedAfterValidationNeverReTransformsAnApprovedDocument"}, ""},
	{"INT-NP-037", "E-invoice adapter uses wrong network/profile version", Covered, []string{
		"EV:adapter/einvoice_test.go::TestWrongProfileIsRefusedExplicitly",
		"TX:authority/einvoice_test.go::TestRefusalsBlockBeforeSend",
		"TX:authority/einvoice_test.go::TestAnInterfaceKindMustMatchTheProfileKind"}, ""},
	{"INT-NP-038", "Transformed e-invoice totals differ from sealed canonical invoice", Covered, []string{
		"EV:adapter/einvoice_test.go::TestATransformedDocumentThatChangesTheMoneyIsCaught",
		"EV:adapter/einvoice_test.go::TestInconsistentEconomicsAreRefusedNotRepaired"}, ""},
	{"INT-NP-039", "Same email exists under two identity issuers", Covered, []string{
		"IC:federation/login_test.go::TestEmailCollisionAcrossIssuersCannotReachAnotherPrincipal",
		"IC:store/pg_provisioning_test.go::TestProvisioning_EmailCollisionQuarantinesInsteadOfMerging",
		"IC:store/pg_provisioning_test.go::TestProvisioning_EmailCollisionAcrossTenantsIsNotACollision"}, ""},
	{"INT-NP-040", "SCIM group maps directly to privileged role without approved mapping", Covered, []string{
		"IC:store/pg_provisioning_test.go::TestProvisioning_MappingNeedsABaselineRoleAndAnIndependentApprover",
		"AC:handler/federation_test.go::TestFederationOrigin_PrivilegedIsRefusedNotQueued",
		"IC:federation/mapping_test.go::TestEvaluateQuarantinesInsteadOfChoosing"}, ""},
	{"INT-NP-041", "Deprovision event fails/delays while account retains access", Covered, []string{
		"IC:store/pg_provisioning_test.go::TestProvisioning_SessionEvictionFailureIsRetriedAndEscalated",
		"IC:store/pg_provisioning_test.go::TestProvisioning_DeprovisionEndsAccessFirstAndAsksQuestionsAfterwards",
		"IC:store/pg_provisioning_test.go::TestProvisioning_SuspendedConnectionStillTakesAccessAway"}, ""},
	{"INT-NP-042", "OIDC/SAML assertion has wrong issuer/audience/signature", Deviation, []string{
		"IC:context/resolver_test.go::TestResolve_SAMLAssertionFailsClosedWithNoFallback",
		"IC:federation/oidc_test.go::TestVerifyRefusesWhatItMust",
		"IC:federation/oidc_test.go::TestVerifyRefusesForgeriesAndAlgorithmConfusion",
		"IC:federation/login_test.go::TestEveryRefusalLooksTheSameToTheCaller"},
		"OIDC fails closed on wrong issuer, audience and signature, and a SAML assertion is refused with no fallback. SAML federation itself was deferred by decision; a named owner must accept that or schedule it."},
	{"INT-NP-043", "Provider callback cannot correlate to durable attempt", Covered, []string{
		"PS:store/callback_store_test.go::TestCallback_UnknownPaymentIsNotDecidedSoItCanApplyLater",
		"PS:store/callback_store_test.go::TestCallback_AmbiguousReferenceChangesNothing",
		"TX:store/callback_store_test.go::TestCallback_UnknownAmbiguousAndCrossTenant",
		"PS:store/callback_store_test.go::TestCallback_UncorrelatedIsQuarantinedForANamedOwnerOnce",
		"TX:store/callback_store_test.go::TestCallback_UncorrelatedIsQuarantinedForANamedOwnerOnce"}, ""},
	{"INT-NP-044", "Integration backlog is silently dropped during overload", Partial, []string{
		"GA:handler/admission_test.go::TestOverloadIsShedWithBackpressureNotQueued_INT_NP_044",
		"GA:admission/admission_test.go::TestGateConcurrentNeverExceedsCeiling",
		"EV:outbox/outbox_test.go::TestRelay_BrokerOutageDoesNotSpendAttempts",
		"GA:siem/client_test.go::TestSaturationDropsAreExposedAsAMetric",
		"EV:outbox/load_test.go::TestLoad_BrokerOutageMidBacklogLosesNothingAndBlamesNoEvent"},
		"The outbox never drops (proven under load) and SIEM drops are now a metric. The gateway's shed decisions are held in an in-process ring and streamed to the SIEM, not stored durably by the gateway itself."},
	{"INT-NP-045", "Replay/dead-letter is moved to non-compliant region", Covered, []string{
		"EV:outbox/controls_test.go::TestRepairCannotCrossRegions_INT_NP_045",
		"EV:inbox/inbox_test.go::TestRequeueCannotCrossRegions_INT_NP_045",
		"NS:whout/replay_region_test.go::TestReplayRefusesADestinationRegionThisDeploymentDoesNotServe_INT_NP_045",
		"NS:store/webhook_out_test.go::TestWebhook_EgressIsReCheckedOnEverySend"},
		"Outbox repair, inbox requeue and webhook replay are region-gated. Import retry never leaves the deployment: items are redelivered only to the owning-domain targets configured for it."},
	{"INT-NP-046", "Provider rate-limit response triggers unbounded retry storm", Covered, []string{
		"NS:store/webhook_out_test.go::TestWebhook_RateLimitedHonoursRetryAfter",
		"PI:providers/engine_test.go::TestProviderRateLimitIsNotProcessed",
		"PI:providers/engine_test.go::TestSendingPausesWhenTheProviderIsFailing"}, ""},
	{"INT-NP-047", "Raw provider response includes secrets/PII in normal logs", Deviation, []string{
		"EV:inbox/inbox_test.go::TestStoredErrorTextIsScrubbed_INT_NP_047",
		"PI:providers/engine_test.go::TestCredentialIsUsedForOneCallAndNeverLogged",
		"TX:authority/engine_test.go::TestSchemaFailureBlocksAndNamesPathsNeverValues"},
		"By design no raw provider response is stored anywhere: only request and response hashes. The specification's 'raw evidence stored separately with restricted access' is not implemented; a named owner must accept hash-only evidence or ask for a restricted store."},
	{"INT-NP-048", "Integration reports success although downstream reconciliation differs", Covered, []string{
		"DI:imports/outcome_test.go::TestAnImportIsCertifiedOnlyWhenItReconciled",
		"EV:integration/integration_test.go::TestOutcomeCannotHideReconciliationBreak_INT_NP_048",
		"EV:integration/integration_test.go::TestOutcomePrecedence",
		"DI:store/import_test.go::TestImport_PartialFailureIsExplicitAndNeverReconciled"},
		""},
}
