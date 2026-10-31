"""`/codearts/*` 管理代理：面板鉴权、账号 CRUD、OAuth、设置保存。"""
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

_TMP = isolated_data_dirs("wb-codearts-admin-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]

import wb_codearts  # noqa: E402
import wb_proxy  # noqa: E402
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


def call(method, path, payload=None, token=None, key=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["X-Panel-Token"] = token
    if key:
        headers["Authorization"] = "Bearer " + key
    data = None if payload is None else json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(HUB + path, data=data, headers=headers,
                                 method=method)
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            raw = resp.read().decode("utf-8", "replace")
            return resp.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", "replace")
        try:
            return exc.code, json.loads(raw)
        except Exception:
            return exc.code, {"raw": raw}


fake, fake_base, state = start_server()
hub = wb_proxy.GatewayServer(("127.0.0.1", 0), wb_proxy.Handler)
threading.Thread(target=hub.serve_forever, daemon=True).start()
HUB = "http://127.0.0.1:%d" % hub.server_address[1]

try:
    wb_proxy.POOL = None
    wb_proxy.API_KEY = None
    state.api_key = "super-secret-key"
    wb_codearts.save_config(ACCOUNTS, {
        "enabled": True, "base_url": fake_base,
        "api_key": "super-secret-key",
    })

    status, body = call("POST", "/panel/login", {"password": "admin"})
    check("panel login", status == 200 and body.get("token"), (status, body))
    token = body.get("token")

    print("== auth ==")
    status, body = call("GET", "/codearts/overview")
    check("overview without panel -> 401", status == 401, (status, body))
    status, body = call("POST", "/codearts/accounts/delete", {"uid": "u1"},
                        key="any-key")
    check("delete with api key only -> 401", status == 401, (status, body))
    status, body = call("GET", "/codearts/overview", token=token)
    check("overview with panel", status == 200, (status, body))
    check("overview accounts", len(body.get("accounts") or []) == 2, body)
    check("overview online", body.get("online") is True, body)
    check("overview base_url", body.get("base_url") == fake_base, body)
    check("overview hides key", "super-secret-key" not in json.dumps(body), body)

    print("== account actions ==")
    for action, path in (("enable", "/codearts/accounts/enable"),
                         ("disable", "/codearts/accounts/disable"),
                         ("clear-cooldown", "/codearts/accounts/clear-cooldown"),
                         ("delete", "/codearts/accounts/delete"),
                         ("checkin", "/codearts/accounts/checkin")):
        status, body = call("POST", path, {"uid": "u1"}, token=token)
        check("%s ok" % action, status == 200 and body.get("ok") is True,
              (status, body))
        check("%s uid sent" % action, state.last_body.get("uid") == "u1",
              state.last_body)

    status, body = call("POST", "/codearts/accounts/enable", {}, token=token)
    check("missing uid -> 400", status == 400, (status, body))

    status, body = call("POST", "/codearts/keepalive", {}, token=token)
    check("keepalive", status == 200, (status, body))

    status, body = call("POST", "/codearts/models/refresh", {}, token=token)
    check("models refresh", status == 200 and body.get("models") == 3,
          (status, body))

    print("== oauth ==")
    status, body = call("POST", "/codearts/accounts/oauth/start", {}, token=token)
    check("oauth start", status == 200 and body.get("session_id") == "sess-1",
          (status, body))
    status, body = call("POST", "/codearts/accounts/oauth/poll",
                        {"session_id": "sess-1"}, token=token)
    check("oauth poll", status == 200 and body.get("status") == "pending",
          (status, body))
    status, body = call("POST", "/codearts/accounts/oauth/callback",
                        {"session_id": "sess-1",
                         "callback_url": "http://127.0.0.1/oauth/callback?code=x"},
                        token=token)
    check("oauth callback continue", status == 200 and
          body.get("status") == "continue", (status, body))
    check("oauth next_url passthrough", body.get("next_url") ==
          "https://example.test/next", body)

    print("== delete old upstream ==")
    state.delete_status = 404
    state.delete_error = "404 page not found"
    status, body = call("POST", "/codearts/accounts/delete", {"uid": "u1"},
                        token=token)
    check("delete route 404 -> 409", status == 409, (status, body))
    check("delete 409 message", "版本过旧" in json.dumps(body, ensure_ascii=False),
          body)
    state.delete_error = "account not found"
    status, body = call("POST", "/codearts/accounts/delete", {"uid": "nope"},
                        token=token)
    check("delete missing account -> 404", status == 404, (status, body))
    state.delete_status = 200

    print("== settings ==")
    status, body = call("GET", "/settings", token=token)
    check("settings has codearts", isinstance(body.get("codearts"), dict), body)
    check("settings hides key", "super-secret-key" not in json.dumps(body), body)
    check("settings key set", body["codearts"].get("api_key_set") is True, body)

    status, body = call("POST", "/settings/save", {
        "codearts": {"enabled": True, "base_url": fake_base,
                     "api_key": "", "models_ttl_seconds": 30},
    }, token=token)
    check("settings save", status == 200, (status, body))
    check("settings save keeps key", body["codearts"].get("api_key_set") is True,
          body)
    check("settings save ttl", body["codearts"].get("models_ttl_seconds") == 30,
          body)

    status, body = call("POST", "/realm", {"realm": "codearts"}, token=token)
    check("realm switch to codearts", status == 200 and
          body.get("current") == "codearts", (status, body))
    status, body = call("POST", "/realm", {"realm": "intl"}, token=token)
    check("realm switch back to intl", status == 200 and
          body.get("current") == "intl", (status, body))

    wb_proxy.CURRENT_REALM = "codearts"
    status, body = call("POST", "/settings/save", {
        "codearts": {"enabled": False},
    }, token=token)
    check("disable save", status == 200, (status, body))
    check("disable warning", "warning" in body, body)
    check("disable resets realm", wb_proxy.CURRENT_REALM == "intl",
          wb_proxy.CURRENT_REALM)

    status, body = call("POST", "/settings/save", {
        "codearts": {"base_url": "ftp://bad"},
    }, token=token)
    check("bad base url -> 400", status == 400, (status, body))
finally:
    try:
        hub.shutdown()
        hub.server_close()
    except Exception:
        pass
    stop_server(fake)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
