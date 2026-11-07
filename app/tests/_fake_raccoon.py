# -*- coding: utf-8 -*-
"""Deterministic fake Raccoon (小浣熊) upstream for the provider suites.

The suites point `wb_raccoon.BASE_URL` at this server, so no test touches the
real gateway or a real account. Tokens are unsigned JWT-shaped strings: they
carry a `sub` and `exp` claim so the provider can derive the UID and refresh
window, but there is no signature to verify.

Task 1 needs the auth/refresh/catalog skeleton and a 501 chat placeholder; Task 2
adds the two-model catalog fixture and failure controls; later tasks extend the
chat branch without changing the server lifecycle.

The file name does not start with `_test_`, so `tests/run_all.py` treats it as
a helper rather than a suite.
"""

import base64
import datetime
import json
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


CATALOG_FIXTURE = {
    "code": 0,
    "data": {
        "categories": [
            {
                "type": "chat",
                "default_model": "raccoon-090dd3",
                "models": [
                    {
                        "name": "raccoon-090dd3",
                        "display_name": "Raccoon-Work",
                        "params": {"context_window": 1000000, "max_tokens": 100000},
                        "tags": ["general", "code", "vision", "reasoning"],
                    },
                    {
                        "name": "sn-glm-5-3",
                        "display_name": "GLM-5-3",
                        "params": {"context_window": 1000000, "max_tokens": 100000},
                        "tags": ["code", "reasoning"],
                    },
                ],
            }
        ]
    },
}


def default_catalog():
    """A deep copy of the fixture so a test can mutate `state.catalog` safely."""
    return json.loads(json.dumps(CATALOG_FIXTURE))


def _b64url(raw):
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def make_jwt(uid, expires_in=3600, jti=None):
    """Return an unsigned, JWT-shaped token with `sub` and `exp` claims."""
    now = int(time.time())
    header = _b64url(
        json.dumps({"alg": "none", "typ": "JWT"}, separators=(",", ":")).encode("utf-8")
    )
    claims = {"sub": str(uid or ""), "iat": now, "exp": now + int(expires_in)}
    if jti:
        claims["jti"] = str(jti)
    payload = _b64url(json.dumps(claims, separators=(",", ":")).encode("utf-8"))
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
        self.refresh_delay = 0.0
        self.refresh_started = threading.Event()
        self.catalog_status = 200
        # Task 2 failure control: a non-zero status wins over `catalog_status`.
        self.catalog_error = 0
        self.catalog_error_body = None
        self.exchange_status = 200
        self.exchange_body = None
        # When set, the exchange fails even though `exchange_status` is 200.
        # A dict is sent verbatim; any other value becomes {"message": ...}.
        self.exchange_error = None
        # Chat SSE controls. `chat_status` is the HTTP status; a negative
        # `chat_status_remaining` means "always", 0 recovers, and a positive
        # value fails that many times. `chat_status_by_token` overrides the
        # global status per bearer token so failover can pick a healthy account.
        self.chat_status = 200
        self.chat_status_remaining = -1
        self.chat_status_by_token = {}
        self.chat_calls = 0
        self.chat_text = "你好"
        self.reasoning_text = ""
        self.tool_calls = None
        self.truncate_after = None
        self.embedded_error = None
        self.embedded_error_by_token = {}
        self.embedded_error_after = None
        self.saw_image = False
        self.extra_tokens = set()
        self.code = "test-code"
        self.office_identity = "personal"
        self.office_org_name = "Raccoon Test"
        self.office_org_role = ""
        self.catalog = default_catalog()
        self.balance_calls = 0
        self.bills_calls = 0
        self.balance_status = 200
        self.bills_status = 200
        now = datetime.datetime.now().astimezone()
        def when(days=0):
            return (now - datetime.timedelta(days=days)).isoformat(timespec="seconds")
        self.balance = {
            "code": 0,
            "message": "success",
            "data": {
                "available_points": 1234,
                "daily_points": 234,
                "monthly_points": 0,
                "reward_points": 1000,
                "topup_points": 0,
                "enterprise_topup_points": 0,
                "total_chargeable_points": 1234,
                "topup_frozen": False,
            },
        }
        self.bills = {
            "code": 0,
            "message": "success",
            "data": {
                "items": [
                    {"event_name": "桌面端 LLM 对话消耗", "points": -1,
                     "biz_type": "desktop", "client_type": "desktop",
                     "created_at": when(0)},
                    {"event_name": "桌面端 LLM 对话消耗", "points": -2,
                     "biz_type": "desktop", "client_type": "desktop",
                     "created_at": when(0)},
                    {"event_name": "三天前对话消耗", "points": -4,
                     "biz_type": "desktop", "client_type": "desktop",
                     "created_at": when(3)},
                    {"event_name": "八天前对话消耗", "points": -5,
                     "biz_type": "desktop", "client_type": "desktop",
                     "created_at": when(8)},
                    {"event_name": "每日积分发放", "points": 300,
                     "biz_type": "daily_grant", "client_type": "web",
                     "created_at": when(0)},
                    {"event_name": "奖励积分", "points": 1000,
                     "biz_type": "reward_grant", "client_type": "web",
                     "created_at": when(2)},
                ],
                "has_more": False,
                "next_cursor": "",
            },
        }
        self.last_header_names = None
        self.lock = threading.Lock()

    def reset_requests(self):
        self.requests = []
        self.last_headers = None
        self.last_header_names = None
        self.last_body = None

    def token_payload(self):
        return {
            "access_token": self.token,
            "refresh_token": self.refresh_token,
            "token_type": "Bearer",
            "office_identity": self.office_identity,
            "office_org_name": self.office_org_name,
            "office_org_role": self.office_org_role,
        }

    def rotate(self):
        """Issue a fresh token pair. Caller must hold `self.lock`."""
        self.token = make_jwt(self.uid, 3600, jti=uuid.uuid4().hex)
        self.refresh_token = "refresh-%d" % (
            self.refresh_calls + self.exchange_calls + 2
        )
        return self.token_payload()


