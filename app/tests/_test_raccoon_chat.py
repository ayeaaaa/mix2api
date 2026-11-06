"""Raccoon chat transport: allowlist, SSE pass-through, retry and failover."""

import json
import os
import shutil
import sys
import time
from http.client import IncompleteRead

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.path.insert(0, ROOT)

from _isolated_dirs import isolated_data_dirs  # noqa: E402

_TMP = isolated_data_dirs("wb-raccoon-chat-")
ACCOUNTS = os.environ["ACCOUNTS_DIR"]

import wb_proxy as P  # noqa: E402
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
    """Fresh settings, accounts and provider caches for each test."""
    shutil.rmtree(os.path.join(ACCOUNTS, "raccoon"), ignore_errors=True)
    wb_settings.save(ACCOUNTS, {"raccoon": {"enabled": True}})
    R.invalidate_pool()
    R.invalidate_health()
    R.invalidate_models()


def reset_fake():
    state.token = make_jwt(state.uid, 3600)
    state.refresh_token = "refresh-1"
    state.refresh_calls = 0
    state.catalog_calls = 0
    state.exchange_calls = 0
    state.chat_calls = 0
    state.chat_status = 200
    state.chat_status_remaining = -1
    state.chat_status_by_token = {}
    state.chat_text = "你好"
    state.reasoning_text = ""
    state.tool_calls = None
    state.truncate_after = None
    state.embedded_error = None
    state.embedded_error_by_token = {}
    state.saw_image = False
    state.extra_tokens = set()
    state.catalog = default_catalog()
    state.reset_requests()


def add_account(uid, token=None, refresh="refresh-1", office_identity="personal"):
    token = token or state.token
    saved = R.save_auth(
        ACCOUNTS,
        {
            "access_token": token,
            "refresh_token": refresh,
            "uid": uid,
            "office_identity": office_identity,
            "office_org_name": uid,
            "updated_at": int(time.time()),
        },
    )
    return R.pool_for(ACCOUNTS).add(saved)


def chat_body(**overrides):
    payload = {
        "model": "raccoon-090dd3",
        "messages": [{"role": "user", "content": "hi"}],
    }
    payload.update(overrides)
    return R.build_chat_body(payload, "session-1")


def open_stream(body, allowed_accounts=None):
    return R.open_chat(
        R.load_config(ACCOUNTS),
        body,
        "session-1",
        allowed_accounts=allowed_accounts,
    )


def consume(stream):
    try:
        return list(stream)
    finally:
        stream.close()


def aggregate(stream, model):
    try:
        return P.aggregate_stream(stream, model, None)
    finally:
        stream.close()


def test_build_chat_body_allows_openai_fields_and_forces_stream():
    reset()
    image_url = "data:image/png;base64,iVBORw0KGgo="
    payload = {
        "model": "raccoon-090dd3",
        "messages": [
            {"role": "system", "content": "be brief"},
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "what is this?"},
                    {"type": "image_url", "image_url": {"url": image_url}},
                ],
            },
        ],
        "tools": [
            {
                "type": "function",
                "function": {"name": "lookup", "parameters": {"type": "object"}},
            }
        ],
        "tool_choice": "auto",
        "parallel_tool_calls": True,
        "temperature": 0.2,
        "top_p": 0.9,
        "n": 1,
        "stream": False,
        "stop": ["END"],
        "max_tokens": 999999,
        "max_completion_tokens": 888888,
        "presence_penalty": 0.1,
        "frequency_penalty": 0.2,
        "logit_bias": {"1": 1},
        "user": "user-1",
        "seed": 7,
        "response_format": {"type": "json_object"},
        "reasoning_effort": "high",
        "thinking": {"type": "enabled"},
        "enable_thinking": True,
        "api_key": "must-not-leak",
        "metadata": {"must": "not-leak"},
        "stream_options": {"include_usage": True},
        "functions": [{"name": "legacy"}],
    }
    body = R.build_chat_body(payload, "session-1")
    check("stream forced true", body.get("stream") is True, body)
    check("input payload was not mutated", payload.get("stream") is False)
    check(
        "messages and image content forwarded unchanged",
        body.get("messages") == payload["messages"],
        body.get("messages"),
    )
    check("tools preserved", body.get("tools") == payload["tools"], body)
    check("tool choice preserved", body.get("tool_choice") == "auto", body)
    check("parallel tool calls preserved", body.get("parallel_tool_calls") is True)
    for key in (
        "temperature",
        "top_p",
        "stop",
        "max_tokens",
        "max_completion_tokens",
        "presence_penalty",
        "frequency_penalty",
        "user",
        "seed",
        "response_format",
        "reasoning_effort",
        "thinking",
        "enable_thinking",
    ):
        check("%s preserved" % key, body.get(key) == payload[key], body.get(key))
    for key in (
        "api_key",
        "metadata",
        "stream_options",
        "functions",
        "n",
        "logit_bias",
    ):
        check("%s dropped" % key, key not in body, body)

    for bad in (None, [], "body"):
        raised = False
        try:
            R.build_chat_body(bad)
        except R.RaccoonBadRequest:
            raised = True
        check("non-object body rejected (%r)" % (bad,), raised)
    for bad in (
        {"model": "raccoon-090dd3", "messages": []},
        {"messages": [{"role": "user", "content": "hi"}]},
    ):
        raised = False
        try:
            R.build_chat_body(bad)
        except R.RaccoonBadRequest:
            raised = True
        check("incomplete chat body rejected", raised, bad)


