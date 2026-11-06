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


def reset():
    """Fresh settings and accounts for each test; the data root is isolated."""
    shutil.rmtree(os.path.join(ACCOUNTS, "raccoon"), ignore_errors=True)
    wb_settings.save(ACCOUNTS, {})
    R.invalidate_pool()
    R.invalidate_health()
    R.invalidate_models()


def reset_fake(expires_in=3600, refresh_token="refresh-1"):
    state.token = make_jwt(state.uid, expires_in)
    state.refresh_token = refresh_token
    state.refresh_calls = 0
    state.catalog_calls = 0
    state.exchange_calls = 0
    state.refresh_status = 200
    state.refresh_body = None
    state.catalog_status = 200
    state.catalog_error = 0
    state.catalog_error_body = None
    state.catalog = default_catalog()
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

    # Unknown subkeys describe future settings; a save must not erase them.
    wb_settings.save(
        ACCOUNTS,
        {
            "raccoon": {
                "enabled": True,
                "timeout_seconds": 30,
                "future_raccoon_flag": {"keep": 1},
            }
        },
    )
    saved = R.save_config(ACCOUNTS, {"max_concurrent": 2})
    raw = wb_settings.load(ACCOUNTS)["raccoon"]
    check(
        "save keeps unknown raccoon subkeys",
        raw.get("future_raccoon_flag") == {"keep": 1},
        raw,
    )
    check("save keeps existing known value", saved["timeout_seconds"] == 30.0)
    check("save applies patched known value", saved["max_concurrent"] == 2)

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

    saved = R.save_config(
        ACCOUNTS,
        {
            "timeout_seconds": float("nan"),
            "stream_timeout_seconds": float("inf"),
            "models_ttl_seconds": float("-inf"),
            "max_concurrent": float("inf"),
        },
    )
    check("nan timeout falls back", saved["timeout_seconds"] == 60.0)
    check(
        "infinite stream timeout falls back", saved["stream_timeout_seconds"] == 900.0
    )
    check("negative infinite ttl falls back", saved["models_ttl_seconds"] == 600.0)
    check("infinite max concurrent falls back", saved["max_concurrent"] == 1)

    wb_settings.save(
        ACCOUNTS,
        {
            "raccoon": {
                "timeout_seconds": float("nan"),
                "stream_timeout_seconds": float("inf"),
                "models_ttl_seconds": float("-inf"),
                "max_concurrent": float("inf"),
            }
        },
    )
    cfg = R.load_config(ACCOUNTS)
    check("load rejects nan timeout", cfg["timeout_seconds"] == 60.0)
    check(
        "load rejects infinite stream timeout", cfg["stream_timeout_seconds"] == 900.0
    )
    check("load rejects negative infinite ttl", cfg["models_ttl_seconds"] == 600.0)
    check("load rejects infinite max concurrent", cfg["max_concurrent"] == 1)

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


SEED_IDS = {
    "raccoon-090dd3",
    "raccoon-19b265",
    "raccoon-405a1c",
    "sn-sensenova-6-8-flash",
    "sn-sensenova-6-8-flash-lite",
    "sn-glm-5-3",
    "sn-kimi-k3",
    "sn-glm-5-3-flash",
    "sn-deepseek-v4-1-flash-1017",
}
SEED_VISION = {
    "raccoon-090dd3",
    "sn-sensenova-6-8-flash",
    "sn-sensenova-6-8-flash-lite",
    "sn-kimi-k3",
    "sn-glm-5-3-flash",
    "sn-deepseek-v4-1-flash-1017",
}
SEED_REASONING = {
    "raccoon-090dd3",
    "sn-glm-5-3",
    "sn-kimi-k3",
    "sn-deepseek-v4-1-flash-1017",
}


def _catalog_account(token=None, refresh=None):
    auth = R.save_auth(
        ACCOUNTS,
        R.auth_from_payload(
            {
                "access_token": state.token if token is None else token,
                "refresh_token": (state.refresh_token if refresh is None else refresh),
            }
        ),
    )
    return R.pool_for(ACCOUNTS).add(auth)


