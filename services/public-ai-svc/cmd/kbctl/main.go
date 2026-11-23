// Command kbctl is the offline operator tool for knowledge and claim
// releases (STEP 6). It never edits a release in place.
//
//	kbctl keygen
//	kbctl build-kb     -in testdata/fixtures/kb   -id kb-dev-1     -out kb.release.json
//	kbctl build-claims -in testdata/fixtures/claims.json -id claims-dev-1 -out claims.release.json
//	kbctl publish      -file kb.release.json [-promote]
//	kbctl promote      -kind kb -id kb-dev-1          (also used for rollback)
//	kbctl revoke       -kind source -id src-x -reason CODE
//	kbctl flag         -name public_ai_enabled -value false -actor sre-1 -approval CHG-1
//	kbctl manifest     -id suivo-r0-20261027-01 -git <sha> -image ghcr.io/zoiko/public-ai-svc@sha256:<64 hex> -env staging //	                   -eval eval-report.json -drill drill-report.json -sbom sbom.cdx.json -scan trivy.json -out release-manifest.json
//
// Signing keys come from PUBLIC_AI_SIGNING_KEY / PUBLIC_AI_SIGNING_PUBLIC_KEY;
// the database from PUBLIC_AI_DATABASE_URL.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"go.uber.org/zap"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/public-ai-svc/deployments/migrations"
	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/events"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/release"
	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen()
	case "build-kb":
		err = buildKB(os.Args[2:])
	case "build-claims":
		err = buildClaims(os.Args[2:])
	case "publish":
		err = publish(ctx, os.Args[2:])
	case "promote":
		err = promote(ctx, os.Args[2:])
	case "revoke":
		err = revoke(ctx, os.Args[2:])
	case "flag":
		err = setFlag(ctx, os.Args[2:])
	case "manifest":
		err = manifest(ctx, os.Args[2:])
	case "audit":
		err = audit(ctx, os.Args[2:])
	case "conflicts":
		err = conflicts(ctx, os.Args[2:])
	case "policy-sign":
		err = policySign()
	case "review-kb":
		err = reviewKB(os.Args[2:])
	case "quality":
		err = quality(ctx, os.Args[2:])
	case "signoff":
		err = signoff(ctx, os.Args[2:])
	case "verify-manifest":
		err = verifyManifest(ctx, os.Args[2:])
	case "dlq-replay":
		err = dlqReplay(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kbctl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kbctl keygen|build-kb|build-claims|publish|promote|revoke|flag|manifest|audit|conflicts|policy-sign|signoff|verify-manifest|review-kb|quality|dlq-replay [flags]")
	os.Exit(2)
}

func keygen() error {
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		return err
	}
	fmt.Println("# Dev signing keys. Keep the private key out of git (.env is ignored).")
	fmt.Println("PUBLIC_AI_SIGNING_PUBLIC_KEY=" + pub)
	fmt.Println("PUBLIC_AI_SIGNING_KEY=" + priv)
	return nil
}

func privKey() (string, error) {
	k := os.Getenv("PUBLIC_AI_SIGNING_KEY")
	if k == "" {
		return "", fmt.Errorf("PUBLIC_AI_SIGNING_KEY is not set (run: kbctl keygen)")
	}
	return k, nil
}

func buildKB(args []string) error {
	fs := flag.NewFlagSet("build-kb", flag.ExitOnError)
	in := fs.String("in", "", "fixture/import directory")
	id := fs.String("id", "", "release id")
	out := fs.String("out", "", "output release file")
	_ = fs.Parse(args)
	if *in == "" || *id == "" || *out == "" {
		return fmt.Errorf("-in, -id and -out are required")
	}
	key, err := privKey()
	if err != nil {
		return err
	}
	// Drafts are admitted only for local/dev builds (KB_ALLOW_DRAFT=true with
	// PUBLIC_AI_ENVIRONMENT dev or test); any other build with a draft fails.
	env := envOr("PUBLIC_AI_ENVIRONMENT", "dev")
	devLike := env == "dev" || env == "test"
	allowDraft := os.Getenv("KB_ALLOW_DRAFT") == "true" && devLike
	manifest, sig, reports, err := release.BuildKBWith(*in, *id, time.Now(), key, knowledge.IngestOptions{AllowDraft: allowDraft, AllowSynthetic: devLike})
	for _, r := range reports {
		fmt.Printf("%-30s %-24s %-20s %s\n", r.File, r.SourceID, r.State, r.Reason)
	}
	if err != nil {
		return err
	}
	if err := signing.WriteFile(*out, *id, "kb", manifest, sig); err != nil {
		return err
	}
	fmt.Printf("wrote %s (sha256 %s)\n", *out, signing.Digest(manifest))
	return nil
}

