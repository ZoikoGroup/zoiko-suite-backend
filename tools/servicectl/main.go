// servicectl runs the Zoiko backend without Docker.
//
// A Go service is a static binary that reads its configuration from the
// environment and listens on a port. Docker was supplying three things around
// that: a build step, DNS names for service-to-service calls, and per-service
// environment blocks. `go build` replaces the first, the generated registry has
// already rewritten the second to 127.0.0.1, and the global env file replaces
// the third. What is left is process supervision, which is this program.
//
// The point of it being a server rather than a script is laziness in the useful
// sense: 86 services is far more than a laptop should run, and the console knows
// which handful any given page actually reads. It asks for those on navigation,
// and they are stopped again once nothing has asked for a while.
//
//	servicectl serve                 the daemon the console talks to
//	servicectl status                what is up
//	servicectl start <svc|/page>...  bring services up and wait for health
//	servicectl stop  <svc|/page>...  stop them (--all for everything)
//	servicectl build [svc]...        prefill the binary cache (no args = all)
//	servicectl env   <svc>           the environment a service would receive
//	servicectl ports                 the port allocation, and console overrides
//	servicectl pages                 the route-to-services map
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const defaultAddr = "127.0.0.1:8079"

func main() {
	log.SetFlags(log.Ltime)
	lg := log.New(os.Stderr, "", log.Ltime)

	root, err := backendRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}

	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	reg, err := LoadRegistry()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal: service registry is invalid:", err)
		os.Exit(1)
	}
	env, err := LoadGlobalEnv(DefaultEnvFiles(root)...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}

	var code int
	switch cmd {
	case "serve":
		code = cmdServe(root, reg, env, lg, args)
	case "status":
		code = cmdStatus(reg, env, root)
	case "start":
		code = cmdStart(root, reg, env, args)
	case "stop":
		code = cmdStop(args)
	case "build":
		code = cmdBuild(root, reg, env, args)
	case "env":
		code = cmdEnv(reg, env, args)
	case "ports":
		code = cmdPorts(reg)
	case "pages":
		code = cmdPages(reg)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		code = 2
	}
	os.Exit(code)
}

const usage = `servicectl -- run the Zoiko backend as OS processes, no Docker

  serve  [--addr host:port] [--idle 20m] [--start-timeout 90s] [--no-reap]
  status
  start  <service|/admin/page>...
  stop   <service|/admin/page>... | --all
  build  [service]...            (no arguments builds everything)
  env    <service>
  ports
  pages

The daemon listens on ` + defaultAddr + ` by default. The console calls
POST /v1/ensure on navigation; the dashboard is at the root path.
`

// backendRoot locates the directory holding services/, so the tool works from
// anywhere in the tree rather than only from the repository root.
func backendRoot() (string, error) {
	if v := os.Getenv("ZOIKO_BACKEND_ROOT"); v != "" {
		if ok, _ := isBackendRoot(v); ok {
			return filepath.Abs(v)
		}
		return "", fmt.Errorf("ZOIKO_BACKEND_ROOT=%s has no services/ directory", v)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if ok, _ := isBackendRoot(dir); ok {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no services/ directory found in any parent; run from inside the backend or set ZOIKO_BACKEND_ROOT")
		}
		dir = parent
	}
}

func isBackendRoot(dir string) (bool, error) {
	fi, err := os.Stat(filepath.Join(dir, "services"))
	return err == nil && fi.IsDir(), err
}

