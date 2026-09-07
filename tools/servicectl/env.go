// The global environment: ONE file for the whole backend.
//
// Compose gave every service its own environment block, which meant the database
// password appeared 46 times and the Kafka broker 78. That duplication is what
// this file removes. There is now one env file; a service's own block carries
// only what is genuinely specific to it (its Kafka topic, the peers it calls),
// and everything shared is resolved here, once, at launch.
package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// GlobalEnv is the parsed global env file set.
type GlobalEnv struct {
	vals   map[string]string
	loaded []string // files actually read, in precedence order (last wins)
}

// managedKeys are computed per service and must never be inherited from the
// global file or from the launcher's own process environment. PORT is the reason
// this list exists: a stray PORT in the shell would otherwise be handed to all
// 86 services at once, and 85 of them would fail to bind with no obvious cause.
var managedKeys = map[string]bool{
	"PORT": true, "DB_NAME": true, "DB_SCHEMA": true, "DATABASE_URL": true,
	"DB_HOST": true, "DB_PORT": true, "DB_USER": true, "DB_PASSWORD": true,
	"DB_SSLMODE": true, "DB_OPTIONS": true,
}

// secretMarkers drive masking when an environment is printed. Substring
// matching, so it errs toward masking: a SUPABASE_SERVICE_ROLE_KEY added later
// is hidden the day it appears rather than the day someone remembers to list it.
var secretMarkers = []string{
	"PASSWORD", "SECRET", "TOKEN", "KEY", "CREDENTIAL", "DSN", "REDIS_URL", "DATABASE_URL",
}

func secretish(key string) bool {
	k := strings.ToUpper(key)
	// A key id names a key, it is not one, and masking it makes the output
	// useless for checking which signing key a service came up with.
	if strings.HasSuffix(k, "_KEY_ID") {
		return false
	}
	for _, m := range secretMarkers {
		if strings.Contains(k, m) {
			return true
		}
	}
	return false
}

// Mask renders a value safe to print while keeping its shape: the length, and
// for a URL everything except the password. "Is it set, and is it the one I
// think it is" is the question this output has to answer.
//
// The URL check runs FIRST and ignores the key name, because the key name is not
// a reliable signal. SUPABASE_DB_URL matches none of the markers above and
// carries a live database password; keying the decision off the name printed it
// in full.
func Mask(key, val string) string {
	if val == "" {
		return val
	}
	if masked, ok := maskURLPassword(val); ok {
		return masked
	}
	if !secretish(key) {
		return val
	}
	return fmt.Sprintf("****(%d chars)", len(val))
}

// maskURLPassword replaces the password in a URL's userinfo, leaving the scheme,
// user, host, path and query readable.
//
// The mask is spliced into the rendered string rather than passed through
// url.UserPassword, because URL encoding turns "****" into "%2A%2A%2A%2A" -- which
// reads like a real value and is the opposite of what a mask is for.
func maskURLPassword(val string) (string, bool) {
	u, err := url.Parse(val)
	if err != nil || u.Scheme == "" || u.User == nil {
		return "", false
	}
	pw, hasPassword := u.User.Password()
	if !hasPassword || pw == "" {
		return "", false
	}
	u.User = url.User(u.User.Username())
	s := u.String()
	i := strings.Index(s, "://")
	if i < 0 {
		return "", false
	}
	j := strings.Index(s[i+3:], "@")
	if j < 0 {
		return "", false
	}
	cut := i + 3 + j
	return s[:cut] + ":****" + s[cut:], true
}

// LoadGlobalEnv reads the given env files in order; later files override earlier
// ones. A missing file is skipped rather than fatal, so a machine can run on
// .env alone, or on .env plus a personal .env.local, with no flag change.
func LoadGlobalEnv(paths ...string) (*GlobalEnv, error) {
	g := &GlobalEnv{vals: map[string]string{}}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			t := strings.TrimSpace(sc.Text())
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			t = strings.TrimPrefix(t, "export ")
			eq := strings.IndexByte(t, '=')
			if eq <= 0 {
				continue
			}
			k := strings.TrimSpace(t[:eq])
			g.vals[k] = unquote(strings.TrimSpace(t[eq+1:]))
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		g.loaded = append(g.loaded, p)
	}
	return g, nil
}