func buildClaims(args []string) error {
	fs := flag.NewFlagSet("build-claims", flag.ExitOnError)
	in := fs.String("in", "", "claims registry JSON")
	id := fs.String("id", "", "release id")
	out := fs.String("out", "", "output release file")
	_ = fs.Parse(args)
	if *in == "" || *id == "" || *out == "" {
		return fmt.Errorf("-in, -id and -out are required")
	}
	key, err := privKey()
	if err != nil {
		return err
	}
	env := envOr("PUBLIC_AI_ENVIRONMENT", "dev")
	devLike := env == "dev" || env == "test"
	manifest, sig, err := release.BuildClaimsWith(*in, *id, time.Now(), key, release.ClaimsOptions{AllowSynthetic: devLike})
	if err != nil {
		return err
	}
	if err := signing.WriteFile(*out, *id, "claims", manifest, sig); err != nil {
		return err
	}
	fmt.Printf("wrote %s (sha256 %s)\n", *out, signing.Digest(manifest))
	return nil
}

func openStore(ctx context.Context) (store.Store, error) {
	dsn := os.Getenv("PUBLIC_AI_DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("PUBLIC_AI_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		return nil, err
	}
	return store.NewPostgres(pool), nil
}

func publish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	file := fs.String("file", "", "signed release file")
	andPromote := fs.Bool("promote", false, "also promote the release")
	op, eval, rollback := operatorFlags(fs)
	_ = fs.Parse(args)
	f, manifest, sig, err := signing.ReadFile(*file)
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	if err := release.Publish(ctx, st, os.Getenv("PUBLIC_AI_SIGNING_PUBLIC_KEY"), f.ReleaseID, f.Kind, manifest, sig); err != nil {
		return err
	}
	fmt.Println("published", f.Kind, f.ReleaseID)
	if *andPromote {
		return doPromote(ctx, st, f.Kind, f.ReleaseID, *op(), *eval, *rollback)
	}
	return nil
}

func promote(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	kind := fs.String("kind", "", "kb or claims")
	id := fs.String("id", "", "release id")
	op, eval, rollback := operatorFlags(fs)
	_ = fs.Parse(args)
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	return doPromote(ctx, st, *kind, *id, *op(), *eval, *rollback)
}

// operatorFlags are the authority every control action records (Doc 06
// §21, Doc 09 §22): -actor, -approval and -why, plus the evaluation report
// for a promotion (Doc 03 §10-11) and -rollback to an earlier release.
func operatorFlags(fs *flag.FlagSet) (func() *release.Operator, *string, *bool) {
	actor := fs.String("actor", "", "who performs the action (required)")
	approval := fs.String("approval", "", "approval or change reference (required)")
	why := fs.String("why", "", "reason for the action")
	eval := fs.String("eval", "", "evaluation report JSON (verdict, knowledge_release_id); not needed for -rollback")
	rollback := fs.Bool("rollback", false, "roll back to a release that was already promoted")
	return func() *release.Operator {
		return &release.Operator{Actor: *actor, Approval: *approval, Reason: *why, Environment: os.Getenv("PUBLIC_AI_ENVIRONMENT")}
	}, eval, rollback
}

