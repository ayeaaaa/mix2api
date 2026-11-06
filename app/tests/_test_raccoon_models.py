"""`/v1/models?realm=raccoon`：裸 ID、Key 过滤、停用 503 与 realm=all 隔离。"""

import json
import os
import shutil
import sys
import threading
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-raccoon-models-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]

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


def check(label, cond, extra=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print("  [PASS] " + label)
    else:
        FAIL += 1
        print("  [FAIL] " + label + ("  " + str(extra) if extra else ""))


def get(url, key):
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + key})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", "replace")
        try:
            return exc.code, json.loads(raw)
        except Exception:
            return exc.code, {"raw": raw}


def reset():
    shutil.rmtree(os.path.join(ACCOUNTS, "raccoon"), ignore_errors=True)
    wb_settings.save(ACCOUNTS, {"raccoon": {"enabled": True}})
    R.invalidate_pool()
    R.invalidate_health()
    R.invalidate_models()
    wb_proxy.CURRENT_REALM = "intl"


def add_account(uid, token):
    saved = R.save_auth(
        ACCOUNTS,
        {
            "access_token": token,
            "refresh_token": "refresh-1",
            "uid": uid,
            "office_identity": "personal",
            "office_org_name": uid,
            "updated_at": int(time.time()),
        },
    )
    return R.pool_for(ACCOUNTS).add(saved)


srv, base, state = start_server()
R.BASE_URL = base

hub = wb_proxy.GatewayServer(("127.0.0.1", 0), wb_proxy.Handler)
threading.Thread(target=hub.serve_forever, daemon=True).start()
HUB = "http://127.0.0.1:%d" % hub.server_address[1]

try:
    reset()
    wb_settings.set_api_keys(
        ACCOUNTS,
        [
            {
                "key": "hub-key-1234",
                "name": "test",
                "realm": "raccoon",
            }
        ],
    )
    wb_proxy.API_KEY = None
    add_account("pu-raccoon-test", state.token)

    print("== catalog ==")
    status, payload = get(HUB + "/v1/models?realm=raccoon", "hub-key-1234")
    check("status 200", status == 200, (status, payload))
    ids = [m["id"] for m in payload.get("data") or []]
    check("bare ids", set(ids) == {"raccoon-090dd3", "sn-glm-5-3"}, ids)
    check("no provider prefix", all("/" not in mid for mid in ids), ids)
    check("realm marker", payload.get("realm") == "raccoon", payload.get("realm"))
    check(
        "catalog source",
        (payload.get("catalog") or {}).get("source") == "raccoon",
        payload.get("catalog"),
    )
    raccoon = next(m for m in payload["data"] if m["id"] == "raccoon-090dd3")
    glm = next(m for m in payload["data"] if m["id"] == "sn-glm-5-3")
    check("owned_by", raccoon.get("owned_by") == "sensetime", raccoon)
    check("provider", raccoon.get("provider") == "raccoon", raccoon)
    check(
        "vision capability",
        raccoon.get("capabilities", {}).get("vision") is True,
        raccoon,
    )
    check(
        "tool capability",
        raccoon.get("capabilities", {}).get("tool_calls") is True,
        raccoon,
    )
    check(
        "reasoning capability",
        raccoon.get("capabilities", {}).get("reasoning") is True,
        raccoon,
    )
    check(
        "no vision on text model",
        glm.get("capabilities", {}).get("vision") is False,
        glm,
    )
    check("context window", raccoon.get("context_window") == 1000000, raccoon)
    check("max tokens", raccoon.get("max_tokens") == 100000, raccoon)

    print("== key filter ==")
    wb_settings.set_filter_models_by_key(ACCOUNTS, True)
    wb_settings.set_api_keys(
        ACCOUNTS,
        [
            {
                "key": "hub-key-1234",
                "name": "test",
                "realm": "raccoon",
                "models": ["raccoon-*"],
            }
        ],
    )
    R.invalidate_models()
    status, payload = get(HUB + "/v1/models?realm=raccoon", "hub-key-1234")
    check("filtered status", status == 200, (status, payload))
    check(
        "filtered ids",
        [m["id"] for m in payload["data"]] == ["raccoon-090dd3"],
        payload.get("data"),
    )
    wb_settings.set_filter_models_by_key(ACCOUNTS, False)

    print("== realm all ==")
    # An unbound key so ?realm=all is not overridden by the key binding.
    wb_settings.set_api_keys(ACCOUNTS, [{
        "key": "hub-key-1234", "name": "test",
    }])
    original_fetch = wb_proxy.fetch_models
    original_refresh = wb_proxy.wb_modelsdev.refresh_async
    wb_proxy.fetch_models = lambda realm=None: [("workbuddy-only", {})]
    wb_proxy.wb_modelsdev.refresh_async = lambda *args, **kwargs: False
    try:
        status, payload = get(HUB + "/v1/models?realm=all", "hub-key-1234")
        check("realm all status", status == 200, (status, payload))
        all_ids = [m["id"] for m in payload.get("data") or []]
        check("realm all is WorkBuddy only", all_ids == ["workbuddy-only"], all_ids)
        check(
            "realm all excludes raccoon",
            not any(mid in ("raccoon-090dd3", "sn-glm-5-3") for mid in all_ids),
            all_ids,
        )
    finally:
        wb_proxy.fetch_models = original_fetch
        wb_proxy.wb_modelsdev.refresh_async = original_refresh

    print("== health ==")
    status, health = get(HUB + "/health", "hub-key-1234")
    check("health status", status == 200, (status, health))
    block = health.get("raccoon") or {}
    check("health configured", block.get("configured") is True, block)
    check("health accounts", block.get("accounts") == 1, block)
    check("health healthy", block.get("healthy") == 1, block)
    check("health catalog source", block.get("catalog_source") == "raccoon", block)

    print("== disabled ==")
    R.save_config(ACCOUNTS, {"enabled": False})
    R.invalidate_models()
    status, payload = get(HUB + "/v1/models?realm=raccoon", "hub-key-1234")
    check("disabled 503", status == 503, (status, payload))
finally:
    try:
        hub.shutdown()
        hub.server_close()
    except Exception:
        pass
    stop_server(srv)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
