// The HTTP surface: the API the console calls on navigation, and a dashboard for
// driving the estate by hand.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Server is the launcher's HTTP surface.
type Server struct {
	sup *Supervisor
	reg *Registry
	env *GlobalEnv
	log *log.Logger
}

func NewServer(sup *Supervisor, reg *Registry, env *GlobalEnv, lg *log.Logger) *Server {
	return &Server{sup: sup, reg: reg, env: env, log: lg}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/services", s.handleServices)
	mux.HandleFunc("GET /v1/pages", s.handlePages)
	mux.HandleFunc("POST /v1/ensure", s.handleEnsure)
	mux.HandleFunc("POST /v1/stop", s.handleStop)
	mux.HandleFunc("GET /v1/logs", s.handleLogs)
	mux.HandleFunc("GET /v1/env", s.handleEnv)
	mux.HandleFunc("GET /", s.handleDashboard)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"services": len(s.reg.All()),
		"running":  s.sup.Running(),
		"provider": s.env.Provider(),
		"envFiles": s.env.Files(),
	})
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"services": s.sup.Statuses()})
}

func (s *Server) handlePages(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"pages": s.reg.Pages()})
}

type ensureRequest struct {
	// Page is a console route: /admin/finance. Resolved through the generated
	// page map to the services that route reads.
	Page string `json:"page,omitempty"`
	// Services names services directly, for the dashboard and the CLI.
	Services []string `json:"services,omitempty"`
	// Wait, when false, starts everything and returns immediately. The console's
	// middleware sets it true, because a page that renders before its services
	// are listening shows empty panels and a reload is the only way back.
	Wait *bool `json:"wait,omitempty"`
	// TimeoutMS bounds the wait. Zero uses the server default.
	TimeoutMS int `json:"timeoutMs,omitempty"`
}

func (s *Server) handleEnsure(w http.ResponseWriter, r *http.Request) {
	var req ensureRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed JSON body: " + err.Error()})
			return
		}
	}
	// Query parameters are accepted too, so the dashboard and curl need no body.
	if p := r.URL.Query().Get("page"); p != "" {
		req.Page = p
	}
	if sv := r.URL.Query().Get("services"); sv != "" {
		req.Services = append(req.Services, strings.Split(sv, ",")...)
	}
	if wq := r.URL.Query().Get("wait"); wq != "" {
		b, err := strconv.ParseBool(wq)
		if err == nil {
			req.Wait = &b
		}
	}

	items := append([]string(nil), req.Services...)
	if req.Page != "" {
		items = append(items, req.Page)
	}
	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "give a page or a service list",
		})
		return
	}

	names, unknown := s.reg.Resolve(items)

	// A page that maps to no service is a normal outcome, not an error: two
	// console routes genuinely read no backend at all. Answering 200 with an
	// empty list keeps the caller's code the same for both.
	wait := true
	if req.Wait != nil {
		wait = *req.Wait
	}
	timeout := s.sup.StartTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	started := time.Now()
	results := s.sup.Ensure(ctx, names, wait)

	ready, failed := 0, 0
	for _, res := range results {
		switch res.State {
		case StateReady:
			ready++
		case StateFailed:
			failed++
		}
	}
	if len(names) > 0 {
		s.log.Printf("ensure %s -> %d requested, %d ready, %d failed in %s",
			describe(req.Page, names), len(names), ready, failed, time.Since(started).Round(time.Millisecond))
	}

	code := http.StatusOK
	if failed > 0 {
		// 207: some services came up and some did not, and the body says which.
		// A blanket 500 would make the console treat a single dead service as a
		// dead launcher and stop asking.
		code = http.StatusMultiStatus
	}
	writeJSON(w, code, map[string]any{
		"page":     req.Page,
		"waited":   wait,
		"ready":    ready,
		"failed":   failed,
		"unknown":  unknown,
		"services": results,
	})
}