def test_catalog_normalizes_ids_limits_and_tags():
    reset()
    reset_fake()
    acct = _catalog_account()

    catalog = R.fetch_catalog(acct)
    check("catalog source", catalog.get("source") == "raccoon", catalog)
    check(
        "catalog default model",
        catalog.get("default_model") == "raccoon-090dd3",
        catalog,
    )
    by_id = {item["id"]: item for item in catalog.get("models") or []}
    check(
        "catalog keeps the two chat models",
        set(by_id) == {"raccoon-090dd3", "sn-glm-5-3"},
        by_id,
    )
    raccoon = by_id.get("raccoon-090dd3") or {}
    glm = by_id.get("sn-glm-5-3") or {}
    check("catalog context window", raccoon.get("context_window") == 1000000, raccoon)
    check("catalog max tokens", raccoon.get("max_tokens") == 100000, raccoon)
    check("catalog display name", raccoon.get("name") == "Raccoon-Work", raccoon)
    check("catalog vision tag", raccoon.get("vision") is True, raccoon)
    check("catalog reasoning tag", raccoon.get("reasoning") is True, raccoon)
    check("catalog keeps tags", "vision" in (raccoon.get("tags") or []), raccoon)
    check("text model is not vision", glm.get("vision") is False, glm)
    check("text model still reasons", glm.get("reasoning") is True, glm)
    check("catalog fetched timestamp", catalog.get("fetched_at", 0) > 0, catalog)


def test_catalog_401_refreshes_once_then_retries():
    reset()
    reset_fake()
    # Valid-looking but not the fake's accepted token: ensure_token does not
    # refresh eagerly, so the first catalog request is the 401 that matters.
    acct = _catalog_account(token=make_jwt("pu-catalog-401", 3600))

    catalog = R.fetch_catalog(acct)
    check("catalog survives one 401", len(catalog.get("models") or []) == 2, catalog)
    check("catalog refreshed once", state.refresh_calls == 1, state.refresh_calls)
    check("catalog retried after 401", state.catalog_calls == 2, state.catalog_calls)
    check("account uses rotated token", acct.auth["access_token"] == state.token)
    check(
        "account persisted rotated refresh token",
        acct.auth["refresh_token"] == state.refresh_token,
    )
    requests = [item for item in state.requests if item["path"] == R.EP_CATALOG]
    check("catalog sent bearer auth", len(requests) == 2, requests)
    check(
        "catalog retry carried the new token",
        requests[-1]["headers"].get("authorization") == "Bearer " + state.token,
        requests[-1]["headers"],
    )


def test_catalog_seed_fallback_when_upstream_fails():
    reset()
    reset_fake()
    acct = _catalog_account()
    state.catalog_error = 500
    # The top-level code is token-shaped on purpose: the warning must be
    # redacted even though `_error_text` turns it into `error_code=...`.
    state.catalog_error_body = {"code": state.token}

    entries = R.model_entries(R.load_config(ACCOUNTS))
    ids = [item["id"] for item in entries]
    check("catalog failure does not raise", len(entries) == 9, entries)
    check("catalog failure falls back to the nine seeds", set(ids) == SEED_IDS, ids)
    meta = R.models_meta(R.load_config(ACCOUNTS))
    check("catalog failure source", meta.get("source") == "seed", meta)
    check("catalog failure warning", bool(meta.get("warning")), meta)
    check(
        "catalog failure does not leak the access token",
        state.token not in json.dumps(meta, ensure_ascii=False),
        meta,
    )
    check(
        "catalog failure does not leak the refresh token",
        state.refresh_token not in json.dumps(meta, ensure_ascii=False),
        meta,
    )
    check(
        "catalog failure left the account usable",
        acct.disabled is False and acct.cool_until == 0,
        acct.view(),
    )