func cmdServe(root string, reg *Registry, env *GlobalEnv, lg *log.Logger, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "listen address")
	idle := fs.Duration("idle", 20*time.Minute, "stop a service after this long with nothing asking for it")
	startTimeout := fs.Duration("start-timeout", 90*time.Second, "bound on build-plus-boot for one service")
	noReap := fs.Bool("no-reap", false, "never stop idle services")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	sup := NewSupervisor(reg, env, root)
	sup.StartTimeout = *startTimeout
	if *noReap {
		sup.IdleTTL = 0
	} else {
		sup.IdleTTL = *idle
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           NewServer(sup, reg, env, lg).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go sup.ReapLoop(ctx, func(name string, idle time.Duration) {
		lg.Printf("reaped %s after %s idle", name, idle.Round(time.Second))
	})

	lg.Printf("servicectl on http://%s", *addr)
	lg.Printf("backend root   %s", root)
	if files := env.Files(); len(files) == 0 {
		lg.Printf("global env     NONE FOUND -- every service will fall back to its own defaults")
	} else {
		lg.Printf("global env     %s", strings.Join(files, ", "))
	}
	lg.Printf("db provider    %s", env.Provider())
	lg.Printf("registry       %d services, %d console routes", len(reg.All()), len(reg.Pages()))
	if sup.IdleTTL > 0 {
		lg.Printf("idle reap      after %s", sup.IdleTTL)
	} else {
		lg.Printf("idle reap      disabled")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		lg.Printf("listen failed: %v", err)
		return 1
	case <-ctx.Done():
	}

	lg.Printf("shutting down; stopping every service it started")
	stopped, refused := sup.StopAll()
	lg.Printf("stopped %d service(s)", len(stopped))
	if len(refused) > 0 {
		// Adopted listeners. Named rather than counted, because they are still
		// serving and the reader has to go and find them.
		lg.Printf("STILL RUNNING (adopted, not ours to stop): %s", strings.Join(refused, ", "))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	return 0
}

// cmdStart runs services in the foreground without a daemon. Useful on its own,
// and the reason the supervisor is a library rather than something welded to the
// HTTP handlers.
func cmdStart(root string, reg *Registry, env *GlobalEnv, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "start: name at least one service or /admin/page")
		return 2
	}
	names, unknown := reg.Resolve(args)
	for _, u := range unknown {
		fmt.Fprintf(os.Stderr, "unknown: %s\n", u)
	}
	if len(names) == 0 {
		return 1
	}

	sup := NewSupervisor(reg, env, root)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("starting %d service(s) with provider %s\n\n", len(names), env.Provider())
	results := sup.Ensure(ctx, names, true)
	failed := printResults(results)

	if failed == len(results) {
		return 1
	}
	fmt.Printf("\nrunning. Ctrl-C to stop.\n")
	<-ctx.Done()
	fmt.Printf("\nstopping...\n")
	if _, refused := sup.StopAll(); len(refused) > 0 {
		fmt.Printf("still running (adopted, not ours to stop): %s\n", strings.Join(refused, ", "))
	}
	return 0
}

func printResults(results []Result) int {
	failed := 0
	for _, r := range results {
		switch r.State {
		case StateReady:
			note := ""
			if r.Built {
				note = " (built)"
			}
			if r.Adopted {
				note = " (adopted an existing listener)"
			}
			if r.Existing {
				note = " (already up)"
			}
			fmt.Printf("  ok      %-34s :%d  %dms%s\n", r.Name, r.Port, r.TookMS, note)
		case StateFailed:
			failed++
			fmt.Printf("  FAILED  %-34s :%d  %s\n", r.Name, r.Port, r.Error)
			if r.LogTail != "" {
				for _, line := range strings.Split(r.LogTail, "\n") {
					fmt.Printf("            | %s\n", line)
				}
			}
		default:
			fmt.Printf("  %-7s %-34s :%d\n", r.State, r.Name, r.Port)
		}
	}
	return failed
}

// cmdStop talks to a running daemon; there is nothing for it to stop otherwise,
// because a foreground `start` owns its own children and ends with Ctrl-C.
func cmdStop(args []string) int {
	url := "http://" + defaultAddr + "/v1/stop"
	if len(args) == 1 && (args[0] == "--all" || args[0] == "-all") {
		url += "?all=true"
	} else if len(args) > 0 {
		url += "?services=" + strings.Join(args, ",")
	} else {
		fmt.Fprintln(os.Stderr, "stop: name services, or --all")
		return 2
	}
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no daemon on %s: %v\n", defaultAddr, err)
		return 1
	}
	defer resp.Body.Close()
	io := make([]byte, 4096)
	n, _ := resp.Body.Read(io)
	fmt.Print(string(io[:n]))
	return 0
}