func describe(page string, names []string) string {
	if page != "" {
		return page
	}
	if len(names) <= 3 {
		return strings.Join(names, ",")
	}
	return fmt.Sprintf("%s +%d more", strings.Join(names[:3], ","), len(names)-3)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Services []string `json:"services,omitempty"`
		All      bool     `json:"all,omitempty"`
	}
	if r.ContentLength != 0 {
		json.NewDecoder(r.Body).Decode(&req)
	}
	if r.URL.Query().Get("all") == "true" {
		req.All = true
	}
	if sv := r.URL.Query().Get("services"); sv != "" {
		req.Services = append(req.Services, strings.Split(sv, ",")...)
	}

	if req.All {
		stopped, refused := s.sup.StopAll()
		s.log.Printf("stopped all: %d stopped, %d could not be stopped", len(stopped), len(refused))
		writeJSON(w, http.StatusOK, map[string]any{
			"stopped": stopped,
			// Adopted listeners this launcher did not start. Reported separately
			// so a caller is not told a port is free when it is not.
			"refused": refused,
		})
		return
	}
	names, unknown := s.reg.Resolve(req.Services)
	var stopped []string
	for _, n := range names {
		if err := s.sup.Stop(n); err != nil {
			s.log.Printf("stop %s: %v", n, err)
			continue
		}
		stopped = append(stopped, n)
	}
	s.log.Printf("stopped %d service(s)", len(stopped))
	writeJSON(w, http.StatusOK, map[string]any{"stopped": stopped, "unknown": unknown})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("service")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "service is required"})
		return
	}
	if _, ok := s.reg.Get(name); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown service " + name})
		return
	}
	lines, running := s.sup.Logs(name)
	writeJSON(w, http.StatusOK, map[string]any{
		"service": name, "running": running, "lines": lines,
	})
}

// handleEnv shows the environment one service would receive, secrets masked. It
// exists because "which database did it actually connect to" is the first
// question when a service starts and behaves oddly, and the answer used to be
// spread over a compose file and four env files.
func (s *Server) handleEnv(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("service")
	svc, ok := s.reg.Get(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown service " + name})
		return
	}
	env := s.env.ServiceEnv(svc)
	masked := make(map[string]string, len(env))
	for k, v := range env {
		masked[k] = Mask(k, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": name, "provider": s.env.Provider(),
		"envFiles": s.env.Files(), "env": masked,
	})
}

