# -*- coding: utf-8 -*-
"""`/raccoon/*` admin surface: panel gate, import, loopback OAuth and actions."""

import json
import os
import shutil
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-raccoon-admin-")
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


def call(method, path, payload=None, token=None, key=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["X-Panel-Token"] = token
    if key:
        headers["Authorization"] = "Bearer " + key
    data = None if payload is None else json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(HUB + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            raw = resp.read().decode("utf-8", "replace")
            return resp.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", "replace")
        try:
            return exc.code, json.loads(raw)
        except Exception:
            return exc.code, {"raw": raw}


def get_html(path):
    try:
        with urllib.request.urlopen(HUB + path, timeout=20) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")


def get_html_with_headers(path):
    try:
        with urllib.request.urlopen(HUB + path, timeout=20) as resp:
            return resp.status, resp.read().decode("utf-8", "replace"), resp.headers
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace"), exc.headers


def reset_accounts():
    shutil.rmtree(os.path.join(ACCOUNTS, "raccoon"), ignore_errors=True)
    wb_settings.save(ACCOUNTS, {"raccoon": {"enabled": False}})
    R.invalidate_pool()
    R.invalidate_health()
    R.invalidate_models()
    lock = getattr(R, "_OAUTH_LOCK", None)
    sessions = getattr(R, "_OAUTH_SESSIONS", None)
    if lock is not None and sessions is not None:
        with lock:
            sessions.clear()


def reset_fake():
    state.token = make_jwt(state.uid, 3600)
    state.refresh_token = "refresh-1"
    state.refresh_calls = 0
    state.catalog_calls = 0
    state.exchange_calls = 0
    state.refresh_status = 200
    state.refresh_body = None
    state.catalog_status = 200
    state.catalog_error = 0
    state.catalog_error_body = None
    state.exchange_status = 200
    state.exchange_body = None
    state.exchange_error = None
    state.catalog = default_catalog()
    state.office_identity = "personal"
    state.office_org_name = "Raccoon Test"
    state.code = "test-code"
    state.reset_requests()


def start_oauth():
    status, body = call("POST", "/raccoon/accounts/oauth/start", {}, token=TOKEN)
    check("oauth start status", status == 200, (status, body))
    query = urllib.parse.parse_qs(urllib.parse.urlsplit(body.get("auth_url", "")).query)
    return body, {
        "state": (query.get("state") or [""])[0],
        "redirect": (query.get("redirect") or [""])[0],
        "session_id": body.get("session_id"),
    }


def import_current_account(uid=None, expires_in=3600):
    uid = uid or state.uid
    token = state.token if uid == state.uid else make_jwt(uid, expires_in)
    status, body = call(
        "POST",
        "/raccoon/accounts/import",
        {
            "auth": {
                "access_token": token,
                "refresh_token": state.refresh_token,
                "office_identity": "personal",
                "office_org_name": "Admin One",
            }
        },
        token=TOKEN,
    )
    check("import status", status == 200, (status, body))
    return body


def test_overview_and_accounts_require_panel_session():
    print("== auth gate ==")
    reset_accounts()
    for path in ("/raccoon/overview", "/raccoon/accounts"):
        status, body = call("GET", path)
        check("%s without panel -> 401" % path, status == 401, (status, body))
    status, body = call(
        "POST", "/raccoon/accounts/delete", {"uid": "nope"}, key="any-key"
    )
    check("write with api key only -> 401", status == 401, (status, body))
    status, body = call("GET", "/raccoon/overview", token=TOKEN)
    check("overview with panel -> 200", status == 200, (status, body))
    status, body = call("GET", "/raccoon/accounts", token=TOKEN)
    check("accounts with panel -> 200", status == 200, (status, body))


def test_overview_never_leaks_tokens():
    print("== overview ==")
    reset_accounts()
    reset_fake()
    refresh = "refresh-visible-secret"
    state.refresh_token = refresh
    access = state.token
    status, body = call(
        "POST",
        "/raccoon/accounts/import",
        {
            "access_token": access,
            "refresh_token": refresh,
            "office_org_name": "Visible",
        },
        token=TOKEN,
    )
    check(
        "flat import accepted",
        status == 200 and body.get("uid") == state.uid,
        (status, body),
    )
    check("import auto-enables realm", R.configured(ACCOUNTS) is True)
    for path in ("/raccoon/overview", "/raccoon/accounts"):
        status, body = call("GET", path, token=TOKEN)
        blob = json.dumps(body, ensure_ascii=False)
        check("%s provider" % path, body.get("provider") == "raccoon", body)
        check("%s has accounts" % path, len(body.get("accounts") or []) == 1, body)
        check("%s has stats" % path, isinstance(body.get("stats"), dict), body)
        check("%s has models" % path, isinstance(body.get("models"), list), body)
        check("%s has base url" % path, body.get("base_url") == R.BASE_URL, body)
        check("%s no access token" % path, access not in blob, body)
        check("%s no refresh token" % path, refresh not in blob, body)
        check(
            "%s no token keys" % path,
            "access_token" not in blob and "refresh_token" not in blob,
            body,
        )
    overview = call("GET", "/raccoon/overview", token=TOKEN)[1]
    check("overview health block", isinstance(overview.get("health"), dict), overview)
    check("overview online", overview.get("online") is True, overview)


def test_oauth_start_builds_loopback_url_and_state():
    print("== oauth start ==")
    reset_accounts()
    reset_fake()
    body, info = start_oauth()
    parsed = urllib.parse.urlsplit(body.get("auth_url", ""))
    query = urllib.parse.parse_qs(parsed.query)
    check("authorize scheme", parsed.scheme == "https", parsed)
    check("authorize host", parsed.hostname == "xiaohuanxiong.com", parsed)
    check("authorize path", parsed.path == "/authorize", parsed)
    check("login source", (query.get("login_source") or [""])[0] == "desktop", query)
    check("app name", (query.get("appname") or [""])[0] == "办公小浣熊客户端", query)
    check(
        "desktop callback",
        (query.get("desktop_callback") or [""])[0] == "loopback",
        query,
    )
    check(
        "redirect is the gateway loopback",
        info["redirect"] == "http://127.0.0.1:%d/raccoon/oauth/callback" % HUB_PORT,
        info["redirect"],
    )
    check(
        "state is 32-byte hex",
        len(info["state"]) == 64
        and all(ch in "0123456789abcdef" for ch in info["state"]),
        info["state"],
    )
    check("session id present", bool(info["session_id"]), body)
    blob = json.dumps(body, ensure_ascii=False)
    check(
        "start has no code/token",
        "access_token" not in blob and "refresh_token" not in blob,
        body,
    )
    status, poll = call(
        "POST",
        "/raccoon/accounts/oauth/poll",
        {"session_id": info["session_id"]},
        token=TOKEN,
    )
    check(
        "poll pending",
        status == 200 and poll.get("status") == "pending",
        (status, poll),
    )
    check("poll has auth url", poll.get("auth_url") == body.get("auth_url"), poll)
    raised = False
    try:
        R.oauth_start(R.load_config(ACCOUNTS), 80)
    except R.RaccoonBadRequest:
        raised = True
    check("oauth start rejects privileged ports", raised)


def test_oauth_callback_creates_account_and_poll_reports_done():
    print("== oauth callback ==")
    reset_accounts()
    reset_fake()
    state.code = "oauth-code-success"
    body, info = start_oauth()
    path = "/raccoon/oauth/callback?" + urllib.parse.urlencode(
        {"code": state.code, "state": info["state"]}
    )
    status, html = get_html(path)
    check("callback status 200", status == 200, (status, html[:200]))
    check("callback success html", "登录成功" in html, html[:200])
    check("callback html no code", state.code not in html, html[:200])
    check(
        "callback exchange called once", state.exchange_calls == 1, state.exchange_calls
    )
    check("callback saved account", R.pool_for(ACCOUNTS).get(state.uid) is not None)
    check("callback auto-enables", R.configured(ACCOUNTS) is True)
    status, poll = call(
        "POST",
        "/raccoon/accounts/oauth/poll",
        {"session_id": info["session_id"]},
        token=TOKEN,
    )
    check("poll done", status == 200 and poll.get("status") == "done", (status, poll))
    check("poll has account", (poll.get("account") or {}).get("uid") == state.uid, poll)
    blob = json.dumps(poll, ensure_ascii=False)
    check(
        "poll has no token",
        state.token not in blob
        and "access_token" not in blob
        and "refresh_token" not in blob,
        poll,
    )


def test_oauth_callback_rejects_bad_state_replay_and_expiry():
    print("== oauth state ==")
    reset_accounts()
    reset_fake()
    state.code = "oauth-code-state"
    body, info = start_oauth()
    wrong = "de" * 32
    status, html = get_html(
        "/raccoon/oauth/callback?"
        + urllib.parse.urlencode({"code": state.code, "state": wrong})
    )
    check("wrong state rejected", status == 400, (status, html[:200]))
    check(
        "wrong state did not exchange", state.exchange_calls == 0, state.exchange_calls
    )
    check("wrong state created nothing", R.load_auths(ACCOUNTS) == [])

    path = "/raccoon/oauth/callback?" + urllib.parse.urlencode(
        {"code": state.code, "state": info["state"]}
    )
    status, _ = get_html(path)
    check("first callback accepted", status == 200, status)
    saved = R.load_auths(ACCOUNTS)
    check("one account after callback", len(saved) == 1, saved)
    first_token = saved[0]["access_token"]
    status, html = get_html(path)
    check("replay rejected", status == 400, (status, html[:200]))
    check("replay did not exchange", state.exchange_calls == 1, state.exchange_calls)
    check(
        "replay did not replace account",
        R.load_auths(ACCOUNTS)[0]["access_token"] == first_token,
    )

    body, expired = start_oauth()
    session = R._OAUTH_SESSIONS[expired["session_id"]]
    session.created_at = time.time() - 301
    status, html = get_html(
        "/raccoon/oauth/callback?"
        + urllib.parse.urlencode({"code": state.code, "state": expired["state"]})
    )
    check("expired state rejected", status == 400, (status, html[:200]))
    check(
        "expired state did not exchange",
        state.exchange_calls == 1,
        state.exchange_calls,
    )
    check("expired state created nothing else", len(R.load_auths(ACCOUNTS)) == 1)

    status, body = call(
        "POST",
        "/raccoon/accounts/oauth/callback",
        {
            "session_id": info["session_id"],
            "callback_url": "http://127.0.0.1:%d/raccoon/oauth/callback?%s"
            % (
                HUB_PORT,
                urllib.parse.urlencode({"code": state.code, "state": info["state"]}),
            ),
        },
        token=TOKEN,
    )
    check("done session callback rejected", status in (400, 409), (status, body))
    check(
        "done session did not exchange", state.exchange_calls == 1, state.exchange_calls
    )


def test_manual_callback_url_creates_account():
    print("== manual callback ==")
    reset_accounts()
    reset_fake()
    state.code = "oauth-code-manual"
    body, info = start_oauth()
    callback_url = "http://127.0.0.1:%d/raccoon/oauth/callback?%s" % (
        HUB_PORT,
        urllib.parse.urlencode({"code": state.code, "state": info["state"]}),
    )
    status, result = call(
        "POST",
        "/raccoon/accounts/oauth/callback",
        {
            "session_id": info["session_id"],
            "callback_url": callback_url,
        },
        token=TOKEN,
    )
    check(
        "manual callback done",
        status == 200 and result.get("status") == "done",
        (status, result),
    )
    check(
        "manual callback saved account", R.pool_for(ACCOUNTS).get(state.uid) is not None
    )

    reset_accounts()
    reset_fake()
    state.code = "oauth-code-raw"
    body, info = start_oauth()
    status, result = call(
        "POST",
        "/raccoon/accounts/oauth/callback",
        {
            "session_id": info["session_id"],
            "code": state.code,
        },
        token=TOKEN,
    )
    check(
        "raw code callback done",
        status == 200 and result.get("status") == "done",
        (status, result),
    )
    check("raw code saved account", R.pool_for(ACCOUNTS).get(state.uid) is not None)

    reset_accounts()
    reset_fake()
    state.code = "oauth-code-invalid"
    body, info = start_oauth()
    bad_urls = [
        "https://127.0.0.1:%d/raccoon/oauth/callback?%s"
        % (
            HUB_PORT,
            urllib.parse.urlencode({"code": state.code, "state": info["state"]}),
        ),
        "http://evil.invalid:%d/raccoon/oauth/callback?%s"
        % (
            HUB_PORT,
            urllib.parse.urlencode({"code": state.code, "state": info["state"]}),
        ),
        "http://127.0.0.1:%d/wrong/path?%s"
        % (
            HUB_PORT,
            urllib.parse.urlencode({"code": state.code, "state": info["state"]}),
        ),
        "http://127.0.0.1:%d/raccoon/oauth/callback?%s"
        % (
            HUB_PORT + 1,
            urllib.parse.urlencode({"code": state.code, "state": info["state"]}),
        ),
        "http://127.0.0.1:%d/raccoon/oauth/callback?%s"
        % (HUB_PORT, urllib.parse.urlencode({"code": state.code, "state": "ff" * 32})),
    ]
    for url in bad_urls:
        status, result = call(
            "POST",
            "/raccoon/accounts/oauth/callback",
            {
                "session_id": info["session_id"],
                "callback_url": url,
            },
            token=TOKEN,
        )
        check(
            "bad callback url %s -> 400" % url.split("?")[0],
            status == 400,
            (status, result),
        )
    check(
        "bad callback urls did not exchange",
        state.exchange_calls == 0,
        state.exchange_calls,
    )
    check("bad callback urls created nothing", R.load_auths(ACCOUNTS) == [])
    status, result = call(
        "POST",
        "/raccoon/accounts/oauth/callback",
        {
            "session_id": info["session_id"],
        },
        token=TOKEN,
    )
    check("callback without code -> 400", status == 400, (status, result))


def test_account_actions_enable_disable_clear_note_proxy_test():
    print("== account actions ==")
    reset_accounts()
    reset_fake()
    body = import_current_account(expires_in=3600)
    uid = body.get("uid")
    status, result = call(
        "POST", "/raccoon/accounts/disable", {"uid": uid}, token=TOKEN
    )
    check("disable ok", status == 200 and result.get("ok") is True, (status, result))
    overview = call("GET", "/raccoon/overview", token=TOKEN)[1]
    account = (overview.get("accounts") or [{}])[0]
    check("overview disabled", account.get("disabled") is True, account)
    status, result = call("POST", "/raccoon/accounts/enable", {"uid": uid}, token=TOKEN)
    check("enable ok", status == 200 and result.get("ok") is True, (status, result))
    overview = call("GET", "/raccoon/overview", token=TOKEN)[1]
    check(
        "overview enabled",
        (overview.get("accounts") or [{}])[0].get("disabled") is False,
        overview.get("accounts"),
    )

    status, result = call(
        "POST", "/raccoon/accounts/note", {"uid": uid, "note": "hello"}, token=TOKEN
    )
    check(
        "note saved", status == 200 and result.get("note") == "hello", (status, result)
    )
    status, result = call(
        "POST", "/raccoon/accounts/tasks", {"uid": uid, "enabled": False}, token=TOKEN
    )
    check(
        "tasks disabled",
        status == 200 and result.get("tasks_enabled") is False,
        (status, result),
    )

    acct = R.pool_for(ACCOUNTS).get(uid)
    R.pool_for(ACCOUNTS).note_error(acct, "rate limited", soft=True)
    check("cooldown set", acct.cool_until > time.time())
    status, result = call(
        "POST", "/raccoon/accounts/clear-cooldown", {"uid": uid}, token=TOKEN
    )
    check(
        "clear cooldown ok",
        status == 200 and result.get("ok") is True,
        (status, result),
    )
    check("cooldown cleared", acct.cool_until == 0)

    wb_settings.set_proxy_slots(
        ACCOUNTS,
        [
            {
                "id": "slot-1",
                "name": "one",
                "url": "http://127.0.0.1:7890",
                "enabled": True,
            },
        ],
    )
    status, result = call(
        "POST",
        "/raccoon/accounts/proxy",
        {"uid": uid, "slot_id": "slot-1"},
        token=TOKEN,
    )
    check(
        "proxy bind ok",
        status == 200 and result.get("proxy_slot") == "slot-1",
        (status, result),
    )
    check("proxy resolved", result.get("proxy") == "http://127.0.0.1:7890", result)
    check(
        "proxy persisted",
        R.load_auths(ACCOUNTS)[0].get("proxySlot") == "slot-1",
        R.load_auths(ACCOUNTS)[0],
    )
    status, result = call(
        "POST", "/raccoon/accounts/proxy", {"uid": uid, "slot_id": ""}, token=TOKEN
    )
    check(
        "proxy direct ok",
        status == 200 and result.get("proxy_slot") == "",
        (status, result),
    )
    check("proxy direct resolved", result.get("proxy") == "", result)

    status, result = call("POST", "/raccoon/accounts/test", {"uid": uid}, token=TOKEN)
    check(
        "account test ok", status == 200 and result.get("ok") is True, (status, result)
    )
    check("account test models", result.get("models", 0) >= 2, result)

    status, result = call("POST", "/raccoon/accounts/enable", {}, token=TOKEN)
    check("missing uid -> 400", status == 400, (status, result))
    status, result = call(
        "POST",
        "/raccoon/accounts/proxy",
        {"uid": "nope", "slot_id": "slot-1"},
        token=TOKEN,
    )
    check("unknown account -> 404", status == 404, (status, result))
    wb_settings.set_proxy_slots(ACCOUNTS, [])


def test_delete_removes_account_file():
    print("== delete ==")
    reset_accounts()
    reset_fake()
    body = import_current_account(uid="pu-delete", expires_in=3600)
    uid = body.get("uid")
    path = R.load_auths(ACCOUNTS)[0]["_path"]
    check("auth file exists", os.path.isfile(path), path)
    status, result = call("POST", "/raccoon/accounts/delete", {"uid": uid}, token=TOKEN)
    check(
        "delete ok", status == 200 and result.get("deleted") is True, (status, result)
    )
    check("account gone", R.pool_for(ACCOUNTS).get(uid) is None)
    check("auth file gone", not os.path.exists(path))
    check("auth list empty", R.load_auths(ACCOUNTS) == [])
    status, result = call("POST", "/raccoon/accounts/delete", {"uid": uid}, token=TOKEN)
    check("delete missing -> 404", status == 404, (status, result))


def test_models_refresh_and_keepalive():
    print("== refresh/keepalive ==")
    reset_accounts()
    reset_fake()
    state.token = make_jwt(state.uid, -10)
    state.refresh_token = "refresh-keepalive"
    import_current_account(uid=state.uid, expires_in=-10)
    status, result = call("POST", "/raccoon/keepalive", {}, token=TOKEN)
    check("keepalive ok", status == 200, (status, result))
    check("keepalive refreshed", result.get("refreshed", 0) >= 1, result)
    check("keepalive catalog", result.get("models", 0) >= 2, result)
    check("keepalive message", "续期" in str(result.get("message") or ""), result)

    status, result = call("POST", "/raccoon/models/refresh", {}, token=TOKEN)
    check(
        "models refresh ok",
        status == 200 and result.get("ok") is True,
        (status, result),
    )
    check("models refresh count", result.get("models", 0) >= 2, result)

    reset_accounts()
    reset_fake()
    state.token = make_jwt(state.uid, -10)
    state.refresh_token = "refresh-wanted"
    state.refresh_status = 500
    state.catalog_error = 500
    import_current_account(uid="pu-broken", expires_in=-10)
    status, result = call("POST", "/raccoon/keepalive", {}, token=TOKEN)
    blob = json.dumps(result, ensure_ascii=False)
    check("broken keepalive responds 200", status == 200, (status, result))
    check("broken keepalive not ok", result.get("ok") is False, result)
    check("broken keepalive message", bool(result.get("message")), result)
    check("broken keepalive no access token", state.token not in blob, result)
    check("broken keepalive no refresh token", state.refresh_token not in blob, result)


def test_admin_errors_are_readable_and_secret_free():
    print("== errors/redaction ==")
    reset_accounts()
    reset_fake()
    status, result = call("POST", "/raccoon/accounts/import", {"auth": {}}, token=TOKEN)
    check("invalid import -> 400", status == 400, (status, result))
    check(
        "invalid import readable",
        "access_token" in json.dumps(result, ensure_ascii=False).lower(),
        result,
    )
    status, result = call(
        "POST", "/raccoon/accounts/oauth/poll", {"session_id": "missing"}, token=TOKEN
    )
    check("unknown oauth session -> 4xx", 400 <= status < 500, (status, result))
    status, result = call("POST", "/raccoon/accounts/not-a-route", {}, token=TOKEN)
    check("unknown admin route -> 404", status == 404, (status, result))

    reset_accounts()
    reset_fake()
    state.code = "echoed-authorization-code-123"
    state.exchange_status = 400
    state.exchange_error = {"code": state.code}
    body, info = start_oauth()
    status, html = get_html(
        "/raccoon/oauth/callback?"
        + urllib.parse.urlencode({"code": state.code, "state": info["state"]})
    )
    check("exchange error html 400", status == 400, (status, html[:200]))
    check("exchange error html readable", "登录失败" in html, html[:200])
    check("exchange error html hides code", state.code not in html, html[:200])
    status, poll = call(
        "POST",
        "/raccoon/accounts/oauth/poll",
        {"session_id": info["session_id"]},
        token=TOKEN,
    )
    check(
        "exchange error session",
        status == 200 and poll.get("status") == "error",
        (status, poll),
    )
    poll_blob = json.dumps(poll, ensure_ascii=False)
    check("exchange error poll hides code", state.code not in poll_blob, poll)
    check("exchange error created nothing", R.load_auths(ACCOUNTS) == [])

    status, result = call(
        "POST",
        "/raccoon/accounts/oauth/callback",
        {
            "session_id": info["session_id"],
            "code": state.code,
        },
        token=TOKEN,
    )
    result_blob = json.dumps(result, ensure_ascii=False)
    check("error session replay rejected", status in (400, 409), (status, result))
    check("error session response hides code", state.code not in result_blob, result)
    check(
        "error session did not exchange again",
        state.exchange_calls == 1,
        state.exchange_calls,
    )


def test_callback_requests_are_redacted_and_nostore():
    print("== callback log/headers ==")
    reset_accounts()
    reset_fake()
    state.code = "log-secret-code-123"
    body, info = start_oauth()
    with wb_proxy._LOG_LOCK:
        wb_proxy.LOG_BUFFER.clear()
    status, html, headers = get_html_with_headers(
        "/raccoon/oauth/callback?"
        + urllib.parse.urlencode({"code": state.code, "state": info["state"]})
    )
    check("callback 200", status == 200, (status, html[:120]))
    check(
        "callback cache-control",
        headers.get("Cache-Control") == "no-store",
        dict(headers),
    )
    check(
        "callback referrer-policy",
        headers.get("Referrer-Policy") == "no-referrer",
        dict(headers),
    )

    codearts_code = "codearts-log-code-456"
    codearts_state = "codearts-log-state-456"
    status, _, headers = get_html_with_headers(
        "/oauth/callback?"
        + urllib.parse.urlencode({"code": codearts_code, "state": codearts_state})
    )
    check(
        "codearts callback cache-control",
        headers.get("Cache-Control") == "no-store",
        dict(headers),
    )
    check(
        "codearts callback referrer-policy",
        headers.get("Referrer-Policy") == "no-referrer",
        dict(headers),
    )

    with wb_proxy._LOG_LOCK:
        logs = "\n".join(str(item.get("msg") or "") for item in wb_proxy.LOG_BUFFER)
    for secret in (state.code, info["state"], codearts_code, codearts_state):
        check("callback log hides %s" % secret[:18], secret not in logs, logs[-500:])
    check("callback log shows redaction", "<redacted>" in logs, logs[-500:])


def test_callback_missing_code_does_not_burn_state():
    print("== missing callback code ==")
    reset_accounts()
    reset_fake()
    state.code = "keep-pending-code"
    body, info = start_oauth()
    status, html = get_html(
        "/raccoon/oauth/callback?" + urllib.parse.urlencode({"state": info["state"]})
    )
    check("missing code -> 400", status == 400, (status, html[:120]))
    session = R._OAUTH_SESSIONS[info["session_id"]]
    check(
        "missing code keeps state pending", session.status == "pending", session.status
    )
    check(
        "missing code did not exchange", state.exchange_calls == 0, state.exchange_calls
    )
    status, html = get_html(
        "/raccoon/oauth/callback?"
        + urllib.parse.urlencode({"code": state.code, "state": info["state"]})
    )
    check("retry after missing code succeeds", status == 200, (status, html[:120]))
    check("retry exchanged once", state.exchange_calls == 1, state.exchange_calls)


def test_oauth_session_table_is_capped():
    print("== oauth session cap ==")
    reset_accounts()
    reset_fake()
    for _ in range(R.OAUTH_MAX_SESSIONS + 5):
        R.oauth_start(R.load_config(ACCOUNTS), HUB_PORT)
    check(
        "session table capped",
        len(R._OAUTH_SESSIONS) <= R.OAUTH_MAX_SESSIONS,
        len(R._OAUTH_SESSIONS),
    )
    reset_accounts()


def test_delete_failure_keeps_account_and_removes_duplicates():
    print("== delete failure/duplicates ==")
    reset_accounts()
    reset_fake()
    body = import_current_account(uid="pu-delete-fail")
    uid = body.get("uid")
    path = R.load_auths(ACCOUNTS)[0]["_path"]
    with mock.patch.object(R, "delete_auth", return_value=False):
        status, result = call(
            "POST", "/raccoon/accounts/delete", {"uid": uid}, token=TOKEN
        )
    check("failed delete is not success", status in (409, 500), (status, result))
    check(
        "failed delete does not claim deleted",
        result.get("deleted") is not True,
        result,
    )
    check("failed delete keeps pool entry", R.pool_for(ACCOUNTS).get(uid) is not None)
    check("failed delete keeps auth file", os.path.exists(path))

    status, result = call("POST", "/raccoon/accounts/delete", {"uid": uid}, token=TOKEN)
    check(
        "retry delete succeeds",
        status == 200 and result.get("deleted") is True,
        (status, result),
    )

    body = import_current_account(uid="pu-delete-dup")
    uid = body.get("uid")
    path = R.load_auths(ACCOUNTS)[0]["_path"]
    duplicate = os.path.join(os.path.dirname(path), "raccoon-duplicate.json")
    shutil.copyfile(path, duplicate)
    status, result = call("POST", "/raccoon/accounts/delete", {"uid": uid}, token=TOKEN)
    check(
        "duplicate delete succeeds",
        status == 200 and result.get("deleted") is True,
        (status, result),
    )
    check("canonical file gone", not os.path.exists(path))
    check("duplicate file gone", not os.path.exists(duplicate))
    check("duplicate pool entry gone", R.pool_for(ACCOUNTS).get(uid) is None)


def test_proxy_credentials_are_masked_in_views():
    print("== proxy masking ==")
    reset_accounts()
    reset_fake()
    import_current_account()
    wb_settings.set_proxy_slots(
        ACCOUNTS,
        [
            {
                "id": "slot-secret",
                "name": "secret",
                "url": "http://alice:hunter2@127.0.0.1:7890",
                "enabled": True,
            },
        ],
    )
    status, result = call(
        "POST",
        "/raccoon/accounts/proxy",
        {"uid": state.uid, "slot_id": "slot-secret"},
        token=TOKEN,
    )
    check("proxy bind with credentials", status == 200, (status, result))
    check("proxy action hides password", "hunter2" not in json.dumps(result), result)
    view = R.pool_for(ACCOUNTS).get(state.uid).view()
    check("account view hides password", "hunter2" not in json.dumps(view), view)
    overview = call("GET", "/raccoon/overview", token=TOKEN)[1]
    check("overview hides password", "hunter2" not in json.dumps(overview), overview)
    check("masked marker present", "<redacted>" in json.dumps(view), view)
    wb_settings.set_proxy_slots(ACCOUNTS, [])


def test_import_rolls_back_auth_file_when_enable_fails():
    print("== import rollback ==")
    reset_accounts()
    reset_fake()
    with mock.patch.object(R, "save_config", side_effect=RuntimeError("boom")):
        raised = False
        try:
            R.import_auth(
                R.load_config(ACCOUNTS),
                {
                    "access_token": make_jwt("pu-rollback", 3600),
                    "refresh_token": "rt-rollback",
                },
            )
        except RuntimeError:
            raised = True
    check("enable failure raised", raised)
    check("rolled back auth file", R.load_auths(ACCOUNTS) == [])
    check("rolled back realm stays disabled", R.configured(ACCOUNTS) is False)


srv, base, state = start_server()
R.BASE_URL = base

hub = wb_proxy.GatewayServer(("127.0.0.1", 0), wb_proxy.Handler)
threading.Thread(target=hub.serve_forever, daemon=True).start()
HUB = "http://127.0.0.1:%d" % hub.server_address[1]
HUB_PORT = hub.server_address[1]

try:
    wb_proxy.POOL = None
    wb_proxy.API_KEY = None
    reset_accounts()
    status, body = call("POST", "/panel/login", {"password": "admin"})
    check("panel login", status == 200 and body.get("token"), (status, body))
    TOKEN = body.get("token")

    test_overview_and_accounts_require_panel_session()
    test_overview_never_leaks_tokens()
    test_oauth_start_builds_loopback_url_and_state()
    test_oauth_callback_creates_account_and_poll_reports_done()
    test_oauth_callback_rejects_bad_state_replay_and_expiry()
    test_manual_callback_url_creates_account()
    test_account_actions_enable_disable_clear_note_proxy_test()
    test_delete_removes_account_file()
    test_delete_failure_keeps_account_and_removes_duplicates()
    test_proxy_credentials_are_masked_in_views()
    test_import_rolls_back_auth_file_when_enable_fails()
    test_models_refresh_and_keepalive()
    test_admin_errors_are_readable_and_secret_free()
    test_callback_requests_are_redacted_and_nostore()
    test_callback_missing_code_does_not_burn_state()
    test_oauth_session_table_is_capped()
finally:
    try:
        hub.shutdown()
        hub.server_close()
    except Exception:
        pass
    stop_server(srv)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
