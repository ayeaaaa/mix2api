"""单进程 Raccoon 路由：chat / responses / messages 与用量归因。

假小浣熊上游 + 真 hub handler；不碰 WorkBuddy 账号池、不碰外网。
"""

import json
import os
import shutil
import sys
import threading
import time
import types
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-raccoon-routing-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]
USAGE = os.environ["WB_PROXY_USAGE_DIR"]

import wb_pricing  # noqa: E402
import wb_proxy  # noqa: E402
import wb_raccoon as R  # noqa: E402
import wb_settings  # noqa: E402
from _fake_raccoon import (  # noqa: E402
    default_catalog,
    make_jwt,
    start_server,
    stop_server,
)

PASS = FAIL = 0
KEY = "hub-key-1234"


def check(label, cond, extra=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print("  [PASS] " + label)
    else:
        FAIL += 1
        print("  [FAIL] " + label + ("  " + str(extra) if extra else ""))


def post(path, payload, key=KEY):
    req = urllib.request.Request(
        HUB + path,
        data=json.dumps(payload).encode("utf-8"),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
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
    def __init__(self):
        self.calls = []

    def get(self, uid):
        return None

    def __getattr__(self, name):
        self.calls.append(name)
        raise AssertionError("WorkBuddy pool used: %s" % name)


def reset_fake():
    state.token = make_jwt(state.uid, 3600)
    state.refresh_token = "refresh-1"
    state.refresh_calls = 0
    state.catalog_calls = 0
    state.exchange_calls = 0
    state.chat_calls = 0
    state.chat_status = 200
    state.chat_status_remaining = -1
    state.chat_status_by_token = {}
    state.chat_text = "你好"
    state.reasoning_text = ""
    state.tool_calls = None
    state.truncate_after = None
    state.embedded_error = None
    state.embedded_error_by_token = {}
    state.saw_image = False
    state.extra_tokens = set()
    state.catalog = default_catalog()
    state.reset_requests()


def reset_gateway():
    shutil.rmtree(os.path.join(ACCOUNTS, "raccoon"), ignore_errors=True)
    wb_settings.save(ACCOUNTS, {})
    wb_settings.set_enable_anthropic_messages(ACCOUNTS, True)
    wb_settings.set_filter_models_by_key(ACCOUNTS, False)
    wb_settings.set_api_keys(
        ACCOUNTS,
        [
            {
                "key": KEY,
                "name": "test",
                "realm": "raccoon",
            }
        ],
    )
    wb_proxy.API_KEY = None
    wb_proxy.CURRENT_REALM = "intl"
    wb_proxy.POOL = FakePool()
    R.save_config(ACCOUNTS, {"enabled": True})
    R.invalidate_pool()
    R.invalidate_health()
    R.invalidate_models()


def add_account(uid, token=None, refresh="refresh-1"):
    token = token or state.token
    saved = R.save_auth(
        ACCOUNTS,
        {
            "access_token": token,
            "refresh_token": refresh,
            "uid": uid,
            "office_identity": "personal",
            "office_org_name": uid,
            "updated_at": int(time.time()),
        },
    )
    return R.pool_for(ACCOUNTS).add(saved)


def usage_rows():
    path = os.path.join(USAGE, "usage.jsonl")
    rows = []
    if not os.path.exists(path):
        return rows
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def rows_for(uid):
    return [row for row in usage_rows() if row.get("account") == uid]


def chat_body(stream=False, **overrides):
    payload = {
        "model": "raccoon-090dd3",
        "messages": [{"role": "user", "content": "hi"}],
        "stream": stream,
    }
    payload.update(overrides)
    return payload


srv, base, state = start_server()
R.BASE_URL = base

hub = wb_proxy.GatewayServer(("127.0.0.1", 0), wb_proxy.Handler)
threading.Thread(target=hub.serve_forever, daemon=True).start()
HUB = "http://127.0.0.1:%d" % hub.server_address[1]

try:
    print("== chat ==")
    reset_fake()
    reset_gateway()
    add_account("pu-a")

    status, raw = post("/v1/chat/completions", chat_body())
    check("nonstream status", status == 200, (status, raw[:300]))
    obj = json.loads(raw)
    check("nonstream text", obj["choices"][0]["message"]["content"] == "你好", obj)
    check("nonstream usage", (obj.get("usage") or {}).get("total_tokens") == 5, obj)
    check(
        "upstream forced stream", state.last_body.get("stream") is True, state.last_body
    )
    check(
        "upstream headers",
        state.last_headers.get("x-client-platform") == "desktop-linux-x64"
        and state.last_headers.get("x-raccoon-language") == "zh",
        state.last_headers,
    )

    status, raw = post("/v1/chat/completions", chat_body(stream=True))
    check("stream status", status == 200, (status, raw[:300]))
    events = sse_objects(raw)
    text = "".join(
        e["choices"][0]["delta"].get("content") or ""
        for e in events
        if e.get("choices") and e["choices"][0].get("delta")
    )
    check("stream text", "你好" in text, text)
    check("stream usage", any(e.get("usage") for e in events), events[-3:])

    print("== responses ==")
    reset_fake()
    reset_gateway()
    add_account("pu-a")
    responses_req = {
        "model": "raccoon-090dd3",
        "input": [{"role": "user", "content": [{"type": "input_text", "text": "hi"}]}],
        "stream": False,
    }
    status, raw = post("/v1/responses", responses_req)
    check("responses status", status == 200, (status, raw[:300]))
    check("responses text", "你好" in raw, raw[:300])
    status, raw = post("/v1/responses", dict(responses_req, stream=True))
    check("responses stream status", status == 200, (status, raw[:300]))
    check("responses stream completed", "response.completed" in raw, raw[:300])

    print("== responses compaction usage ==")
    reset_fake()
    reset_gateway()
    add_account("pu-a")
    wb_proxy.CURRENT_REALM = "intl"
    compaction_req = {
        "model": "raccoon-090dd3",
        "input": [
            {"role": "user", "content": [{"type": "input_text", "text": "summarize"}]},
            {"type": "compaction_trigger"},
        ],
        "stream": False,
    }
    status, raw = post("/v1/responses", compaction_req)
    check("compaction status", status == 200, (status, raw[:300]))
    check("compaction item returned", "encrypted_content" in raw, raw[:300])
    compaction_row = rows_for("pu-a")[-1]
    check("compaction realm raccoon",
          compaction_row.get("realm") == "raccoon", compaction_row)
    check("compaction unpriced",
          compaction_row.get("cost_policy") == "raccoon:unpriced",
          compaction_row)

    print("== responses stream web tools ==")
    reset_fake()
    reset_gateway()
    add_account("pu-a")
    pool = wb_proxy.POOL
    wb_settings.set_local_web_tools(ACCOUNTS, True)
    state.tool_calls = [
        {
            "index": 0,
            "id": "call_web",
            "type": "function",
            "function": {"name": "web_search", "arguments": '{"query":"x"}'},
        }
    ]
    web_req = {
        "model": "raccoon-090dd3",
        "input": [{"role": "user", "content": "search"}],
        "tools": [{"type": "web_search"}],
        "stream": True,
    }
    status, raw = post("/v1/responses", web_req)
    check("web tools stream status", status == 200, (status, raw[:300]))
    check("web tools stream completed", "response.completed" in raw, raw[:300])
    check("web tools stream not aborted",
          "upstream stream aborted" not in raw, raw[:300])
    check("web tools stream never used WorkBuddy pool", pool.calls == [], pool.calls)
    wb_settings.set_local_web_tools(ACCOUNTS, False)

    print("== messages ==")
    messages_req = {
        "model": "raccoon-090dd3",
        "max_tokens": 64,
        "messages": [{"role": "user", "content": "hi"}],
        "stream": False,
    }
    status, raw = post("/v1/messages", messages_req)
    check("messages status", status == 200, (status, raw[:300]))
    check("messages text", "你好" in raw, raw[:300])
    status, raw = post("/v1/messages", dict(messages_req, stream=True))
    check("messages stream status", status == 200, (status, raw[:300]))
    stream_text = "".join(
        (event.get("delta") or {}).get("text") or ""
        for event in sse_objects(raw))
    check("messages stream text", "你好" in stream_text, sse_objects(raw)[-4:])

    print("== usage ==")
    rows = rows_for("pu-a")
    check("usage rows written", len(rows) >= 6, len(rows))
    check(
        "all realm raccoon",
        all(r.get("realm") == "raccoon" for r in rows),
        [r.get("realm") for r in rows],
    )
    check(
        "all account pu-a",
        all(r.get("account") == "pu-a" for r in rows),
        [r.get("account") for r in rows],
    )
    check(
        "all unpriced",
        all(r.get("cost_policy") == "raccoon:unpriced" for r in rows),
        [r.get("cost_policy") for r in rows],
    )
    acct = R.pool_for(ACCOUNTS).get("pu-a")
    acct.proxy_slot = "slot-x"
    status, raw = post("/v1/chat/completions", chat_body())
    check(
        "proxy slot attributed",
        status == 200 and rows_for("pu-a")[-1].get("proxy_slot") == "slot-x",
        rows_for("pu-a")[-1:],
    )
    acct.proxy_slot = ""

    class FakeCodeArtsStream(object):
        _wb_billed_model = "GLM-5.2"
        acct = types.SimpleNamespace(
            uid="codearts-1", realm="codearts", proxy_slot="slot-ca", proxy="")

    wb_proxy.record_upstream_usage(
        FakeCodeArtsStream(), "GLM-5.2",
        {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
        elapsed_ms=1, account="codearts-1", realm="codearts",
        key=None)
    codearts_row = usage_rows()[-1]
    check("codearts proxy attribution unchanged",
          codearts_row.get("realm") == "codearts"
          and "proxy_slot" not in codearts_row, codearts_row)

    cost = wb_pricing.cost_for_row(
        {
            "model": "raccoon-090dd3",
            "cost_policy": "raccoon:unpriced",
            "at": time.time(),
            "prompt_tokens": 10,
            "completion_tokens": 5,
        }
    )
    check(
        "raccoon row unpriced",
        cost["known"] is False
        and cost["cny"] == 0.0
        and cost["source"] == "raccoon:unpriced",
        cost,
    )

    print("== tools and image ==")
    reset_fake()
    reset_gateway()
    add_account("pu-a")
    state.tool_calls = [
        {
            "index": 0,
            "id": "call_1",
            "type": "function",
            "function": {"name": "lookup", "arguments": '{"q":"x"}'},
        }
    ]
    tools = [
        {
            "type": "function",
            "function": {
                "name": "lookup",
                "parameters": {"type": "object", "properties": {}},
            },
        }
    ]
    status, raw = post("/v1/chat/completions", chat_body(tools=tools))
    check("tools status", status == 200, (status, raw[:300]))
    obj = json.loads(raw)
    calls = obj["choices"][0]["message"].get("tool_calls") or []
    check(
        "tool call preserved",
        calls and calls[0].get("function", {}).get("name") == "lookup",
        calls,
    )
    check(
        "tools forwarded",
        (state.last_body or {}).get("tools") == tools,
        state.last_body,
    )

    state.tool_calls = None
    image_chat = chat_body(
        messages=[
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "describe"},
                    {
                        "type": "image_url",
                        "image_url": {"url": "data:image/png;base64,abc"},
                    },
                ],
            }
        ]
    )
    status, raw = post("/v1/chat/completions", image_chat)
    check("image status", status == 200, (status, raw[:300]))
    check("image reached upstream", state.saw_image is True, state.last_body)
    content = ((state.last_body or {}).get("messages") or [{}])[0].get("content")
    check(
        "image content preserved",
        isinstance(content, list)
        and any(
            isinstance(p, dict)
            and p.get("type") == "image_url"
            and (p.get("image_url") or {}).get("url") == "data:image/png;base64,abc"
            for p in content
        ),
        content,
    )

    print("== 401 refresh ==")
    reset_fake()
    reset_gateway()
    old_token = state.token
    add_account("pu-a", token=old_token)
    state.token = make_jwt(state.uid, 3600, jti="rotated")
    before = state.chat_calls
    status, raw = post("/v1/chat/completions", chat_body())
    check("refresh request succeeds", status == 200, (status, raw[:300]))
    check("refresh called once", state.refresh_calls == 1, state.refresh_calls)
    check(
        "retried same account",
        state.chat_calls == before + 2,
        state.chat_calls - before,
    )
    check(
        "refresh usage row",
        rows_for("pu-a")[-1].get("realm") == "raccoon",
        rows_for("pu-a")[-1:],
    )

    print("== 429 failover ==")
    reset_fake()
    reset_gateway()
    token_a = state.token
    token_b = make_jwt("pu-b", 3600, jti="b")
    state.extra_tokens.add(token_b)
    add_account("pu-a", token=token_a)
    add_account("pu-b", token=token_b)
    state.chat_status_by_token[token_a] = 429
    before = state.chat_calls
    status, raw = post("/v1/chat/completions", chat_body())
    check("failover status", status == 200, (status, raw[:300]))
    check(
        "failover tried both", state.chat_calls == before + 2, state.chat_calls - before
    )
    check("failover used account b", rows_for("pu-b"), rows_for("pu-b")[-1:])

    print("== key account restriction ==")
    reset_fake()
    reset_gateway()
    token_a = state.token
    token_b = make_jwt("pu-b", 3600, jti="b2")
    state.extra_tokens.add(token_b)
    add_account("pu-a", token=token_a)
    add_account("pu-b", token=token_b)
    state.chat_status_by_token[token_a] = 429
    wb_settings.set_api_keys(
        ACCOUNTS,
        [
            {
                "key": KEY,
                "name": "test",
                "realm": "raccoon",
                "accounts": ["pu-b"],
            }
        ],
    )
    before = len(state.requests)
    status, raw = post("/v1/chat/completions", chat_body())
    check("allowed account works", status == 200, (status, raw[:300]))
    chat_auth = [
        item["headers"].get("authorization")
        for item in state.requests[before:]
        if item["path"] == "/api/web/llm/v2/chat/completions"
    ]
    check(
        "restricted account never called", chat_auth == ["Bearer " + token_b], chat_auth
    )
    check("allowed account usage", rows_for("pu-b"), rows_for("pu-b")[-1:])
finally:
    try:
        hub.shutdown()
        hub.server_close()
    except Exception:
        pass
    stop_server(srv)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
