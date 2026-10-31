# -*- coding: utf-8 -*-
"""CodeArts（华为云）第三出口的客户端与配置。

这个模块只做三件事：

1. 读写 `accounts/settings.json` 里的 `codearts` 配置；
2. 用标准库直连 codearts2api 的 OpenAI 兼容接口（推理）和 admin API（账号管理）；
3. 把 codearts2api 的模型目录/流式响应适配成 wb-hub 现有回程处理器能吃的形状。

它**不**保存任何华为云凭据：账号、refresh token、DPoP 私钥、签到、冷却都在
codearts2api 进程里。wb-hub 只持有 codearts2api 的 base_url 和 API Key。

依赖：Python 3.9+ 标准库。不要引入第三方包。
"""

import hashlib
import json
import socket
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

import wb_settings

#: 配置默认值。`enabled` 默认 False：老安装升级后行为不变。
DEFAULTS = {
    "enabled": False,
    "base_url": "http://127.0.0.1:7866",
    "api_key": "",
    "timeout_seconds": 30.0,
    "stream_timeout_seconds": 600.0,
    "models_ttl_seconds": 60.0,
}

#: 错误体最多读这么多，防止一个异常大的 body 吃掉内存。
_ERROR_BODY_MAX = 64 * 1024

#: 模型缓存的 TTL 下限/上限，防止面板填 0 或一个荒唐的大数。
_MIN_TTL = 1.0
_MAX_TTL = 3600.0

_models_lock = threading.Lock()
#: key -> {"entries": [...], "fetched_at": float, "expires_at": float}
_models_cache = {}
#: key -> bool：最近一次刷新是否失败、当前返回的是不是旧缓存
_models_stale = {}


class CodeArtsError(Exception):
    """CodeArts 上游的基类错误。message 已经是可以直接给用户看的中文。"""

    def __init__(self, message, status=None, code=None):
        super(CodeArtsError, self).__init__(message)
        self.status = status
        self.code = code


class CodeArtsUnavailable(CodeArtsError):
    """连不上、超时、DNS/路由失败。"""


class CodeArtsAuthError(CodeArtsError):
    """codearts2api 拒绝了配置的 API Key（401/403）。"""


class CodeArtsBadRequest(CodeArtsError):
    """请求本身不合法（400/422），原样转给客户端。"""


class CodeArtsNotFound(CodeArtsError):
    """资源不存在（404），例如旧版本没有 delete 端点。"""


class CodeArtsConflict(CodeArtsError):
    """状态冲突（409）。"""


def _positive_float(value, default, low, high):
    try:
        out = float(value)
    except (TypeError, ValueError):
        return float(default)
    if out < low:
        return float(low)
    if out > high:
        return float(high)
    return out


def normalize_base_url(value):
    """校验并规范化 codearts2api 的地址。

    只接受 http/https；拒绝 URL 内嵌用户名密码、query、fragment；去掉尾部
    斜杠。允许一个路径前缀（反代到 `/codearts` 的部署）。
    """
    raw = str(value or "").strip()
    if not raw:
        raise ValueError("base_url is required")
    parsed = urllib.parse.urlsplit(raw)
    if parsed.scheme not in ("http", "https"):
        raise ValueError("base_url must start with http:// or https://")
    if not parsed.netloc:
        raise ValueError("base_url has no host")
    if parsed.username or parsed.password:
        raise ValueError("base_url must not embed credentials")
    if parsed.query or parsed.fragment:
        raise ValueError("base_url must not contain query or fragment")
    path = parsed.path.rstrip("/")
    return "%s://%s%s" % (parsed.scheme, parsed.netloc, path)


def load_config(accounts_dir):
    """读取并收敛 CodeArts 配置。

    非法值回落到默认值而不是抛异常：一个手改坏的 settings.json 不应该让
    整个网关起不来。真正要写配置时走 save_config()，那里会严格校验。
    """
    raw = wb_settings.load(accounts_dir).get("codearts")
    if not isinstance(raw, dict):
        raw = {}
    try:
        base_url = normalize_base_url(raw.get("base_url") or DEFAULTS["base_url"])
    except ValueError:
        base_url = DEFAULTS["base_url"]
    return {
        "enabled": raw.get("enabled") is True,
        "base_url": base_url,
        "api_key": str(raw.get("api_key") or "").strip(),
        "timeout_seconds": _positive_float(
            raw.get("timeout_seconds"), DEFAULTS["timeout_seconds"], 1.0, 600.0),
        "stream_timeout_seconds": _positive_float(
            raw.get("stream_timeout_seconds"),
            DEFAULTS["stream_timeout_seconds"], 1.0, 3600.0),
        "models_ttl_seconds": _positive_float(
            raw.get("models_ttl_seconds"), DEFAULTS["models_ttl_seconds"],
            _MIN_TTL, _MAX_TTL),
    }


