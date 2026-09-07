// Process supervision: build a service, run it as a plain OS process, wait for
// it to answer its health probe, and stop it again when nothing is using it.
//
// There is no container anywhere in this file. A Go service is a single static
// binary that reads its configuration from the environment and listens on a
// port; the only things Docker was contributing to that were DNS names, which
// the registry has already resolved to 127.0.0.1, and a build step, which is
// `go build`.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is where a service is in its lifecycle.
type State string

const (
	StateStopped  State = "stopped"
	StateBuilding State = "building"
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateFailed   State = "failed"
)

// logLines is how much of each service's output is kept in memory. A service
// that fails at startup says why in its first dozen lines, and the whole point
// of keeping any is so the dashboard can show that without a terminal.
const logLines = 300

type proc struct {
	svc      *Service
	state    State
	since    time.Time
	lastUsed time.Time
	err      string
	pid      int

	cmd *exec.Cmd
	// dead is closed when the child exits. Health-waiting races against it, so a
	// service that fails its startup checks -- an unreachable database is the
	// common one -- reports in the hundreds of milliseconds it actually took
	// instead of sitting out the full start timeout.
	dead     chan struct{}
	exitErr  error
	deadOnce sync.Once

	// adopted means this listener was already running when we found it, so we
	// have no child handle and cannot stop it. Tracked because Stop used to
	// report success for these while the process kept serving -- a false
	// success, and the worst kind: the caller believes the port is free.
	adopted bool

	logs []string
	mu   sync.Mutex // guards logs only; Supervisor.mu guards the rest
}

func (p *proc) appendLog(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logs = append(p.logs, line)
	if len(p.logs) > logLines {
		p.logs = p.logs[len(p.logs)-logLines:]
	}
}

func (p *proc) snapshotLogs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.logs...)
}

// Supervisor owns every running service.
type Supervisor struct {
	reg  *Registry
	env  *GlobalEnv
	root string // backend root, the directory holding services/
	bin  string // where built binaries are cached

	// IdleTTL stops a service that nothing has asked for in this long. Zero
	// disables reaping. This is the half of "only runs when I am on that page"
	// that is easy to forget: without it, visiting every page once leaves all
	// 86 running.
	IdleTTL time.Duration

	// StartTimeout bounds build-plus-boot for one service.
	StartTimeout time.Duration

	mu    sync.Mutex
	procs map[string]*proc
}

func NewSupervisor(reg *Registry, env *GlobalEnv, root string) *Supervisor {
	return &Supervisor{
		reg:          reg,
		env:          env,
		root:         root,
		bin:          filepath.Join(root, ".servicectl", "bin"),
		IdleTTL:      20 * time.Minute,
		StartTimeout: 90 * time.Second,
		procs:        map[string]*proc{},
	}
}

// Status is one service's externally visible state.
type Status struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	State    State  `json:"state"`
	PID      int    `json:"pid,omitempty"`
	Since    string `json:"since,omitempty"`
	LastUsed string `json:"lastUsed,omitempty"`
	Error    string `json:"error,omitempty"`
	URL      string `json:"url"`
	DBName   string `json:"dbName,omitempty"`
	Adopted  bool   `json:"adopted,omitempty"`
}

func (s *Supervisor) statusLocked(svc *Service) Status {
	st := Status{
		Name:   svc.Name,
		Port:   svc.Port,
		State:  StateStopped,
		URL:    fmt.Sprintf("http://127.0.0.1:%d", svc.Port),
		DBName: svc.DBName,
	}
	if p, ok := s.procs[svc.Name]; ok {
		st.State, st.Error, st.PID, st.Adopted = p.state, p.err, p.pid, p.adopted
		if !p.since.IsZero() {
			st.Since = p.since.UTC().Format(time.RFC3339)
		}
		if !p.lastUsed.IsZero() {
			st.LastUsed = p.lastUsed.UTC().Format(time.RFC3339)
		}
	}
	return st
}

// Statuses returns every service's state, name-ordered.
func (s *Supervisor) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.reg.All()))
	for _, svc := range s.reg.All() {
		out = append(out, s.statusLocked(svc))
	}
	return out
}

// Logs returns the retained output for one service.
func (s *Supervisor) Logs(name string) ([]string, bool) {
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	return p.snapshotLogs(), true
}

// binaryPath is where a service's compiled binary lives.
func (s *Supervisor) binaryPath(name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(s.bin, name+".exe")
	}
	return filepath.Join(s.bin, name)
}

