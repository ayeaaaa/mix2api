"""Raccoon native provider foundation: config, auth, refresh, pool, views."""

import json
import os
import shutil
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-raccoon-native-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]

import wb_settings  # noqa: E402
import wb_raccoon as R  # noqa: E402
from _fake_raccoon import make_jwt, start_server, stop_server  # noqa: E402

PASS = FAIL = 0


def check(label, cond, extra=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print("  [PASS] " + label)
    else:
        FAIL += 1
        print("  [FAIL] " + label + ("  " + str(extra) if extra else ""))


def reset():
    """Fresh settings and accounts for each test; the data root is isolated."""
    shutil.rmtree(os.path.join(ACCOUNTS, "raccoon"), ignore_errors=True)
    wb_settings.save(ACCOUNTS, {})
    R.invalidate_pool()
    R.invalidate_health()


def reset_fake(expires_in=3600, refresh_token="refresh-1"):
    state.token = make_jwt(state.uid, expires_in)
    state.refresh_token = refresh_token
    state.refresh_calls = 0
    state.catalog_calls = 0
    state.exchange_calls = 0
    state.refresh_status = 200
    state.refresh_body = None
    state.catalog_status = 200
    state.exchange_status = 200
    state.exchange_body = None
    state.chat_status = 501
    state.reset_requests()


def _auth(uid, expires_in=3600, refresh="refresh-x"):
    return {
        "access_token": make_jwt(uid, expires_in),
        "refresh_token": refresh,
    }


def test_config_defaults_and_clamping():
    reset()
    cfg = R.load_config(ACCOUNTS)
    check("default enabled false", cfg["enabled"] is False)
    check("default timeout 60", cfg["timeout_seconds"] == 60.0)
    check("default stream timeout 900", cfg["stream_timeout_seconds"] == 900.0)
    check("default models ttl 600", cfg["models_ttl_seconds"] == 600.0)
    check("default max concurrent 1", cfg["max_concurrent"] == 1)
    check("accounts dir attached", cfg["accounts_dir"] == ACCOUNTS)
    check("configured false", R.configured(ACCOUNTS) is False)
    check("configured_config false", R.configured_config(cfg) is False)

    wb_settings.save(ACCOUNTS, {"unrelated": {"keep": [1, 2]}})
    saved = R.save_config(
        ACCOUNTS,
        {
            "enabled": True,
            "timeout_seconds": 2,
            "stream_timeout_seconds": 999999,
            "models_ttl_seconds": 1,
            "max_concurrent": 99,
        },
    )
    check("save clamps timeout low end", saved["timeout_seconds"] == 2.0)
    check("save clamps stream high end", saved["stream_timeout_seconds"] == 3600.0)
    check("save clamps models ttl low end", saved["models_ttl_seconds"] == 60.0)
    check("save clamps max concurrent high end", saved["max_concurrent"] == 10)
    check("save enables", R.configured(ACCOUNTS) is True)
    check(
        "save preserves unrelated settings",
        wb_settings.load(ACCOUNTS).get("unrelated") == {"keep": [1, 2]},
    )

    saved = R.save_config(
        ACCOUNTS,
        {
            "timeout_seconds": "not-a-number",
            "stream_timeout_seconds": None,
            "models_ttl_seconds": {},
            "max_concurrent": "many",
        },
    )
    check("invalid timeout falls back", saved["timeout_seconds"] == 60.0)
    check("invalid stream timeout falls back", saved["stream_timeout_seconds"] == 900.0)
    check("invalid models ttl falls back", saved["models_ttl_seconds"] == 600.0)
    check("invalid max concurrent falls back", saved["max_concurrent"] == 1)

    # load_config must clamp hand-edited settings too, not only save_config.
    wb_settings.save(
        ACCOUNTS,
        {
            "raccoon": {
                "timeout_seconds": -5,
                "stream_timeout_seconds": -2,
                "models_ttl_seconds": 1000000000,
                "max_concurrent": 0,
            }
        },
    )
    cfg = R.load_config(ACCOUNTS)
    check("load clamps timeout", cfg["timeout_seconds"] == 1.0)
    check("load clamps stream timeout", cfg["stream_timeout_seconds"] == 1.0)
    check("load clamps models ttl", cfg["models_ttl_seconds"] == 86400.0)
    check("load clamps max concurrent", cfg["max_concurrent"] == 1)
    masked = R.masked_config(ACCOUNTS)
    check("masked has no accounts dir", "accounts_dir" not in masked)
    check("masked configured flag", masked["configured"] is False)
    check("masked account count", masked["accounts"] == 0)


def test_import_auth_derives_uid_and_auto_enables():
    reset()
    R.save_config(ACCOUNTS, {"enabled": False})
    token = make_jwt("pu-import-1", 3600)
    result = R.import_auth(
        R.load_config(ACCOUNTS),
        {
            "auth": {
                "access_token": "Bearer " + token,
                "refresh_token": "rt-import-1",
                "office_identity": "personal",
                "office_org_name": "小浣熊 A",
            }
        },
    )
    blob = json.dumps(result, ensure_ascii=False)
    check("import result has no access token key", "access_token" not in blob)
    check("import result has no refresh token key", "refresh_token" not in blob)
    check("import does not echo token", token not in blob)
    check("import auto-enables", R.configured(ACCOUNTS) is True)
    check("import reports uid", result.get("uid") == "pu-import-1", result)

    auths = R.load_auths(ACCOUNTS)
    check("auth file saved", len(auths) == 1, auths)
    auth = auths[0]
    check("uid from jwt sub", auth.get("uid") == "pu-import-1", auth)
    check("bearer prefix stripped", auth.get("access_token") == token)
    check("org name kept", auth.get("office_org_name") == "小浣熊 A")
    check("path attached", os.path.isfile(auth.get("_path") or ""))
    if os.name == "posix":
        mode = os.stat(auth["_path"]).st_mode & 0o777
        check("auth file mode 0600", mode == 0o600, oct(mode))

    acct = R.pool_for(ACCOUNTS).get("pu-import-1")
    check("pool account exists", acct is not None)
    view = acct.view()
    check("view nickname from org", view["nickname"] == "小浣熊 A", view)
    check("view realm", view.get("realm") == "raccoon", view)
    check(
        "view token remaining positive",
        acct.token_remaining() > 3000,
        acct.token_remaining(),
    )
    check(
        "view has no token keys",
        "access_token" not in json.dumps(view)
        and "refresh_token" not in json.dumps(view),
        view,
    )

    # Opaque tokens get a deterministic hash UID instead of an empty one.
    opaque = R.auth_from_payload({"access_token": "opaque-token"})
    opaque_uid = R.uid_from_auth(opaque)
    check(
        "opaque uid has fallback prefix", opaque_uid.startswith("raccoon-"), opaque_uid
    )
    check(
        "opaque uid is deterministic",
        opaque_uid == R.uid_from_auth({"access_token": "opaque-token"}),
    )
    check("malformed jwt claims empty", R.jwt_claims("not-a-jwt") == {})


def test_import_rejects_missing_access_token():
    reset()
    cfg = R.load_config(ACCOUNTS)
    for payload in ({}, {"auth": {}}, {"auth": {"access_token": "   "}}):
        raised = False
        try:
            R.import_auth(cfg, payload)
        except R.RaccoonBadRequest:
            raised = True
        check("rejects payload %r" % (payload,), raised)
    check("rejected import saved nothing", R.load_auths(ACCOUNTS) == [])
    check("rejected import stays disabled", R.configured(ACCOUNTS) is False)


def test_refresh_rotates_tokens_and_persists():
    reset()
    reset_fake(expires_in=-10, refresh_token="refresh-seed")
    auth = R.save_auth(
        ACCOUNTS,
        R.auth_from_payload(
            {
                "access_token": state.token,
                "refresh_token": state.refresh_token,
            }
        ),
    )
    old_access = auth["access_token"]
    old_refresh = auth["refresh_token"]
    check("saved auth expired", R.expires_at(auth) < time.time())

    new = R.refresh_auth(auth)
    check("one refresh call", state.refresh_calls == 1, state.refresh_calls)
    check("access rotated", new["access_token"] != old_access)
    check("refresh rotated", new["refresh_token"] != old_refresh)
    check("uid preserved", new["uid"] == state.uid, new)
    check("new expiry in the future", R.expires_at(new) > time.time() + 3000)

    disk = R.load_auths(ACCOUNTS)[0]
    check("new access token persisted", disk["access_token"] == new["access_token"])
    check("new refresh token persisted", disk["refresh_token"] == new["refresh_token"])

    refresh_requests = [
        item for item in state.requests if item["path"] == "/api/web/auth/v1/refresh"
    ]
    check("refresh request sent", len(refresh_requests) == 1, refresh_requests)
    sent = refresh_requests[0]
    check(
        "refresh body carries refresh token",
        sent["body"].get("refresh_token") == old_refresh,
        sent,
    )
    check(
        "refresh bearer carries access token",
        sent["headers"].get("authorization") == "Bearer " + old_access,
        sent,
    )

    state.refresh_status = 401
    raised = False
    try:
        R.refresh_auth(disk)
    except R.RaccoonAuthError:
        raised = True
    check("refresh 401 raises auth error", raised)
    check(
        "failed refresh did not overwrite tokens",
        R.load_auths(ACCOUNTS)[0]["access_token"] == new["access_token"],
    )
    state.refresh_status = 200


def test_concurrent_refresh_single_flight():
    reset()
    reset_fake(expires_in=-120, refresh_token="refresh-concurrent")
    auth = R.save_auth(
        ACCOUNTS,
        R.auth_from_payload(
            {
                "access_token": state.token,
                "refresh_token": state.refresh_token,
            }
        ),
    )
    acct = R.pool_for(ACCOUNTS).add(auth)
    check("account starts expired", acct.expires_at() < time.time())

    errors = []
    barrier = threading.Barrier(8)

    def worker():
        barrier.wait()
        try:
            R.ensure_token(acct)
        except Exception as exc:  # noqa: BLE001 - reported by the test
            errors.append(repr(exc))

    threads = [threading.Thread(target=worker) for _ in range(8)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(15)
    check("all refreshers finished", all(not t.is_alive() for t in threads))
    check("no refresher raised", errors == [], errors)
    check("exactly one refresh", state.refresh_calls == 1, state.refresh_calls)
    check("account token advanced", acct.expires_at() > time.time() + 3000)
    disk = R.load_auths(ACCOUNTS)[0]
    check("disk token advanced", R.expires_at(disk) > time.time() + 3000)

    R.ensure_token(acct)
    check(
        "fresh token does not refresh again",
        state.refresh_calls == 1,
        state.refresh_calls,
    )


def test_pool_state_and_actions():
    reset()
    pool = R.pool_for(ACCOUNTS)
    a = pool.add(
        R.save_auth(ACCOUNTS, R.auth_from_payload(_auth("pu-a", refresh="rt-a")))
    )
    b = pool.add(
        R.save_auth(ACCOUNTS, R.auth_from_payload(_auth("pu-b", refresh="rt-b")))
    )
    check("two accounts", len(pool.accounts) == 2)
    check("get by uid", pool.get("pu-a") is a and pool.get("pu-b") is b)
    check("stats total", pool.stats()["total"] == 2, pool.stats())

    check("pick allowed", pool.pick("m", allowed_accounts={"pu-a"}) is a)
    check("pick excludes", pool.pick("m", exclude={"pu-a"}) is b)
    check(
        "pick allowed and excluded",
        pool.pick("m", allowed_accounts={"pu-a"}, exclude={"pu-a"}) is None,
    )

    check("acquire", pool.acquire(a) is True)
    check("pick honours concurrency", pool.pick("m", allowed_accounts={"pu-a"}) is None)
    pool.release(a)
    check("release", a.active_concurrent == 0)

    pool.bind_session("session-1", "pu-a")
    check("session affinity", pool.pick("m", session_key="session-1") is a)

    check("disable returns true", pool.disable("pu-a", "manual") is True)
    check("account disabled", a.disabled is True)
    check("stats disabled", pool.stats()["disabled"] == 1, pool.stats())
    check(
        "disabled account not picked", pool.pick("m", allowed_accounts={"pu-a"}) is None
    )
    check(
        "disable persisted",
        json.load(open(pool.state_file))["pu-a"]["disabled"] is True,
    )
    check("enable returns true", pool.enable("pu-a") is True)
    check("account enabled", a.disabled is False)

    pool.note_error(b, "rate limited", soft=True)
    check("soft error cools", b.cool_until > time.time())
    check(
        "cooling account not picked", pool.pick("m", allowed_accounts={"pu-b"}) is None
    )
    check("healthy account preferred", pool.pick("m") is a)
    check("clear cooldown", pool.clear_cooldown("pu-b") is True)
    check("cooldown cleared", b.cool_until == 0)

    check("set note", pool.set_note("pu-a", "primary") is True)
    check("set tasks", pool.set_tasks_enabled("pu-a", False) is True)
    R.invalidate_pool(ACCOUNTS)
    reloaded = R.pool_for(ACCOUNTS)
    re_a = reloaded.get("pu-a")
    check("note persisted", re_a.note == "primary", re_a.note)
    check("tasks persisted", re_a.tasks_enabled is False)
    check("list has views", len(reloaded.list()) == 2)
    check(
        "list views have no token keys",
        "access_token" not in json.dumps(reloaded.list()),
    )

    check("remove returns true", reloaded.remove("pu-b") is True)
    check("removed from pool", reloaded.get("pu-b") is None)
    check("removed file gone", R.load_auths(ACCOUNTS)[0]["uid"] == "pu-a")
    check("remove unknown false", reloaded.remove("pu-b") is False)


def test_proxy_slot_binding():
    reset()
    slots = wb_settings.set_proxy_slots(
        ACCOUNTS,
        [
            {
                "id": "slot-1",
                "name": "one",
                "url": "http://127.0.0.1:7890",
                "enabled": True,
            },
            {
                "id": "slot-2",
                "name": "two",
                "url": "http://127.0.0.1:7891",
                "enabled": False,
            },
        ],
    )
    pool = R.pool_for(ACCOUNTS)
    auth = R.save_auth(
        ACCOUNTS,
        R.auth_from_payload(
            {
                "access_token": make_jwt("pu-slot", 3600),
                "refresh_token": "rt-slot",
                "proxySlot": "slot-1",
            }
        ),
    )
    acct = pool.add(auth)

    pool.apply_proxy_slots(slots)
    check("enabled slot resolves", acct.proxy == "http://127.0.0.1:7890", acct.proxy)
    acct.proxy_slot = "slot-2"
    pool.apply_proxy_slots(slots)
    check("disabled slot falls back direct", acct.proxy == "", acct.proxy)
    acct.proxy_slot = "slot-missing"
    pool.apply_proxy_slots(slots)
    check("unknown slot falls back direct", acct.proxy == "", acct.proxy)
    acct.proxy_slot = ""
    pool.apply_proxy_slots(slots)
    check("unbound slot is direct", acct.proxy == "", acct.proxy)

    acct.proxy_slot = "slot-1"
    pool.apply_proxy_slots()
    check(
        "defaults to settings slots", acct.proxy == "http://127.0.0.1:7890", acct.proxy
    )
    check(
        "set proxy slot returns true", pool.set_proxy_slot("pu-slot", "slot-2") is True
    )
    check("rebound proxy is direct", acct.proxy == "", acct.proxy)
    persisted = R.load_auths(ACCOUNTS)[0]
    check(
        "proxy binding persisted",
        persisted.get("proxySlot") == "slot-2",
        persisted.get("proxySlot"),
    )
    wb_settings.set_proxy_slots(ACCOUNTS, [])


def test_view_never_contains_secret():
    reset()
    access = make_jwt("pu-secret", 3600)
    refresh = "refresh-secret-value"
    R.import_auth(
        R.load_config(ACCOUNTS),
        {
            "access_token": access,
            "refresh_token": refresh,
            "office_org_name": "Secret Test",
        },
    )
    acct = R.pool_for(ACCOUNTS).get("pu-secret")
    blobs = [
        acct.view(),
        R.pool_for(ACCOUNTS).list(),
        R.overview(R.load_config(ACCOUNTS)),
        R.health(R.load_config(ACCOUNTS), force=True),
        R.masked_config(ACCOUNTS),
    ]
    blob = json.dumps(blobs, ensure_ascii=False)
    check("no access_token key", "access_token" not in blob)
    check("no refresh_token key", "refresh_token" not in blob)
    check("no access token value", access not in blob)
    check("no refresh token value", refresh not in blob)
    check(
        "overview has base url",
        R.overview(R.load_config(ACCOUNTS))["base_url"] == R.BASE_URL,
    )
    check(
        "health configured",
        R.health(R.load_config(ACCOUNTS), force=True)["configured"] is True,
    )


def test_terminal_error_marks_disabled_and_soft_marks_cooldown():
    reset()
    pool = R.pool_for(ACCOUNTS)
    a = pool.add(
        R.save_auth(
            ACCOUNTS, R.auth_from_payload(_auth("pu-terminal", refresh="rt-terminal"))
        )
    )
    b = pool.add(
        R.save_auth(ACCOUNTS, R.auth_from_payload(_auth("pu-soft", refresh="rt-soft")))
    )

    pool.note_error(a, "re-login required", terminal=True)
    check("terminal disables", a.disabled is True)
    check("terminal reason", "re-login required" in a.view()["reason"])
    check(
        "terminal not picked", pool.pick("m", allowed_accounts={"pu-terminal"}) is None
    )

    pool.note_error(b, "rate limited", soft=True)
    check("soft does not disable", b.disabled is False)
    check("soft sets cooldown", b.cool_until > time.time())
    check(
        "soft account not picked", pool.pick("m", allowed_accounts={"pu-soft"}) is None
    )
    check(
        "terminal and cooling stats",
        pool.stats() == {"total": 2, "healthy": 0, "disabled": 1, "cooling": 1},
        pool.stats(),
    )

    R.invalidate_pool(ACCOUNTS)
    reloaded = R.pool_for(ACCOUNTS)
    check("terminal persisted", reloaded.get("pu-terminal").disabled is True)
    check("soft cooldown persisted", reloaded.get("pu-soft").cool_until > time.time())

    soft = reloaded.get("pu-soft")
    reloaded.note_success(soft)
    check("success clears cooldown", soft.cool_until == 0)
    check("success clears error", soft.last_error == "")

    for _ in range(3):
        reloaded.note_error(soft, "temporary failure")
    check("repeated errors cool down", soft.cool_until > time.time(), soft.cool_until)


try:
    srv, base, state = start_server()
    R.BASE_URL = base
    print("== config ==")
    test_config_defaults_and_clamping()
    print("== import ==")
    test_import_auth_derives_uid_and_auto_enables()
    test_import_rejects_missing_access_token()
    print("== refresh ==")
    test_refresh_rotates_tokens_and_persists()
    test_concurrent_refresh_single_flight()
    print("== pool ==")
    test_pool_state_and_actions()
    test_proxy_slot_binding()
    print("== views ==")
    test_view_never_contains_secret()
    test_terminal_error_marks_disabled_and_soft_marks_cooldown()
finally:
    stop_server(srv)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
