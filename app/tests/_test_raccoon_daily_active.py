"""Raccoon daily-activity check-in: one ping, then verify the 300-point grant.

The upstream rewards a daily-active account with a positive `daily_grant` bill
(实测 300 分) and exposes no claim API, so the check-in is: send one minimal
chat, then read the bill list and record whether today's reward landed. These
tests pin the four behaviours that matter:

  * an account that already has today's grant is never pinged again;
  * a ping that lands the grant records the points and the day;
  * a ping that does not land it stays retryable, at most
    DAILY_ACTIVE_MAX_ATTEMPTS times a day and DAILY_ACTIVE_RETRY_GAP apart;
  * disabled accounts / accounts with the task toggle off are skipped, and the
    day's result survives a pool reload (it is persisted in state.json).

Run with: python tests/_test_raccoon_daily_active.py
No real upstream is contacted: `_fake_raccoon` serves bills and chat.
"""

import os
import shutil
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-raccoon-daily-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]

import wb_raccoon as R  # noqa: E402
import wb_scheduler  # noqa: E402
import wb_settings  # noqa: E402
from _fake_raccoon import start_server, stop_server  # noqa: E402

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
    shutil.rmtree(os.path.join(ACCOUNTS, "raccoon"), ignore_errors=True)
    wb_settings.save(ACCOUNTS, {})
    R.invalidate_pool()
    R.invalidate_health()
    R.invalidate_models()
    # Fake upstream: fresh bills/counters so each test starts from the fixture
    # (today's 300-point grant present, no chats yet).
    state.reset_bills()
    state.reset_points()
    state.chat_calls = 0
    state.chat_status = 200
    state.chat_bill_points = 0
    state.daily_grant_on_chat = False
    state.bills_status = 200
    state.balance_status = 200


def save_cfg(**patch):
    section = {"enabled": True}
    section.update(patch)
    wb_settings.save(ACCOUNTS, {"raccoon": section})
    return R.load_config(ACCOUNTS)


def make_account(uid="pu-daily-active"):
    auth = R.save_auth(
        ACCOUNTS,
        R.auth_from_payload({
            "access_token": state.token,
            "refresh_token": state.refresh_token,
        }),
    )
    return R.pool_for(ACCOUNTS).add(auth)


def clear_today_grant():
    """Drop the fixture's daily_grant bill so the day starts unclaimed."""
    data = state.bills.setdefault("data", {})
    items = [item for item in data.get("items") or []
             if str(item.get("biz_type") or "") != "daily_grant"]
    data["items"] = items


def _today():
    return time.strftime("%Y-%m-%d")


def test_config_defaults():
    reset()
    cfg = R.load_config(ACCOUNTS)
    check("default daily_active off", cfg["daily_active"] is False, cfg)
    check("default daily_active_model empty", cfg["daily_active_model"] == "", cfg)

    saved = R.save_config(ACCOUNTS, {"daily_active": True,
                                     "daily_active_model": "  sn-glm-5-3  "})
    check("save enables daily active", saved["daily_active"] is True, saved)
    check("save trims the model id",
          saved["daily_active_model"] == "sn-glm-5-3", saved)

    saved = R.save_config(ACCOUNTS, {"daily_active": False})
    check("save disables daily active", saved["daily_active"] is False, saved)


def test_already_granted_skips_the_ping():
    reset()
    cfg = save_cfg()
    acct = make_account()
    result = R.daily_active(cfg, acct.uid)

    check("already-granted run is ok", result.get("ok") is True, result)
    check("already-granted counts one grant", result.get("granted") == 1, result)
    check("already-granted reports 300 points", result.get("points") == 300, result)
    check("already-granted sends no chat", state.chat_calls == 0, state.chat_calls)
    row = (result.get("results") or [{}])[0]
    check("already-granted row marks chat=False", row.get("chat") is False, row)
    check("state records today", acct.daily_active.get("date") == _today(),
          acct.daily_active)
    check("state records the points", acct.daily_active.get("points") == 300,
          acct.daily_active)


def test_ping_lands_the_grant():
    reset()
    clear_today_grant()
    state.daily_grant_on_chat = True
    state.chat_bill_points = 1
    cfg = save_cfg(daily_active_model="sn-glm-5-3")
    acct = make_account()

    result = R.daily_active(cfg, acct.uid)

    check("ping run is ok", result.get("ok") is True, result)
    check("ping sent exactly one chat", state.chat_calls == 1, state.chat_calls)
    check("ping used the configured model",
          state.last_body and state.last_body.get("model") == "sn-glm-5-3",
          state.last_body)
    row = (result.get("results") or [{}])[0]
    check("ping row marks chat=True", row.get("chat") is True, row)
    check("ping reports the landed points", row.get("points") == 300, row)
    check("message says the reward landed",
          "已到账" in str(row.get("message") or ""), row)
    check("state stores the model",
          acct.daily_active.get("model") == "sn-glm-5-3", acct.daily_active)
    check("state counts one attempt",
          acct.daily_active.get("attempts") == 1, acct.daily_active)