// needsBuild reports whether the cached binary is missing or older than any Go
// source or go.mod in the service's module.
//
// Timestamps rather than content hashes: this runs on every start request, and
// walking one module's tree costs a few milliseconds where hashing it costs
// hundreds. The failure mode of a timestamp check is a needless rebuild, which
// `go build` then makes cheap anyway through its own cache.
func (s *Supervisor) needsBuild(name string) (bool, string, error) {
	bin := s.binaryPath(name)
	fi, err := os.Stat(bin)
	if err != nil {
		if os.IsNotExist(err) {
			return true, "no cached binary", nil
		}
		return true, "cannot stat cached binary", err
	}
	builtAt := fi.ModTime()

	dir := filepath.Join(s.root, "services", name)
	newer := ""
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not a reason to refuse to run
		}
		if d.IsDir() {
			if n := d.Name(); n == "testdata" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && d.Name() != "go.mod" && d.Name() != "go.sum" {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(builtAt) {
			newer = path
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return true, "cannot walk source tree", walkErr
	}
	if newer != "" {
		rel, _ := filepath.Rel(s.root, newer)
		return true, "newer source: " + filepath.ToSlash(rel), nil
	}
	return false, "", nil
}

// Build compiles one service into the binary cache. Safe to call when nothing
// has changed: it returns immediately.
func (s *Supervisor) Build(ctx context.Context, name string) (built bool, err error) {
	svc, ok := s.reg.Get(name)
	if !ok {
		return false, fmt.Errorf("unknown service %q", name)
	}
	need, why, err := s.needsBuild(svc.Name)
	if err != nil {
		return false, err
	}
	if !need {
		return false, nil
	}
	if err := os.MkdirAll(s.bin, 0o755); err != nil {
		return false, fmt.Errorf("creating binary cache: %w", err)
	}

	dir := filepath.Join(s.root, "services", svc.Name)
	cmd := exec.CommandContext(ctx, "go", "build", "-o", s.binaryPath(svc.Name), "./cmd/server")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The compiler's own message is the whole diagnosis, so it is returned
		// rather than summarised.
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, fmt.Errorf("building %s (%s): %s", svc.Name, why, msg)
	}
	return true, nil
}

