# -*- coding: utf-8 -*-
"""测试用的假 codearts2api。

不依赖网络、不依赖真实账号：起一个 127.0.0.1 的 HTTP 服务，实现 wb-hub 用到
的推理/管理端点，并把收到的请求记在 state 里供断言。

只给测试用；文件名不以 `_test_` 开头，`tests/run_all.py` 不会把它当套件跑。
"""

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class FakeState(object):
    def __init__(self):
        self.api_key = "test-key"
        self.models = [
            {"id": "GLM-5.2", "object": "model", "created": 1700000000},
            {"id": "glm-5.2", "object": "model", "created": 1700000000},
            {"id": "deepseek-v4-pro-0813", "object": "model", "created": 1700000000},
        ]
        self.models_hits = 0
        self.fail_models = False
        self.models_error = (500, {"error": {"message": "models exploded"}})
        self.chat_text = "你好"
        self.chat_usage = {"prompt_tokens": 3, "completion_tokens": 2,
                           "total_tokens": 5}
        self.chat_error = None  # (status, body_dict) 或 None
        self.requests = []
        self.last_auth = None
        self.last_body = None
        self.last_headers = None
        self.overview = {
            "service": "codearts2api",
            "region": "cn",
            "stats": {"total": 2, "healthy": 1, "disabled": 1, "cooling": 0,
                      "credits": 1},
            "accounts": [
                {"uid": "u1", "nickname": "账号一", "disabled": False,
                 "cooling": False, "until": "", "reason": "", "err_count": 0,
                 "active_concurrent": 0, "max_concurrent": 5,
                 "token_remaining": "1h0m0s",
                 "expires_at": "2026-10-31T12:00:00+08:00", "checkin": None},
                {"uid": "u2", "nickname": "账号二", "disabled": True,
                 "cooling": False, "until": "", "reason": "manual",
                 "err_count": 2, "active_concurrent": 0, "max_concurrent": 5,
                 "token_remaining": "2h0m0s",
                 "expires_at": "2026-10-31T13:00:00+08:00", "checkin": None},
            ],
            "models": [{"id": "GLM-5.2"}, {"id": "glm-5.2"}],
            "schedule": {"watch": {"enabled": True}},
        }
        self.oauth_start = {"ok": True, "session_id": "sess-1",
                            "auth_url": "https://example.test/auth",
                            "expires_in": 600}
        self.oauth_poll = {"ok": True, "status": "pending",
                           "message": "等待浏览器授权"}
        self.oauth_callback = {"ok": True, "status": "continue",
                               "next_url": "https://example.test/next",
                               "message": "回调已接收"}
        self.delete_status = 200
        self.delete_body = {"ok": True, "deleted": True, "message": "已删除 u1"}

    def reset_requests(self):
        self.requests = []
        self.last_auth = None
        self.last_body = None
        self.last_headers = None


def _sse_body(state, model):
    created = int(time.time())
    chunks = [
        {"id": "chatcmpl-fake", "object": "chat.completion.chunk",
         "created": created, "model": model,
         "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""},
                      "finish_reason": None}]},
        {"id": "chatcmpl-fake", "object": "chat.completion.chunk",
         "created": created, "model": model,
         "choices": [{"index": 0, "delta": {"content": state.chat_text},
                      "finish_reason": None}]},
        {"id": "chatcmpl-fake", "object": "chat.completion.chunk",
         "created": created, "model": model,
         "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
        {"id": "chatcmpl-fake", "object": "chat.completion.chunk",
         "created": created, "model": model, "choices": [],
         "usage": state.chat_usage},
    ]
    out = b""
    for chunk in chunks:
        out += ("data: " + json.dumps(chunk, ensure_ascii=False) + "\n\n").encode("utf-8")
    out += b"data: [DONE]\n\n"
    return out


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

    def _auth_ok(self):
        auth = self.headers.get("Authorization") or ""
        return auth == "Bearer %s" % self.state.api_key

    def _read_json(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            return json.loads(raw.decode("utf-8")) if raw else {}
        except Exception:
            return {}

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/healthz":
            raw = b"ok"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
            return
        if not self._auth_ok():
            self._send_json(401, {"error": {"message": "invalid api key"}})
            return
        if path == "/v1/models":
            self.state.models_hits += 1
            if self.state.fail_models:
                self._send_json(*self.state.models_error)
                return
            self._send_json(200, {"object": "list", "data": self.state.models})
            return
        if path == "/admin/api/overview":
            self._send_json(200, self.state.overview)
            return
        self._send_json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        path = self.path.split("?")[0]
        body = self._read_json()
        self.state.requests.append({"path": path, "body": body,
                                    "headers": dict(self.headers)})
        self.state.last_auth = self.headers.get("Authorization")
        self.state.last_headers = dict(self.headers)
        self.state.last_body = body
        if not self._auth_ok():
            self._send_json(401, {"error": {"message": "invalid api key"}})
            return
        if path == "/v1/chat/completions":
            if self.state.chat_error:
                self._send_json(*self.state.chat_error)
                return
            raw = _sse_body(self.state, body.get("model") or "GLM-5.2")
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Content-Length", str(len(raw)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(raw)
            return
        if path == "/admin/api/accounts/enable":
            self._send_json(200, {"ok": True, "message": "已启用"})
            return
        if path == "/admin/api/accounts/disable":
            self._send_json(200, {"ok": True, "message": "已停用"})
            return
        if path == "/admin/api/accounts/clear-cooldown":
            self._send_json(200, {"ok": True, "message": "已清冷却"})
            return
        if path == "/admin/api/accounts/delete":
            if self.state.delete_status != 200:
                self._send_json(self.state.delete_status,
                                {"ok": False, "message": "account not found"})
                return
            self._send_json(200, self.state.delete_body)
            return
        if path == "/admin/api/checkin":
            self._send_json(200, {"ok": True, "message": "签到完成"})
            return
        if path == "/admin/api/keepalive":
            self._send_json(200, {"ok": True, "message": "保活完成"})
            return
        if path == "/admin/api/oauth/start":
            self._send_json(200, self.state.oauth_start)
            return
        if path == "/admin/api/oauth/poll":
            self._send_json(200, self.state.oauth_poll)
            return
        if path == "/admin/api/oauth/import-callback":
            self._send_json(200, self.state.oauth_callback)
            return
        self._send_json(404, {"error": {"message": "not found"}})


def start_server(state=None):
    """起服务，返回 (server, base_url, state)。调用方负责 shutdown()。"""
    state = state or FakeState()
    handler = type("FakeHandler", (_Handler,), {"state": state})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base = "http://127.0.0.1:%d" % server.server_address[1]
    return server, base, state


def stop_server(server):
    try:
        server.shutdown()
        server.server_close()
    except Exception:
        pass
