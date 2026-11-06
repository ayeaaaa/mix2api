# -*- coding: utf-8 -*-
"""Deterministic fake Raccoon (小浣熊) upstream for the provider suites.

The suites point `wb_raccoon.BASE_URL` at this server, so no test touches the
real gateway or a real account. Tokens are unsigned JWT-shaped strings: they
carry a `sub` and `exp` claim so the provider can derive the UID and refresh
window, but there is no signature to verify.

Task 1 only needs the auth/refresh/catalog skeleton and a 501 chat placeholder;
later tasks extend `FakeState.catalog` and the chat branch without changing the
server lifecycle.

The file name does not start with `_test_`, so `tests/run_all.py` treats it as
a helper rather than a suite.
"""

import base64
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def _b64url(raw):
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def make_jwt(uid, expires_in=3600):
    """Return an unsigned, JWT-shaped token with `sub` and `exp` claims."""
    now = int(time.time())
    header = _b64url(
        json.dumps({"alg": "none", "typ": "JWT"}, separators=(",", ":")).encode("utf-8")
    )
    payload = _b64url(
        json.dumps(
            {"sub": str(uid or ""), "iat": now, "exp": now + int(expires_in)},
            separators=(",", ":"),
        ).encode("utf-8")
    )
    return "%s.%s.sig" % (header, payload)


class FakeState(object):
    def __init__(self):
        self.uid = "pu-raccoon-test"
        self.token = make_jwt(self.uid, 3600)
        self.refresh_token = "refresh-1"
        self.refresh_calls = 0
        self.catalog_calls = 0
        self.exchange_calls = 0
        self.requests = []
        self.last_headers = None
        self.last_body = None
        # Response overrides: status + optional JSON body.
        self.refresh_status = 200
        self.refresh_body = None
        self.catalog_status = 200
        self.exchange_status = 200
        self.exchange_body = None
        self.chat_status = 501
        self.code = "test-code"
        self.office_identity = "personal"
        self.office_org_name = "Raccoon Test"
        # Task 2 replaces this with the real two-model fixture.
        self.catalog = {
            "code": 0,
            "data": {
                "categories": [
                    {"type": "chat", "models": []},
                ]
            },
        }
        self.lock = threading.Lock()

    def reset_requests(self):
        self.requests = []
        self.last_headers = None
        self.last_body = None

    def token_payload(self):
        return {
            "access_token": self.token,
            "refresh_token": self.refresh_token,
            "token_type": "Bearer",
            "office_identity": self.office_identity,
            "office_org_name": self.office_org_name,
        }

    def rotate(self):
        """Issue a fresh token pair. Caller must hold `self.lock`."""
        self.token = make_jwt(self.uid, 3600)
        self.refresh_token = "refresh-%d" % (
            self.refresh_calls + self.exchange_calls + 2
        )
        return self.token_payload()


def _request_headers(handler):
    return {str(k).lower(): str(v) for k, v in handler.headers.items()}


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    state = None

    def log_message(self, fmt, *args):
        pass

    def _send_json(self, status, payload):
        raw = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)

    def _read(self):
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b""

    def _record(self, method, path, raw):
        state = self.state
        headers = _request_headers(self)
        try:
            body = json.loads(raw.decode("utf-8")) if raw else {}
        except Exception:
            body = raw.decode("utf-8", "replace")
        state.requests.append(
            {
                "method": method,
                "path": path,
                "headers": headers,
                "body": body,
            }
        )
        state.last_headers = headers
        state.last_body = body
        return body

    def _authorized(self):
        return self.headers.get("Authorization") == "Bearer " + self.state.token

    def do_GET(self):
        path = self.path.split("?")[0]
        self._record("GET", path, b"")
        state = self.state
        if path == "/__requests":
            self._send_json(
                200,
                {
                    "requests": state.requests,
                    "refresh_calls": state.refresh_calls,
                    "catalog_calls": state.catalog_calls,
                    "exchange_calls": state.exchange_calls,
                    "last_headers": state.last_headers,
                },
            )
            return
        if path == "/api/web/llm/v2/model_catalog":
            state.catalog_calls += 1
            if not self._authorized():
                self._send_json(401, {"message": "invalid access token"})
                return
            if state.catalog_status != 200:
                self._send_json(state.catalog_status, {"message": "catalog failed"})
                return
            self._send_json(200, state.catalog)
            return
        self._send_json(404, {"message": "not found"})

    def do_POST(self):
        path = self.path.split("?")[0]
        body = self._record("POST", path, self._read())
        state = self.state
        if path == "/api/web/auth/v1/refresh":
            with state.lock:
                state.refresh_calls += 1
                if state.refresh_status != 200:
                    self._send_json(
                        state.refresh_status,
                        state.refresh_body or {"message": "refresh rejected"},
                    )
                    return
                supplied = str((body or {}).get("refresh_token") or "")
                if supplied != state.refresh_token:
                    self._send_json(401, {"message": "invalid refresh token"})
                    return
                payload = state.rotate()
            self._send_json(200, {"code": 0, "data": payload})
            return
        if path == "/api/web/auth/v1/login_with_authorization_code":
            with state.lock:
                state.exchange_calls += 1
                if state.exchange_status != 200:
                    self._send_json(
                        state.exchange_status,
                        state.exchange_body or {"message": "exchange rejected"},
                    )
                    return
                supplied = str((body or {}).get("authorization_code") or "")
                if supplied != state.code:
                    self._send_json(400, {"message": "invalid authorization code"})
                    return
                payload = state.rotate()
            self._send_json(200, {"code": 0, "data": payload})
            return
        if path == "/api/web/llm/v2/chat/completions":
            if not self._authorized():
                self._send_json(401, {"message": "invalid access token"})
                return
            self._send_json(
                state.chat_status, {"message": "chat is implemented in task 3"}
            )
            return
        self._send_json(404, {"message": "not found"})


def start_server(state=None):
    state = state or FakeState()
    handler = type("FakeRaccoon", (_Handler,), {"state": state})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base = "http://127.0.0.1:%d" % server.server_address[1]
    return server, base, state


def stop_server(server):
    try:
        server.shutdown()
        server.server_close()
    except Exception:
        pass