def test_model_entries_are_bare_and_capability_complete():
    reset()
    reset_fake()
    entries = R.model_entries(R.load_config(ACCOUNTS))
    ids = [item["id"] for item in entries]
    check("seed entries use the verified nine", set(ids) == SEED_IDS, ids)
    check("seed entries are unique", len(ids) == len(set(ids)), ids)
    check(
        "seed ids are bare",
        all("/" not in mid and not mid.startswith("YYLS/") for mid in ids),
        ids,
    )

    for item in entries:
        mid = item["id"]
        vision = mid in SEED_VISION
        reasoning = mid in SEED_REASONING
        check("%s object" % mid, item.get("object") == "model", item)
        check("%s owned_by" % mid, item.get("owned_by") == "sensetime", item)
        check("%s provider" % mid, item.get("provider") == "raccoon", item)
        check("%s created" % mid, isinstance(item.get("created"), int), item)
        capabilities = item.get("capabilities") or {}
        check(
            "%s capabilities vision" % mid, capabilities.get("vision") is vision, item
        )
        check(
            "%s capabilities tools" % mid, capabilities.get("tool_calls") is True, item
        )
        check(
            "%s capabilities reasoning" % mid,
            capabilities.get("reasoning") is reasoning,
            item,
        )
        for key in (
            "supports_vision",
            "supports_images",
            "vision",
            "multimodal",
        ):
            check("%s %s" % (mid, key), item.get(key) is vision, item)
        check(
            "%s supports_tool_calls" % mid,
            item.get("supports_tool_calls") is True,
            item,
        )
        check(
            "%s supports_reasoning" % mid,
            item.get("supports_reasoning") is reasoning,
            item,
        )
        abilities = item.get("abilities") or {}
        check("%s abilities vision" % mid, abilities.get("vision") is vision, item)
        check(
            "%s abilities functionCall" % mid,
            abilities.get("functionCall") is True,
            item,
        )
        check(
            "%s abilities function_call" % mid,
            abilities.get("function_call") is True,
            item,
        )
        inputs = ["text"] + (["image"] if vision else [])
        check("%s input_modalities" % mid, item.get("input_modalities") == inputs, item)
        check(
            "%s output_modalities" % mid,
            item.get("output_modalities") == ["text"],
            item,
        )
        check(
            "%s modalities" % mid,
            item.get("modalities") == {"input": inputs, "output": ["text"]},
            item,
        )
        check(
            "%s architecture" % mid,
            item.get("architecture")
            == {
                "input_modalities": inputs,
                "output_modalities": ["text"],
                "modality": "+".join(inputs) + "->text",
            },
            item,
        )
        expected_context = (
            256000 if mid.startswith("sn-sensenova-6-8-flash") else 1000000
        )
        expected_max = 63999 if mid.startswith("sn-sensenova-6-8-flash") else 100000
        for key in ("context_window", "context_length", "max_input_tokens"):
            check("%s %s" % (mid, key), item.get(key) == expected_context, item)
        for key in (
            "max_tokens",
            "max_output_tokens",
            "max_completion_tokens",
            "max_output",
        ):
            check("%s %s" % (mid, key), item.get(key) == expected_max, item)


def test_model_entries_respect_ttl_and_refresh():
    reset()
    reset_fake()
    _catalog_account()
    cfg = R.load_config(ACCOUNTS)

    first = R.model_entries(cfg)
    second = R.model_entries(cfg)
    check(
        "model entries use the normalized catalog",
        {item["id"] for item in first} == {"raccoon-090dd3", "sn-glm-5-3"},
        first,
    )
    check("second read reuses the cache", state.catalog_calls == 1, state.catalog_calls)
    check(
        "two reads return the same ids",
        first == second or ([i["id"] for i in first] == [i["id"] for i in second]),
    )

    refreshed = R.refresh_models(cfg)
    check("refresh ok", refreshed.get("ok") is True, refreshed)
    check("refresh reports two models", refreshed.get("models") == 2, refreshed)
    check(
        "refresh catalog source",
        (refreshed.get("catalog") or {}).get("source") == "raccoon",
        refreshed,
    )
    check(
        "refresh forces a second upstream call",
        state.catalog_calls == 2,
        state.catalog_calls,
    )
    check(
        "refresh metadata has no warning",
        not (refreshed.get("catalog") or {}).get("warning"),
        refreshed,
    )


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