def save_config(accounts_dir, patch):
    """保存 CodeArts 配置；空 api_key 表示保留旧值。

    返回保存后的完整配置（含默认值）。非法值抛 ValueError，调用方负责转成
    400；不会写半份配置。
    """
    if not isinstance(patch, dict):
        raise ValueError("codearts settings must be an object")
    current = load_config(accounts_dir)
    merged = dict(current)
    merged.update({k: v for k, v in patch.items() if k in DEFAULTS})
    merged["base_url"] = normalize_base_url(merged.get("base_url"))
    merged["enabled"] = merged.get("enabled") is True
    key = str(patch.get("api_key") or "").strip()
    if key:
        merged["api_key"] = key
    else:
        merged["api_key"] = current.get("api_key") or ""
    merged["timeout_seconds"] = _positive_float(
        merged.get("timeout_seconds"), DEFAULTS["timeout_seconds"], 1.0, 600.0)
    merged["stream_timeout_seconds"] = _positive_float(
        merged.get("stream_timeout_seconds"),
        DEFAULTS["stream_timeout_seconds"], 1.0, 3600.0)
    merged["models_ttl_seconds"] = _positive_float(
        merged.get("models_ttl_seconds"), DEFAULTS["models_ttl_seconds"],
        _MIN_TTL, _MAX_TTL)
    data = wb_settings.load(accounts_dir)
    data["codearts"] = merged
    wb_settings.save(accounts_dir, data)
    return merged


def configured(accounts_dir):
    """出口是否可用：开关打开 + 有地址 + 有 Key。"""
    cfg = load_config(accounts_dir)
    return bool(cfg["enabled"] and cfg["base_url"] and cfg["api_key"])


def masked_config(accounts_dir):
    """给看板的配置视图：不返回明文 Key，只告诉它有没有设置。"""
    cfg = load_config(accounts_dir)
    out = dict(cfg)
    out["api_key"] = ""
    out["api_key_set"] = bool(cfg["api_key"])
    out["configured"] = bool(cfg["enabled"] and cfg["api_key"])
    return out


def _cache_key(config):
    key_hash = hashlib.sha256(
        str(config.get("api_key") or "").encode("utf-8")).hexdigest()[:8]
    return (config.get("base_url") or "", key_hash)


def _sanitize(text, config):
    """把可能被上游回显的 Key 从错误文本里抹掉。"""
    out = str(text or "")
    key = str(config.get("api_key") or "")
    if key:
        out = out.replace(key, "***")
    return out


def _error_message(body):
    """从 OpenAI 风格或 codearts2api 风格错误体里取一条可读消息。"""
    try:
        parsed = json.loads(body)
    except Exception:
        parsed = None
    if isinstance(parsed, dict):
        err = parsed.get("error")
        if isinstance(err, dict):
            for field in ("message", "code", "type"):
                value = err.get(field)
                if value:
                    return str(value)
        for field in ("message", "detail", "error"):
            value = parsed.get(field)
            if isinstance(value, str) and value:
                return value
    text = str(body or "").strip()
    return text[:500]


def _opener():
    """直连 opener。

    不用环境变量里的 HTTP_PROXY：codearts2api 通常在 127.0.0.1 或容器网络里，
    一个全局代理会把本机请求带出去。需要代理的部署应在 base_url 前面放反代。
    """
    return urllib.request.build_opener(urllib.request.ProxyHandler({}))


