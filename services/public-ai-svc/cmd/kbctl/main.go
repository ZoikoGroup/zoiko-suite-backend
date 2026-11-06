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
	fmt.Fprintln(os.Stderr, "usage: kbctl keygen|build-kb|build-claims|publish|promote|revoke|flag|manifest|audit|conflicts|dlq-replay [flags]")
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
		k := events.NewKafkaPublisher(strings.Split(brokers, ","))
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

// audit prints the newest control-audit entries (Doc 06 §21).
func audit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	limit := fs.Int("n", 20, "number of entries")
	verify := fs.Bool("verify", false, "verify the whole hash chain (Doc 06 §21) instead of listing")
	_ = fs.Parse(args)
	st, err := openStore(ctx)
	if err != nil {
		return err
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
