"""CodeArts 路由：chat / responses / messages 与用量归因。

用假 codearts2api + 真 hub handler；不碰 WorkBuddy 账号池、不碰外网。
"""
import json
import os
import sys
import threading
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-codearts-routing-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]
USAGE = os.environ["WB_PROXY_USAGE_DIR"]

import wb_codearts  # noqa: E402
import wb_proxy  # noqa: E402
import wb_settings  # noqa: E402
from _fake_codearts import start_server, stop_server  # noqa: E402

PASS = FAIL = 0


def check(label, cond, extra=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print("  [PASS] " + label)
    else:
        FAIL += 1
        print("  [FAIL] " + label + ("  " + str(extra) if extra else ""))


def post(path, payload, key, stream=False):
    req = urllib.request.Request(
        HUB + path,
        data=json.dumps(payload).encode("utf-8"),
        headers={"Authorization": "Bearer " + key,
                 "Content-Type": "application/json"},
        method="POST")
    try:
        resp = urllib.request.urlopen(req, timeout=30)
        return resp.status, resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")


def sse_objects(raw):
    out = []
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        data = line[6:].strip()
        if not data or data == "[DONE]":
            continue
        try:
            out.append(json.loads(data))
        except Exception:
            pass
    return out


class FakePool(object):
    """WorkBuddy 池替身：get 返回 None，其余访问直接失败。"""

    def get(self, uid):
        return None

    def __getattr__(self, name):
        raise AssertionError("WorkBuddy pool used: %s" % name)


fake, fake_base, state = start_server()
hub = wb_proxy.GatewayServer(("127.0.0.1", 0), wb_proxy.Handler)
threading.Thread(target=hub.serve_forever, daemon=True).start()
HUB = "http://127.0.0.1:%d" % hub.server_address[1]

try:
    wb_proxy.POOL = FakePool()
    wb_proxy.CURRENT_REALM = "intl"
    wb_settings.set_enable_anthropic_messages(ACCOUNTS, True)
    wb_settings.set_api_keys(ACCOUNTS, [{
        "key": "hub-key-1234", "name": "test", "realm": "codearts",
    }])
    wb_proxy.API_KEY = None
    wb_codearts.save_config(ACCOUNTS, {
        "enabled": True, "base_url": fake_base, "api_key": "test-key",
    })
    wb_codearts.invalidate_models()

    chat = {"model": "GLM-5.2",
            "messages": [{"role": "user", "content": "hi"}]}

    print("== chat stream ==")
    status, raw = post("/v1/chat/completions",
                       dict(chat, stream=True), "hub-key-1234")
    check("stream status", status == 200, (status, raw[:300]))
    events = sse_objects(raw)
    text = "".join(
        e["choices"][0]["delta"].get("content") or ""
        for e in events if e.get("choices") and e["choices"][0].get("delta"))
    check("stream text", "你好" in text, text)
    check("stream usage", any(e.get("usage") for e in events), events[-3:])
    check("upstream stream forced", state.last_body.get("stream") is True)
    check("upstream usage requested",
          state.last_body.get("stream_options") == {"include_usage": True})

    print("== chat nonstream ==")
    status, raw = post("/v1/chat/completions",
                       dict(chat, stream=False), "hub-key-1234")
    check("nonstream status", status == 200, (status, raw[:300]))
    obj = json.loads(raw)
    check("nonstream text", obj["choices"][0]["message"]["content"] == "你好",
          obj)

    print("== responses ==")
    responses_req = {
        "model": "GLM-5.2",
        "input": [{"role": "user",
                   "content": [{"type": "input_text", "text": "hi"}]}],
        "stream": False,
    }
    status, raw = post("/v1/responses", responses_req, "hub-key-1234")
    check("responses status", status == 200, (status, raw[:300]))
    check("responses text", "你好" in raw, raw[:300])

    status, raw = post("/v1/responses",
                       dict(responses_req, stream=True), "hub-key-1234")
    check("responses stream status", status == 200, (status, raw[:300]))
    check("responses stream completed", "response.completed" in raw, raw[:300])

    print("== messages ==")
    messages_req = {
        "model": "GLM-5.2",
        "max_tokens": 64,
        "messages": [{"role": "user", "content": "hi"}],
        "stream": False,
    }
    status, raw = post("/v1/messages", messages_req, "hub-key-1234")
    check("messages status", status == 200, (status, raw[:300]))
    check("messages text", "你好" in raw, raw[:300])

    status, raw = post("/v1/messages",
                       dict(messages_req, stream=True), "hub-key-1234")
    check("messages stream status", status == 200, (status, raw[:300]))
    check("messages stream text", "你好" in raw, raw[:300])

    print("== usage attribution ==")
    rows = []
    with open(os.path.join(USAGE, "usage.jsonl"), encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    check("usage rows written", len(rows) >= 6, len(rows))
    check("all realm codearts", all(r.get("realm") == "codearts" for r in rows),
          [r.get("realm") for r in rows])
    check("all account codearts",
          all(r.get("account") == "codearts" for r in rows),
          [r.get("account") for r in rows])
    check("all unpriced",
          all(r.get("cost_policy") == "codearts:unpriced" for r in rows),
          [r.get("cost_policy") for r in rows])

    print("== errors ==")
    state.chat_error = (400, {"error": {"message": "bad request"}})
    status, raw = post("/v1/chat/completions", chat, "hub-key-1234")
    check("400 passthrough", status == 400, (status, raw[:300]))
    check("400 message", "bad request" in raw, raw[:300])

    state.chat_error = (500, {"error": {"message": "boom"}})
    status, raw = post("/v1/chat/completions", chat, "hub-key-1234")
    check("500 -> 502", status == 502, (status, raw[:300]))

    state.chat_error = None
    wb_codearts.save_config(ACCOUNTS, {"base_url": "http://127.0.0.1:1"})
    status, raw = post("/v1/chat/completions", chat, "hub-key-1234")
    check("unreachable -> 502", status == 502, (status, raw[:300]))
    check("unreachable message", "CodeArts" in raw, raw[:300])
finally:
    try:
        hub.shutdown()
        hub.server_close()
    except Exception:
        pass
    stop_server(fake)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