def test_chat_headers_are_exact_and_org_gated():
    reset()
    reset_fake()
    acct = add_account("pu-header")

    headers = R._chat_headers(acct)
    expected = {
        "Authorization",
        "Content-Type",
        "Accept",
        "X-Client-Platform",
        "X-Client-Version",
        "X-Raccoon-Language",
        "Cache-Control",
    }
    check("chat header keys are exact", set(headers) == expected, headers)
    check(
        "authorization is the account bearer",
        headers.get("Authorization") == "Bearer " + state.token,
        headers,
    )
    check(
        "accept is event stream",
        headers.get("Accept") == "text/event-stream",
        headers,
    )
    check(
        "content type is json",
        headers.get("Content-Type") == "application/json",
        headers,
    )
    check(
        "client identity headers",
        headers.get("X-Client-Platform") == "desktop-linux-x64"
        and headers.get("X-Client-Version") == "v1.1.5"
        and headers.get("X-Raccoon-Language") == "zh",
        headers,
    )
    check(
        "cache control is no-store",
        headers.get("Cache-Control") == "no-store",
        headers,
    )
    check("personal identity has no org header", "X-Org-Code" not in headers)

    acct.auth["office_identity"] = "org-code-1"
    org_headers = R._chat_headers(acct)
    check(
        "org identity becomes X-Org-Code",
        org_headers.get("X-Org-Code") == "org-code-1",
        org_headers,
    )

    stream, _acct, _model = open_stream(chat_body())
    consume(stream)
    sent = state.last_headers or {}
    check(
        "wire headers carry the desktop identity",
        sent.get("authorization") == "Bearer " + acct.auth["access_token"]
        and sent.get("accept") == "text/event-stream"
        and sent.get("content-type") == "application/json"
        and sent.get("x-client-platform") == "desktop-linux-x64"
        and sent.get("x-client-version") == "v1.1.5"
        and sent.get("x-raccoon-language") == "zh"
        and sent.get("cache-control") == "no-store",
        sent,
    )
    check(
        "wire request carries the org header",
        sent.get("x-org-code") == "org-code-1",
        sent,
    )
    wire_names = set(state.last_header_names or ())
    check(
        "wire header names keep the exact desktop casing",
        {
            "Authorization",
            "Content-Type",
            "Accept",
            "X-Client-Platform",
            "X-Client-Version",
            "X-Raccoon-Language",
            "Cache-Control",
            "X-Org-Code",
        }
        <= wire_names,
        wire_names,
    )

    acct.auth["office_identity"] = ""
    empty_headers = R._chat_headers(acct)
    check("empty identity has no org header", "X-Org-Code" not in empty_headers)


