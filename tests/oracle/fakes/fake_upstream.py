#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Deterministic loopback fake upstreams for the oracle recorder.

This process starts three HTTP servers:

* the v1.6.5 Raccoon fake (``_fake_raccoon.start_server``),
* the v1.6.5 CodeArts fake (``_fake_codearts.start_server``),
* a WorkBuddy fake for the intl and cn realms.

It also starts a fourth control server. The oracle harness calls
``POST /__setup`` before a failure case and ``GET /__observe`` afterwards.
The observation contains only call counts and method/path/status tuples, never
credentials or request bodies. That is what makes retry/failover/refresh
behaviour part of the compared contract instead of an inference.

No real network is used. The v1 tree is read-only; the only v1 code this
process imports is the ignored, test-only copy under tests/oracle/v1.6.5.
"""

import argparse
import base64
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

# The oracle recorder is serial, but the load harness opens 200 loopback
# connections at once. The stdlib default backlog is 5, which turns a
# concurrency benchmark into TCP SYN-retransmit timing. Raise it for every
# fake server in this process; this changes no response bytes.
ThreadingHTTPServer.request_queue_size = 256

ROOT = Path(__file__).resolve().parents[3]
V1 = ROOT / "tests" / "oracle" / "v1.6.5"
sys.path.insert(0, str(V1 / "app" / "tests"))
sys.path.insert(0, str(V1 / "app"))

import _fake_codearts  # noqa: E402
import _fake_raccoon  # noqa: E402


def b64url(raw):
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def make_jwt(uid, expires_in=3600):
    now = int(time.time())
    header = b64url(json.dumps({"alg": "none", "typ": "JWT"},
                               separators=(",", ":")).encode("utf-8"))
    claims = {"sub": str(uid), "iat": now, "exp": now + int(expires_in)}
    payload = b64url(json.dumps(claims, separators=(",", ":")).encode("utf-8"))
    return "%s.%s.sig" % (header, payload)


class WorkBuddyState(object):
    def __init__(self):
        self.lock = threading.Lock()
        self.scenario = "ok"
        self.chat_calls = 0
        self.refresh_calls = 0
        self.catalog_calls = 0
        self.media_catalog = False
        self.video_scenario = "completed"
        self.image_calls = 0
        self.edit_calls = 0
        self.video_submit_calls = 0
        self.video_task_calls = 0
        self.media_asset_calls = 0
        self.requests = []
        self.initial_token = make_jwt("pu-raccoon-test")
        self.token = self.initial_token
        self.refresh_token = "refresh-1"
        self.intl_token = make_jwt("oracle-intl")
        self.cn_token = make_jwt("oracle-cn")
        self.refreshed_token = make_jwt("oracle-intl", 7200)
        self.accepted_tokens = set()

    def reset(self, scenario="ok"):
        with self.lock:
            self.scenario = scenario
            self.chat_calls = 0
            self.refresh_calls = 0
            self.catalog_calls = 0
            self.media_catalog = scenario.startswith("media")
            self.video_scenario = "running" if scenario == "media_running" else "completed"
            self.image_calls = 0
            self.edit_calls = 0
            self.video_submit_calls = 0
            self.video_task_calls = 0
            self.media_asset_calls = 0
            self.requests = []
            self.accepted_tokens = set()

    def record(self, method, path, status):
        with self.lock:
            self.requests.append({"method": method, "path": path, "status": status})


CHAT_MODELS = [
    {
        "id": "hy4-preview",
        "name": "Hy4 Preview",
        "descriptionEn": "Oracle WorkBuddy model",
        "credits": "x0.00",
        "supportsToolCall": True,
        "supportsReasoning": True,
        "supportsImages": False,
    }
]

#: Media catalog entries. Only fields v1's read_video_generation projection
#: preserves are present, so the recorded /v1/media/models body is stable.
MEDIA_MODELS = [
    {
        "id": "hunyuan-image-alpha",
        "name": "Hunyuan Image Alpha",
        "tags": ["text-to-image"],
    },
    {
        "id": "gemini-image-oracle",
        "name": "Gemini Image Oracle",
        "tags": ["text-to-image"],
    },
    {
        "id": "hunyuan-image-alpha-edit",
        "name": "Hunyuan Image Alpha Edit",
        "tags": ["image-to-image"],
    },
    {
        "id": "kling-video-v3",
        "name": "Kling-Video-V3",
        "tags": ["text-to-video", "image-to-video"],
        "videoGeneration": {
            "minSeconds": 3,
            "maxSeconds": 15,
            "defaultSeconds": 5,
            "resolutions": ["720P", "1080P", "4K"],
            "defaultResolution": "720P",
            "aspectRatios": ["16:9", "9:16", "1:1"],
            "defaultAspectRatio": "16:9",
            "maxPromptLength": 2500,
            "audioMode": "switchable",
            "defaultEnableAudio": True,
        },
    },
]


def catalog_payload(include_media=False):
    models = list(CHAT_MODELS)
    if include_media:
        models.extend(MEDIA_MODELS)
    return {
        "code": 0,
        "data": {
            "agents": {
                "cli": {
                    "models": ["hy4-preview"],
                }
            },
            "models": models,
        },
    }


#: Deterministic media assets. The video bytes are asserted by the fixture
#: integrity test; the PNG is a 1x1 transparent image.
IMAGE_BYTES = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z0xQAAAAASUVORK5CYII="
)
VIDEO_BYTES = b"oracle-media-video-bytes-v1\n"
IMAGE_B64 = base64.b64encode(b"oracle-image").decode("ascii")


def _sse_chunk(model, delta, finish=None, usage=None):
    payload = {
        "id": "chatcmpl-wb-oracle",
        "object": "chat.completion.chunk",
        "created": 1700000000,
        "model": model,
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }
    if usage is not None:
        payload["usage"] = usage
        payload["choices"] = []
    return "data: " + json.dumps(payload, ensure_ascii=False) + "\n\n"


class WorkBuddyHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    state = None

    def log_message(self, fmt, *args):
        pass

    def _read(self):
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b""

    def _json(self, status, payload):
        raw = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)
        self.state.record(self.command, self.path.split("?")[0], status)

    def _sse(self, raw, status=200):
        self.send_response(status)
        self.send_header("Content-Type", "text/event-stream; charset=utf-8")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)
        self.state.record(self.command, self.path.split("?")[0], status)

    def _bytes(self, raw, content_type, status=200):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)
        self.state.record(self.command, self.path.split("?")[0], status)

    def _base(self):
        return "http://127.0.0.1:%d" % self.server.server_address[1]

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/v3/config":
            with self.state.lock:
                self.state.catalog_calls += 1
                include_media = self.state.media_catalog
            return self._json(200, catalog_payload(include_media))
        if path in ("/v2/activity/growth/tasks", "/activity/growth/streak",
                    "/activity/growth/energy", "/v2/activity/growth/energy"):
            return self._json(200, {"code": 0, "data": {}})
        if path == "/oracle/media/image.png":
            return self._media_asset(IMAGE_BYTES, "image/png")
        if path == "/oracle/media/image-edit.png":
            return self._media_asset(IMAGE_BYTES, "image/png")
        if path == "/oracle/media/video.mp4":
            return self._media_asset(VIDEO_BYTES, "video/mp4")
        return self._json(404, {"code": 404, "msg": "not found"})

    def _media_asset(self, raw, content_type):
        with self.state.lock:
            self.state.media_asset_calls += 1
        return self._bytes(raw, content_type)

    def do_POST(self):
        path = self.path.split("?")[0]
        body = self._read()
        try:
            payload = json.loads(body.decode("utf-8")) if body else {}
        except Exception:
            payload = {}
        if path == "/v2/chat/completions":
            return self._chat(payload)
        if path == "/v2/images/generations":
            return self._image(payload, edit=False)
        if path == "/v2/images/edits":
            return self._image(payload, edit=True)
        if path == "/v2/videos/generations":
            return self._video_submit(payload)
        if path == "/v2/videos/tasks":
            return self._video_task(payload)
        if path == "/v2/plugin/auth/token/refresh":
            with self.state.lock:
                self.state.refresh_calls += 1
            return self._json(200, {
                "code": 0,
                "data": {
                    "accessToken": self.state.refreshed_token,
                    "refreshToken": "oracle-intl-refresh-2",
                    "expiresAt": 4102444800,
                },
            })
        if path == "/billing/meter/get-user-resource-summary":
            return self._json(200, {
                "code": 0,
                "data": {
                    "IsPaidUser": False,
                    "Packages": [
                        {
                            "PackageCode": "pkg-oracle",
                            "CycleTotalCapacity": "100",
                            "CycleRemainCapacity": "60",
                            "CycleUsedCapacity": "40",
                        }
                    ],
                },
            })
        if path == "/billing/meter/get-user-resource-free-packages":
            return self._json(200, {"code": 0, "data": {"Accounts": []}})
        if path == "/billing/meter/get-user-resource-paid-packages":
            return self._json(200, {"code": 0, "data": {"Accounts": []}})
        if path == "/v2/billing/meter/get-user-resource":
            return self._json(200, {
                "code": 0,
                "data": {
                    "Response": {
                        "Data": {
                            "Accounts": [
                                {
                                    "AccountId": "pkg-oracle",
                                    "PackageName": "Oracle Package",
                                    "PackageCode": "pkg-oracle",
                                    "CycleCapacitySize": "100",
                                    "CycleCapacityRemain": "60",
                                    "CycleCapacityUsed": "40",
                                    "Status": 0,
                                }
                            ]
                        }
                    }
                },
            })
        if path == "/v2/billing/meter/checkin-activity-status":
            return self._json(200, {
                "code": 0,
                "data": {
                    "today_checked_in": False,
                    "streak_days": 2,
                    "daily_credit": 5,
                    "today_credit": 0,
                    "active": True,
                },
            })
        if path == "/v2/billing/meter/daily-checkin":
            return self._json(200, {"code": 0, "msg": "签到成功", "data": {}})
        if path == "/v2/report":
            return self._json(200, {"code": 0, "msg": "ok"})
        return self._json(404, {"code": 404, "msg": "not found"})

    def _chat(self, payload):
        with self.state.lock:
            self.state.chat_calls += 1
            call = self.state.chat_calls
            scenario = self.state.scenario
        if scenario == "429" and call == 1:
            return self._json(429, {"error": {"message": "usage exceeds frequency limit"}})
        if scenario == "401" and call == 1:
            return self._json(401, {"error": {"message": "token expired"}})
        if scenario == "5xx" and call == 1:
            return self._json(500, {"error": {"message": "upstream unavailable"}})
        model = str(payload.get("model") or "hy4-preview")
        raw = (
            _sse_chunk(model, {"role": "assistant", "content": "你"})
            + _sse_chunk(model, {"content": "好"})
            + _sse_chunk(model, {}, "stop")
            + _sse_chunk(model, {}, None, {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5})
            + "data: [DONE]\n\n"
        ).encode("utf-8")
        return self._sse(raw)

    def _image(self, payload, edit):
        model = str(payload.get("model") or "")
        with self.state.lock:
            if edit:
                self.state.edit_calls += 1
            else:
                self.state.image_calls += 1
        item = {}
        if model.startswith("gemini-"):
            # The gateway forwards b64_json unchanged; no product fetch is
            # involved, so this path is identical on both targets.
            item["b64_json"] = IMAGE_B64
        else:
            suffix = "image-edit.png" if edit else "image.png"
            item["url"] = self._base() + "/oracle/media/" + suffix
        item["revised_prompt"] = "oracle revised prompt"
        credit = 6.14 if edit else 5.71
        return self._json(200, {
            "code": 0,
            "data": {
                "created": 1700000001,
                "data": [item],
                "usage": {"credit": credit},
            },
        })

    def _video_submit(self, payload):
        with self.state.lock:
            self.state.video_submit_calls += 1
            task_id = "task-oracle-video-%d" % self.state.video_submit_calls
        return self._json(200, {"code": 0, "data": {"id": task_id}})

    def _video_task(self, payload):
        with self.state.lock:
            self.state.video_task_calls += 1
            scenario = self.state.video_scenario
        if scenario == "running":
            return self._json(200, {
                "code": 0,
                "data": {"status": "running", "data": []},
            })
        return self._json(200, {
            "code": 0,
            "data": {
                "status": "completed",
                "data": [{"url": self._base() + "/oracle/media/video.mp4"}],
                "usage": {"credit": 128.57},
            },
        })


class ControlHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    providers = None

    def log_message(self, fmt, *args):
        pass

    def _json(self, status, payload):
        raw = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path != "/__observe":
            return self._json(404, {"error": "not found"})
        provider = (parse_qs(parsed.query).get("provider") or [""])[0]
        entry = self.providers.get(provider)
        if entry is None:
            return self._json(404, {"error": "unknown provider"})
        return self._json(200, entry["observe"]())

    def do_POST(self):
        parsed = urlparse(self.path)
        if parsed.path != "/__setup":
            return self._json(404, {"error": "not found"})
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            payload = json.loads(raw.decode("utf-8"))
        except Exception:
            return self._json(400, {"error": "bad json"})
        provider = str(payload.get("provider") or "")
        scenario = str(payload.get("scenario") or "ok")
        entry = self.providers.get(provider)
        if entry is None:
            return self._json(404, {"error": "unknown provider"})
        try:
            entry["setup"](scenario)
        except Exception as exc:
            return self._json(500, {"error": str(exc)})
        return self._json(200, {"ok": True})


def start_workbuddy(port=0):
    state = WorkBuddyState()
    handler = type("FakeWorkBuddy", (WorkBuddyHandler,), {"state": state})
    server = ThreadingHTTPServer(("127.0.0.1", port), handler)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, "http://127.0.0.1:%d" % server.server_address[1], state


def setup_raccoon(state, scenario):
    if not hasattr(state, "initial_token"):
        state.initial_token = state.token
    state.token = state.initial_token
    state.refresh_token = "refresh-1"
    state.refresh_calls = 0
    state.chat_calls = 0
    state.catalog_calls = 0
    state.requests = []
    state.chat_status = 200
    state.chat_status_remaining = -1
    state.chat_status_by_token = {}
    state.chat_status_body = None
    state.embedded_error = None
    state.embedded_error_by_token = {}
    state.embedded_error_after = None
    state.truncate_after = None
    if scenario == "429":
        state.chat_status = 429
        state.chat_status_remaining = 1
    elif scenario == "401":
        state.chat_status = 401
        state.chat_status_remaining = 1
    elif scenario == "credit_400":
        state.chat_status = 400
        state.chat_status_remaining = 1
        state.chat_status_body = {
            "code": 400,
            "message": "insufficient credits; please top up",
        }
    elif scenario == "concurrency_400":
        state.chat_status = 400
        state.chat_status_remaining = 1
        state.chat_status_body = {
            "code": 400,
            "message": "concurrent session limit reached; retry later",
        }
    elif scenario != "ok":
        raise ValueError("unknown raccoon scenario %r" % scenario)


def observe_raccoon(state):
    return {
        "provider": "raccoon",
        "chat_calls": state.chat_calls,
        "refresh_calls": state.refresh_calls,
        "catalog_calls": state.catalog_calls,
        "requests": [
            {"method": item.get("method"), "path": item.get("path")}
            for item in state.requests
            if item.get("path") in (
                "/api/web/llm/v2/chat/completions",
                "/api/web/auth/v1/refresh",
                "/api/web/llm/v2/model_catalog",
            )
        ],
    }


def setup_codearts(state, scenario):
    state.requests = []
    state.chat_error = None
    state.chat_error_remaining = -1
    state.credit_error_once = False
    state.embedded_error = None
    state.token_calls = 0
    state.model_calls = 0
    if scenario == "429":
        state.chat_error = (429, {"error_msg": "too many requests"})
        state.chat_error_remaining = 1
    elif scenario == "401":
        state.chat_error = (401, {"error_msg": "token expired"})
        state.chat_error_remaining = 1
    elif scenario == "credit_400":
        state.credit_error_once = True
    elif scenario == "concurrency_400":
        state.chat_error = (
            400,
            {"error_msg": "并发会话数已达上限(3个)，请关闭部分会话后重试。"},
        )
        state.chat_error_remaining = 1
    elif scenario != "ok":
        raise ValueError("unknown codearts scenario %r" % scenario)


def observe_codearts(state):
    paths = [item.get("path") for item in state.requests]
    return {
        "provider": "codearts",
        "chat_calls": sum(1 for path in paths if path.endswith("/chat/completions")),
        "token_calls": sum(1 for path in paths if path == "/v1/oauth2/tokens"),
        "requests": [
            {"method": item.get("method", "POST"), "path": item.get("path")}
            for item in state.requests
            if item.get("path") in (
                "/api/v2/chat/completions",
                "/v1/oauth2/tokens",
            )
        ],
    }


def observe_workbuddy(state):
    with state.lock:
        return {
            "provider": "workbuddy",
            "chat_calls": state.chat_calls,
            "refresh_calls": state.refresh_calls,
            "catalog_calls": state.catalog_calls,
            "image_calls": state.image_calls,
            "edit_calls": state.edit_calls,
            "video_submit_calls": state.video_submit_calls,
            "video_task_calls": state.video_task_calls,
            "media_asset_calls": state.media_asset_calls,
            "requests": [
                item for item in state.requests
                if item.get("path") in (
                    "/v2/chat/completions",
                    "/v2/plugin/auth/token/refresh",
                    "/v3/config",
                    "/v2/images/generations",
                    "/v2/images/edits",
                    "/v2/videos/generations",
                    "/v2/videos/tasks",
                    "/oracle/media/image.png",
                    "/oracle/media/image-edit.png",
                    "/oracle/media/video.mp4",
                )
            ],
        }


def start_control(providers, port=0):
    handler = type("FakeControl", (ControlHandler,), {"providers": providers})
    server = ThreadingHTTPServer(("127.0.0.1", port), handler)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, "http://127.0.0.1:%d" % server.server_address[1]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--info", required=True, help="write the loopback bases here")
    parser.add_argument("--workbuddy-port", type=int, default=0,
                        help="fixed WorkBuddy fake port (0 picks a free port)")
    parser.add_argument("--control-port", type=int, default=0,
                        help="fixed control port (0 picks a free port)")
    args = parser.parse_args()

    raccoon_server, raccoon_base, raccoon_state = _fake_raccoon.start_server()
    raccoon_state.initial_token = raccoon_state.token
    codearts_server, codearts_base, codearts_state = _fake_codearts.start_server()
    wb_server, wb_base, wb_state = start_workbuddy(args.workbuddy_port)
    providers = {
        "raccoon": {
            "setup": lambda scenario: setup_raccoon(raccoon_state, scenario),
            "observe": lambda: observe_raccoon(raccoon_state),
        },
        "codearts": {
            "setup": lambda scenario: setup_codearts(codearts_state, scenario),
            "observe": lambda: observe_codearts(codearts_state),
        },
        "workbuddy": {
            "setup": lambda scenario: wb_state.reset(scenario),
            "observe": lambda: observe_workbuddy(wb_state),
        },
    }
    control_server, control_base = start_control(providers, args.control_port)
    info = {
        "raccoon": raccoon_base,
        "codearts": codearts_base,
        "workbuddy": wb_base,
        "control": control_base,
        "raccoon_uid": raccoon_state.uid,
        "raccoon_token": raccoon_state.token,
        "raccoon_refresh_token": raccoon_state.refresh_token,
        "codearts_uid": codearts_state.user_id,
        "codearts_token": codearts_state.token,
        "codearts_refresh_token": codearts_state.refresh_token,
        "codearts_ak": codearts_state.api_key,
        "codearts_sk": codearts_state.secret_key,
        "workbuddy_intl_token": wb_state.intl_token,
        "workbuddy_cn_token": wb_state.cn_token,
    }
    with open(args.info, "w", encoding="utf-8") as fh:
        json.dump(info, fh, indent=2)
    print(json.dumps(info), flush=True)
    try:
        while True:
            time.sleep(1)
    finally:
        for server in (control_server, wb_server, raccoon_server, codearts_server):
            try:
                server.shutdown()
            except Exception:
                pass


if __name__ == "__main__":
    main()