def _request_headers(handler):
    return {str(k).lower(): str(v) for k, v in handler.headers.items()}


def _bearer_token(handler):
    value = str(handler.headers.get("Authorization") or "").strip()
    if value.lower().startswith("bearer "):
        return value[7:].strip()
    return ""


def _has_image(body):
    """True when any message content part carries an image URL."""
    for message in (body or {}).get("messages") or ():
        if not isinstance(message, dict):
            continue
        content = message.get("content")
        if not isinstance(content, list):
            continue
        for part in content:
            if isinstance(part, dict) and (
                part.get("type") == "image_url" or "image_url" in part
            ):
                return True
    return False


def _text_pieces(text):
    """Split a text into deterministic one-character SSE deltas."""
    return [text[index] for index in range(len(text))]


def _chat_chunks(state, model):
    """Standard OpenAI chat chunks: role, reasoning, text, tools, finish, usage."""
    created = int(time.time())
    chunks = []

    def add(delta, finish=None, usage=None):
        chunk = {
            "id": "chatcmpl-raccoon-fake",
            "object": "chat.completion.chunk",
            "created": created,
            "model": model,
        }
        if usage is None:
            chunk["choices"] = [{"index": 0, "delta": delta, "finish_reason": finish}]
        else:
            chunk["choices"] = []
            chunk["usage"] = usage
        chunks.append(chunk)

    add({"role": "assistant", "content": ""})
    for piece in _text_pieces(str(state.reasoning_text or "")):
        add({"reasoning_content": piece})
    for piece in _text_pieces(
        str(state.chat_text if state.chat_text is not None else "")
    ):
        add({"content": piece})
    if state.tool_calls:
        add({"tool_calls": json.loads(json.dumps(state.tool_calls))})
    add({}, "tool_calls" if state.tool_calls else "stop")
    add(
        {},
        usage={
            "prompt_tokens": 3,
            "completion_tokens": 2,
            "total_tokens": 5,
        },
    )
    return chunks


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

    def _send_sse(self, raw, truncated=False):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream; charset=utf-8")
        self.send_header("Cache-Control", "no-cache")
        if truncated:
            # No Content-Length and no chunk terminator: the peer closing the
            # socket is the only end-of-body signal, which is exactly the
            # half-finished stream the provider has to detect.
            self.send_header("Connection", "close")
            self.close_connection = True
        else:
            self.send_header("Content-Length", str(len(raw)))
            self.send_header("Connection", "close")
        self.end_headers()
        try:
            self.wfile.write(raw)
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

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
        state.last_header_names = [str(name) for name in self.headers.keys()]
        state.last_body = body
        return body

    def _authorized(self):
        token = _bearer_token(self)
        return token == self.state.token or token in self.state.extra_tokens

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
        if path == "/api/web/points/v1/balance":
            state.balance_calls += 1
            if not self._authorized():
                self._send_json(401, {"message": "invalid access token"})
                return
            if state.balance_status != 200:
                self._send_json(state.balance_status, {"message": "balance failed"})
                return
            self._send_json(200, state.balance)
            return
        if path == "/api/web/points/v2/bills":
            state.bills_calls += 1
            if not self._authorized():
                self._send_json(401, {"message": "invalid access token"})
                return
            if state.bills_status != 200:
                self._send_json(state.bills_status, {"message": "bills failed"})
                return
            self._send_json(200, state.bills)
            return
        if path == "/api/web/llm/v2/model_catalog":
            state.catalog_calls += 1
            if not self._authorized():
                self._send_json(401, {"message": "invalid access token"})
                return
            if state.catalog_error:
                self._send_json(
                    state.catalog_error,
                    state.catalog_error_body or {"message": "catalog failed"},
                )
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
                state.refresh_started.set()
                if state.refresh_delay:
                    time.sleep(float(state.refresh_delay))
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
                if state.exchange_error is not None:
                    payload = state.exchange_error
                    if not isinstance(payload, dict):
                        payload = {"message": str(payload)}
                    self._send_json(400, payload)
                    return
                supplied = str((body or {}).get("authorization_code") or "")
                if supplied != state.code:
                    self._send_json(400, {"message": "invalid authorization code"})
                    return
                payload = state.rotate()
            self._send_json(200, {"code": 0, "data": payload})
            return
        if path == "/api/web/llm/v2/chat/completions":
            with state.lock:
                state.chat_calls += 1
            if not self._authorized():
                self._send_json(401, {"message": "invalid access token"})
                return
            if (
                not isinstance(body, dict)
                or not body.get("model")
                or not body.get("messages")
            ):
                self._send_json(400, {"message": "model and messages are required"})
                return
            state.saw_image = _has_image(body)
            token = _bearer_token(self)
            with state.lock:
                status = state.chat_status_by_token.get(token, state.chat_status)
                if status != 200:
                    if state.chat_status_remaining == 0:
                        status = 200
                    elif state.chat_status_remaining > 0:
                        state.chat_status_remaining -= 1
            if status != 200:
                self._send_json(
                    status, {"message": "chat failed with HTTP %s" % status}
                )
                return
            embedded = state.embedded_error_by_token.get(token, state.embedded_error)
            if embedded and state.embedded_error_after is None:
                raw = (
                    "data: " + json.dumps(embedded, ensure_ascii=False) + "\n\n"
                ).encode("utf-8")
                self._send_sse(raw)
                return
            chunks = _chat_chunks(state, body.get("model") or "")
            frames = [
                ("data: " + json.dumps(chunk, ensure_ascii=False) + "\n\n").encode(
                    "utf-8"
                )
                for chunk in chunks
            ]
            if state.embedded_error_after is not None:
                try:
                    limit = max(0, int(state.embedded_error_after))
                except (TypeError, ValueError):
                    limit = 0
                error = state.embedded_error_by_token.get(token, state.embedded_error)
                if not error:
                    error = {"error_code": 500, "error_msg": "mid-stream failure"}
                raw = b"".join(frames[:limit]) + (
                    "data: " + json.dumps(error, ensure_ascii=False) + "\n\n"
                ).encode("utf-8")
                self._send_sse(raw)
                return
            if state.truncate_after is not None:
                try:
                    limit = max(0, int(state.truncate_after))
                except (TypeError, ValueError):
                    limit = 0
                self._send_sse(b"".join(frames[:limit]), truncated=True)
                return
            self._send_sse(b"".join(frames) + b"data: [DONE]\n\n")
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