def test_open_chat_nonstream_aggregates_text_usage_and_clamps_tokens():
    reset()
    reset_fake()
    acct = add_account("pu-nonstream")
    body = chat_body(stream=False, max_tokens=999999, max_completion_tokens=888888)
    stream, used_acct, model = open_stream(body)
    check("open_chat returns the selected account", used_acct is acct)
    check("model stays bare", model == "raccoon-090dd3", model)

    sent = state.last_body or {}
    check("upstream is always streamed", sent.get("stream") is True, sent)
    check(
        "max_tokens clamped to catalog limit",
        sent.get("max_tokens") == 100000,
        sent.get("max_tokens"),
    )
    check(
        "max_completion_tokens clamped to catalog limit",
        sent.get("max_completion_tokens") == 100000,
        sent.get("max_completion_tokens"),
    )

    result = aggregate(stream, model)
    choice = (result.get("choices") or [{}])[0]
    message = choice.get("message") or {}
    check("aggregated text", message.get("content") == "你好", message)
    check("finish reason stop", choice.get("finish_reason") == "stop", choice)
    usage = result.get("usage") or {}
    check("usage total 5", usage.get("total_tokens") == 5, usage)
    check("usage prompt 3", usage.get("prompt_tokens") == 3, usage)
    check("usage completion 2", usage.get("completion_tokens") == 2, usage)
    check("response model is bare", result.get("model") == model, result.get("model"))
    check("slot released after aggregation", acct.active_concurrent == 0)
    stream.close()
    check("second close keeps the slot released", acct.active_concurrent == 0)


def test_stream_passes_raw_chunks_and_close_releases_slot():
    reset()
    reset_fake()
    acct = add_account("pu-stream")
    stream, _acct, _model = open_stream(chat_body())
    stream.preflight()
    lines = consume(stream)
    blob = b"".join(lines)
    check("stream ends with DONE", b"data: [DONE]" in blob, blob[-120:])
    check(
        "first data line is the cached role event",
        bool(lines) and b'"role": "assistant"' in lines[0],
        lines[:1],
    )
    check(
        "second preflight did not duplicate the cached event",
        blob.count(b'"role": "assistant"') == 1,
        blob[:200],
    )
    check(
        "raw chunk text is not re-encoded",
        '"content": "你"'.encode("utf-8") in blob,
        blob[:200],
    )
    check(
        "every yielded line is a data line",
        all(line.strip().startswith(b"data:") for line in lines if line.strip()),
        lines[:3],
    )
    check("slot released by iteration", acct.active_concurrent == 0)
    check("response is closed", getattr(stream.raw, "isclosed", lambda: True)())
    stream.close()
    check("second close is harmless", acct.active_concurrent == 0)


def test_tools_are_forwarded_and_returned():
    reset()
    reset_fake()
    add_account("pu-tools")
    state.tool_calls = [
        {
            "index": 0,
            "id": "call_weather",
            "type": "function",
            "function": {"name": "get_weather", "arguments": '{"city":"Beijing"}'},
        }
    ]
    tools = [
        {
            "type": "function",
            "function": {
                "name": "get_weather",
                "description": "weather",
                "parameters": {
                    "type": "object",
                    "properties": {"city": {"type": "string"}},
                },
            },
        }
    ]
    body = chat_body(
        tools=tools,
        tool_choice={"type": "function", "function": {"name": "get_weather"}},
    )
    stream, _acct, model = open_stream(body)
    result = aggregate(stream, model)
    sent = state.last_body or {}
    check("tools recorded upstream", sent.get("tools") == tools, sent)
    check(
        "named tool choice preserved",
        sent.get("tool_choice") == body.get("tool_choice"),
        sent.get("tool_choice"),
    )
    choice = (result.get("choices") or [{}])[0]
    message = choice.get("message") or {}
    calls = message.get("tool_calls") or []
    check("tool calls returned", len(calls) == 1, calls)
    check(
        "tool call name and arguments",
        calls
        and calls[0].get("function", {}).get("name") == "get_weather"
        and calls[0].get("function", {}).get("arguments") == '{"city":"Beijing"}',
        calls,
    )
    check(
        "finish reason tool_calls", choice.get("finish_reason") == "tool_calls", choice
    )


