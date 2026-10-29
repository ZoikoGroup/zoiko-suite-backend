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
//	kbctl manifest     -id suivo-r0-20261027-01 -git <sha> -image <digest> -eval eval-report.json -out release-manifest.json
//
// Signing keys come from PUBLIC_AI_SIGNING_KEY / PUBLIC_AI_SIGNING_PUBLIC_KEY;
// the database from PUBLIC_AI_DATABASE_URL.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/public-ai-svc/deployments/migrations"
	"zoiko.io/public-ai-svc/internal/events"
	"zoiko.io/public-ai-svc/internal/flags"
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
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kbctl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kbctl keygen|build-kb|build-claims|publish|promote|revoke|flag|manifest [flags]")
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
	manifest, sig, reports, err := release.BuildKB(*in, *id, time.Now(), key)
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
	manifest, sig, err := release.BuildClaims(*in, *id, time.Now(), key)
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
		return doPromote(ctx, st, f.Kind, f.ReleaseID)
	}
	return nil
}

func promote(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	kind := fs.String("kind", "", "kb or claims")
	id := fs.String("id", "", "release id")
	_ = fs.Parse(args)
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	return doPromote(ctx, st, *kind, *id)
}

func doPromote(ctx context.Context, st store.Store, kind, id string) error {
	rel, err := st.GetRelease(ctx, id)
	if err != nil {
		return err
	}
	if err := signing.Verify(os.Getenv("PUBLIC_AI_SIGNING_PUBLIC_KEY"), rel.Manifest, rel.Signature); err != nil {
		return err
	}
	ev, err := events.ReleasePromoted(id, signing.Digest(rel.Manifest), []string{"kbctl"}, time.Now())
	if err != nil {
		return err
	}
	if err := release.Promote(ctx, st, kind, id, []store.OutboxEvent{ev}); err != nil {
		return err
	}
	fmt.Println("promoted", kind, id)
	return nil
}

func revoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	kind := fs.String("kind", "", "source or claim")
	id := fs.String("id", "", "source id, source version id or claim id")
	reason := fs.String("reason", "", "reason code")
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
	wm, err := release.Revoke(ctx, st, *kind, *id, *reason, evs)
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
	image := fs.String("image", "", "container image digest (sha256:...)")
	stage := fs.String("stage", "staging", "rollout stage")
	evalPath := fs.String("eval", "", "eval-report.json from cmd/evalrun")
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
		ReleaseID: *id, GitCommit: *git, ImageDigest: *image, RolloutStage: *stage,
		ModelProfile: envOr("PUBLIC_AI_MODEL_PROFILE", "fake-v0"), EvalReportPath: *evalPath,
		PolicyVersion: envOr("PUBLIC_AI_POLICY_VERSION", "pub-policy-1.0"),
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