// unquote strips one layer of matching quotes. Values are otherwise taken
// literally: there is no ${VAR} expansion, because compose substitution syntax
// appearing in an env file would silently become the literal string.
func unquote(v string) string {
	const singleQuote = '\''
	if len(v) < 2 {
		return v
	}
	first, last := rune(v[0]), rune(v[len(v)-1])
	if (first == '"' && last == '"') || (first == singleQuote && last == singleQuote) {
		return v[1 : len(v)-1]
	}
	return v
}

// Files lists the env files that were actually found.
func (g *GlobalEnv) Files() []string { return g.loaded }

// Get returns a global value, or def when it is unset or empty.
func (g *GlobalEnv) Get(key, def string) string {
	if v, ok := g.vals[key]; ok && v != "" {
		return v
	}
	return def
}

// Provider is the selected database backend, from ZOIKO_DB_PROVIDER. The name
// and the accepted values match .env.example, which already documented this
// selector before there was a launcher to read it.
func (g *GlobalEnv) Provider() string {
	switch p := strings.ToLower(g.Get("ZOIKO_DB_PROVIDER", "docker")); p {
	case "supabase", "docker", "local":
		return p
	default:
		return "docker"
	}
}

// dbSettings resolves the shared database connection for the active provider.
// schema is empty for a database-per-service host and set for a managed
// single-database one.
func (g *GlobalEnv) dbSettings(svc *Service) (host, port, user, pass, sslmode, dbname, schema, options string) {
	switch g.Provider() {
	case "supabase":
		host = g.Get("SUPABASE_DB_HOST", "")
		port = g.Get("SUPABASE_DB_PORT", "6543")
		// EACH SERVICE CONNECTS AS ITS OWN ROLE. On a single-database host the
		// only thing separating one service's data from another's is that its
		// role holds USAGE on its own schema and no other; a shared login would
		// give every service reach into every schema and leave the schemas as
		// naming convention rather than isolation.
		//
		// SUPABASE_DB_USER overrides this for the whole estate. It is an escape
		// hatch for a host provisioned with one role, and using it gives up the
		// separation above -- which is why the per-service role is the default
		// and not the other way round.
		user = g.Get("SUPABASE_DB_USER", svc.DBRole)
		if user == "" {
			user = "postgres"
		}
		// Pooler usernames are <role>.<project_ref>; a bare role name is
		// rejected as an authentication failure, so the ref is appended here
		// rather than left to whoever fills in the env file.
		if ref := g.Get("SUPABASE_PROJECT_REF", ""); ref != "" && !strings.Contains(user, ".") {
			user = user + "." + ref
		}
		pass = g.Get("APP_DB_PASSWORD", g.Get("SUPABASE_DB_PASSWORD", ""))
		sslmode = g.Get("SUPABASE_DB_SSLMODE", "require")
		// Supabase has ONE database and it is called postgres. Per-service
		// separation is by schema, so the service's logical database name
		// becomes its search_path instead of its dbname.
		dbname = g.Get("SUPABASE_DB_NAME", "postgres")
		schema = svc.DBName
		// Required on the transaction pooler: pgx's prepared-statement cache
		// does not survive it, and without this you get "prepared statement
		// already exists" intermittently, under concurrency, after every smoke
		// test has passed.
		options = g.Get("DB_OPTIONS", "default_query_exec_mode=exec statement_cache_capacity=0")
	default:
		host = g.Get("DB_HOST", g.Get("POSTGRES_HOST", "127.0.0.1"))
		port = g.Get("DB_PORT", "5432")
		user = g.Get("DB_USER", g.Get("POSTGRES_USER", "postgres"))
		pass = g.Get("DB_PASSWORD", g.Get("POSTGRES_PASSWORD", "postgres"))
		sslmode = g.Get("DB_SSLMODE", "disable")
		dbname = svc.DBName
		options = g.Get("DB_OPTIONS", "")
	}
	return
}