def test_image_content_is_forwarded_unchanged():
    reset()
    reset_fake()
    add_account("pu-image")
    image_url = "data:image/png;base64,AAAA"
    content = [
        {"type": "text", "text": "read this"},
        {"type": "image_url", "image_url": {"url": image_url}},
    ]
    body = chat_body(messages=[{"role": "user", "content": content}])
    stream, _acct, _model = open_stream(body)
    consume(stream)
    check("fake saw an image part", state.saw_image is True)
    sent = state.last_body or {}
    sent_content = ((sent.get("messages") or [{}])[0]).get("content")
    check("image content forwarded unchanged", sent_content == content, sent_content)
    check("data URL survived", image_url in json.dumps(sent_content), sent_content)


def test_401_refreshes_and_retries_same_account():
    reset()
    reset_fake()
    old_token = make_jwt("pu-chat-401", 3600)
    old_refresh = state.refresh_token
    acct = add_account("pu-chat-401", token=old_token, refresh=state.refresh_token)
    body = chat_body()
    stream, used_acct, model = open_stream(body)
    check("same account is retried", used_acct is acct)
    check("one refresh call", state.refresh_calls == 1, state.refresh_calls)
    check("chat was attempted twice", state.chat_calls == 2, state.chat_calls)
    check("account adopted the rotated token", acct.auth["access_token"] == state.token)
    check(
        "account adopted the rotated refresh token",
        acct.auth["refresh_token"] == state.refresh_token
        and state.refresh_token != old_refresh,
        (acct.auth.get("refresh_token"), state.refresh_token),
    )
    disk = R.load_auths(ACCOUNTS)[0]
    check(
        "rotated refresh token persisted to disk",
        disk["access_token"] == state.token
        and disk["refresh_token"] == state.refresh_token,
        disk,
    )
    chat_requests = [item for item in state.requests if item["path"] == R.EP_CHAT]
    check("two chat requests", len(chat_requests) == 2, chat_requests)
    check(
        "first chat used the stale token",
        chat_requests[0]["headers"].get("authorization") == "Bearer " + old_token,
        chat_requests[0]["headers"],
    )
    check(
        "retry used the refreshed token",
        chat_requests[-1]["headers"].get("authorization") == "Bearer " + state.token,
        chat_requests[-1]["headers"],
    )
    result = aggregate(stream, model)
    check(
        "retried stream serves content",
        ((result.get("choices") or [{}])[0].get("message") or {}).get("content")
        == "你好",
        result,
    )
    check("slot released", acct.active_concurrent == 0)


def test_5xx_fails_over_to_second_account():
    reset()
    reset_fake()
    state.chat_status = 500
    state.chat_status_remaining = 1
    first = add_account("pu-5xx-a")
    second = add_account("pu-5xx-b")
    stream, used_acct, model = open_stream(chat_body())
    check("5xx fails over to the second account", used_acct is second, used_acct.uid)
    check("5xx account cools down", first.cool_until > time.time(), first.cool_until)
    check("5xx chat attempted twice", state.chat_calls == 2, state.chat_calls)
    result = aggregate(stream, model)
    check(
        "5xx failover serves content",
        ((result.get("choices") or [{}])[0].get("message") or {}).get("content")
        == "你好",
        result,
    )
    check("5xx first slot released", first.active_concurrent == 0)
    check("5xx second slot released", second.active_concurrent == 0)


def test_403_refreshes_and_retries_same_account():
    reset()
    reset_fake()
    state.chat_status = 403
    state.chat_status_remaining = 1
    acct = add_account("pu-403")
    stream, used_acct, model = open_stream(chat_body())
    check("403 retries the same account", used_acct is acct)
    check("403 triggers one refresh", state.refresh_calls == 1, state.refresh_calls)
    check("403 chat attempted twice", state.chat_calls == 2, state.chat_calls)
    check("403 account adopted the new token", acct.auth["access_token"] == state.token)
    result = aggregate(stream, model)
    check(
        "403 retry serves content",
        ((result.get("choices") or [{}])[0].get("message") or {}).get("content")
        == "你好",
        result,
    )


def test_second_401_marks_the_account_terminal():
    reset()
    reset_fake()
    state.chat_status = 401
    acct = add_account("pu-401-terminal")
    raised = None
    try:
        open_stream(chat_body())
    except R.RaccoonAuthError as exc:
        raised = exc
    check("second 401 raises auth error", raised is not None, raised)
    check("second 401 refreshed once", state.refresh_calls == 1, state.refresh_calls)
    check("second 401 chat attempted twice", state.chat_calls == 2, state.chat_calls)
    check("terminal account is disabled", acct.disabled is True, acct.view())
    check("terminal account released its slot", acct.active_concurrent == 0)


