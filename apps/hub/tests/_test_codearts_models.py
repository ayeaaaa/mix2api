"""`/v1/models?realm=codearts`：目录、Key 过滤、缓存与失败映射。"""
import json
import os
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

_TMP = isolated_data_dirs("wb-codearts-models-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]

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


def get(url, key):
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + key})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", "replace")
        try:
            return exc.code, json.loads(raw)
        except Exception:
            return exc.code, {"raw": raw}


fake, fake_base, state = start_server()
hub = wb_proxy.GatewayServer(("127.0.0.1", 0), wb_proxy.Handler)
threading.Thread(target=hub.serve_forever, daemon=True).start()
hub_base = "http://127.0.0.1:%d" % hub.server_address[1]

try:
    wb_settings.set_api_keys(ACCOUNTS, [{
        "key": "hub-key-1234", "name": "test", "realm": "codearts",
    }])
    wb_proxy.API_KEY = None
    wb_codearts.save_config(ACCOUNTS, {
        "enabled": True, "base_url": fake_base, "api_key": "test-key",
        "models_ttl_seconds": 1,
    })
    wb_codearts.invalidate_models()

    status, payload = get(hub_base + "/v1/models?realm=codearts", "hub-key-1234")
    check("status 200", status == 200, (status, payload))
    check("realm codearts", payload.get("realm") == "codearts", payload)
    check("ids", [m["id"] for m in payload["data"]] ==
          ["GLM-5.2", "glm-5.2", "deepseek-v4-pro-0813"], payload)
    check("owned_by", payload["data"][0]["owned_by"] == "codearts")
    check("catalog source", payload["catalog"]["source"] == "codearts2api")
    check("catalog not stale", payload["catalog"]["stale"] is False)

    # Key 模型限制
    wb_settings.set_filter_models_by_key(ACCOUNTS, True)
    wb_settings.set_api_keys(ACCOUNTS, [{
        "key": "hub-key-1234", "name": "test", "realm": "codearts",
        "models": ["glm-*"],
    }])
    status, payload = get(hub_base + "/v1/models?realm=codearts", "hub-key-1234")
    check("filtered status", status == 200, (status, payload))
    check("filtered ids", [m["id"] for m in payload["data"]] ==
          ["GLM-5.2", "glm-5.2"], payload)
    wb_settings.set_filter_models_by_key(ACCOUNTS, False)

    # stale：TTL 过期后上游 500，仍返回旧目录
    time.sleep(1.2)
    state.fail_models = True
    status, payload = get(hub_base + "/v1/models?realm=codearts", "hub-key-1234")
    check("stale status", status == 200, (status, payload))
    check("stale flag", payload["catalog"]["stale"] is True, payload.get("catalog"))
    check("stale ids", len(payload["data"]) == 3)

    # 无缓存 + 不可达 -> 502
    state.fail_models = False
    wb_codearts.save_config(ACCOUNTS, {"base_url": "http://127.0.0.1:1"})
    wb_codearts.invalidate_models()
    status, payload = get(hub_base + "/v1/models?realm=codearts", "hub-key-1234")
    check("unreachable 502", status == 502, (status, payload))
    check("unreachable message", "CodeArts" in json.dumps(payload, ensure_ascii=False),
          payload)

    # 未启用 -> 503
    wb_codearts.save_config(ACCOUNTS, {"enabled": False})
    status, payload = get(hub_base + "/v1/models?realm=codearts", "hub-key-1234")
    check("disabled 503", status == 503, (status, payload))
finally:
    try:
        hub.shutdown()
        hub.server_close()
    except Exception:
        pass
    stop_server(fake)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