def test_not_granted_is_bounded_and_retryable():
    reset()
    clear_today_grant()
    state.daily_grant_on_chat = False
    state.chat_bill_points = 1
    cfg = save_cfg()
    acct = make_account()

    # The real verifier polls with a 2s gap; the outcome is what matters here.
    original = R._verify_daily_grant
    R._verify_daily_grant = lambda account, attempts=1, gap=0.0: (
        R.daily_grant_today(account)
    )
    try:
        first = R.daily_active(cfg, acct.uid)
        check("first attempt sends one chat", state.chat_calls == 1,
              state.chat_calls)
        check("first attempt is not ok", first.get("ok") is False, first)
        check("first attempt records one attempt",
              acct.daily_active.get("attempts") == 1, acct.daily_active)
        check("message says the reward has not landed",
              "尚未到账" in str(acct.daily_active.get("message") or ""),
              acct.daily_active)

        second = R.daily_active(cfg, acct.uid)
        check("second call is throttled", state.chat_calls == 1,
              state.chat_calls)
        check("throttled row is skipped",
              (second.get("results") or [{}])[0].get("skipped") is True, second)
        check("throttled message names the gap",
              "不足" in str((second.get("results") or [{}])[0].get("message") or ""),
              second)

        # Pretend the retry gap has passed: one more ping is allowed.
        acct.daily_active["last_attempt"] = (
            time.time() - R.DAILY_ACTIVE_RETRY_GAP - 1
        )
        third = R.daily_active(cfg, acct.uid)
        check("third call pings again", state.chat_calls == 2, state.chat_calls)
        check("third attempt records two attempts",
              acct.daily_active.get("attempts") == 2, acct.daily_active)

        fourth = R.daily_active(cfg, acct.uid)
        check("fourth call hits the daily cap", state.chat_calls == 2,
              state.chat_calls)
        check("daily-cap row is skipped",
              (fourth.get("results") or [{}])[0].get("skipped") is True, fourth)
        check("daily-cap message names the attempt count",
              "已打卡 2 次" in str((fourth.get("results") or [{}])[0].get("message") or ""),
              fourth)

        forced = R.daily_active(cfg, acct.uid, force=True)
        check("force bypasses the daily cap", state.chat_calls == 3,
              state.chat_calls)
        check("forced row is a real attempt",
              (forced.get("results") or [{}])[0].get("skipped") is not True,
              forced)
    finally:
        R._verify_daily_grant = original


def test_skips_disabled_and_task_off():
    reset()
    cfg = save_cfg()
    acct = make_account()

    acct.disabled = True
    disabled = R.daily_active(cfg, acct.uid)
    row = (disabled.get("results") or [{}])[0]
    check("disabled account is skipped", row.get("skipped") is True, row)
    check("disabled message", "停用" in str(row.get("message") or ""), row)
    check("disabled account sends no chat", state.chat_calls == 0,
          state.chat_calls)

    acct.disabled = False
    acct.tasks_enabled = False
    off = R.daily_active(cfg, acct.uid)
    row = (off.get("results") or [{}])[0]
    check("task-off account is skipped", row.get("skipped") is True, row)
    check("task-off message", "任务开关" in str(row.get("message") or ""), row)
    check("task-off account sends no chat", state.chat_calls == 0,
          state.chat_calls)


def test_state_survives_pool_reload():
    reset()
    clear_today_grant()
    state.daily_grant_on_chat = True
    state.chat_bill_points = 1
    cfg = save_cfg()
    acct = make_account()
    R.daily_active(cfg, acct.uid)

    R.invalidate_pool()
    reloaded = R.pool_for(ACCOUNTS).get(acct.uid)
    check("reload keeps the account", reloaded is not None)
    check("reload keeps today's result",
          reloaded.daily_active.get("date") == _today(), reloaded.daily_active)
    check("reload keeps the points",
          reloaded.daily_active.get("points") == 300, reloaded.daily_active)

    view = reloaded.view()
    check("view exposes daily_active", isinstance(view.get("daily_active"), dict),
          view.get("daily_active"))


def test_scheduler_gate():
    reset()
    save_cfg(daily_active=False)
    acct = make_account()

    class _Pool(object):
        dir = ACCOUNTS
        accounts = [acct]

    scheduler = wb_scheduler.Scheduler.__new__(wb_scheduler.Scheduler)
    scheduler.pool = _Pool()
    scheduler.logs = []
    scheduler.log = lambda msg, tag=None: scheduler.logs.append(msg)

    check("gate is closed outside the check-in hours",
          scheduler._raccoon_daily_active_pending(False) is False)
    check("gate follows the config switch",
          scheduler._raccoon_daily_active_pending(True) is False)
    check("run is a no-op when the switch is off",
          scheduler._run_raccoon_daily_active(True) == 0)
    check("no chat while the switch is off", state.chat_calls == 0,
          state.chat_calls)

    save_cfg(daily_active=True)
    check("gate opens with an enabled account",
          scheduler._raccoon_daily_active_pending(True) is True)
    check("gate still closed when the hour does not match",
          scheduler._raccoon_daily_active_pending(False) is False)


def test_unknown_uid_raises():
    reset()
    save_cfg()
    try:
        R.daily_active(R.load_config(ACCOUNTS), "no-such-uid")
    except R.RaccoonNotFound:
        check("unknown uid raises RaccoonNotFound", True)
    else:
        check("unknown uid raises RaccoonNotFound", False)


srv, base, state = start_server()
R.BASE_URL = base
try:
    print("== config ==")
    test_config_defaults()
    print("== already granted ==")
    test_already_granted_skips_the_ping()
    print("== ping lands ==")
    test_ping_lands_the_grant()
    print("== bounded retries ==")
    test_not_granted_is_bounded_and_retryable()
    print("== skips ==")
    test_skips_disabled_and_task_off()
    print("== persistence ==")
    test_state_survives_pool_reload()
    print("== scheduler ==")
    test_scheduler_gate()
    print("== unknown uid ==")
    test_unknown_uid_raises()
finally:
    stop_server(srv)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