def test_embedded_401_refreshes_and_retries_same_account():
    reset()
    reset_fake()
    acct = add_account("pu-embedded-401")
    state.embedded_error_by_token[state.token] = {
        "error_code": 401,
        "error_msg": "token expired",
    }
    stream, used_acct, model = open_stream(chat_body())
    check("embedded 401 retries the same account", used_acct is acct)
    check("embedded 401 refreshes once", state.refresh_calls == 1, state.refresh_calls)
    check("embedded 401 chat attempted twice", state.chat_calls == 2, state.chat_calls)
    check(
        "embedded 401 account adopted the new token",
        acct.auth["access_token"] == state.token,
    )
    result = aggregate(stream, model)
    check(
        "embedded 401 retry serves content",
        ((result.get("choices") or [{}])[0].get("message") or {}).get("content")
        == "你好",
        result,
    )
    check("embedded 401 released its slot", acct.active_concurrent == 0)


def test_preflight_failure_releases_only_its_own_slot():
    reset()
    reset_fake()
    R.save_config(ACCOUNTS, {"max_concurrent": 2})
    acct = add_account("pu-owned-slot")
    pool = R.pool_for(ACCOUNTS)
    check("pre-held slot acquired", pool.acquire(acct) is True)
    state.embedded_error = {"error_code": 500, "error_msg": "boom"}
    raised = None
    try:
        open_stream(chat_body())
    except R.RaccoonUpstreamError as exc:
        raised = exc
    check("preflight failure raised upstream error", raised is not None, raised)
    check(
        "preflight failure released only its own slot",
        acct.active_concurrent == 1,
        acct.active_concurrent,
    )
    pool.release(acct)
    check("pre-held slot released", acct.active_concurrent == 0)


def test_open_chat_filters_raw_payload_fields():
    reset()
    reset_fake()
    add_account("pu-raw")
    raw = {
        "model": "raccoon-090dd3",
        "messages": [{"role": "user", "content": "hi"}],
        "stream": False,
        "api_key": "secret",
        "metadata": {"must": "not-leak"},
        "n": 2,
        "logit_bias": {"1": 1},
    }
    stream, _acct, _model = R.open_chat(R.load_config(ACCOUNTS), raw)
    consume(stream)
    sent = state.last_body or {}
    check("raw payload still forces stream", sent.get("stream") is True, sent)
    for key in ("api_key", "metadata", "n", "logit_bias"):
        check("raw payload %s filtered" % key, key not in sent, sent)


def test_disabled_config_rejects_chat():
    reset()
    reset_fake()
    R.save_config(ACCOUNTS, {"enabled": False})
    raised = None
    try:
        open_stream(chat_body())
    except R.RaccoonBadRequest as exc:
        raised = exc
    check("disabled config raises bad request", raised is not None, raised)
    check("disabled config message is readable", "未启用" in str(raised or ""), raised)
    check(
        "disabled config never reaches upstream",
        state.chat_calls == 0,
        state.chat_calls,
    )


def test_chat_pool_bypass_keeps_the_account_proxy():
    from unittest import mock
    import io

    import wb_accounts
    import wb_upstream_pool

    reset()
    reset_fake()
    acct = add_account("pu-bypass")
    proxy = "http://127.0.0.1:7899"
    acct.proxy = proxy
    seen = {}

    class FakeOpener(object):
        def open(self, req, timeout=None):
            seen["url"] = req.full_url
            seen["timeout"] = timeout
            return io.BytesIO(b"data: [DONE]\n\n")

    def fake_for_proxy(value):
        seen["proxy"] = value
        return FakeOpener()

    with mock.patch.object(
        R.wb_upstream_pool,
        "urlopen",
        side_effect=wb_upstream_pool.PoolBypass("forced"),
    ):
        with mock.patch.object(
            wb_accounts, "opener_for_proxy", side_effect=fake_for_proxy
        ):
            stream, _model = R._client_chat(acct, chat_body(), "session-1")
            try:
                lines = list(stream)
            finally:
                stream.close()
    check("pool bypass keeps the account proxy", seen.get("proxy") == proxy, seen)
    check(
        "pool bypass opens the chat endpoint",
        seen.get("url") == R.BASE_URL + R.EP_CHAT,
        seen,
    )
    check(
        "pool bypass stream still works",
        any(b"[DONE]" in line for line in lines),
        lines,
    )