def _request(config, method, path, payload=None, timeout=None, stream=False):
    """发一个请求；返回 urllib 响应（stream=True）或解析后的 JSON。"""
    base = str(config.get("base_url") or "").rstrip("/")
    if not base:
        raise CodeArtsUnavailable("CodeArts 出口未配置 Base URL")
    url = base + path
    data = None
    if payload is not None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    headers = {
        "Authorization": "Bearer %s" % (config.get("api_key") or ""),
        "Accept": "text/event-stream" if stream else "application/json",
    }
    if data is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    timeout = float(timeout or config.get("timeout_seconds")
                    or DEFAULTS["timeout_seconds"])
    try:
        resp = _opener().open(req, timeout=timeout)
    except urllib.error.HTTPError as exc:
        try:
            raw = exc.read(_ERROR_BODY_MAX)
        except Exception:
            raw = b""
        message = _error_message(raw.decode("utf-8", "replace"))
        message = _sanitize(message, config) or ("HTTP %s" % exc.code)
        if exc.code in (401, 403):
            raise CodeArtsAuthError(
                "CodeArts 拒绝了当前 API Key（HTTP %s）：%s" % (exc.code, message),
                status=exc.code)
        if exc.code in (400, 422):
            raise CodeArtsBadRequest(message, status=exc.code)
        if exc.code == 404:
            raise CodeArtsNotFound(message, status=404)
        if exc.code == 409:
            raise CodeArtsConflict(message, status=409)
        raise CodeArtsError(
            "CodeArts 上游返回 HTTP %s：%s" % (exc.code, message), status=exc.code)
    except (urllib.error.URLError, socket.timeout, TimeoutError,
            ConnectionError, OSError) as exc:
        reason = getattr(exc, "reason", None) or exc
        host = urllib.parse.urlsplit(base).netloc or base
        raise CodeArtsUnavailable(
            "连不上 CodeArts（%s）：%s - 请确认 codearts2api 正在运行，"
            "且 wb-hub 能访问该地址。" % (host, _sanitize(reason, config)))
    if stream:
        return resp
    try:
        raw = resp.read(_ERROR_BODY_MAX * 4)
    finally:
        try:
            resp.close()
        except Exception:
            pass
    try:
        parsed = json.loads(raw.decode("utf-8", "replace"))
    except Exception:
        raise CodeArtsError(
            "CodeArts 返回了无法解析的响应：%s"
            % _sanitize(raw[:200].decode("utf-8", "replace"), config))
    if isinstance(parsed, dict) and parsed.get("error"):
        raise CodeArtsError(_sanitize(_error_message(raw), config))
    return parsed


def _json(config, method, path, payload=None, timeout=None):
    return _request(config, method, path, payload=payload, timeout=timeout,
                    stream=False)


def models(config, force=False):
    """返回 codearts2api 的模型条目；失败时回退到上次成功的缓存。"""
    key = _cache_key(config)
    now = time.time()
    with _models_lock:
        cached = _models_cache.get(key)
        if cached and not force and now < cached["expires_at"]:
            return list(cached["entries"])
    try:
        payload = _json(config, "GET", "/v1/models")
        data = payload.get("data") if isinstance(payload, dict) else None
        entries = [dict(item) for item in (data or [])
                   if isinstance(item, dict) and item.get("id")]
        if not entries:
            raise CodeArtsError("CodeArts 返回了空的模型目录")
    except (CodeArtsAuthError, CodeArtsBadRequest):
        # Key 错/请求错时不要把旧目录端出来：那会让看板看起来正常，实际每个
        # 请求都会失败。只有网络/上游 5xx 才回退到上次成功的缓存。
        raise
    except CodeArtsError:
        with _models_lock:
            cached = _models_cache.get(key)
            if cached:
                _models_stale[key] = True
                return list(cached["entries"])
        raise
    with _models_lock:
        _models_cache[key] = {
            "entries": entries,
            "fetched_at": now,
            "expires_at": now + float(config.get("models_ttl_seconds")
                                      or DEFAULTS["models_ttl_seconds"]),
        }
        _models_stale[key] = False
    return list(entries)


def models_meta(config):
    """模型目录的来源信息，给 /v1/models 的 `catalog` 字段用。"""
    key = _cache_key(config)
    with _models_lock:
        cached = _models_cache.get(key)
        stale = bool(_models_stale.get(key))
        fetched_at = cached["fetched_at"] if cached else None
    return {
        "source": "codearts2api",
        "fetched_at": fetched_at,
        "stale": stale,
    }


def model_entries(config):
    """把上游模型条目映射成 wb-hub /v1/models 的形状。

    只宣告 id/object/created/owned_by/provider，不伪造能力或价格：CodeArts
    的模型能力以它自己的目录为准。
    """
    raw = models(config)
    out = []
    seen = set()
    created = int(time.time())
    for item in raw:
        mid = str(item.get("id") or "").strip()
        if not mid or mid in seen:
            continue
        seen.add(mid)
        entry = {
            "id": mid,
            "object": "model",
            "created": int(item.get("created") or created),
            "owned_by": "codearts",
            "provider": "codearts",
        }
        name = item.get("name")
        if name:
            entry["name"] = name
        out.append(entry)
    return out


def invalidate_models():
    """清空模型缓存；配置改动或手动刷新时调用。"""
    with _models_lock:
        _models_cache.clear()
        _models_stale.clear()


