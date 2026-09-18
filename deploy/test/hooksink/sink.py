"""Webhook receiver for Phase 0 probes.

POST /hook/<tag>      store a delivery (Forgejo webhooks point here)
GET  /events?after=N  deliveries with seq > N, as JSON

Checks the HMAC-SHA256 signature against HOOK_SECRET and keeps only what the
probes need: event headers, action, sender and pusher.
"""

import hashlib
import hmac
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

SECRET = os.environ.get("HOOK_SECRET", "").encode()
events = []
lock = threading.Lock()


def header(headers, name):
    return headers.get("X-Forgejo-" + name) or headers.get("X-Gitea-" + name)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        sig = header(self.headers, "Signature") or ""
        expected = hmac.new(SECRET, body, hashlib.sha256).hexdigest()
        try:
            payload = json.loads(body)
        except ValueError:
            payload = {}
        with lock:
            events.append({
                "seq": len(events) + 1,
                "path": self.path,
                "event": header(self.headers, "Event"),
                "event_type": header(self.headers, "Event-Type"),
                "delivery": header(self.headers, "Delivery"),
                "signature_valid": bool(sig) and hmac.compare_digest(sig, expected),
                "signature_headers": sorted(k for k in self.headers if k.lower().endswith("-signature")),
                "action": payload.get("action"),
                "sender": (payload.get("sender") or {}).get("login"),
                "pusher": (payload.get("pusher") or {}).get("login"),
            })
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def do_GET(self):
        after = int(parse_qs(urlparse(self.path).query).get("after", ["0"])[0])
        with lock:
            data = json.dumps([e for e in events if e["seq"] > after]).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(data)


ThreadingHTTPServer(("", 8099), Handler).serve_forever()