def test_429_fails_over_to_second_account():
    reset()
    reset_fake()
    state.chat_status = 429
    state.chat_status_remaining = 1
    first = add_account("pu-a")
    second = add_account("pu-b")
    stream, used_acct, model = open_stream(chat_body())
    check("second account served", used_acct is second, used_acct.uid)
    check("first account is cooling", first.cool_until > time.time(), first.cool_until)
    check(
        "first account has a readable error", bool(first.last_error), first.last_error
    )
    check("first account released its slot", first.active_concurrent == 0)
    check("second account is not cooling", second.cool_until == 0, second.cool_until)
    check("chat attempted twice", state.chat_calls == 2, state.chat_calls)
    result = aggregate(stream, model)
    check(
        "failover result has text",
        ((result.get("choices") or [{}])[0].get("message") or {}).get("content")
        == "你好",
        result,
    )
    check("serving account released its slot", second.active_concurrent == 0)


def test_bad_request_does_not_fail_over():
    reset()
    reset_fake()
    state.chat_status = 400
    first = add_account("pu-bad-a")
    second = add_account("pu-bad-b")
    raised = None
    try:
        open_stream(chat_body())
    except R.RaccoonBadRequest as exc:
        raised = exc
    check("400 raises RaccoonBadRequest", raised is not None)
    check("400 message is readable", bool(str(raised)) if raised else False, raised)
    check("only one upstream chat call", state.chat_calls == 1, state.chat_calls)
    check("first account not cooled", first.cool_until == 0, first.cool_until)
    check(
        "second account untouched",
        second.active_concurrent == 0 and second.cool_until == 0,
    )
    check("first account slot released", first.active_concurrent == 0)


def test_truncated_stream_raises_and_close_releases():
    reset()
    reset_fake()
    state.truncate_after = 2
    acct = add_account("pu-truncated")
    stream, _acct, _model = open_stream(chat_body())
    yielded = []
    failure = None
    try:
        for line in stream:
            yielded.append(line)
    except (IncompleteRead, R.RaccoonUpstreamError) as exc:
        failure = exc
    finally:
        stream.close()
    check("truncated stream raises", failure is not None, failure)
    check(
        "truncated stream has no DONE",
        not any(b"[DONE]" in line for line in yielded),
        yielded,
    )
    check("truncated stream yields earlier chunks", bool(yielded), yielded)
    check("truncated slot released", acct.active_concurrent == 0)
    check(
        "truncated response is closed", getattr(stream.raw, "isclosed", lambda: True)()
    )


def test_embedded_error_raises_before_a_stream_is_returned():
    reset()
    reset_fake()
    token = state.token
    state.embedded_error = {
        "error_code": 500,
        "error_msg": "upstream exploded with " + token,
    }
    acct = add_account("pu-embedded")
    raised = None
    try:
        open_stream(chat_body())
    except R.RaccoonUpstreamError as exc:
        raised = exc
    check("embedded error raises upstream error", raised is not None)
    check("embedded error is not a stream", raised is not None)
    check(
        "embedded error does not leak the token", token not in str(raised or ""), raised
    )
    check("embedded error released the slot", acct.active_concurrent == 0)
    check("embedded error did not fail over", state.chat_calls == 1, state.chat_calls)


def test_empty_accounts_raises_unavailable():
    reset()
    reset_fake()
    raised = None
    try:
        open_stream(chat_body())
    except R.RaccoonUnavailable as exc:
        raised = exc
    check("empty pool raises unavailable", raised is not None)
    check(
        "empty pool message is readable",
        "Raccoon" in str(raised or "") and "账号" in str(raised or ""),
        raised,
    )
    check("no upstream chat attempted", state.chat_calls == 0, state.chat_calls)


def test_max_concurrent_is_respected():
    reset()
    reset_fake()
    acct = add_account("pu-busy")
    pool = R.pool_for(ACCOUNTS)
    check("acquire the only slot", pool.acquire(acct) is True)
    raised = None
    try:
        open_stream(chat_body())
    except R.RaccoonUnavailable as exc:
        raised = exc
    finally:
        pool.release(acct)
    check("busy pool raises unavailable", raised is not None, raised)
    check("busy pool never reached upstream", state.chat_calls == 0, state.chat_calls)
    check("slot restored", acct.active_concurrent == 0)


