"""CodeArts 客户端：配置、直连 HTTP 与模型缓存。

全部用本机假 codearts2api（tests/_fake_codearts.py），不碰外网、不碰真账号。
"""
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-codearts-client-")
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


print("== config ==")
cfg = C.load_config(ACCOUNTS)
check("defaults disabled", cfg["enabled"] is False)
check("default base url", cfg["base_url"] == "http://127.0.0.1:7866")
check("default key empty", cfg["api_key"] == "")
check("not configured", C.configured(ACCOUNTS) is False)

saved = C.save_config(ACCOUNTS, {
    "enabled": True, "base_url": "http://x:7866/", "api_key": "k1"})
check("normalizes trailing slash", saved["base_url"] == "http://x:7866", saved)
check("configured", C.configured(ACCOUNTS) is True)
check("empty key keeps old", C.save_config(ACCOUNTS, {"api_key": ""})["api_key"] == "k1")
check("masked hides key", C.masked_config(ACCOUNTS)["api_key"] == "")
check("masked reports set", C.masked_config(ACCOUNTS)["api_key_set"] is True)

for bad in ("ftp://x", "http://u:p@x", "", "http://x?q=1"):
    expect_raises("reject base_url %r" % (bad,), ValueError,
                  lambda b=bad: C.save_config(ACCOUNTS, {"base_url": b}))
check("bad base_url did not overwrite", C.load_config(ACCOUNTS)["base_url"] == "http://x:7866")

print("== models ==")
server, base, state = start_server()
try:
    cfg = C.load_config(ACCOUNTS)
    cfg.update({"base_url": base, "api_key": "test-key", "enabled": True,
                "models_ttl_seconds": 60})
    C.invalidate_models()
    first = C.models(cfg)
    check("models returns ids", [m["id"] for m in first] ==
          ["GLM-5.2", "glm-5.2", "deepseek-v4-pro-0813"])
    check("models hit once", state.models_hits == 1, state.models_hits)
    second = C.models(cfg)
    check("models cached", state.models_hits == 1, state.models_hits)
    check("cache returns same ids", [m["id"] for m in second] == [m["id"] for m in first])

    entries = C.model_entries(cfg)
    check("entries owned_by codearts", entries[0]["owned_by"] == "codearts")
    check("entries provider codearts", entries[0]["provider"] == "codearts")
    check("entries preserve case", entries[0]["id"] == "GLM-5.2")
    check("entries deduped", len(entries) == 3)

    state.fail_models = True
    stale = C.models(cfg, force=True)
    check("stale fallback", [m["id"] for m in stale] == [m["id"] for m in first])
    check("stale flag", C.models_meta(cfg)["stale"] is True)

    C.invalidate_models()
    expect_raises("no cache + failure raises", C.CodeArtsError, lambda: C.models(cfg, force=True))
    check("meta source", C.models_meta(cfg)["source"] == "codearts2api")

    state.fail_models = False
    C.invalidate_models()
    C.models(cfg)
    state.api_key = "other"
    expect_raises("wrong key -> auth error", C.CodeArtsAuthError, lambda: C.models(cfg, force=True))
finally:
    stop_server(server)

print("== unreachable ==")
cfg = C.load_config(ACCOUNTS)
cfg.update({"base_url": "http://127.0.0.1:1", "api_key": "test-key"})
err = expect_raises("connection refused -> unavailable", C.CodeArtsUnavailable,
                    lambda: C.models(cfg, force=True))
if err:
    check("unavailable message has host", "127.0.0.1:1" in str(err), str(err))
    check("unavailable message has no key", "test-key" not in str(err), str(err))

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