func doPromote(ctx context.Context, st store.Store, kind, id string, op release.Operator, evalPath string, rollback bool) error {
	rel, err := st.GetRelease(ctx, id)
	if err != nil {
		return err
	}
	if err := signing.Verify(os.Getenv("PUBLIC_AI_SIGNING_PUBLIC_KEY"), rel.Manifest, rel.Signature); err != nil {
		return err
	}
	var ev *release.EvalEvidence
	if evalPath != "" {
		b, err := os.ReadFile(evalPath)
		if err != nil {
			return err
		}
		ev = &release.EvalEvidence{}
		if err := json.Unmarshal(b, ev); err != nil {
			return fmt.Errorf("eval report: %w", err)
		}
	}
	// Two distinct approvers, an independent promoter and a passing
	// evaluation before the pointer moves (Doc 03 §10-11, KB-016, KB-027).
	// A rollback is accepted only to a release with a prior completed
	// promotion in the control audit.
	if err := release.PromotionGateAudited(ctx, st, kind, id, rel.Manifest, op, ev, rollback); err != nil {
		return err
	}
	approvers, _ := release.Approvers(kind, rel.Manifest)
	event, err := events.ReleasePromotedFor(kind, id, signing.Digest(rel.Manifest), approvers, time.Now())
	if err != nil {
		return err
	}
	if err := release.PromoteAudited(ctx, st, op, kind, id, []store.OutboxEvent{event}); err != nil {
		return err
	}
	fmt.Println("promoted", kind, id, "by", op.Actor)
	return nil
}

// dlqReplay re-publishes dead-lettered events after rechecking each one
// (Doc 05 CT-042): authorization, envelope and allowlisted fields, and
// freshness against the active releases, flags and revocation ledger. The
// input file holds one CloudEvents JSON envelope per line. The replay is
// recorded in the control audit before anything is published.
func dlqReplay(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dlq-replay", flag.ExitOnError)
	file := fs.String("file", "", "file with one dead-lettered envelope per line")
	op, _, _ := operatorFlags(fs)
	_ = fs.Parse(args)
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var envelopes [][]byte
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			envelopes = append(envelopes, []byte(line))
		}
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	o := op()
	if err := st.AppendAudit(ctx, store.AuditEntry{Action: "dlq.replay", Target: *file, Actor: o.Actor, Approval: o.Approval,
		Reason: o.Reason, AfterHash: store.Hash(raw), Environment: o.Environment, At: time.Now().UTC()}); err != nil {
		return fmt.Errorf("control audit: %w", err)
	}
	logger, _ := zap.NewProduction()
	var pub events.Publisher = events.LogPublisher{Log: logger}
	if brokers := os.Getenv("PUBLIC_AI_KAFKA_BROKERS"); brokers != "" {
		k, err := events.NewKafkaPublisher(strings.Split(brokers, ","), events.KafkaOptions{
			TLS: os.Getenv("PUBLIC_AI_KAFKA_TLS") == "true", SASLMechanism: os.Getenv("PUBLIC_AI_KAFKA_SASL_MECHANISM"),
			Username: os.Getenv("PUBLIC_AI_KAFKA_USERNAME"), Password: os.Getenv("PUBLIC_AI_KAFKA_PASSWORD"),
			SigningKey: os.Getenv("PUBLIC_AI_EVENT_SIGNING_KEY"), SignerID: os.Getenv("PUBLIC_AI_EVENT_SIGNER_ID")})
		if err != nil {
			return err
		}
		defer k.Close() //nolint:errcheck
		pub = k
	}
	rep, err := (&events.Replayer{Store: st, Pub: pub}).Replay(ctx, events.ReplayAuth{Actor: o.Actor, Approval: o.Approval}, envelopes)
	if err != nil {
		return err
	}
	fmt.Printf("replayed %d, rejected %d\n", len(rep.Replayed), len(rep.Rejected))
	for _, r := range rep.Rejected {
		fmt.Printf("  rejected %s: %s\n", r.EventID, r.Reason)
	}
	return nil
}

// exportAudit writes the whole audit (oldest first) and its chain check to
// path. Doc 09 §21, ADM-T006: an export is purpose-bound and approved (the
// store refuses an audit row without an actor and an approval reference), and
// it is recorded in the audit before the file is written, so a refused export
// leaves no file behind.
func exportAudit(ctx context.Context, st store.Store, path, purpose string, o release.Operator) (int, string, error) {
	if strings.TrimSpace(purpose) == "" {
		return 0, "", fmt.Errorf("-purpose is required for an audit export")
	}
	if err := st.AppendAudit(ctx, store.AuditEntry{Action: "audit.exported", Target: path, Actor: o.Actor, Approval: o.Approval,
		Reason: purpose, Environment: o.Environment, At: time.Now().UTC()}); err != nil {
		return 0, "", fmt.Errorf("audit export refused: %w", err)
	}
	all, err := st.ListAudit(ctx, 1<<30)
	if err != nil {
		return 0, "", err
	}
	chain := make([]store.AuditEntry, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		chain = append(chain, all[i])
	}
	check := "intact"
	if err := store.VerifyAuditChain(chain); err != nil {
		check = "BROKEN: " + err.Error()
	}
	b, _ := json.MarshalIndent(map[string]any{"purpose": purpose, "exported_by": o.Actor, "approval": o.Approval,
		"exported_at": time.Now().UTC(), "chain": check, "entries": chain}, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return 0, "", err
	}
	return len(chain), check, nil
}

