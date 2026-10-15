package oidc

import (
	"html/template"
	"net/http"
	"net/url"

	"zoiko.io/identity-context-svc/internal/domain"
)

type loginView struct {
	Pending *Pending
	CSRF    string
	Tenant  string
	Email   string
	Error   string
	// Step is "" (password) or "second".
	Step    string
	Factors []string
	IdPs    []domain.IdentityProvider
	Status  int
	Nonce   string
}

func (v loginView) Has(f string) bool { return containsString(v.Factors, f) }

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sign in · ZoikoSuite</title>
<style nonce="{{.Nonce}}">
:root{--bg:#f6f7f9;--card:#fff;--fg:#1d2330;--muted:#5b6475;--line:#d8dce3;--accent:#1f5eff;--err:#b42318}
@media (prefers-color-scheme:dark){:root{--bg:#11141a;--card:#1a1f28;--fg:#e8ebf1;--muted:#9aa3b5;--line:#2c3340;--accent:#6c95ff;--err:#ff8a7a}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;padding:16px}
main{width:100%;max-width:380px;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px}
h1{font-size:20px;margin:0 0 4px}p.sub{margin:0 0 20px;color:var(--muted)}
label{display:block;font-size:13px;color:var(--muted);margin:14px 0 6px}
input{width:100%;padding:10px 12px;border:1px solid var(--line);border-radius:8px;background:transparent;color:var(--fg);font:inherit}
button,.btn{display:block;width:100%;margin-top:20px;padding:11px;border:0;border-radius:8px;background:var(--accent);color:#fff;font:inherit;font-weight:600;cursor:pointer;text-align:center;text-decoration:none}
.btn.alt{background:transparent;color:var(--fg);border:1px solid var(--line);margin-top:10px}
.err{color:var(--err);margin:12px 0 0;font-size:14px}.or{margin:22px 0 0;text-align:center;color:var(--muted);font-size:13px}
</style></head><body><main>
<h1>Sign in</h1>
<p class="sub">ZoikoSuite{{if .Pending}} · {{.Pending.ClientID}}{{end}}</p>
{{if eq .Step "second"}}
  {{if .Has "passkey"}}
  <button type="button" id="pk">Use your passkey</button>
  {{end}}
  {{if .Has "totp"}}
  <form method="post" action="/oauth2/login/mfa" autocomplete="off">
    <input type="hidden" name="pending" value="{{.Pending.ID}}"><input type="hidden" name="csrf" value="{{.CSRF}}">
    <label for="code">Authenticator code</label>
    <input id="code" name="code" inputmode="numeric" pattern="[0-9 ]{6,8}" autocomplete="one-time-code" required autofocus>
    <button type="submit">Verify</button>
  </form>
  {{end}}
{{else}}
  <form method="post" action="/oauth2/login">
    <input type="hidden" name="pending" value="{{.Pending.ID}}"><input type="hidden" name="csrf" value="{{.CSRF}}">
    {{if .Pending.TenantHint}}<input type="hidden" name="tenant" value="{{.Pending.TenantHint}}">{{else}}
    <label for="tenant">Organisation ID</label><input id="tenant" name="tenant" value="{{.Tenant}}" required autocomplete="organization">{{end}}
    <label for="email">Email</label><input id="email" name="email" type="email" value="{{.Email}}" required autocomplete="username" autofocus>
    <label for="password">Password</label><input id="password" name="password" type="password" required autocomplete="current-password">
    <button type="submit">Continue</button>
  </form>
  {{if .IdPs}}<p class="or">or</p>{{range .IdPs}}
  <a class="btn alt" href="/oauth2/login/federate/{{.IdPID}}?pending={{$.Pending.ID}}&amp;csrf={{$.CSRF}}">Continue with {{.DisplayName}}</a>{{end}}{{end}}
{{end}}
{{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
</main>
{{if and (eq .Step "second") (.Has "passkey")}}
<script nonce="{{.Nonce}}">
(function(){
  var b64u=function(b){return btoa(String.fromCharCode.apply(null,new Uint8Array(b))).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'')};
  var unb64u=function(s){s=s.replace(/-/g,'+').replace(/_/g,'/');while(s.length%4)s+='=';var r=atob(s),a=new Uint8Array(r.length);for(var i=0;i<r.length;i++)a[i]=r.charCodeAt(i);return a.buffer};
  var form=function(o){return Object.keys(o).map(function(k){return encodeURIComponent(k)+'='+encodeURIComponent(o[k])}).join('&')};
  var base={pending:{{.Pending.ID}},csrf:{{.CSRF}}};
  document.getElementById('pk').addEventListener('click',async function(){
    try{
      var r=await fetch('/oauth2/login/passkey/begin',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:form(base)});
      var j=await r.json(); if(!r.ok) throw new Error(j.error||'begin');
      var pk=j.options.publicKey; pk.challenge=unb64u(pk.challenge);
      (pk.allowCredentials||[]).forEach(function(c){c.id=unb64u(c.id)});
      var cred=await navigator.credentials.get({publicKey:pk});
      var a={id:cred.id,rawId:b64u(cred.rawId),type:cred.type,response:{authenticatorData:b64u(cred.response.authenticatorData),
        clientDataJSON:b64u(cred.response.clientDataJSON),signature:b64u(cred.response.signature),
        userHandle:cred.response.userHandle?b64u(cred.response.userHandle):null}};
      var f=await fetch('/oauth2/login/passkey/finish',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded','X-Passkey-Session':j.session},
        body:form(Object.assign({assertion:JSON.stringify(a)},base))});
      var fj=await f.json(); if(!f.ok) throw new Error(fj.error||'finish'); location.href=fj.redirect;
    }catch(e){alert('The passkey was not accepted. Try again or use your authenticator code.')}
  });
})();
</script>{{end}}
</body></html>`))

var problemTmpl = template.Must(template.New("problem").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in · ZoikoSuite</title>
<style nonce="{{.Nonce}}">body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font:15px/1.5 system-ui,sans-serif;background:#f6f7f9;color:#1d2330;padding:16px}
@media (prefers-color-scheme:dark){body{background:#11141a;color:#e8ebf1}}main{max-width:420px}</style>
</head><body><main><h1>Sign-in could not continue</h1><p>{{.Message}}</p></main></body></html>`))

func (p *Provider) securityHeaders(w http.ResponseWriter, nonce, formTarget string) {
	formAction := "'self'"
	if u, err := url.Parse(formTarget); err == nil && u.Scheme != "" && u.Host != "" {
		// Chrome applies form-action to the redirect that follows a form
		// post, so the client's origin must be allowed for the login to land.
		formAction += " " + u.Scheme + "://" + u.Host
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+nonce+"'; script-src 'nonce-"+nonce+
		"'; connect-src 'self'; form-action "+formAction+"; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

func (p *Provider) renderLogin(w http.ResponseWriter, r *http.Request, v loginView) {
	v.Nonce = RandomToken(16)
	if v.Step == "" && p.o.Federation != nil && v.Pending != nil && v.Pending.TenantHint != "" {
		if idps, err := p.o.Federation.ActiveIdPs(r.Context(), v.Pending.TenantHint); err == nil {
			v.IdPs = idps
		}
	}
	target := ""
	if v.Pending != nil {
		target = v.Pending.RedirectURI
	}
	p.securityHeaders(w, v.Nonce, target)
	status := v.Status
	if status == 0 {
		status = http.StatusOK
		if v.Error != "" {
			status = http.StatusUnauthorized
		}
	}
	w.WriteHeader(status)
	_ = loginTmpl.Execute(w, v)
}

func (p *Provider) renderProblem(w http.ResponseWriter, status int, msg string) {
	nonce := RandomToken(16)
	p.securityHeaders(w, nonce, "")
	w.WriteHeader(status)
	_ = problemTmpl.Execute(w, map[string]string{"Message": msg, "Nonce": nonce})
}
