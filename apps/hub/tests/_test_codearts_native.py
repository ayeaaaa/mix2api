"""单进程 CodeArts：配置、账号文件、账号池、模型、续期、签到、OAuth。"""
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-codearts-native-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]

import wb_codearts as C  # noqa: E402
import wb_codearts_crypto as crypto  # noqa: E402
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


def make_auth(state, uid="u1", name="账号一", expiration=None):
    return {
        "user_id": uid,
        "user_name": name,
        "domain_id": "d1",
        "cloud_dragon_token": state.token,
        "access_key_id": state.api_key,
        "secret_access_key": state.secret_key,
        "expiration": expiration or state.expiration,
        "refresh_token": state.refresh_token,
        "code_verifier": "verifier",
        "client_id": C.CLIENT_ID,
        "dpop_private_key": crypto.jwk_from_keypair(*crypto.generate_keypair()),
        "updated_at": int(time.time()),
    }


srv, base, state = start_server()
C.SNAP_HOST = base
C.SNAP_MANAGER = base + "/snap-manager"
C.STS_HOST = base
C.BENEFIT_HOST = base
C.PORTAL_HOST = base + "/portal"

try:
    print("== config ==")
    cfg = C.load_config(ACCOUNTS)
    check("default disabled", cfg["enabled"] is False)
    check("default max concurrent", cfg["max_concurrent"] == 5)
    C.save_config(ACCOUNTS, {"enabled": True, "max_concurrent": 2})
    check("enabled", C.configured(ACCOUNTS) is True)
    check("max concurrent saved", C.load_config(ACCOUNTS)["max_concurrent"] == 2)
    check("masked no secrets", "api_key" not in C.masked_config(ACCOUNTS))

    print("== auth storage ==")
    auth = make_auth(state)
    saved = C.save_auth(ACCOUNTS, auth)
    check("auth file exists", os.path.isfile(saved["_path"]), saved["_path"])
    loaded = C.load_auths(ACCOUNTS)
    check("auth loaded", len(loaded) == 1 and loaded[0]["user_id"] == "u1")
    check("dpop persisted", isinstance(loaded[0].get("dpop_private_key"), dict))

    print("== pool ==")
    pool = C.pool_for(ACCOUNTS)
    acct = pool.add(saved)
    check("pool has account", pool.get("u1") is acct)
    check("stats healthy", pool.stats()["healthy"] == 1, pool.stats())
    check("pick", pool.pick("GLM-5.2") is acct)
    check("acquire", pool.acquire(acct) is True)
    check("second acquire", pool.acquire(acct) is True)
    check("third acquire blocked at max 2", pool.acquire(acct) is False)
    pool.release(acct)
    pool.release(acct)
    check("release", acct.active_concurrent == 0)
    pool.disable("u1", "manual")
    check("disabled", acct.disabled is True and pool.stats()["disabled"] == 1)
    pool.enable("u1")
    check("enabled again", acct.disabled is False)
    pool.note_error(acct, "boom")
    pool.note_error(acct, "boom")
    pool.note_error(acct, "boom")
    check("cooldown after threshold", acct.cool_until > time.time())
    pool.clear_cooldown("u1")
    check("cooldown cleared", acct.cool_until == 0)

    print("== models ==")
    models = C.fetch_models(acct)
    ids = [m["id"] for m in models]
    check("agent/builtin/benefit merged", set(ids) ==
          {"GLM-5.2", "GLM-4.7", "deepseek-v4-pro-0813"}, ids)
    check("benefit marked", acct.is_benefit("deepseek-v4-pro-0813") is True)
    check("normal not benefit", acct.is_benefit("GLM-5.2") is False)
    entries = C.model_entries(C.load_config(ACCOUNTS))
    check("entries owned_by", entries[0]["owned_by"] == "codearts")
    check("entries benefit flag",
          any(e.get("benefit") for e in entries if e["id"] == "deepseek-v4-pro-0813"))
    check("models meta source", C.models_meta(C.load_config(ACCOUNTS))["source"] == "codearts")

    print("== refresh ==")
    acct.auth["expiration"] = time.strftime(
        "%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 10))
    before = state.token_calls
    C._ensure_token(acct)
    check("refresh called", state.token_calls > before)
    check("token restored", acct.expires_at() > time.time())
    check("auth file rewritten", C.load_auths(ACCOUNTS)[0]["expiration"] ==
          acct.auth["expiration"])

    print("== checkin ==")
    result = C.checkin(C.load_config(ACCOUNTS))
    check("checkin ok", result["ok"] is True, result)
    check("checkin credits", result["credits"] == 1000, result)
    check("checkin stored", acct.checkin and acct.checkin.get("ok") is True)

    print("== oauth ==")
    started = C.oauth_start(C.load_config(ACCOUNTS), port=8788)
    check("oauth session", bool(started.get("session_id")))
    check("oauth url", "authorize" in started["auth_url"] and
          "code_challenge_method=SHA-256" in started["auth_url"])
    pending = C.oauth_poll(C.load_config(ACCOUNTS), started["session_id"])
    check("oauth pending", pending["status"] == "pending", pending)
    done = C.oauth_callback(
        C.load_config(ACCOUNTS), started["session_id"],
        "http://127.0.0.1:8788/oauth/callback?code=abc")
    check("oauth done", done["status"] == "done", done)
    check("oauth account saved", C.pool_for(ACCOUNTS).get("u1") is not None)

    started2 = C.oauth_start(C.load_config(ACCOUNTS), port=8788)
    cont = C.oauth_callback(
        C.load_config(ACCOUNTS), started2["session_id"],
        "http://127.0.0.1:8788/oauth/callback?secret=s1&redirect="
        + __import__("urllib.parse").parse.quote("https://codearts.huaweicloud.com/portal/x?ticket_id="
                                                  + started2["session_id"], safe=""))
    check("oauth continue", cont["status"] == "continue", cont)
    check("oauth next_url", cont["next_url"].startswith("https://"), cont)

    print("== overview ==")
    ov = C.overview(C.load_config(ACCOUNTS))
    check("overview service", ov["service"] == "codearts")
    check("overview accounts", len(ov["accounts"]) >= 1, ov)
    check("overview models", len(ov["models"]) >= 1, ov)
    check("health enabled", C.health(C.load_config(ACCOUNTS))["ok"] is True)
finally:
    stop_server(srv)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