func cmdBuild(root string, reg *Registry, env *GlobalEnv, args []string) int {
	names := args
	if len(names) == 0 {
		for _, s := range reg.All() {
			names = append(names, s.Name)
		}
	} else {
		var unknown []string
		names, unknown = reg.Resolve(names)
		for _, u := range unknown {
			fmt.Fprintf(os.Stderr, "unknown: %s\n", u)
		}
	}

	sup := NewSupervisor(reg, env, root)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	built, cached, failed := 0, 0, 0
	start := time.Now()
	for i, n := range names {
		fmt.Printf("[%3d/%3d] %-34s ", i+1, len(names), n)
		t0 := time.Now()
		didBuild, err := sup.Build(ctx, n)
		switch {
		case err != nil:
			failed++
			fmt.Printf("FAILED\n%s\n", indent(err.Error(), "          "))
		case didBuild:
			built++
			fmt.Printf("built  %s\n", time.Since(t0).Round(time.Millisecond))
		default:
			cached++
			fmt.Printf("cached\n")
		}
		if ctx.Err() != nil {
			fmt.Println("interrupted")
			break
		}
	}
	fmt.Printf("\n%d built, %d already cached, %d failed in %s\n",
		built, cached, failed, time.Since(start).Round(time.Second))
	if failed > 0 {
		return 1
	}
	return 0
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func cmdStatus(reg *Registry, env *GlobalEnv, root string) int {
	resp, err := http.Get("http://" + defaultAddr + "/healthz")
	if err != nil {
		fmt.Printf("no daemon on %s\n\n", defaultAddr)
		fmt.Printf("backend root   %s\n", root)
		fmt.Printf("global env     %s\n", envFilesText(env))
		fmt.Printf("db provider    %s\n", env.Provider())
		fmt.Printf("registry       %d services, %d console routes\n", len(reg.All()), len(reg.Pages()))
		return 1
	}
	defer resp.Body.Close()
	buf := make([]byte, 1<<16)
	n, _ := resp.Body.Read(buf)
	fmt.Print(string(buf[:n]))
	return 0
}

func envFilesText(env *GlobalEnv) string {
	if f := env.Files(); len(f) > 0 {
		return strings.Join(f, ", ")
	}
	return "NONE FOUND"
}

func cmdEnv(reg *Registry, env *GlobalEnv, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "env: name exactly one service")
		return 2
	}
	svc, ok := reg.Get(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown service %q\n", args[0])
		return 1
	}
	composed := env.ServiceEnv(svc)

	// Only the variables this estate defines are printed. The inherited process
	// environment is in there too and dumping PATH and 40 Windows variables
	// would bury the six lines the reader came for.
	keys := make([]string, 0, len(composed))
	for k := range composed {
		if relevantKey(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	fmt.Printf("# %s -- port %d, database %q\n", svc.Name, svc.Port, svc.DBName)
	fmt.Printf("# provider %s, from %s\n", env.Provider(), envFilesText(env))
	fmt.Printf("# secrets masked; %d inherited process variables omitted\n\n",
		len(composed)-len(keys))
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, Mask(k, composed[k]))
	}
	return 0
}

// relevantKey matches the variables this platform defines, so `env` shows the
// service's configuration rather than the whole shell.
var relevantKey = regexp.MustCompile(
	`^(PORT|DB_|DATABASE_URL|REDIS_|KAFKA_|JWT_|ZOIKO_|SUPABASE_|OTEL_|AUTHZ_|ARGON2_|AUTH_|` +
		`APP_DB_|CLOSE_|GATEWAY_|VAULT_|DOCUMENT_VAULT_|SIEM_|IDP_|ENVELOPE_|SESSION_|ROLE_PROFILE_|` +
		`TENANT_|ACCESS_|DELEGATED_|JURISDICTION_|EMPLOYEE_|EMPLOYMENT_|EVIDENCE_|GENERAL_|LEDGER_|` +
		`AP_|AR_|GOVERNANCE_|POLICY_|OBLIGATIONS_|PURCHASE_|SPEND_|TAX_|WORKFLOW_|CARTA_|` +
		`COUNTERPARTY_|INTERCOMPANY_|IDENTITY_|AUTHORIZATION_|NOTIFICATION_|SEARCH_|MINIO_|S3_|` +
		`OPENSEARCH_|MTLS_|KEY_MANAGEMENT_|ZS_)`).MatchString

func cmdPorts(reg *Registry) int {
	fmt.Printf("# %d services, one port each.\n", len(reg.All()))
	fmt.Printf("# Ports are allocated here, not inherited: the compose files claimed\n")
	fmt.Printf("# sixteen of them twice across four files that never ran together.\n\n")
	for _, s := range reg.All() {
		db := s.DBName
		if db == "" {
			db = "-"
		}
		fmt.Printf("%-34s %d  %s\n", s.Name, s.Port, db)
	}
	return 0
}

func cmdPages(reg *Registry) int {
	pages := reg.Pages()
	routes := make([]string, 0, len(pages))
	for p := range pages {
		routes = append(routes, p)
	}
	sort.Strings(routes)
	fmt.Printf("# %d console routes. A route with no services reads no backend.\n\n", len(routes))
	for _, r := range routes {
		svcs := pages[r]
		if len(svcs) == 0 {
			fmt.Printf("%-26s -\n", r)
			continue
		}
		fmt.Printf("%-26s %s\n", r, strings.Join(svcs, ", "))
	}
	return 0
}