def test_upstream_error_redacts_known_secrets():
    reset()
    secret = "opaque-refresh-secret-12345"
    reset_fake(expires_in=-10, refresh_token=secret)
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
    state.refresh_status = 400
    state.refresh_body = {"message": "invalid refresh token: " + secret}

    message = ""
    try:
        R.ensure_token(acct)
    except R.RaccoonAuthError as exc:
        message = str(exc)
    check("refresh error raised", bool(message))
    check("error does not echo refresh token", secret not in message, message)
    check("error does not echo access token", state.token not in message, message)
    view_blob = json.dumps(acct.view(), ensure_ascii=False)
    check("view does not echo refresh token", secret not in view_blob, view_blob)
    check(
        "view still explains the failure",
        "invalid refresh token" in view_blob,
        view_blob,
    )
    check("terminal state kept", acct.disabled is True)

    labels = R._redact(
        "authorization_code=auth-code-secret code=plain-code-secret "
        "access_token=access-token-secret refresh_token=refresh-token-secret"
    )
    for value in (
        "auth-code-secret",
        "plain-code-secret",
        "access-token-secret",
        "refresh-token-secret",
    ):
        check("label pattern redacts %s" % value, value not in labels, labels)
    quoted = R._redact('{"authorization_code": "json-auth-code", "code": "json-code"}')
    check("quoted code redacted", "json-code" not in quoted, quoted)
    check("quoted authorization_code redacted", "json-auth-code" not in quoted, quoted)

    state.refresh_status = 200
    state.refresh_body = None


def test_pool_bypass_keeps_account_proxy():
    from unittest import mock
    import wb_accounts
    import wb_upstream_pool

    seen = {}

    class FakeResponse(object):
        status = 200

        def read(self, size=None):
            return b'{"ok": true}'

        def close(self):
            pass

    class FakeOpener(object):
        def open(self, req, timeout=None):
            seen["url"] = req.full_url
            seen["timeout"] = timeout
            return FakeResponse()

    def fake_for_proxy(proxy):
        seen["proxy"] = proxy
        return FakeOpener()

    proxy = "http://127.0.0.1:7899"
    with mock.patch.object(
        wb_upstream_pool,
        "urlopen",
        side_effect=wb_upstream_pool.PoolBypass("forced"),
    ):
        with mock.patch.object(
            wb_accounts, "opener_for_proxy", side_effect=fake_for_proxy
        ):
            payload = R._request_json(
                "POST",
                R.BASE_URL + R.EP_REFRESH,
                {"Content-Type": "application/json"},
                b"{}",
                proxy=proxy,
            )
    check("bypass returns parsed json", payload == {"ok": True}, payload)
    check("bypass retains account proxy", seen.get("proxy") == proxy, seen)
    check(
        "bypass opens the same url",
        seen.get("url") == R.BASE_URL + R.EP_REFRESH,
        seen,
    )


def test_health_cache_tracks_config_and_accounts():
    reset()
    first = R.health(R.load_config(ACCOUNTS))
    check(
        "health starts empty",
        first["accounts"] == 0 and first["healthy"] == 0,
        first,
    )
    check("health starts unconfigured", first["configured"] is False)

    R.import_auth(
        R.load_config(ACCOUNTS),
        {
            "access_token": make_jwt("pu-health", 3600),
            "refresh_token": "rt-health",
            "office_org_name": "Health Test",
        },
    )
    second = R.health(R.load_config(ACCOUNTS))
    check("health sees the imported account", second["accounts"] == 1, second)
    check("health sees enabled config", second["configured"] is True, second)

    pool = R.pool_for(ACCOUNTS)
    pool.disable("pu-health", "manual")
    third = R.health(R.load_config(ACCOUNTS))
    check(
        "health sees disabled state",
        third["healthy"] == 0 and third["accounts"] == 1,
        third,
    )
    check("health cache key changed with state", second != third)


try:
    srv, base, state = start_server()
    R.BASE_URL = base
    print("== config ==")
    test_config_defaults_and_clamping()
    print("== import ==")
    test_import_auth_derives_uid_and_auto_enables()
    test_import_rejects_missing_access_token()
    print("== catalog ==")
    test_catalog_normalizes_ids_limits_and_tags()
    test_catalog_401_refreshes_once_then_retries()
    test_catalog_seed_fallback_when_upstream_fails()
    test_model_entries_are_bare_and_capability_complete()
    test_model_entries_respect_ttl_and_refresh()
    print("== refresh ==")
    test_refresh_rotates_tokens_and_persists()
    test_concurrent_refresh_single_flight()
    print("== pool ==")
    test_pool_state_and_actions()
    test_proxy_slot_binding()
    print("== views ==")
    test_view_never_contains_secret()
    test_terminal_error_marks_disabled_and_soft_marks_cooldown()
    test_health_cache_tracks_config_and_accounts()
    print("== redaction ==")
    test_upstream_error_redacts_known_secrets()
    print("== proxy fallback ==")
    test_pool_bypass_keeps_account_proxy()
finally:
    stop_server(srv)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
