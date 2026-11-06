"""Raccoon 出口：REALMS、Key 绑定、持久化、停用收敛与跨区放行。"""

import json
import os
import shutil
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-raccoon-realm-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]

import wb_raccoon as R  # noqa: E402
import wb_proxy  # noqa: E402
import wb_settings  # noqa: E402

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
    wb_settings.save(ACCOUNTS, {"raccoon": {"enabled": False}})
    R.invalidate_pool()
    R.invalidate_health()
    R.invalidate_models()
    wb_proxy.CURRENT_REALM = "intl"


def test_realms_include_raccoon_and_key_binding_keeps_it():
    print("== settings ==")
    reset()
    check("REALMS includes raccoon", "raccoon" in wb_settings.REALMS)
    check("limit realms unchanged", wb_settings.LIMIT_REALMS == ("intl", "cn"))

    entry = wb_settings._clean_key_entry({"key": "abcd1234", "realm": "raccoon"})
    check("key keeps raccoon", entry["realm"] == "raccoon", entry)
    entry = wb_settings._clean_key_entry({"key": "abcd1234", "realm": "nope"})
    check("unknown realm clears", entry["realm"] == "", entry)


def test_realm_options_shrink_and_grow_with_enabled():
    print("== options ==")
    reset()
    check("disabled options", wb_proxy.realm_options() == ["intl", "cn"])
    R.save_config(ACCOUNTS, {"enabled": True})
    check("enabled options", wb_proxy.realm_options() == ["intl", "cn", "raccoon"])
    view = wb_proxy.runtime_settings_view()
    check(
        "panel settings expose raccoon",
        view.get("raccoon", {}).get("configured") is True,
        view.get("raccoon"),
    )
    R.save_config(ACCOUNTS, {"enabled": False})
    check("options shrink", wb_proxy.realm_options() == ["intl", "cn"])


def test_persisted_realm_round_trip_and_disable_reconcile():
    print("== persistence ==")
    reset()
    R.save_config(ACCOUNTS, {"enabled": True})
    wb_proxy.CURRENT_REALM = "intl"
    wb_proxy.save_persisted_realm("raccoon")
    check("saved raccoon", wb_proxy.CURRENT_REALM == "raccoon")
    check(
        "persisted file",
        json.load(open(wb_proxy.realm_state_file()))["realm"] == "raccoon",
    )
    wb_proxy.CURRENT_REALM = "intl"
    check("loaded raccoon", wb_proxy.load_persisted_realm() == "raccoon")

    print("== disable reconciles ==")
    R.save_config(ACCOUNTS, {"enabled": False})
    check("reconcile returns True", wb_proxy.reconcile_raccoon_realm() is True)
    check("current reset to intl", wb_proxy.CURRENT_REALM == "intl")
    check(
        "persisted reset",
        json.load(open(wb_proxy.realm_state_file()))["realm"] == "intl",
    )
    check("options shrink", wb_proxy.realm_options() == ["intl", "cn"])

    print("== unconfigured persisted realm falls back ==")
    with open(wb_proxy.realm_state_file(), "w", encoding="utf-8") as fh:
        json.dump({"realm": "raccoon"}, fh)
    wb_proxy.CURRENT_REALM = "intl"
    check("fallback on load", wb_proxy.load_persisted_realm() == "intl")
    wb_proxy.CURRENT_REALM = "intl"
    wb_proxy.save_persisted_realm("raccoon")
    check("cannot save unconfigured raccoon", wb_proxy.CURRENT_REALM == "intl")


def test_raccoon_bypasses_exclusive_realm_check():
    print("== cross realm ==")
    h = wb_proxy.Handler.__new__(wb_proxy.Handler)
    h.key_entry = {"name": "测试 Key"}
    check(
        "raccoon bypasses exclusive realm",
        h._cross_realm_error("deepseek-v4-pro-0813", "raccoon") == "",
    )
    check(
        "workbuddy still rejects mismatch",
        h._cross_realm_error("deepseek-v4-pro-0813", "intl") != "",
    )

    print("== account surface ==")
    R.BASE_URL = "http://127.0.0.1:9"
    check(
        "chat base url follows module",
        R.RaccoonAccount.chat_base_url(type("A", (), {})()) == R.BASE_URL,
    )
    check(
        "chat path is fixed",
        R.RaccoonAccount.chat_path == "/api/web/llm/v2/chat/completions",
    )
    check("account realm", R.RaccoonAccount.realm == "raccoon")
    check("account product", R.RaccoonAccount.product == "raccoon")


def main():
    test_realms_include_raccoon_and_key_binding_keeps_it()
    test_realm_options_shrink_and_grow_with_enabled()
    test_persisted_realm_round_trip_and_disable_reconcile()
    test_raccoon_bypasses_exclusive_realm_check()
    print("PASS=%d FAIL=%d" % (PASS, FAIL))
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