// audit prints the newest control-audit entries (Doc 06 §21).
func audit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	limit := fs.Int("n", 20, "number of entries")
	verify := fs.Bool("verify", false, "verify the whole hash chain (Doc 06 §21) instead of listing")
	export := fs.String("export", "", "write the whole audit (oldest first) and its chain check to this JSON file")
	purpose := fs.String("purpose", "", "why the export is needed (required with -export, Doc 09 ADM-T006)")
	op, _, _ := operatorFlags(fs)
	_ = fs.Parse(args)
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	if *export != "" {
		// Doc 09 §21, ADM-T006: an export is purpose-bound, approved and
		// itself recorded in the audit before the file is written.
		n, check, err := exportAudit(ctx, st, *export, *purpose, *op())
		if err != nil {
			return err
		}
		fmt.Printf("exported %d audit rows to %s (chain %s)\n", n, *export, check)
		return nil
	}
	if *verify {
		all, err := st.ListAudit(ctx, 1<<30)
		if err != nil {
			return err
		}
		chain := make([]store.AuditEntry, 0, len(all))
		for i := len(all) - 1; i >= 0; i-- {
			chain = append(chain, all[i])
		}
		if err := store.VerifyAuditChain(chain); err != nil {
			return fmt.Errorf("control audit chain BROKEN: %w", err)
		}
		fmt.Printf("control audit chain intact: %d rows\n", len(chain))
		return nil
	}
	entries, err := st.ListAudit(ctx, *limit)
	if err != nil {
		return err
	}
	for _, e := range entries {
		fmt.Printf("%s %-18s %-30s actor=%s role=%s approval=%s request=%s reason=%q before=%.12s after=%.12s env=%s hash=%.12s\n",
			e.At.UTC().Format(time.RFC3339), e.Action, e.Target, e.Actor, e.ActorRole, e.Approval, e.RequestID, e.Reason,
			e.BeforeHash, e.AfterHash, e.Environment, e.RowHash)
	}
	return nil
}

func revoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	kind := fs.String("kind", "", "source or claim")
	id := fs.String("id", "", "source id, source version id or claim id")
	reason := fs.String("reason", "", "reason code")
	op, _, _ := operatorFlags(fs)
	_ = fs.Parse(args)
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	var evs []store.OutboxEvent
	if *kind == "source" {
		ev, err := events.SourceRevoked(*id, *reason, time.Now())
		if err != nil {
			return err
		}
		evs = append(evs, ev)
	}
	wm, err := release.RevokeAudited(ctx, st, *op(), *kind, *id, *reason, evs)
	if err != nil {
		return err
	}
	fmt.Printf("revoked %s %s (watermark %d)\n", *kind, *id, wm)
	return nil
}

func setFlag(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("flag", flag.ExitOnError)
	name := fs.String("name", "", "flag name")
	value := fs.String("value", "", "JSON value, e.g. false or [\"I05\"]")
	actor := fs.String("actor", "", "actor reference")
	approval := fs.String("approval", "", "approval / change reference")
	env := fs.String("env", "local", "environment name")
	_ = fs.Parse(args)
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	if err := flags.Set(ctx, st, *name, []byte(*value), *actor, *approval, *env); err != nil {
		return err
	}
	fmt.Println("flag set", *name, "=", *value)
	return nil
}

