"""Stand-in authorization-svc (:18089) and identity-context-svc (:18080) for
ncd_live_check.py. Authorization grants every principal except "mallory";
identity knows every principal except "ghost" (as <id>@example.test).
See README-live-check.md."""
import json, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Authz(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        out = "DENIED" if body.get("principal_id") == "mallory" else "GRANTED"
        data = json.dumps({"decision_outcome": out}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers(); self.wfile.write(data)

    def log_message(self, *a): pass


class Identity(BaseHTTPRequestHandler):
    def do_GET(self):
        pid = self.path.rstrip("/").split("/")[-1].split("?")[0]
        if pid == "ghost":
            self.send_response(404); self.end_headers(); return
        data = json.dumps({"principal_id": pid, "email": pid + "@example.test"}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers(); self.wfile.write(data)

    def log_message(self, *a): pass


threading.Thread(target=ThreadingHTTPServer(("0.0.0.0", 18089), Authz).serve_forever, daemon=True).start()
ThreadingHTTPServer(("0.0.0.0", 18080), Identity).serve_forever()