def test_test_account_forces_catalog_refresh_and_reports_counts():
    reset()
    reset_fake()
    acct = add_account("pu-test")
    result = R.test_account(R.load_config(ACCOUNTS), acct.uid)
    check("test account ok", result.get("ok") is True, result)
    check("test account model count", result.get("models") == 2, result)
    check("test account token remaining", bool(result.get("token_remaining")), result)
    check(
        "test account used the catalog", state.catalog_calls == 1, state.catalog_calls
    )

    state.catalog_error = 500
    failed = R.test_account(R.load_config(ACCOUNTS), acct.uid)
    check("test account reports upstream failure", failed.get("ok") is False, failed)
    check("test account failure is readable", bool(failed.get("message")), failed)
    state.catalog_error = 0

    raised = False
    try:
        R.test_account(R.load_config(ACCOUNTS), "missing-uid")
    except R.RaccoonNotFound:
        raised = True
    check("test account rejects unknown uid", raised)


def test_keepalive_refreshes_expiring_tokens_only():
    reset()
    reset_fake()
    state.refresh_token = "refresh-keepalive"
    old_token = state.token
    expired = add_account(
        state.uid,
        token=make_jwt(state.uid, -10),
        refresh="refresh-keepalive",
    )
    fresh = add_account("pu-fresh", token=old_token, refresh="refresh-other")
    result = R.keepalive(R.load_config(ACCOUNTS))
    check("keepalive ok", result.get("ok") is True, result)
    check("keepalive counts accounts", result.get("accounts") == 2, result)
    check("keepalive refreshes one token", result.get("refreshed") == 1, result)
    check("one refresh call", state.refresh_calls == 1, state.refresh_calls)
    check("expired account rotated", expired.auth["access_token"] == state.token)
    check("fresh account untouched", fresh.auth["access_token"] == old_token)
    check(
        "keepalive refreshed the catalog", state.catalog_calls == 1, state.catalog_calls
    )
    check("keepalive reports model count", result.get("models") == 2, result)
    check(
        "keepalive reports catalog source",
        (result.get("catalog") or {}).get("source") == "raccoon",
        result,
    )
    check("keepalive has no errors", not result.get("errors"), result)
    paths = {item["path"] for item in state.requests}
    check(
        "keepalive did not check in",
        not any("checkin" in path for path in paths),
        paths,
    )


try:
    srv, base, state = start_server()
    R.BASE_URL = base
    print("== body ==")
    test_build_chat_body_allows_openai_fields_and_forces_stream()
    print("== headers ==")
    test_chat_headers_are_exact_and_org_gated()
    print("== transport ==")
    test_open_chat_nonstream_aggregates_text_usage_and_clamps_tokens()
    test_stream_passes_raw_chunks_and_close_releases_slot()
    test_tools_are_forwarded_and_returned()
    test_image_content_is_forwarded_unchanged()
    print("== retry / failover ==")
    test_401_refreshes_and_retries_same_account()
    test_429_fails_over_to_second_account()
    test_5xx_fails_over_to_second_account()
    test_403_refreshes_and_retries_same_account()
    test_second_401_marks_the_account_terminal()
    test_embedded_401_refreshes_and_retries_same_account()
    test_bad_request_does_not_fail_over()
    test_truncated_stream_raises_and_close_releases()
    test_embedded_error_raises_before_a_stream_is_returned()
    test_preflight_failure_releases_only_its_own_slot()
    print("== payload boundary ==")
    test_open_chat_filters_raw_payload_fields()
    test_disabled_config_rejects_chat()
    test_chat_pool_bypass_keeps_the_account_proxy()
    print("== pool limits ==")
    test_empty_accounts_raises_unavailable()
    test_max_concurrent_is_respected()
    print("== admin helpers ==")
    test_test_account_forces_catalog_refresh_and_reports_counts()
    test_keepalive_refreshes_expiring_tokens_only()
finally:
    stop_server(srv)

print("PASS=%d FAIL=%d" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