func manifest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	id := fs.String("id", "", "release id, e.g. suivo-r0-20261027-01")
	git := fs.String("git", "", "application git commit")
	image := fs.String("image", "", "immutable image reference name@sha256:<64 hex> (required)")
	stage := fs.String("stage", "staging", "rollout stage")
	environment := fs.String("env", os.Getenv("PUBLIC_AI_ENVIRONMENT"), "environment (default PUBLIC_AI_ENVIRONMENT)")
	evalPath := fs.String("eval", "", "eval-report.json from cmd/evalrun (its model profile must match)")
	drillPath := fs.String("drill", "", "drill-report.json from cmd/drill (sha256 recorded)")
	sbomPath := fs.String("sbom", "", "SBOM file (sha256 recorded)")
	scanPath := fs.String("scan", "", "image/vulnerability scan report (sha256 recorded)")
	a11yPath := fs.String("a11y", "", "accessibility test report (sha256 recorded)")
	functionalPath := fs.String("functional", "", "functional test run report (sha256 recorded)")
	prevKB := fs.String("previous-kb", "", "knowledge release id this release replaces")
	prevClaims := fs.String("previous-claims", "", "claim release id this release replaces")
	out := fs.String("out", "release-manifest.json", "output file")
	op, _, _ := operatorFlags(fs)
	_ = fs.Parse(args)
	key, err := privKey()
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	rec, err := release.BuildManifest(ctx, st, release.ManifestInput{
		ReleaseID: *id, GitCommit: *git, ImageDigest: *image, RolloutStage: *stage, Environment: *environment,
		// Derived from PUBLIC_AI_MODEL_PROVIDER / PUBLIC_AI_MODEL_ID / GROQ_MODEL
		// exactly as the server derives it.
		ModelProfile: config.ModelProfileFromEnv(), EvalReportPath: *evalPath,
		PolicyVersion:   envOr("PUBLIC_AI_POLICY_VERSION", "pub-policy-1.0"),
		DrillReportPath: *drillPath, SBOMPath: *sbomPath, ScanReportPath: *scanPath, A11yReportPath: *a11yPath,
		FunctionalRunPath: *functionalPath, PreviousKB: *prevKB, PreviousClaims: *prevClaims,
	})
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	sig, err := signing.Sign(key, body)
	if err != nil {
		return err
	}
	if err := signing.WriteFile(*out, rec.ReleaseID, "release_manifest", body, sig); err != nil {
		return err
	}
	// Doc 08 §06, OPR-048: the manifest is recorded in the hash-chained audit.
	if err := release.RecordManifest(ctx, st, *op(), rec.ReleaseID, body); err != nil {
		return fmt.Errorf("record manifest: %w", err)
	}
	fmt.Println(string(body))
	fmt.Printf("wrote signed manifest %s (decision %s)\n", *out, rec.Decision)
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// conflicts lists conflict cases, or closes one with its evidence (Doc 03
// §16). Closing does not unblock a claim: a corrected, signed release does.
//
//	kbctl conflicts
//	kbctl conflicts -resolve kb-conf-1 -state RESOLVED -evidence PB-2026-11 -actor ops-1 -approval CHG-9
func conflicts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("conflicts", flag.ExitOnError)
	id := fs.String("resolve", "", "conflict id to close")
	state := fs.String("state", store.ConflictResolved, "RESOLVED or DISMISSED")
	evidence := fs.String("evidence", "", "reference to the approved correction (required to close)")
	op, _, _ := operatorFlags(fs)
	_ = fs.Parse(args)
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	if *id != "" {
		o := op()
		if err := st.ResolveConflict(ctx, *id, *state, *evidence, store.AuditEntry{Actor: o.Actor, Approval: o.Approval, Reason: o.Reason,
			Environment: o.Environment, At: time.Now().UTC()}); err != nil {
			return err
		}
		fmt.Println("closed", *id, *state)
		return nil
	}
	cs, err := st.Conflicts(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		fmt.Printf("%s %-9s %-6s %-22s owner=%s blocked=%t subject=%s claims=%v evidence=%s\n", c.DetectedAt.UTC().Format(time.RFC3339),
			c.ResolutionState, c.Severity, c.DetectionType, c.AssignedOwner, c.BlockedClaim, c.SubjectKey, c.ClaimIDs, c.ResolutionEvidenceRef)
	}
	return nil
}

// policySign prints the running build's policy digest and the signers'
// signature over it (Doc 08 OPR-002). Deploy both as PUBLIC_AI_POLICY_DIGEST
// and PUBLIC_AI_POLICY_SIGNATURE; the server refuses to start otherwise.
func policySign() error {
	key, err := privKey()
	if err != nil {
		return err
	}
	digest := policy.BundleDigest(model.PromptTemplateVersion)
	sig, err := signing.Sign(key, []byte(policy.PolicySigningPayload(digest)))
	if err != nil {
		return err
	}
	fmt.Println("PUBLIC_AI_POLICY_DIGEST=" + digest)
	fmt.Println("PUBLIC_AI_POLICY_SIGNATURE=" + base64.StdEncoding.EncodeToString(sig))
	return nil
}