// portBusy reports whether something is already listening on a port. Checked
// before starting, because the alternative is a service that exits immediately
// with a bind error that only appears in its log.
func portBusy(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// healthPaths are probed in order. 75 services expose /healthz and
// identity-context-svc exposes /health, so both are tried rather than making
// the registry carry a per-service path that is right 75 times out of 76.
var healthPaths = []string{"/healthz", "/health"}

// waitHealthy polls until the service answers, the context expires, or dead is
// closed. dead may be nil when there is no child of ours to watch, which is the
// case when another request is already starting the service.
func waitHealthy(ctx context.Context, port int, dead <-chan struct{}) error {
	client := &http.Client{Timeout: 2 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	var last error
	for {
		select {
		case <-dead:
			// The process is gone, so no amount of further polling will help.
			// Its own log says why; this only has to stop waiting.
			return errors.New("process exited during startup")
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("health probe never succeeded: %w", last)
			}
			return errors.New("health probe never succeeded")
		default:
		}
		for _, path := range healthPaths {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				last = err
				continue
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			last = fmt.Errorf("%s answered %d", path, resp.StatusCode)
		}
		select {
		case <-ctx.Done():
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// Result is the outcome of one Ensure for one service.
type Result struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	State    State  `json:"state"`
	URL      string `json:"url"`
	Built    bool   `json:"built,omitempty"`
	Started  bool   `json:"started,omitempty"`
	TookMS   int64  `json:"tookMs"`
	Error    string `json:"error,omitempty"`
	LogTail  string `json:"logTail,omitempty"`
	Adopted  bool   `json:"adopted,omitempty"`
	Existing bool   `json:"existing,omitempty"`
}

// Ensure brings the named services up and, when wait is true, does not return
// until each is answering its health probe or has failed.
//
// Services are started concurrently. A page needing seven of them starts them in
// parallel and waits once, rather than paying seven boots in series.
func (s *Supervisor) Ensure(ctx context.Context, names []string, wait bool) []Result {
	results := make([]Result, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i] = s.ensureOne(ctx, name, wait)
		}(i, name)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results
}

// The return value is NAMED so the deferred timing assignment lands on the value
// the caller receives. With a local variable it wrote to a copy that had already
// been returned, and every duration reported as 0ms.
func (s *Supervisor) ensureOne(ctx context.Context, name string, wait bool) (res Result) {
	start := time.Now()
	svc, ok := s.reg.Get(name)
	if !ok {
		return Result{Name: name, State: StateFailed, Error: "unknown service"}
	}
	res = Result{Name: svc.Name, Port: svc.Port, URL: fmt.Sprintf("http://127.0.0.1:%d", svc.Port)}
	defer func() { res.TookMS = time.Since(start).Milliseconds() }()

	// Already ours and already up: just refresh the idle clock.
	s.mu.Lock()
	if p, exists := s.procs[svc.Name]; exists {
		switch p.state {
		case StateReady:
			p.lastUsed = time.Now()
			s.mu.Unlock()
			res.State, res.Existing = StateReady, true
			return res
		case StateBuilding, StateStarting:
			p.lastUsed = time.Now()
			s.mu.Unlock()
			if !wait {
				res.State = StateStarting
				return res
			}
			// Another request is already bringing it up; wait on its child so a
			// failure there ends this wait too rather than timing out twice.
			hctx, cancel := context.WithTimeout(ctx, s.StartTimeout)
			defer cancel()
			if err := waitHealthy(hctx, svc.Port, p.dead); err != nil {
				res.State, res.Error = StateFailed, err.Error()
				return res
			}
			s.mu.Lock()
			if p2, ok := s.procs[svc.Name]; ok {
				p2.state, p2.lastUsed = StateReady, time.Now()
			}
			s.mu.Unlock()
			res.State = StateReady
			return res
		}
	}
	s.mu.Unlock()

	// Something else is on the port. That is not ours to kill: it could be the
	// service already running from a terminal, in which case adopting it is the
	// right answer and restarting it is not.
	if portBusy(svc.Port) {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := waitHealthy(hctx, svc.Port, nil); err == nil {
			s.mu.Lock()
			s.procs[svc.Name] = &proc{
				svc: svc, state: StateReady, since: time.Now(), lastUsed: time.Now(),
				adopted: true,
				logs: []string{"[servicectl] adopted an already-healthy listener on this port; " +
					"this launcher did not start it and cannot stop it"},
			}
			s.mu.Unlock()
			res.State, res.Adopted = StateReady, true
			return res
		}
		res.State = StateFailed
		res.Error = fmt.Sprintf("port %d is in use by something that is not answering a health probe", svc.Port)
		return res
	}

	p := &proc{svc: svc, state: StateBuilding, since: time.Now(), lastUsed: time.Now()}
	s.mu.Lock()
	s.procs[svc.Name] = p
	s.mu.Unlock()

	bctx, bcancel := context.WithTimeout(ctx, s.StartTimeout)
	defer bcancel()
	built, err := s.Build(bctx, svc.Name)
	res.Built = built
	if err != nil {
		s.fail(svc.Name, err.Error())
		res.State, res.Error = StateFailed, err.Error()
		return res
	}

	if err := s.spawn(p); err != nil {
		s.fail(svc.Name, err.Error())
		res.State, res.Error = StateFailed, err.Error()
		res.LogTail = tail(p.snapshotLogs(), 12)
		return res
	}
	res.Started = true

	s.mu.Lock()
	p.state = StateStarting
	s.mu.Unlock()

	if !wait {
		res.State = StateStarting
		return res
	}

	hctx, hcancel := context.WithTimeout(ctx, s.StartTimeout)
	defer hcancel()
	if err := waitHealthy(hctx, svc.Port, p.dead); err != nil {
		// The service's own log says why far better than the probe does -- a
		// fatal on an unreachable database is one line in it and an opaque
		// connection-refused here.
		s.fail(svc.Name, err.Error())
		res.State, res.Error = StateFailed, err.Error()
		res.LogTail = tail(p.snapshotLogs(), 12)
		return res
	}

	s.mu.Lock()
	p.state, p.lastUsed = StateReady, time.Now()
	s.mu.Unlock()
	res.State = StateReady
	return res
}

// note records a line in the service's own log buffer, so anything the launcher
// does on a service's behalf shows up alongside that service's output rather
// than only in the daemon's terminal.
func (s *Supervisor) note(name, msg string) {
	s.mu.Lock()
	p, ok := s.procs[name]
	s.mu.Unlock()
	if ok {
		p.appendLog("[servicectl] " + msg)
	}
}

func (s *Supervisor) fail(name, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.procs[name]; ok {
		p.state, p.err = StateFailed, msg
	}
}

// spawn starts the built binary. The child gets ONLY the composed environment,
// never the launcher's own inherited one, so what a service sees is exactly what
// `servicectl env <svc>` prints.
func (s *Supervisor) spawn(p *proc) error {
	svc := p.svc
	env := s.env.ServiceEnv(svc)

	// Generated material the compose sidecars used to produce. Done here rather
	// than at setup so a service is startable from a clean checkout.
	if err := s.prepare(svc, env); err != nil {
		return err
	}

	cmd := exec.Command(s.binaryPath(svc.Name))
	cmd.Dir = filepath.Join(s.root, "services", svc.Name)
	cmd.Env = Environ(env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", svc.Name, err)
	}

	s.mu.Lock()
	p.cmd, p.pid = cmd, cmd.Process.Pid
	p.dead = make(chan struct{})
	s.mu.Unlock()

	go pump(p, stdout)
	go pump(p, stderr)

	go func() {
		waitErr := cmd.Wait()
		s.mu.Lock()
		p.exitErr = waitErr
		s.mu.Unlock()
		// Closed exactly once: Stop may also have terminated the process, and a
		// second close would panic.
		p.deadOnce.Do(func() { close(p.dead) })

		s.mu.Lock()
		defer s.mu.Unlock()
		cur, ok := s.procs[svc.Name]
		if !ok || cur != p {
			return // already replaced by a newer start
		}
		if p.state == StateStopped {
			return // we asked it to stop
		}
		p.state = StateFailed
		if waitErr != nil {
			p.err = "exited: " + waitErr.Error()
		} else {
			p.err = "exited cleanly without being asked to"
		}
	}()

	return nil
}

func pump(p *proc, r io.Reader) {
	buf := make([]byte, 8192)
	var partial string
	for {
		n, err := r.Read(buf)
		if n > 0 {
			partial += string(buf[:n])
			for {
				i := strings.IndexByte(partial, '\n')
				if i < 0 {
					break
				}
				p.appendLog(strings.TrimRight(partial[:i], "\r"))
				partial = partial[i+1:]
			}
			// A single unterminated line longer than the buffer would otherwise
			// grow without bound.
			if len(partial) > 64*1024 {
				p.appendLog(partial)
				partial = ""
			}
		}
		if err != nil {
			if partial != "" {
				p.appendLog(partial)
			}
			return
		}
	}
}

func tail(lines []string, n int) string {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Stop terminates one service.
func (s *Supervisor) Stop(name string) error {
	s.mu.Lock()
	p, ok := s.procs[name]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	cmd := p.cmd
	adopted := p.adopted
	p.state = StateStopped
	p.err = ""
	delete(s.procs, name)
	s.mu.Unlock()

	// An adopted listener has no child handle here. Saying nothing would report
	// a stop that did not happen and leave the caller believing the port is
	// free; the honest answer names where it has to be stopped instead.
	if adopted {
		return fmt.Errorf("%s was adopted, not started by this launcher -- stop it where it was started (it is still serving on port %d)",
			name, p.svc.Port)
	}

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// The child is the service binary itself, started without a shell, so it has
	// no descendants of its own and killing the process is killing the tree.
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stopping %s: %w", name, err)
	}
	return nil
}

// StopAll terminates every service this launcher started and returns the names
// it ACTUALLY stopped, plus anything it could not.
//
// The two lists are separate because they were not: returning every name it
// tried claimed adopted listeners had been stopped, which on shutdown printed a
// tidy count while processes carried on serving.
func (s *Supervisor) StopAll() (stopped []string, refused []string) {
	s.mu.Lock()
	names := make([]string, 0, len(s.procs))
	for n := range s.procs {
		names = append(names, n)
	}
	s.mu.Unlock()

	sort.Strings(names)
	for _, n := range names {
		if err := s.Stop(n); err != nil {
			refused = append(refused, n)
			continue
		}
		stopped = append(stopped, n)
	}
	return stopped, refused
}

// Running returns the names of services currently up.
func (s *Supervisor) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n, p := range s.procs {
		if p.state == StateReady || p.state == StateStarting || p.state == StateBuilding {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ReapLoop stops services nothing has asked for in IdleTTL. This is the other
// half of starting on demand: without it, visiting every page once leaves all 86
// services running for the rest of the session.
func (s *Supervisor) ReapLoop(ctx context.Context, onReap func(string, time.Duration)) {
	if s.IdleTTL <= 0 {
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			s.mu.Lock()
			var due []string
			for name, p := range s.procs {
				if p.state == StateReady && !p.adopted && now.Sub(p.lastUsed) > s.IdleTTL {
					due = append(due, name)
				}
			}
			s.mu.Unlock()
			sort.Strings(due)
			for _, n := range due {
				s.mu.Lock()
				idle := time.Duration(0)
				if p, ok := s.procs[n]; ok {
					idle = now.Sub(p.lastUsed)
				}
				s.mu.Unlock()
				if err := s.Stop(n); err == nil && onReap != nil {
					onReap(n, idle)
				}
			}
		}
	}
}
