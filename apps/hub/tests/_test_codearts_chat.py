"""CodeArts 请求体、流式打开与账号适配对象。"""
import hashlib
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-codearts-chat-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]

import wb_codearts as C  # noqa: E402
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


def expect_raises(label, exc_type, fn):
    try:
        fn()
    except exc_type as exc:
        check(label, True)
        return exc
    except Exception as exc:
        check(label, False, "wrong exception: %r" % (exc,))
        return None
    check(label, False, "no exception")
    return None


print("== build_chat_body ==")
body = C.build_chat_body({
    "model": "GLM-5.2",
    "messages": [{"role": "user", "content": "hi"}],
    "stream": False,
    "_namespace_map": {"x": "y"},
    "user": "u",
    "metadata": {"a": 1},
}, "sess-1")
check("stream forced on", body["stream"] is True)
check("usage requested", body["stream_options"] == {"include_usage": True})
check("private marker dropped", "_namespace_map" not in body)
check("user dropped", "user" not in body)
check("metadata dropped", "metadata" not in body)
check("conversation id derived",
      body["conversation_id"] == hashlib.sha256(b"sess-1").hexdigest()[:32])
check("no system prompt injected",
      all(m.get("role") != "system" for m in body["messages"]))
check("model kept", body["model"] == "GLM-5.2")

explicit = C.build_chat_body({
    "model": "GLM-5.2",
    "messages": [{"role": "user", "content": "hi"}],
    "conversation_id": "a" * 32,
}, "sess-1")
check("valid explicit conversation kept", explicit["conversation_id"] == "a" * 32)

no_sess = C.build_chat_body({
    "model": "GLM-5.2",
    "messages": [{"role": "user", "content": "hi"}],
})
check("no session -> no conversation_id", "conversation_id" not in no_sess)

dev = C.build_chat_body({
    "model": "GLM-5.2",
    "messages": [{"role": "developer", "content": "x"},
                 {"role": "user", "content": "hi"}],
})
check("developer mapped to system", dev["messages"][0]["role"] == "system")

split = C.build_chat_body({
    "model": "GLM-5.2",
    "messages": [
        {"role": "assistant", "content": None,
         "tool_calls": [{"id": "c1", "type": "function",
                         "function": {"name": "f", "arguments": "{}"}}]},
        {"role": "assistant", "content": None,
         "tool_calls": [{"id": "c2", "type": "function",
                         "function": {"name": "g", "arguments": "{}"}}]},
        {"role": "tool", "tool_call_id": "c1", "content": "1"},
        {"role": "tool", "tool_call_id": "c2", "content": "2"},
    ],
})
check("adjacent tool calls merged",
      len([m for m in split["messages"] if m.get("tool_calls")]) == 1,
      split["messages"])

expect_raises("empty messages rejected", C.CodeArtsBadRequest,
              lambda: C.build_chat_body({"model": "GLM-5.2", "messages": []}))

print("== CodeArtsAccount ==")
acct = C.CodeArtsAccount({"base_url": "http://x:7866"})
check("uid", acct.uid == "codearts")
check("realm", acct.realm == "codearts")
check("chat base", acct.chat_base_url() == "http://x:7866")
check("chat path", acct.chat_path == "/v1/chat/completions")
check("proxy none", acct.proxy is None)
check("model not disabled", acct.model_disabled("anything") is False)
check("not cli", acct.is_cli_identity() is False)

print("== open_chat ==")
server, base, state = start_server()
try:
    cfg = C.load_config(ACCOUNTS)
    cfg.update({"base_url": base, "api_key": "test-key", "enabled": True})
    req = {"model": "GLM-5.2", "messages": [{"role": "user", "content": "hi"}]}
    body = C.build_chat_body(req, "sess-1")
    resp = C.open_chat(cfg, body)
    raw = resp.read().decode("utf-8")
    resp.close()
    check("auth header", state.last_auth == "Bearer test-key", state.last_auth)
    check("upstream got stream", state.last_body.get("stream") is True)
    check("upstream got usage", state.last_body.get("stream_options") ==
          {"include_usage": True})
    check("upstream got conversation",
          state.last_body.get("conversation_id") ==
          hashlib.sha256(b"sess-1").hexdigest()[:32])
    check("sse has text", "你好" in raw, raw[:200])
    check("sse has usage", '"usage"' in raw, raw[:200])
    check("sse done", "[DONE]" in raw)

    state.chat_error = (401, {"error": {"message": "bad key test-key"}})
    err = expect_raises("401 -> auth error", C.CodeArtsAuthError,
                        lambda: C.open_chat(cfg, body))
    if err:
        check("auth error hides key", "test-key" not in str(err), str(err))
    state.chat_error = (400, {"error": {"message": "bad request"}})
    expect_raises("400 -> bad request", C.CodeArtsBadRequest,
                  lambda: C.open_chat(cfg, body))
    state.chat_error = (500, {"error": {"message": "boom"}})
    err = expect_raises("500 -> generic error", C.CodeArtsError,
                        lambda: C.open_chat(cfg, body))
    if err:
        check("500 message", "boom" in str(err), str(err))
finally:
    stop_server(server)

cfg = C.load_config(ACCOUNTS)
cfg.update({"base_url": "http://127.0.0.1:1", "api_key": "test-key",
            "enabled": True})
err = expect_raises("refused -> unavailable", C.CodeArtsUnavailable,
                    lambda: C.open_chat(cfg, body))
if err:
    check("unavailable hides key", "test-key" not in str(err), str(err))

cfg_off = dict(cfg, enabled=False)
expect_raises("disabled -> unavailable", C.CodeArtsUnavailable,
              lambda: C.open_chat(cfg_off, body))

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