// ── Dashboard ───────────────────────────────────────────────────────────────

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	statuses := s.sup.Statuses()
	byName := make(map[string]Status, len(statuses))
	for _, st := range statuses {
		byName[st.Name] = st
	}

	pages := s.reg.Pages()
	routes := make([]string, 0, len(pages))
	for p := range pages {
		routes = append(routes, p)
	}
	sort.Strings(routes)

	running := 0
	for _, st := range statuses {
		if st.State == StateReady {
			running++
		}
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8">
<title>servicectl</title><meta name="viewport" content="width=device-width,initial-scale=1">
<style>
:root{--bg:#0f1115;--fg:#e6e6e6;--dim:#8b93a1;--line:#242832;--card:#161922;
--ready:#3fb950;--fail:#f85149;--busy:#d29922;--stop:#4b525f;--link:#58a6ff}
@media(prefers-color-scheme:light){:root{--bg:#fff;--fg:#1b1f24;--dim:#5b6472;
--line:#e2e5ea;--card:#f7f8fa;--stop:#aeb6c2}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);
font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;padding:24px}
h1{font-size:16px;margin:0 0 4px}h2{font-size:13px;margin:28px 0 8px;color:var(--dim);
text-transform:uppercase;letter-spacing:.08em}
.sub{color:var(--dim);margin-bottom:20px;font-size:12px}
table{border-collapse:collapse;width:100%;font-size:12px}
td,th{text-align:left;padding:5px 10px;border-bottom:1px solid var(--line);vertical-align:top}
th{color:var(--dim);font-weight:400}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px}
.ready{background:var(--ready)}.failed{background:var(--fail)}
.starting,.building{background:var(--busy)}.stopped{background:var(--stop)}
a{color:var(--link)}button{font:inherit;font-size:11px;background:var(--card);
color:var(--fg);border:1px solid var(--line);border-radius:4px;padding:3px 9px;cursor:pointer}
button:hover{border-color:var(--link)}
.err{color:var(--fail);font-size:11px;white-space:pre-wrap;max-width:52ch}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:8px}
.card{background:var(--card);border:1px solid var(--line);border-radius:6px;padding:10px 12px}
.card b{font-weight:600}.card ul{margin:6px 0 8px;padding-left:16px;color:var(--dim);font-size:11px}
.bar{display:flex;gap:8px;align-items:center;margin-bottom:8px;flex-wrap:wrap}
</style></head><body>`)

	fmt.Fprintf(&b, `<h1>servicectl</h1><div class="sub">%d services &middot; %d ready &middot; db provider <b>%s</b> &middot; env: %s</div>`,
		len(statuses), running, html.EscapeString(s.env.Provider()),
		html.EscapeString(strings.Join(s.env.Files(), ", ")))

	b.WriteString(`<div class="bar">
<button onclick="act('/v1/stop?all=true')">stop all</button>
<button onclick="location.reload()">refresh</button>
<span class="sub" style="margin:0">auto-refreshes every 4s</span></div>`)

	b.WriteString(`<h2>Console routes</h2><div class="grid">`)
	for _, route := range routes {
		svcs := pages[route]
		ready := 0
		for _, n := range svcs {
			if byName[n].State == StateReady {
				ready++
			}
		}
		fmt.Fprintf(&b, `<div class="card"><b>%s</b> <span class="sub" style="margin:0">%d/%d up</span><ul>`,
			html.EscapeString(route), ready, len(svcs))
		if len(svcs) == 0 {
			b.WriteString(`<li>reads no backend service</li>`)
		}
		for _, n := range svcs {
			st := byName[n]
			fmt.Fprintf(&b, `<li><span class="dot %s"></span>%s</li>`,
				html.EscapeString(string(st.State)), html.EscapeString(n))
		}
		b.WriteString(`</ul>`)
		if len(svcs) > 0 {
			fmt.Fprintf(&b, `<button onclick="act('/v1/ensure?page=%s&wait=true')">start</button>
<button onclick="act('/v1/stop?services=%s')">stop</button>`,
				html.EscapeString(route), html.EscapeString(strings.Join(svcs, ",")))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)

	b.WriteString(`<h2>All services</h2><table><tr><th>state</th><th>service</th><th>port</th>
<th>database</th><th>pid</th><th></th><th>error</th></tr>`)
	for _, st := range statuses {
		fmt.Fprintf(&b, `<tr><td><span class="dot %s"></span>%s</td>
<td><a href="%s" target="_blank">%s</a></td><td>%d</td><td>%s</td><td>%s</td>
<td><button onclick="act('/v1/ensure?services=%s&wait=true')">start</button>
<button onclick="act('/v1/stop?services=%s')">stop</button>
<a href="/v1/logs?service=%s" target="_blank">logs</a>
<a href="/v1/env?service=%s" target="_blank">env</a></td>
<td class="err">%s</td></tr>`,
			html.EscapeString(string(st.State)), html.EscapeString(string(st.State)),
			html.EscapeString(st.URL), html.EscapeString(st.Name), st.Port,
			html.EscapeString(st.DBName), pidText(st.PID, st.Adopted),
			html.EscapeString(st.Name), html.EscapeString(st.Name),
			html.EscapeString(st.Name), html.EscapeString(st.Name),
			html.EscapeString(st.Error))
	}
	b.WriteString(`</table>
<script>
async function act(u){await fetch(u,{method:'POST'});location.reload()}
setTimeout(()=>location.reload(),4000)
</script></body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(b.String()))
}

// pidText renders the pid column. An adopted listener has no pid here because
// this launcher has no child handle for it, and a blank cell reads as "not
// running" -- which is the opposite of the truth.
func pidText(pid int, adopted bool) string {
	if adopted {
		return "adopted"
	}
	if pid == 0 {
		return ""
	}
	return strconv.Itoa(pid)
}