def _valid_chat_id(value):
    """codearts2api 的 validChatID：32 位 hex。"""
    text = str(value or "").strip()
    if len(text) != 32:
        return False
    return all(c in "0123456789abcdefABCDEF" for c in text)


def _derive_chat_id(session_key):
    """把 wb-hub 的会话键稳定映射成 32hex。

    同一段对话在 wb-hub 侧会拿到同一个 session_key；映射后 codearts2api 的
    conversation_id 也稳定，多轮会粘在同一个 CodeArts 账号上。
    """
    if not session_key:
        return ""
    return hashlib.sha256(str(session_key).encode("utf-8")).hexdigest()[:32]


#: 这些字段是 wb-hub / OpenAI 客户端私有或 CodeArts 不消费的，转发前删掉。
_DROP_FIELDS = frozenset((
    "user", "metadata", "store", "previous_response_id", "prompt_cache_key",
    "n", "logprobs", "top_logprobs", "stream", "stream_options",
))


def build_chat_body(payload, session_key=None):
    """把客户端的 Chat Completions 载荷收敛成 CodeArts 能吃的标准 OpenAI 体。

    和 WorkBuddy 的 build_upstream_body 刻意不同：
      - 不注入 wb-hub 的 SYSTEM_PROMPT（CodeArts 有自己的上游提示）；
      - 不做 DeepSeek 的 reasoning_content 回填（CodeArts 不要求）；
      - 始终 stream=true + include_usage，回程由 wb-hub 聚合或透传。

    工具配对的修复复用 wb_proxy 里的三个纯函数；局部导入是为了避开
    wb_proxy ↔ wb_codearts 的模块级循环。
    """
    if not isinstance(payload, dict):
        raise CodeArtsBadRequest("request body must be a JSON object")
    from wb_proxy import (cleanup_orphan_tool_calls, merge_adjacent_tool_calls,
                          normalize_roles, repack_tool_result_blocks)
    body = {}
    for key, value in payload.items():
        name = str(key)
        if name.startswith("_") or name in _DROP_FIELDS:
            continue
        body[name] = value
    body["model"] = str(payload.get("model") or "glm-5.2")
    messages = normalize_roles(payload.get("messages") or [])
    messages = [m for m in messages if isinstance(m, dict)]
    messages, _ = merge_adjacent_tool_calls(messages)
    messages, _ = repack_tool_result_blocks(messages)
    messages, _ = cleanup_orphan_tool_calls(messages)
    if not messages:
        raise CodeArtsBadRequest("messages is empty")
    body["messages"] = messages
    body["stream"] = True
    body["stream_options"] = {"include_usage": True}
    conversation = payload.get("conversation_id")
    if not _valid_chat_id(conversation):
        conversation = _derive_chat_id(session_key)
    if conversation:
        body["conversation_id"] = conversation
    else:
        body.pop("conversation_id", None)
    return body


class CodeArtsAccount(object):
    """回程处理器需要的最小账号表面。

    `_chat_stream_response` / `_responses_*` / `_messages_*` 只读 uid、realm、
    chat_base_url()、proxy；这里全部给常量，不参与 WorkBuddy 的账号池、
    设备指纹、积分或熔断。
    """

    uid = "codearts"
    realm = "codearts"
    product = "codearts"
    proxy = None
    nickname = "CodeArts"
    chat_path = "/v1/chat/completions"

    def __init__(self, config=None):
        self.config = dict(config or {})
        self.base_url = str(self.config.get("base_url") or "")

    def chat_base_url(self):
        return self.base_url

    def model_disabled(self, mid):
        return False

    def is_cli_identity(self):
        return False


def open_chat(config, body, timeout=None):
    """POST 到 codearts2api 的 chat 端点，返回 SSE 响应对象。

    始终请求 stream=true：wb-hub 的非流式回程也会用 aggregate_stream 聚合，
    这样一套解析路径同时服务流式和非流式客户端。
    """
    if not configured_config(config):
        raise CodeArtsUnavailable(
            "CodeArts 出口未启用：请到设置页填写 Base URL 和 API Key。")
    timeout = timeout or config.get("stream_timeout_seconds") \
        or DEFAULTS["stream_timeout_seconds"]
    resp = _request(config, "POST", "/v1/chat/completions", payload=body,
                    timeout=timeout, stream=True)
    try:
        resp._wb_billed_model = body.get("model")
    except Exception:
        pass
    return resp


def configured_config(config):
    """已经加载好的配置是否可用（不读盘）。"""
    return bool(config and config.get("base_url") and config.get("api_key")
                and config.get("enabled"))