// signoff records a named approver's sign-off on a recorded manifest
// (Doc 08 §33, Doc 11 §24).
//
//	kbctl signoff -file release-manifest.json -role sre -actor <name> -approval <ref>
func signoff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("signoff", flag.ExitOnError)
	file := fs.String("file", "release-manifest.json", "signed release manifest")
	role := fs.String("role", "", "approver role: "+strings.Join(release.SignoffRoles, ", "))
	op, _, _ := operatorFlags(fs)
	_ = fs.Parse(args)
	f, body, _, err := signing.ReadFile(*file)
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	if err := release.SignOff(ctx, st, *op(), *role, f.ReleaseID, body); err != nil {
		return err
	}
	fmt.Println("signed off", f.ReleaseID, "as", *role)
	return nil
}

// verifyManifest is the deploy-time check: signature, recorded bytes and
// sign-offs. With -require-all it fails until every role has signed.
func verifyManifest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify-manifest", flag.ExitOnError)
	file := fs.String("file", "release-manifest.json", "signed release manifest")
	all := fs.Bool("require-all", false, "fail unless every role has signed off")
	_ = fs.Parse(args)
	f, body, sig, err := signing.ReadFile(*file)
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	signed, missing, err := release.VerifyDeployManifest(ctx, st, os.Getenv("PUBLIC_AI_SIGNING_PUBLIC_KEY"), f.ReleaseID, body, sig)
	if err != nil {
		return err
	}
	for _, s := range signed {
		fmt.Printf("signed  %-8s %s (%s) %s\n", s.Role, s.Actor, s.Approval, s.At.UTC().Format(time.RFC3339))
	}
	for _, r := range missing {
		fmt.Printf("PENDING %s\n", r)
	}
	if *all && len(missing) > 0 {
		return fmt.Errorf("%d sign-offs missing", len(missing))
	}
	fmt.Println("manifest verified and recorded:", f.ReleaseID)
	return nil
}

// reviewKB reports every source's Doc 09 §16 editorial state (DRAFT →
// VALIDATION_FAILED or PENDING_REVIEW → APPROVED) without publishing.
func reviewKB(args []string) error {
	fs := flag.NewFlagSet("review-kb", flag.ExitOnError)
	dir := fs.String("dir", "data/public-kb", "release directory")
	_ = fs.Parse(args)
	m, reports, err := knowledge.IngestDirWith(*dir, "review", time.Now().UTC(), knowledge.IngestOptions{AllowDraft: true, AllowSynthetic: true})
	if err != nil {
		return err
	}
	draft := map[string]bool{}
	for _, src := range m.Sources {
		draft[src.SourceID] = src.ApprovalState == knowledge.ApprovalDraft
	}
	counts := map[string]int{}
	for _, r := range reports {
		st := knowledge.EditorialState(r, draft[r.SourceID])
		counts[st]++
		fmt.Printf("%-18s %-28s %-20s %s\n", st, r.SourceID, r.State, r.Reason)
	}
	fmt.Printf("review: %d approved, %d pending review, %d validation failed\n",
		counts[knowledge.EditorialApproved], counts[knowledge.EditorialPendingReview], counts[knowledge.EditorialValidationFailed])
	return nil
}

// quality lists answers visitors marked not useful (Doc 09 §19), blinded:
// turn, intent and the public reply only, never the session or the question.
func quality(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quality", flag.ExitOnError)
	limit := fs.Int("n", 50, "number of cases")
	_ = fs.Parse(args)
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	q, ok := st.(store.QualityQueue)
	if !ok {
		return fmt.Errorf("this store has no quality queue")
	}
	cases, err := q.QualityCases(ctx, *limit)
	if err != nil {
		return err
	}
	for _, c := range cases {
		var env struct {
			Text   string `json:"text"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(c.Response, &env)
		fmt.Printf("%s %s %-4s %-10s reason=%-12s reply=%.120q\n", c.CreatedAt.UTC().Format(time.RFC3339), c.TurnID, c.IntentID, c.Status, c.FeedbackReason, env.Text)
	}
	fmt.Printf("quality queue: %d not-useful answers\n", len(cases))
	return nil
}