// ServiceEnv composes the full environment for one service.
//
// Precedence, lowest to highest:
//  1. the launcher's own process environment, minus every managed key
//  2. the global env file set
//  3. the service's own block from the registry
//  4. computed values: PORT, and the database wiring for the active provider
//
// The service block sits ABOVE the global file so a service can override a
// shared default, and the computed values sit above everything, because a
// service that does not bind its allocated port is not reachable at all.
func (g *GlobalEnv) ServiceEnv(svc *Service) map[string]string {
	env := map[string]string{}

	for _, kv := range os.Environ() {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			if k := kv[:eq]; !managedKeys[k] {
				env[k] = kv[eq+1:]
			}
		}
	}
	for k, v := range g.vals {
		if !managedKeys[k] {
			env[k] = v
		}
	}
	for k, v := range svc.Env {
		env[k] = v
	}

	env["PORT"] = strconv.Itoa(svc.Port)

	// Messaging and tracing come from the global file. Neither has to be running:
	// kafka-go dials lazily, and the OTLP exporter never dials at construction,
	// so an unreachable endpoint costs a log line rather than a startup.
	env["KAFKA_BROKERS"] = g.Get("KAFKA_BROKERS", "127.0.0.1:9092")

	// THE DEFAULT HERE CANNOT BE EMPTY. 41 services declare
	// env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"), and their
	// env() helper treats an empty value as unset -- so handing them "" does not
	// disable tracing, it restores the Docker DNS name. The observable result was
	// every service logging `lookup otel-collector: no such host` every five
	// seconds, forever, on a machine with no Docker.
	//
	// Loopback instead: a local collector on 4318 just works, and without one the
	// failure is an immediate connection refused rather than a DNS timeout.
	// OTEL_SDK_DISABLED does not help -- InitTracing builds the exporter and
	// tracer provider by hand, so the SDK's own kill switch never sees it.
	env["OTEL_EXPORTER_OTLP_ENDPOINT"] = g.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")

	if svc.DBName == "" {
		return env
	}

	host, port, user, pass, sslmode, dbname, schema, options := g.dbSettings(svc)
	env["DB_HOST"] = host
	env["DB_PORT"] = port
	env["DB_USER"] = user
	env["DB_PASSWORD"] = pass
	env["DB_SSLMODE"] = sslmode
	env["DB_NAME"] = dbname
	if schema != "" {
		env["DB_SCHEMA"] = schema
	}
	if options != "" {
		env["DB_OPTIONS"] = options
	}

	// 34 services read a single DATABASE_URL rather than the discrete parts, and
	// a few read both. Both forms are emitted from the same resolved values so
	// they cannot disagree -- which they could under compose, where DATABASE_URL
	// was a separate hand-written literal per service.
	q := url.Values{}
	if sslmode != "" {
		q.Set("sslmode", sslmode)
	}
	if schema != "" {
		q.Set("search_path", schema)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     host + ":" + port,
		Path:     "/" + dbname,
		RawQuery: q.Encode(),
	}
	env["DATABASE_URL"] = u.String()

	return env
}

// Environ renders a composed environment for exec.Cmd.
func Environ(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// DefaultEnvFiles are the global env files in precedence order. .env holds the
// deployed-shape configuration and .env.local the developer's overrides, which
// is how the two are already documented in this repository.
func DefaultEnvFiles(backendRoot string) []string {
	return []string{
		filepath.Join(backendRoot, ".env"),
		filepath.Join(backendRoot, ".env.local"),
	}
}
