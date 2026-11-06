# -*- coding: utf-8 -*-
"""Raccoon（小浣熊）原生客户端：配置、账号文件、token 续期与账号池。

协议来源是桌面客户端的 `auth.json` 与 `raccoon-desktop-proxy` 的行为：
`access_token` 是 IAM JWT（约 30 天），续期走
`POST /api/web/auth/v1/refresh`；账号池按账号并发、冷却、代理槽位调度。

本模块只负责 provider 自身。网关路由、模型目录、SSE 与面板由其它任务在
本模块上继续扩展；这里刻意不导入 `wb_codearts`，避免两个 provider 的行为
互相影响。

Python 3.9+ 标准库。所有出站 HTTP 都走 `wb_upstream_pool.urlopen`，让账号
代理槽位、no_proxy 与连接复用和其它 provider 保持一致。
"""

import base64
import hashlib
import html as html_module
import json
import math
import os
import re
import secrets
import socket
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

import wb_settings
import wb_upstream_pool

# ---------------------------------------------------------------------------
# 常量
# ---------------------------------------------------------------------------

#: Tests and private deployments monkeypatch this module attribute. Every URL
#: is derived from it at call time; never cache a full endpoint URL.
BASE_URL = os.environ.get("WB_RACCOON_BASE_URL") or "https://xiaohuanxiong.com"

EP_LOGIN = "/api/web/auth/v1/login_with_authorization_code"
EP_REFRESH = "/api/web/auth/v1/refresh"
EP_CATALOG = "/api/web/llm/v2/model_catalog"
EP_CHAT = "/api/web/llm/v2/chat/completions"

#: Browser authorization page. This is deliberately independent of `BASE_URL`:
#: `BASE_URL` points at the API host (and tests monkeypatch it), while the
#: desktop browser always authorizes on the public site.
AUTHORIZE_URL = os.environ.get("WB_RACCOON_AUTHORIZE_URL") or (
    "https://xiaohuanxiong.com/authorize"
)
OAUTH_REDIRECT_PATH = "/raccoon/oauth/callback"
OAUTH_TTL_SECONDS = 300
OAUTH_MAX_SESSIONS = 64

#: Desktop-client headers. The catalog accepts the same client identity as
#: chat; Task 3 reuses these constants for the SSE request.
CLIENT_PLATFORM = "desktop-linux-x64"
CLIENT_VERSION = "v1.1.5"
CLIENT_LANGUAGE = "zh"

ACCOUNTS_SUBDIR = "raccoon"
STATE_FILE = "state.json"

#: token 提前刷新窗口：与 CodeArts 一样，过期前 2 分钟主动续期。
REFRESH_SKEW = 120.0
#: 429/5xx 的软冷却；连续普通错误达到阈值后的冷却。
SOFT_COOLDOWN = 60.0
ERR_THRESHOLD = 3
ERR_COOLDOWN = 600.0
#: 错误体读取上限。
ERROR_BODY_MAX = 256 * 1024

DEFAULTS = {
    "enabled": False,
    "timeout_seconds": 60.0,
    "stream_timeout_seconds": 900.0,
    "models_ttl_seconds": 600.0,
    "max_concurrent": 1,
}

_LOCK = threading.RLock()

#: Absolute auth-file paths whose account was deleted. `refresh_auth()` checks
#: this even when it is called without an account object, so a late write can
#: never recreate a deleted file. `save_auth()` clears the uid's entries on an
#: explicit re-import.
_AUTH_TOMBSTONES = {}
_TOMBSTONE_LOCK = threading.Lock()


def _tombstone_auth_paths(paths, uid):
    with _TOMBSTONE_LOCK:
        for path in paths:
            _AUTH_TOMBSTONES[os.path.abspath(str(path))] = str(uid or "")


def _auth_path_tombstoned(path):
    with _TOMBSTONE_LOCK:
        return os.path.abspath(str(path)) in _AUTH_TOMBSTONES


def _clear_auth_tombstones(uid="", path=""):
    uid = str(uid or "")
    path = os.path.abspath(str(path)) if path else ""
    with _TOMBSTONE_LOCK:
        for key, value in list(_AUTH_TOMBSTONES.items()):
            if (uid and value == uid) or (path and key == path):
                _AUTH_TOMBSTONES.pop(key, None)


# ---------------------------------------------------------------------------
# 异常
# ---------------------------------------------------------------------------


class RaccoonError(Exception):
    def __init__(self, message, status=None, code=None):
        super(RaccoonError, self).__init__(message)
        self.status = status
        self.code = code


class RaccoonUnavailable(RaccoonError):
    """No usable account or the upstream cannot be reached."""


class RaccoonAuthError(RaccoonError):
    """The account needs re-login."""


class RaccoonBadRequest(RaccoonError):
    """The client request/auth document is invalid; never fail over."""


class RaccoonNotFound(RaccoonError):
    """The requested account or session does not exist."""


class RaccoonRateLimited(RaccoonError):
    """The upstream rejected the request with 429."""


class RaccoonUpstreamError(RaccoonError):
    """The upstream failed (5xx, unparseable payload, embedded error)."""


class RaccoonDeleteError(RaccoonError):
    """The account file could not be removed; the account stays in the pool."""


# ---------------------------------------------------------------------------
# 配置
# ---------------------------------------------------------------------------


def _positive_float(value, default, low, high):
    try:
        out = float(value)
    except (TypeError, ValueError, OverflowError):
        return float(default)
    if not math.isfinite(out):
        return float(default)
    return min(max(out, float(low)), float(high))


def _positive_int(value, default, low, high):
    try:
        out = int(value)
    except (TypeError, ValueError, OverflowError):
        return int(default)
    return min(max(out, int(low)), int(high))


def load_config(accounts_dir):
    """Read and clamp the Raccoon config; malformed values fall back."""
    raw = wb_settings.load(accounts_dir).get("raccoon")
    if not isinstance(raw, dict):
        raw = {}
    return {
        "enabled": raw.get("enabled") is True,
        "timeout_seconds": _positive_float(
            raw.get("timeout_seconds"), DEFAULTS["timeout_seconds"], 1.0, 600.0
        ),
        "stream_timeout_seconds": _positive_float(
            raw.get("stream_timeout_seconds"),
            DEFAULTS["stream_timeout_seconds"],
            1.0,
            3600.0,
        ),
        "models_ttl_seconds": _positive_float(
            raw.get("models_ttl_seconds"), DEFAULTS["models_ttl_seconds"], 60.0, 86400.0
        ),
        "max_concurrent": _positive_int(
            raw.get("max_concurrent"), DEFAULTS["max_concurrent"], 1, 10
        ),
        "accounts_dir": accounts_dir,
    }


def save_config(accounts_dir, patch):
    """Merge `patch` under `raccoon`, preserving every other settings section.

    The read-modify-write runs under `_LOCK`; `wb_settings.save()` only locks
    its own write, so two panel saves could otherwise overwrite each other.
    """
    if not isinstance(patch, dict):
        raise ValueError("raccoon settings must be an object")
    with _LOCK:
        current = load_config(accounts_dir)
        merged = dict(current)
        for key in DEFAULTS:
            if key in patch:
                merged[key] = patch[key]
        merged["enabled"] = merged.get("enabled") is True
        merged["timeout_seconds"] = _positive_float(
            merged.get("timeout_seconds"), DEFAULTS["timeout_seconds"], 1.0, 600.0
        )
        merged["stream_timeout_seconds"] = _positive_float(
            merged.get("stream_timeout_seconds"),
            DEFAULTS["stream_timeout_seconds"],
            1.0,
            3600.0,
        )
        merged["models_ttl_seconds"] = _positive_float(
            merged.get("models_ttl_seconds"),
            DEFAULTS["models_ttl_seconds"],
            60.0,
            86400.0,
        )
        merged["max_concurrent"] = _positive_int(
            merged.get("max_concurrent"), DEFAULTS["max_concurrent"], 1, 10
        )

        data = wb_settings.load(accounts_dir)
        stored = data.get("raccoon")
        section = dict(stored) if isinstance(stored, dict) else {}
        for key in DEFAULTS:
            section[key] = merged[key]
        data["raccoon"] = section
        wb_settings.save(accounts_dir, data)
        return load_config(accounts_dir)


def configured(accounts_dir):
    return load_config(accounts_dir)["enabled"]


def configured_config(config):
    return bool(config and config.get("enabled"))


def masked_config(accounts_dir):
    cfg = load_config(accounts_dir)
    pool = pool_for(accounts_dir)
    out = {key: value for key, value in cfg.items() if key != "accounts_dir"}
    out["configured"] = bool(cfg["enabled"])
    out["accounts"] = len(pool.accounts) if pool else 0
    out["healthy"] = pool.stats()["healthy"] if pool else 0
    return out


# ---------------------------------------------------------------------------
# 账号文件
# ---------------------------------------------------------------------------


def accounts_path(accounts_dir):
    return os.path.join(accounts_dir, ACCOUNTS_SUBDIR)


def _auth_filename(uid):
    safe = re.sub(r"[^A-Za-z0-9._-]+", "_", str(uid or ""))
    return "raccoon-%s.json" % (safe or "unknown")


def _atomic_write(path, payload):
    """Write JSON atomically and, on POSIX, keep the file private (0600)."""
    folder = os.path.dirname(path) or "."
    os.makedirs(folder, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".raccoon-", suffix=".tmp", dir=folder)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, ensure_ascii=False, indent=2)
            fh.flush()
            try:
                os.fsync(fh.fileno())
            except OSError:
                pass
        if os.name == "posix":
            os.chmod(tmp, 0o600)
        os.replace(tmp, path)
        if os.name == "posix":
            try:
                os.chmod(path, 0o600)
            except OSError:
                pass
    except Exception:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _clean_token(value):
    text = str(value or "").strip()
    if text.lower().startswith("bearer "):
        text = text[7:].strip()
    return text


def jwt_claims(token):
    """Decode the payload segment of a JWT without verifying its signature.

    Raccoon access tokens are IAM JWTs. A malformed or opaque token returns an
    empty dict rather than raising: the caller only uses `sub` and `exp`.
    """
    text = _clean_token(token)
    parts = text.split(".")
    if len(parts) < 2 or not parts[1]:
        return {}
    segment = parts[1]
    padding = "=" * (-len(segment) % 4)
    try:
        raw = base64.urlsafe_b64decode((segment + padding).encode("ascii"))
        parsed = json.loads(raw.decode("utf-8"))
    except Exception:
        return {}
    return parsed if isinstance(parsed, dict) else {}


def uid_from_auth(auth):
    """UID is the JWT `sub`, or a stable hash for opaque tokens."""
    if not isinstance(auth, dict):
        return ""
    claims = jwt_claims(auth.get("access_token"))
    sub = str(claims.get("sub") or "").strip()
    if sub:
        return sub
    seed = _clean_token(auth.get("refresh_token")) or _clean_token(
        auth.get("access_token")
    )
    if not seed:
        return ""
    return "raccoon-" + hashlib.sha256(seed.encode("utf-8")).hexdigest()[:12]


def expires_at(auth):
    """Token expiry as a Unix timestamp; 0 when the token carries no `exp`."""
    if not isinstance(auth, dict):
        return 0.0
    claims = jwt_claims(auth.get("access_token"))
    try:
        exp = float(claims.get("exp") or 0)
    except (TypeError, ValueError):
        exp = 0.0
    if exp > 0:
        return exp
    try:
        fallback = float(auth.get("expires_at") or 0)
    except (TypeError, ValueError):
        fallback = 0.0
    return fallback if fallback > 0 else 0.0


def auth_from_payload(payload):
    """Normalize a flat `auth.json` or an auth response into account data.

    Accepts `{"data": {...}}` from the login/refresh endpoints, strips a
    `Bearer ` prefix from either token field, and requires an access token.
    """
    if not isinstance(payload, dict):
        raise RaccoonBadRequest("auth payload must be an object")
    source = dict(payload)
    data = payload.get("data")
    if isinstance(data, dict):
        source.update(data)
    access = _clean_token(
        source.get("access_token") or source.get("token") or source.get("auth_token")
    )
    if not access:
        raise RaccoonBadRequest("auth payload requires access_token")
    auth = {
        "access_token": access,
        "refresh_token": _clean_token(source.get("refresh_token")),
        "token_type": str(source.get("token_type") or "Bearer").strip() or "Bearer",
        "office_identity": str(source.get("office_identity") or "").strip(),
        "office_org_name": str(source.get("office_org_name") or "").strip(),
        "office_org_role": str(source.get("office_org_role") or "").strip(),
        "updated_at": int(time.time()),
    }
    proxy_slot = str(source.get("proxySlot") or source.get("proxy_slot") or "").strip()
    if proxy_slot:
        auth["proxySlot"] = proxy_slot
    uid = uid_from_auth(auth)
    if uid:
        auth["uid"] = uid
    return auth


def load_auths(accounts_dir):
    """Read every `accounts/raccoon/raccoon-*.json` document."""
    folder = accounts_path(accounts_dir)
    out = []
    try:
        names = sorted(os.listdir(folder))
    except OSError:
        return out
    for name in names:
        if not name.startswith("raccoon-") or not name.endswith(".json"):
            continue
        path = os.path.join(folder, name)
        try:
            with open(path, encoding="utf-8") as fh:
                data = json.load(fh)
        except Exception:
            continue
        if not isinstance(data, dict):
            continue
        if not _clean_token(data.get("access_token")):
            continue
        if not data.get("uid"):
            data["uid"] = uid_from_auth(data)
        if not data.get("uid"):
            continue
        data["_path"] = path
        out.append(data)
    return out


def save_auth(accounts_dir, auth):
    """Persist one auth document atomically, returning it with `_path`."""
    if not isinstance(auth, dict):
        raise RaccoonBadRequest("auth must be an object")
    auth = dict(auth)
    path = auth.pop("_path", None)
    access = _clean_token(auth.get("access_token"))
    if not access:
        raise RaccoonBadRequest("auth requires access_token")
    auth["access_token"] = access
    uid = str(auth.get("uid") or uid_from_auth(auth))
    if not uid:
        raise RaccoonBadRequest("auth requires a uid")
    auth["uid"] = uid
    if not auth.get("updated_at"):
        auth["updated_at"] = int(time.time())
    if not path:
        path = os.path.join(accounts_path(accounts_dir), _auth_filename(uid))
    # An explicit save (import/re-import) is the only caller that may revive a
    # tombstoned uid; refresh writes use `_atomic_write` directly.
    _clear_auth_tombstones(uid, path)
    _atomic_write(path, auth)
    auth["_path"] = path
    return auth


def _auth_file_uid(path):
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except Exception:
        return ""
    if not isinstance(data, dict):
        return ""
    return str(data.get("uid") or uid_from_auth(data) or "")


def _auth_paths_for_uid(accounts_dir, uid):
    """Every auth file that carries `uid`, including duplicate names."""
    uid = str(uid or "")
    folder = accounts_path(accounts_dir)
    paths = []
    canonical = os.path.join(folder, _auth_filename(uid))
    if os.path.exists(canonical):
        paths.append(canonical)
    try:
        names = sorted(os.listdir(folder))
    except OSError:
        names = []
    for name in names:
        if not name.startswith("raccoon-") or not name.endswith(".json"):
            continue
        path = os.path.join(folder, name)
        if path in paths:
            continue
        if uid and _auth_file_uid(path) == uid:
            paths.append(path)
    return paths


def delete_auth(accounts_dir, uid):
    """Remove every auth file for `uid`; False if any file survives."""
    uid = str(uid or "")
    if not uid:
        return False
    paths = _auth_paths_for_uid(accounts_dir, uid)
    if not paths:
        # No matching file remains: the desired end state already holds.
        return True
    for path in paths:
        try:
            os.remove(path)
        except FileNotFoundError:
            pass
        except OSError:
            pass
    return not any(os.path.exists(path) for path in paths)


# ---------------------------------------------------------------------------
# HTTP / 续期
# ---------------------------------------------------------------------------


_SECRET_AUTH_FIELDS = ("access_token", "refresh_token", "authorization_code", "code")


def _auth_secrets(auth):
    """Known credentials on an auth document, longest first for replacement."""
    if not isinstance(auth, dict):
        return []
    out = []
    for key in _SECRET_AUTH_FIELDS:
        value = _clean_token(auth.get(key))
        if value and value not in out:
            out.append(value)
    out.sort(key=len, reverse=True)
    return out


def _redact(text, secrets=None):
    """Remove known tokens and credential-shaped labels from upstream text."""
    text = str(text or "")
    # Literal replacement first: an opaque token has no recognizable shape, so
    # only the caller's known values can identify it.
    ordered = sorted(
        {str(item) for item in (secrets or ()) if str(item)},
        key=len,
        reverse=True,
    )
    for secret in ordered:
        text = text.replace(secret, "<redacted>")
    text = re.sub(r"(?i)bearer\s+[^\s,;\"']+", "Bearer <redacted>", text)
    # Query/form style and unquoted JSON-ish pairs: code=..., access_token=...
    text = re.sub(
        r"(?i)\b(authorization_code|access_token|refresh_token|authorization|code)"
        r"(\s*[=:]\s*)([^\s,;\"'}&]+)",
        r"\1\2<redacted>",
        text,
    )
    # Quoted JSON keys: {"code": "...", "authorization_code": "..."}.
    text = re.sub(
        r"(?i)([\"'](?:authorization_code|access_token|refresh_token|"
        r"authorization|code)[\"']\s*:\s*)([\"'])([^\"']*)\2",
        r"\1\2<redacted>\2",
        text,
    )
    text = re.sub(
        r"[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*",
        "<redacted-jwt>",
        text,
    )
    return text


def _mask_proxy_url(value):
    """Hide `user:password@` credentials in a resolved proxy URL."""
    text = str(value or "")
    if not text:
        return ""
    return re.sub(
        r"(?i)(\b[a-z][a-z0-9+.-]*://)[^/@\s]+@", r"\1<redacted>@", text
    )


def _error_text(raw, secrets=None):
    try:
        parsed = json.loads(raw.decode("utf-8", "replace"))
    except Exception:
        parsed = None
    message = ""
    if isinstance(parsed, dict):
        for key in ("message", "error_msg", "error_description", "error"):
            value = parsed.get(key)
            if isinstance(value, dict):
                value = value.get("message") or value.get("error_msg")
            if isinstance(value, str) and value.strip():
                message = value.strip()
                break
        if not message and isinstance(parsed.get("data"), dict):
            value = parsed["data"].get("message")
            if isinstance(value, str) and value.strip():
                message = value.strip()
        if not message:
            code = parsed.get("code") or parsed.get("error_code")
            if code not in (None, "", 0, "0"):
                message = "error_code=%s" % code
    if not message:
        message = raw.decode("utf-8", "replace")[:500]
    return _redact(message, secrets)[:500]


def _raise_for_status(status, raw, secrets=None):
    message = _error_text(raw, secrets)
    if status in (401, 403):
        raise RaccoonAuthError(message, status=status)
    if status == 400:
        raise RaccoonBadRequest(message, status=status)
    if status == 404:
        raise RaccoonNotFound(message, status=status)
    if status == 429:
        raise RaccoonRateLimited(message, status=status)
    if status >= 500:
        raise RaccoonUpstreamError(
            "Raccoon returned HTTP %s: %s" % (status, message), status=status
        )
    raise RaccoonError(
        "Raccoon returned HTTP %s: %s" % (status, message), status=status
    )


def _request_json(
    method, url, headers=None, body=None, timeout=30.0, proxy="", secrets=None
):
    """One pooled JSON request; maps transport/HTTP failures to Raccoon*."""
    merged = {"Accept": "application/json"}
    merged.update(headers or {})
    req = urllib.request.Request(url, data=body, headers=merged, method=method)
    try:
        try:
            resp = wb_upstream_pool.urlopen(req, timeout=timeout, proxy=proxy)
        except wb_upstream_pool.PoolBypass:
            # A bypass must keep the same exit IP as the pooled path. Import
            # lazily: wb_accounts is a large module and only this rare fallback
            # needs it.
            import wb_accounts

            opener = wb_accounts.opener_for_proxy(proxy)
            if opener is None:
                resp = urllib.request.urlopen(req, timeout=timeout)
            else:
                resp = opener.open(req, timeout=timeout)
        try:
            status = getattr(resp, "status", 200)
            raw = resp.read(ERROR_BODY_MAX)
        finally:
            try:
                resp.close()
            except Exception:
                pass
    except urllib.error.HTTPError as exc:
        try:
            raw = exc.read(ERROR_BODY_MAX)
        except Exception:
            raw = b""
        _raise_for_status(exc.code, raw, secrets)
        return {}
    except (
        urllib.error.URLError,
        socket.timeout,
        TimeoutError,
        ConnectionError,
        OSError,
    ) as exc:
        reason = getattr(exc, "reason", None) or exc
        raise RaccoonUnavailable(
            _redact("cannot reach Raccoon: %s" % (reason,), secrets)
        )
    if status < 200 or status >= 300:
        _raise_for_status(status, raw, secrets)
    try:
        parsed = json.loads(raw.decode("utf-8", "replace"))
    except Exception:
        raise RaccoonUpstreamError(
            "Raccoon returned a non-JSON response (HTTP %s)" % status, status=status
        )
    if not isinstance(parsed, dict):
        raise RaccoonUpstreamError(
            "Raccoon returned an unexpected response (HTTP %s)" % status, status=status
        )
    return parsed


def refresh_auth(auth, timeout=30.0, proxy="", acct=None):
    """Exchange a refresh token for a new access/refresh pair.

    When `auth` carries `_path` (the normal load/save round-trip), the rotated
    document is written back atomically. A 401/403 or an invalid success body
    raises `RaccoonAuthError`: only re-importing/auth-flow can recover.

    With `acct`, the whole exchange holds the account's refresh lock and the
    write is refused after the account has been deleted. This is what makes
    account deletion durable while a refresh is in flight.
    """
    if acct is None:
        return _refresh_auth(auth, timeout=timeout, proxy=proxy, acct=None)
    with acct._refresh_lock:
        if not acct.live():
            raise RaccoonAuthError("account was deleted during token refresh")
        return _refresh_auth(auth, timeout=timeout, proxy=proxy, acct=acct)


def _refresh_auth(auth, timeout=30.0, proxy="", acct=None):
    if not isinstance(auth, dict):
        raise RaccoonAuthError("auth must be an object")
    if auth.get("_path") and _auth_path_tombstoned(auth.get("_path")):
        raise RaccoonAuthError("account was deleted during token refresh")
    refresh_token = _clean_token(auth.get("refresh_token"))
    if not refresh_token:
        raise RaccoonAuthError(
            "refresh_token is missing; import an auth.json with a refresh token"
        )
    access = _clean_token(auth.get("access_token"))
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    if access:
        headers["Authorization"] = "Bearer " + access
    body = json.dumps({"refresh_token": refresh_token}).encode("utf-8")
    secrets = [access, refresh_token]
    try:
        payload = _request_json(
            "POST",
            BASE_URL + EP_REFRESH,
            headers,
            body,
            timeout=timeout,
            proxy=proxy,
            secrets=secrets,
        )
    except RaccoonBadRequest as exc:
        # The refresh endpoint reports an invalid grant as 400; it is an auth
        # failure for the account, not a client request error.
        raise RaccoonAuthError(_redact(str(exc), secrets), status=exc.status)
    root = payload.get("data") if isinstance(payload.get("data"), dict) else payload
    new_access = _clean_token(root.get("access_token"))
    if not new_access:
        raise RaccoonAuthError("refresh response did not include access_token")

    merged = dict(auth)
    merged["access_token"] = new_access
    new_refresh = _clean_token(root.get("refresh_token"))
    if new_refresh:
        merged["refresh_token"] = new_refresh
    for key in ("token_type", "office_identity", "office_org_name", "office_org_role"):
        if root.get(key):
            merged[key] = str(root.get(key)).strip()
    if root.get("expires_at"):
        merged["expires_at"] = root.get("expires_at")
    else:
        merged.pop("expires_at", None)
    claims = jwt_claims(new_access)
    merged["uid"] = (
        str(claims.get("sub") or "").strip()
        or str(auth.get("uid") or "").strip()
        or uid_from_auth(merged)
    )
    merged["updated_at"] = int(time.time())

    path = auth.get("_path")
    merged.pop("_path", None)
    if path:
        if acct is not None and not acct.live():
            raise RaccoonAuthError("account was deleted during token refresh")
        if _auth_path_tombstoned(path):
            raise RaccoonAuthError("account was deleted during token refresh")
        _atomic_write(path, merged)
        merged["_path"] = path
    return merged


def ensure_token(acct, force=False, expected_generation=None):
    """Refresh `acct` single-flight, double-checking under its lock.

    A force refresh is what a 401 retry uses. Two concurrent 401s both capture
    the same auth generation; the first rotates it and the second returns
    without consuming the one-time refresh token again.

    `expected_generation` lets a caller pin the credentials it sent upstream:
    if a re-import or another request rotated them while the response was in
    flight, this returns without refreshing (or writing) again.
    """
    force = bool(force)
    if acct is None or getattr(acct, "deleted", False):
        raise RaccoonAuthError("account was deleted")
    observed = (
        int(expected_generation)
        if expected_generation is not None
        else acct.auth_generation
    )
    if not force and not acct.expiring_soon() and not acct.expired():
        return
    with acct._refresh_lock:
        if acct.auth_generation != observed:
            return
        if not acct.live():
            raise RaccoonAuthError("account was deleted during token refresh")
        if force:
            if acct.auth_generation != observed:
                return
        elif not acct.expiring_soon() and not acct.expired():
            return
        timeout = float(
            acct.pool.config.get("timeout_seconds") or DEFAULTS["timeout_seconds"]
        )
        try:
            data = refresh_auth(acct.auth, timeout=timeout, proxy=acct.proxy, acct=acct)
        except RaccoonAuthError as exc:
            message = _redact(str(exc), _auth_secrets(acct.auth))
            if message != str(exc):
                exc.args = (message,)
            acct.pool.note_error(acct, message, terminal=True)
            raise
        except RaccoonError:
            if force or acct.expired():
                raise
            return
        if acct.auth_generation != observed:
            return
        acct.update_auth(data, expected_generation=observed)


# ---------------------------------------------------------------------------
# 账号
# ---------------------------------------------------------------------------


def _fmt_duration(seconds):
    seconds = int(max(0, seconds))
    hours, rest = divmod(seconds, 3600)
    minutes, _ = divmod(rest, 60)
    if hours:
        return "%dh%dm" % (hours, minutes)
    return "%dm" % minutes


class RaccoonAccount(object):
    """One Raccoon account plus its runtime state."""

    realm = "raccoon"
    product = "raccoon"
    chat_path = "/api/web/llm/v2/chat/completions"

    def __init__(self, auth, pool):
        self.auth = dict(auth)
        self.pool = pool
        self.uid = str(auth.get("uid") or uid_from_auth(auth))
        self.name = self.uid
        self.nickname = str(auth.get("office_org_name") or self.uid)
        # `proxy_slot` is persisted; `proxy` is derived from the current slots.
        self.proxy_slot = str(auth.get("proxySlot") or "").strip()
        self.proxy = ""
        self.disabled = False
        self.disabled_reason = ""
        self.manual_disabled = False
        self.disabled_by = ""
        self.cool_until = 0.0
        self.err_count = 0
        self.last_error = ""
        self.note = ""
        self.tasks_enabled = True
        self.active_concurrent = 0
        self.max_concurrent = int(pool.config.get("max_concurrent") or 1)
        self.last_used = 0.0
        self.models = []
        self.models_fetched_at = 0.0
        self.models_source = ""
        self.auth_generation = 0
        self.deleted = False
        self._lock = threading.RLock()
        self._refresh_lock = threading.RLock()

    def live(self):
        """True while this object is still the pool's current entry for its uid."""
        if getattr(self, "deleted", False):
            return False
        with self.pool._lock:
            return any(item is self for item in self.pool.accounts)

    def chat_base_url(self):
        """Current upstream base; a test/override can move BASE_URL."""
        return BASE_URL

    # ---- 凭证 ----
    def expires_at(self):
        return expires_at(self.auth)

    def token_remaining(self):
        exp = self.expires_at()
        if not exp:
            return 0
        return max(0, exp - time.time())

    def expired(self):
        exp = self.expires_at()
        return bool(exp and exp <= time.time())

    def expiring_soon(self, skew=REFRESH_SKEW):
        exp = self.expires_at()
        return bool(exp and exp - time.time() <= skew)

    def update_auth(self, data, expected_generation=None):
        """Apply a refresh result, persist it, and recover non-manual terminal state.

        A manual disable is a panel decision, not an auth failure: rotating a
        token must never silently re-enable the account.
        """
        with self._refresh_lock:
            if expected_generation is not None and self.auth_generation != int(
                expected_generation
            ):
                raise RaccoonAuthError(
                    "account credentials changed during token refresh"
                )
            if not self.live():
                raise RaccoonAuthError("account was deleted during token refresh")
            data = dict(data or {})
            updated = dict(self.auth)
            for key in (
                "access_token",
                "refresh_token",
                "token_type",
                "office_identity",
                "office_org_name",
                "office_org_role",
            ):
                value = data.get(key)
                if value not in (None, ""):
                    updated[key] = value
            if data.get("expires_at"):
                updated["expires_at"] = data.get("expires_at")
            claims = jwt_claims(updated.get("access_token"))
            updated["uid"] = (
                str(claims.get("sub") or "").strip()
                or self.uid
                or uid_from_auth(updated)
            )
            updated["updated_at"] = int(time.time())
            if self.proxy_slot:
                updated["proxySlot"] = self.proxy_slot
            self.auth = save_auth(self.pool.accounts_dir, updated)
            self.uid = str(self.auth.get("uid") or self.uid)
            self.name = self.uid
            self.nickname = str(self.auth.get("office_org_name") or self.uid)
            self.proxy_slot = str(self.auth.get("proxySlot") or "").strip()
            self.auth_generation += 1
            if not self.manual_disabled:
                self.disabled = False
                self.disabled_reason = ""
                self.disabled_by = ""
                self.last_error = ""
                self.err_count = 0
                self.cool_until = 0.0
            self.pool.save_state()

    # ---- 状态快照 ----
    def view(self):
        now = time.time()
        cooling = (not self.disabled) and now < self.cool_until
        secrets = _auth_secrets(self.auth)
        reason = _redact(self.disabled_reason or self.last_error, secrets)
        last_error = _redact(self.last_error, secrets)
        return {
            "name": self.uid,
            "uid": self.uid,
            "nickname": self.nickname,
            "realm": "raccoon",
            "disabled": self.disabled,
            "cooling": cooling,
            "until": (
                time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(self.cool_until))
                if cooling
                else ""
            ),
            "err_count": self.err_count,
            "reason": reason,
            "last_error": last_error,
            "active_concurrent": self.active_concurrent,
            "max_concurrent": self.max_concurrent,
            "token_remaining": _fmt_duration(self.token_remaining()),
            "expires_at": (
                time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(self.expires_at()))
                if self.expires_at()
                else ""
            ),
            "models": len(self.models),
            "proxy_slot": self.proxy_slot,
            "proxy": _mask_proxy_url(self.proxy),
            "note": self.note,
            "tasks_enabled": bool(self.tasks_enabled),
        }

    def healthy(self, now=None):
        """Selectable? Expired tokens stay selectable so ensure_token can fix them."""
        now = now or time.time()
        return (not self.disabled) and (not self.deleted) and now >= self.cool_until

    def has_model(self, model):
        mid = str(model or "").strip().lower()
        if not mid or not self.models:
            return True
        return any(
            str(item.get("id") or "").strip().lower() == mid for item in self.models
        )


# ---------------------------------------------------------------------------
# 账号池
# ---------------------------------------------------------------------------


class RaccoonPool(object):
    def __init__(self, accounts_dir, config):
        self.accounts_dir = accounts_dir
        self.folder = accounts_path(accounts_dir)
        self.state_file = os.path.join(self.folder, STATE_FILE)
        self.config = dict(config)
        self.accounts = []
        self._affinity = {}
        self._tombstones = {}
        self._lock = threading.RLock()
        self.load()

    # ---- 持久化 ----
    def load(self):
        auths = load_auths(self.accounts_dir)
        state = self._load_state()
        accounts = []
        seen = set()
        for auth in auths:
            acct = RaccoonAccount(auth, self)
            if not acct.uid or acct.uid in seen:
                continue
            seen.add(acct.uid)
            entry = state.get(acct.uid) or {}
            acct.err_count = int(entry.get("err_count") or 0)
            acct.cool_until = float(entry.get("cool_until") or 0)
            acct.disabled = bool(entry.get("disabled"))
            acct.disabled_reason = str(entry.get("reason") or "")
            acct.manual_disabled = bool(entry.get("manual_disabled"))
            acct.disabled_by = str(entry.get("disabled_by") or "")
            # State written before `manual_disabled` existed only tagged manual
            # disables through the default reason. Preserve those decisions so
            # the first token rotation after an upgrade cannot undo them.
            if acct.disabled and not acct.manual_disabled:
                if (
                    acct.disabled_by == "manual"
                    or acct.disabled_reason == "manual disable"
                ):
                    acct.manual_disabled = True
                    acct.disabled_by = "manual"
                elif not acct.disabled_by:
                    acct.disabled_by = "auth"
            acct.last_error = str(entry.get("last_error") or "")
            acct.note = str(entry.get("note") or "")[:200]
            acct.tasks_enabled = bool(entry.get("tasks_enabled", True))
            accounts.append(acct)
        with self._lock:
            self.accounts = accounts
        self.apply_proxy_slots()

    def _load_state(self):
        try:
            with open(self.state_file, encoding="utf-8") as fh:
                raw = json.load(fh)
        except Exception:
            return {}
        out = {}
        if isinstance(raw, dict):
            if isinstance(raw.get("accounts"), list):
                raw = raw["accounts"]
            else:
                for uid, entry in raw.items():
                    if isinstance(entry, dict):
                        out[str(uid)] = entry
                return out
        if isinstance(raw, list):
            for item in raw:
                if isinstance(item, dict):
                    uid = item.get("uid") or item.get("name")
                    if uid:
                        out[str(uid)] = item
        return out

    def save_state(self):
        with self._lock:
            accounts = list(self.accounts)
        state = {}
        for acct in accounts:
            state[acct.uid] = {
                "disabled": bool(acct.disabled),
                "manual_disabled": bool(acct.manual_disabled),
                "disabled_by": acct.disabled_by,
                "reason": acct.disabled_reason,
                "cool_until": acct.cool_until,
                "err_count": acct.err_count,
                "last_error": acct.last_error,
                "note": acct.note,
                "tasks_enabled": bool(acct.tasks_enabled),
            }
        try:
            _atomic_write(self.state_file, state)
        except Exception:
            pass

    # ---- 查询 ----
    def get(self, uid):
        uid = str(uid or "")
        with self._lock:
            for acct in self.accounts:
                if acct.uid == uid or acct.name == uid:
                    return acct
        return None

    def list(self):
        with self._lock:
            accounts = list(self.accounts)
        return [acct.view() for acct in sorted(accounts, key=lambda a: a.uid)]

    def apply_proxy_slots(self, slots=None):
        """Resolve each persisted `proxySlot` against the current slot list."""
        if slots is None:
            slots = wb_settings.proxy_slots(self.accounts_dir)
        by_id = {entry["id"]: entry for entry in slots if entry.get("enabled")}
        with self._lock:
            for acct in self.accounts:
                slot = by_id.get(acct.proxy_slot)
                acct.proxy = slot["url"] if slot else ""

    def stats(self):
        now = time.time()
        total = healthy = disabled = cooling = 0
        with self._lock:
            accounts = list(self.accounts)
        for acct in accounts:
            total += 1
            if acct.disabled:
                disabled += 1
            elif now < acct.cool_until:
                cooling += 1
            else:
                healthy += 1
        return {
            "total": total,
            "healthy": healthy,
            "disabled": disabled,
            "cooling": cooling,
        }

    # ---- 增删启停 ----
    def add(self, auth):
        candidate = RaccoonAccount(auth, self)
        with self._lock:
            existing = None
            for account in self.accounts:
                if account.uid == candidate.uid:
                    existing = account
                    break
            if existing is None:
                self.accounts.append(candidate)
                self._tombstones.pop(candidate.uid, None)
        if existing is None:
            _clear_auth_tombstones(candidate.uid)
            self.save_state()
            self.apply_proxy_slots()
            return candidate

        # Re-import replaces credentials. Wait for an in-flight refresh so the
        # old pair cannot land afterwards, and bump the generation so a refresh
        # that captured the old pair before this import returns without writing.
        with existing._refresh_lock:
            if not existing.proxy_slot and candidate.proxy_slot:
                existing.proxy_slot = candidate.proxy_slot
            if existing.proxy_slot:
                candidate.auth["proxySlot"] = existing.proxy_slot
            _clear_auth_tombstones(candidate.uid)
            saved = save_auth(self.accounts_dir, candidate.auth)
            with self._lock:
                existing.auth = saved
                existing.proxy_slot = str(saved.get("proxySlot") or "").strip()
                existing.nickname = str(saved.get("office_org_name") or existing.uid)
                existing.auth_generation += 1
                existing.deleted = False
                self._tombstones.pop(existing.uid, None)
                if not existing.manual_disabled:
                    existing.disabled = False
                    existing.disabled_reason = ""
                    existing.disabled_by = ""
                    existing.last_error = ""
                    existing.err_count = 0
                    existing.cool_until = 0.0
                existing.models = []
                existing.models_fetched_at = 0.0
        self.save_state()
        self.apply_proxy_slots()
        return existing

    def remove(self, uid):
        acct = self.get(uid)
        if acct is None:
            return False
        # Tombstone first: a refresh that has not started yet must abort, and a
        # catalog/discovery path must not treat the account as still present.
        with self._lock:
            self._tombstones[acct.uid] = time.time()
            acct.deleted = True
        tombstoned_paths = _auth_paths_for_uid(self.accounts_dir, acct.uid)
        tombstoned_paths.append(
            os.path.join(accounts_path(self.accounts_dir), _auth_filename(acct.uid))
        )
        _tombstone_auth_paths(tombstoned_paths, acct.uid)
        with acct._refresh_lock:
            # An in-flight refresh either finished before we got the lock (so
            # its file is deleted below) or sees the tombstone and refuses to
            # write. Re-check once more to cover a writer that completed just
            # as the tombstone was raised.
            removed = delete_auth(self.accounts_dir, acct.uid)
            if removed and _auth_paths_for_uid(self.accounts_dir, acct.uid):
                removed = delete_auth(self.accounts_dir, acct.uid)
        if not removed:
            _clear_auth_tombstones(acct.uid)
            with self._lock:
                self._tombstones.pop(acct.uid, None)
                acct.deleted = False
            return False
        # Keep the path tombstones after a successful delete: a refresh that
        # was already on the wire must not recreate the file afterwards.
        _tombstone_auth_paths(tombstoned_paths, acct.uid)
        with self._lock:
            self.accounts = [
                account for account in self.accounts if account is not acct
            ]
        self.save_state()
        return True

    def enable(self, uid):
        acct = self.get(uid)
        if acct is None:
            return False
        acct.disabled = False
        acct.manual_disabled = False
        acct.disabled_by = ""
        acct.disabled_reason = ""
        acct.last_error = ""
        acct.err_count = 0
        acct.cool_until = 0.0
        self.save_state()
        return True

    def disable(self, uid, reason="manual disable"):
        acct = self.get(uid)
        if acct is None:
            return False
        acct.disabled = True
        acct.manual_disabled = True
        acct.disabled_by = "manual"
        acct.disabled_reason = str(reason or "manual disable")
        acct.last_error = acct.disabled_reason
        self.save_state()
        return True

    def clear_cooldown(self, uid):
        acct = self.get(uid)
        if acct is None:
            return False
        acct.cool_until = 0.0
        acct.err_count = 0
        self.save_state()
        return True

    def set_note(self, uid, note):
        acct = self.get(uid)
        if acct is None:
            return False
        acct.note = str(note or "").strip()[:200]
        self.save_state()
        return True

    def set_tasks_enabled(self, uid, enabled):
        acct = self.get(uid)
        if acct is None:
            return False
        acct.tasks_enabled = bool(enabled)
        self.save_state()
        return True

    def set_proxy_slot(self, uid, slot_id):
        acct = self.get(uid)
        if acct is None:
            return False
        slot_id = str(slot_id or "").strip()
        acct.proxy_slot = slot_id
        auth = dict(acct.auth)
        if slot_id:
            auth["proxySlot"] = slot_id
        else:
            auth.pop("proxySlot", None)
        acct.auth = save_auth(self.accounts_dir, auth)
        self.apply_proxy_slots()
        return True

    # ---- 调度 ----
    def pick(self, model="", session_key=None, exclude=None, allowed_accounts=None):
        """Choose a healthy, allowed account; least busy then least recently used."""
        exclude = set(exclude or ())
        now = time.time()
        with self._lock:
            accounts = list(self.accounts)
        if session_key:
            uid = self._affinity.get(str(session_key))
            if (
                uid
                and uid not in exclude
                and (allowed_accounts is None or uid in allowed_accounts)
            ):
                acct = self.get(uid)
                if (
                    acct
                    and acct.healthy(now)
                    and acct.has_model(model)
                    and acct.active_concurrent < acct.max_concurrent
                ):
                    return acct
        candidates = [
            a
            for a in accounts
            if a.uid not in exclude
            and (allowed_accounts is None or a.uid in allowed_accounts)
            and a.healthy(now)
            and a.active_concurrent < a.max_concurrent
        ]
        capable = [a for a in candidates if a.has_model(model)]
        selected = capable or candidates
        if not selected:
            return None
        selected.sort(key=lambda a: (a.active_concurrent, a.last_used or 0))
        return selected[0]

    def bind_session(self, session_key, uid):
        if not session_key or not uid:
            return
        self._affinity[str(session_key)] = str(uid)
        if len(self._affinity) > 4096:
            for key in list(self._affinity)[:1024]:
                self._affinity.pop(key, None)

    def acquire(self, acct):
        if acct is None:
            return False
        with acct._lock:
            if acct.active_concurrent >= acct.max_concurrent:
                return False
            acct.active_concurrent += 1
            acct.last_used = time.time()
            return True

    def release(self, acct):
        if acct is None:
            return
        with acct._lock:
            if acct.active_concurrent > 0:
                acct.active_concurrent -= 1

    def note_success(self, acct):
        if acct is None or getattr(acct, "deleted", False):
            return
        if acct.err_count or acct.cool_until or acct.last_error:
            acct.err_count = 0
            acct.cool_until = 0.0
            acct.last_error = ""
            self.save_state()

    def note_error(self, acct, message, soft=False, terminal=False):
        if acct is None or getattr(acct, "deleted", False):
            return
        acct.last_error = _redact(str(message or ""), _auth_secrets(acct.auth))[:200]
        if terminal:
            if not acct.manual_disabled:
                acct.disabled = True
                acct.disabled_by = "auth"
                acct.disabled_reason = acct.last_error
        elif soft:
            acct.cool_until = time.time() + SOFT_COOLDOWN
        else:
            acct.err_count += 1
            if acct.err_count >= ERR_THRESHOLD:
                acct.cool_until = time.time() + ERR_COOLDOWN
                acct.err_count = 0
        self.save_state()


_POOLS = {}
_POOLS_LOCK = threading.Lock()


def pool_for(accounts_dir):
    """One pool per accounts dir; config changes update account concurrency."""
    key = os.path.abspath(str(accounts_dir or "."))
    cfg = load_config(accounts_dir)
    with _POOLS_LOCK:
        pool = _POOLS.get(key)
        if pool is None:
            pool = RaccoonPool(accounts_dir, cfg)
            _POOLS[key] = pool
        else:
            pool.config = cfg
            for acct in pool.accounts:
                acct.max_concurrent = cfg.get("max_concurrent", 1)
    return pool


def invalidate_pool(accounts_dir=None):
    with _POOLS_LOCK:
        if accounts_dir is None:
            _POOLS.clear()
        else:
            _POOLS.pop(os.path.abspath(str(accounts_dir)), None)


def apply_proxy_slots(accounts_dir, slots=None):
    """Re-resolve Raccoon account exits after the panel saves slots."""
    pool_for(accounts_dir).apply_proxy_slots(slots)


def drop_missing_bindings(accounts_dir, slots):
    """Clear Raccoon bindings that point at a slot which no longer exists."""
    valid = {entry["id"] for entry in slots}
    pool = pool_for(accounts_dir)
    changed = 0
    for acct in list(pool.accounts):
        if acct.proxy_slot and acct.proxy_slot not in valid:
            acct.proxy_slot = ""
            auth = dict(acct.auth)
            auth.pop("proxySlot", None)
            acct.auth = save_auth(accounts_dir, auth)
            changed += 1
    # Always re-resolve: a surviving slot may have had its URL edited.
    pool.apply_proxy_slots(slots)
    return changed


# ---------------------------------------------------------------------------
# 模型目录
# ---------------------------------------------------------------------------

#: Verified fallback catalog. The upstream catalogue is authoritative; these
#: entries keep `/v1/models` usable when a fetch fails. IDs stay bare: no
#: provider namespace is added or stripped.
SEED_MODELS = [
    {
        "id": "raccoon-090dd3",
        "name": "Raccoon-Work",
        "context_window": 1000000,
        "max_tokens": 100000,
        "tags": ["vision", "reasoning"],
    },
    {
        "id": "raccoon-19b265",
        "name": "Raccoon-Work-260817-A",
        "context_window": 1000000,
        "max_tokens": 100000,
        "tags": [],
    },
    {
        "id": "raccoon-405a1c",
        "name": "Raccoon-Work-260817-B",
        "context_window": 1000000,
        "max_tokens": 100000,
        "tags": [],
    },
    {
        "id": "sn-sensenova-6-8-flash",
        "name": "SenseNova 6.8 Flash",
        "context_window": 256000,
        "max_tokens": 63999,
        "tags": ["vision"],
    },
    {
        "id": "sn-sensenova-6-8-flash-lite",
        "name": "SenseNova 6.8 Flash Lite",
        "context_window": 256000,
        "max_tokens": 63999,
        "tags": ["vision"],
    },
    {
        "id": "sn-glm-5-3",
        "name": "GLM-5.3",
        "context_window": 1000000,
        "max_tokens": 100000,
        "tags": ["reasoning"],
    },
    {
        "id": "sn-kimi-k3",
        "name": "Kimi K3",
        "context_window": 1000000,
        "max_tokens": 100000,
        "tags": ["vision", "reasoning"],
    },
    {
        "id": "sn-glm-5-3-flash",
        "name": "GLM-5.3 Flash",
        "context_window": 1000000,
        "max_tokens": 100000,
        "tags": ["vision"],
    },
    {
        "id": "sn-deepseek-v4-1-flash-1017",
        "name": "DeepSeek V4.1 Flash 1017",
        "context_window": 1000000,
        "max_tokens": 100000,
        "tags": ["vision", "reasoning"],
    },
]

_SEED_BY_ID = {str(item["id"]).lower(): item for item in SEED_MODELS}

#: `{models, default_model, fetched_at, source}` shared by every account. The
#: TTL comes from `models_ttl_seconds`; a failed fetch never replaces a good
#: cached catalog with an empty one.
_CATALOG_CACHE = {
    "models": [],
    "default_model": "",
    "fetched_at": 0.0,
    "source": "seed",
}
_CATALOG_LAST_ERROR = ""
_CATALOG_LOCK = threading.RLock()


def _int_value(value):
    if isinstance(value, bool):
        return 0
    try:
        number = int(value)
    except (TypeError, ValueError, OverflowError):
        try:
            number = int(float(value))
        except (TypeError, ValueError, OverflowError):
            return 0
    return max(0, number)


def _first_int(*values):
    for value in values:
        number = _int_value(value)
        if number > 0:
            return number
    return 0


def _bare_model_id(value):
    """Trim surrounding whitespace; do not rewrite provider namespaces."""
    return str(value or "").strip()


def _tag_list(value):
    if isinstance(value, str):
        raw = re.split(r"[,\s]+", value)
    elif isinstance(value, (list, tuple)):
        raw = list(value)
    else:
        raw = []
    tags = []
    for item in raw:
        text = str(item or "").strip().lower()
        if text and text not in tags:
            tags.append(text)
    return tags


def _normalize_catalog(payload):
    """Normalize the first `chat` category of a model_catalog response."""
    if not isinstance(payload, dict):
        raise RaccoonUpstreamError("Raccoon returned an unexpected catalog response")
    data = payload.get("data") if isinstance(payload.get("data"), dict) else payload
    categories = data.get("categories") if isinstance(data, dict) else ()
    chat = None
    for category in categories if isinstance(categories, list) else ():
        if not isinstance(category, dict):
            continue
        if str(category.get("type") or "").strip().lower() != "chat":
            continue
        chat = category
        break

    models = []
    seen = set()
    if isinstance(chat, dict):
        for item in chat.get("models") or ():
            if not isinstance(item, dict):
                continue
            mid = _bare_model_id(
                item.get("name") or item.get("model_name") or item.get("id")
            )
            if not mid or mid.lower() in seen:
                continue
            seen.add(mid.lower())
            params = item.get("params") if isinstance(item.get("params"), dict) else {}
            seed = _SEED_BY_ID.get(mid.lower()) or {}
            context_window = _first_int(
                params.get("context_window"),
                item.get("context_window"),
                params.get("context_length"),
                item.get("context_length"),
                params.get("max_input_tokens"),
                item.get("max_input_tokens"),
                seed.get("context_window"),
            )
            max_tokens = _first_int(
                params.get("max_tokens"),
                item.get("max_tokens"),
                params.get("max_output_tokens"),
                item.get("max_output_tokens"),
                params.get("max_completion_tokens"),
                item.get("max_completion_tokens"),
                seed.get("max_tokens"),
            )
            tags = _tag_list(item.get("tags") if "tags" in item else params.get("tags"))
            display = str(
                item.get("display_name") or item.get("description") or mid
            ).strip()[:120]
            models.append(
                {
                    "id": mid,
                    "name": display or mid,
                    "context_window": context_window,
                    "max_tokens": max_tokens,
                    "tags": tags,
                    "vision": "vision" in tags,
                    "reasoning": "reasoning" in tags,
                }
            )

    default_model = ""
    if isinstance(chat, dict):
        default_model = _bare_model_id(
            chat.get("default_model") or chat.get("defaultModel")
        )
    if not default_model and models:
        default_model = models[0]["id"]
    return {
        "models": models,
        "default_model": default_model,
        "fetched_at": time.time(),
        "source": "raccoon",
        "warning": "",
    }


def _account_headers(acct, accept="application/json"):
    """Desktop-client headers; `X-Org-Code` only for a real org identity."""
    headers = {
        "Accept": accept,
        "Authorization": "Bearer "
        + _clean_token((acct.auth or {}).get("access_token")),
        "X-Client-Platform": CLIENT_PLATFORM,
        "X-Client-Version": CLIENT_VERSION,
        "X-Raccoon-Language": CLIENT_LANGUAGE,
    }
    identity = str((acct.auth or {}).get("office_identity") or "").strip()
    if identity and identity.lower() not in ("personal", "none"):
        headers["X-Org-Code"] = identity
    return headers


def _cached_catalog():
    with _CATALOG_LOCK:
        return {
            "models": [dict(item) for item in _CATALOG_CACHE["models"]],
            "default_model": _CATALOG_CACHE["default_model"],
            "fetched_at": _CATALOG_CACHE["fetched_at"],
            "source": _CATALOG_CACHE["source"],
        }


def _seed_model_list():
    return [dict(item, tags=list(item.get("tags") or [])) for item in SEED_MODELS]


def _adopt_catalog(acct, catalog):
    acct.models = [dict(item) for item in catalog.get("models") or []]
    acct.models_fetched_at = float(catalog.get("fetched_at") or 0.0)
    acct.models_source = "raccoon" if catalog.get("source") == "raccoon" else ""


def fetch_catalog(acct, timeout=20.0, force=False):
    """Fetch and normalize the upstream catalog for one account.

    A 401/403 forces exactly one token refresh and retries the same account.
    Catalog calls carry the account's known tokens as redaction secrets, so an
    upstream error body cannot echo them into a warning or log line.
    """
    global _CATALOG_LAST_ERROR
    if acct is None:
        raise RaccoonUnavailable("no Raccoon account available for catalog")
    try:
        timeout = float(timeout)
    except (TypeError, ValueError, OverflowError):
        timeout = 20.0
    # `force` is a cache-bypass hint for callers; a direct fetch is always
    # fresh, and only a 401 below forces a credential refresh.
    ensure_token(acct)

    payload = None
    for attempt in (0, 1):
        headers = _account_headers(acct)
        headers["Cache-Control"] = "no-store"
        try:
            payload = _request_json(
                "GET",
                BASE_URL + EP_CATALOG,
                headers,
                timeout=timeout,
                proxy=acct.proxy,
                secrets=_auth_secrets(acct.auth),
            )
            break
        except RaccoonAuthError:
            if attempt:
                raise
            ensure_token(acct, force=True)

    catalog = _normalize_catalog(payload)
    if not catalog["models"]:
        raise RaccoonUpstreamError("Raccoon catalog returned no chat models")

    with _CATALOG_LOCK:
        _CATALOG_CACHE.update(
            {
                "models": [dict(item) for item in catalog["models"]],
                "default_model": catalog["default_model"],
                "fetched_at": catalog["fetched_at"],
                "source": "raccoon",
            }
        )
        _CATALOG_LAST_ERROR = ""
    _adopt_catalog(acct, catalog)
    return catalog


def ensure_catalog(acct, force=False, timeout=20.0):
    """Return the account's catalog, honoring the shared cache TTL."""
    global _CATALOG_LAST_ERROR
    if acct is None:
        return _seed_model_list()
    ttl = float(
        acct.pool.config.get("models_ttl_seconds") or DEFAULTS["models_ttl_seconds"]
    )
    now = time.time()
    if (
        not force
        and getattr(acct, "models_source", "") == "raccoon"
        and acct.models
        and now - float(acct.models_fetched_at or 0.0) < ttl
    ):
        return [dict(item) for item in acct.models]
    if not force:
        cached = _cached_catalog()
        if (
            cached["source"] == "raccoon"
            and cached["models"]
            and now - float(cached["fetched_at"] or 0.0) < ttl
        ):
            _adopt_catalog(acct, cached)
            return [dict(item) for item in acct.models]

    try:
        catalog = fetch_catalog(acct, timeout=timeout) or {}
        return [dict(item) for item in catalog.get("models") or ()]
    except RaccoonError as exc:
        # A failed refresh must not blank `/v1/models`: keep the account's last
        # good catalog, then the shared cache, then the verified seed list.
        message = _redact(str(exc), _auth_secrets(acct.auth))
        with _CATALOG_LOCK:
            _CATALOG_LAST_ERROR = message
        if getattr(acct, "models_source", "") == "raccoon" and acct.models:
            return [dict(item) for item in acct.models]
        cached = _cached_catalog()
        if cached["source"] == "raccoon" and cached["models"]:
            _adopt_catalog(acct, cached)
            return [dict(item) for item in acct.models]
        return _seed_model_list()


def _merge_catalog_item(merged, item):
    mid = str(item.get("id") or "").strip()
    if not mid:
        return
    key = mid.lower()
    current = merged.get(key)
    if current is None:
        merged[key] = dict(item, tags=list(item.get("tags") or []))
        return
    if not current.get("context_window") and item.get("context_window"):
        current["context_window"] = _int_value(item.get("context_window"))
    if not current.get("max_tokens") and item.get("max_tokens"):
        current["max_tokens"] = _int_value(item.get("max_tokens"))
    if not current.get("name") or current.get("name") == current.get("id"):
        if item.get("name"):
            current["name"] = item["name"]
    current["vision"] = bool(current.get("vision") or item.get("vision"))
    current["reasoning"] = bool(current.get("reasoning") or item.get("reasoning"))
    tags = list(current.get("tags") or [])
    for tag in item.get("tags") or ():
        if tag not in tags:
            tags.append(tag)
    current["tags"] = tags


def _catalog_union(pool):
    """Union account catalogs plus the shared cache; no network access."""
    accounts = list(pool.accounts) if pool else []
    if not accounts:
        return {}, None
    merged = {}
    stamps = []
    for acct in accounts:
        if getattr(acct, "models_source", "") != "raccoon" or not acct.models:
            continue
        for item in acct.models:
            _merge_catalog_item(merged, item)
        if acct.models_fetched_at:
            stamps.append(float(acct.models_fetched_at))
    if not merged:
        cached = _cached_catalog()
        if cached["source"] == "raccoon" and cached["models"]:
            for item in cached["models"]:
                _merge_catalog_item(merged, item)
            if cached["fetched_at"]:
                stamps.append(float(cached["fetched_at"]))
    return merged, (max(stamps) if stamps else None)


def _model_entry(item, created):
    """One OpenAI-style entry with every capability spelling clients read."""
    mid = str(item.get("id") or "").strip()
    seed = _SEED_BY_ID.get(mid.lower()) or {}
    tags = _tag_list(item.get("tags"))
    vision = bool(item.get("vision")) or "vision" in tags
    reasoning = bool(item.get("reasoning")) or "reasoning" in tags
    context_window = _int_value(item.get("context_window")) or _int_value(
        seed.get("context_window")
    )
    max_tokens = _int_value(item.get("max_tokens")) or _int_value(
        seed.get("max_tokens")
    )
    inputs = ["text"] + (["image"] if vision else [])
    entry = {
        "id": mid,
        "object": "model",
        "created": int(created),
        "owned_by": "sensetime",
        "provider": "raccoon",
        "name": str(item.get("name") or mid),
    }
    if context_window:
        entry["context_window"] = context_window
        entry["context_length"] = context_window
        entry["max_input_tokens"] = context_window
    if max_tokens:
        entry["max_tokens"] = max_tokens
        entry["max_output_tokens"] = max_tokens
        entry["max_completion_tokens"] = max_tokens
        entry["max_output"] = max_tokens
    # Same convention set as CodeArts/WorkBuddy: no standard exists, so emit
    # every spelling a common client might read.
    entry["capabilities"] = {
        "vision": vision,
        "tool_calls": True,
        "reasoning": reasoning,
    }
    entry["supports_vision"] = vision
    entry["supports_images"] = vision
    entry["supports_tool_calls"] = True
    entry["supports_reasoning"] = reasoning
    entry["vision"] = vision
    entry["multimodal"] = vision
    entry["abilities"] = {
        "vision": vision,
        "functionCall": True,
        "function_call": True,
        "reasoning": reasoning,
    }
    entry["input_modalities"] = inputs
    entry["output_modalities"] = ["text"]
    entry["modalities"] = {"input": inputs, "output": ["text"]}
    entry["architecture"] = {
        "input_modalities": inputs,
        "output_modalities": ["text"],
        "modality": "+".join(inputs) + "->text",
    }
    if tags:
        entry["tags"] = list(tags)
    return entry


def _config_or_load(config):
    if isinstance(config, dict) and config.get("accounts_dir"):
        return config
    folder = config.get("accounts_dir") if isinstance(config, dict) else "."
    return load_config(folder or ".")


def model_entries(config):
    """Bare-id `/v1/models` entries: union of account catalogs, else seeds."""
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    for acct in list(pool.accounts):
        # Discovery must not poke a disabled or already-deleted account: a
        # manual disable is a panel decision, and a terminal auth failure would
        # only produce noise.
        if acct.disabled or acct.deleted:
            continue
        try:
            ensure_catalog(acct, timeout=8.0)
        except RaccoonError:
            pass
    merged, _ = _catalog_union(pool)
    if not merged:
        for item in SEED_MODELS:
            _merge_catalog_item(merged, item)
    created = int(time.time())
    return [_model_entry(item, created) for item in merged.values()]


def models_meta(config):
    """Catalog provenance for `/v1/models` and `/health`; never fetches."""
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    merged, fetched_at = _catalog_union(pool)
    with _CATALOG_LOCK:
        last_error = _CATALOG_LAST_ERROR
    accounts = len(pool.accounts)
    if merged and accounts:
        warning = ""
        if last_error:
            warning = "using cached catalog after refresh failed: %s" % last_error
        return {
            "source": "raccoon",
            "fetched_at": fetched_at,
            "accounts": accounts,
            "warning": warning,
        }
    if not accounts:
        warning = "no Raccoon accounts configured; using built-in seed models"
    elif last_error:
        warning = (
            "upstream catalog unavailable: %s; using built-in seed models" % last_error
        )
    else:
        warning = "upstream catalog unavailable; using built-in seed models"
    return {
        "source": "seed",
        "fetched_at": None,
        "accounts": accounts,
        "warning": warning,
    }


def invalidate_models():
    """Drop every account catalog and the shared cache."""
    global _CATALOG_LAST_ERROR
    with _CATALOG_LOCK:
        _CATALOG_CACHE.update(
            {
                "models": [],
                "default_model": "",
                "fetched_at": 0.0,
                "source": "seed",
            }
        )
        _CATALOG_LAST_ERROR = ""
    with _POOLS_LOCK:
        pools = list(_POOLS.values())
    for pool in pools:
        for acct in list(pool.accounts):
            acct.models = []
            acct.models_fetched_at = 0.0
            acct.models_source = ""
    invalidate_health()


def refresh_models(config):
    """Invalidate the cache, refetch from the first healthy account, report."""
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    invalidate_models()
    accounts = [
        account
        for account in list(pool.accounts)
        if not account.disabled and not account.deleted
    ]
    target = None
    for acct in accounts:
        if acct.healthy():
            target = acct
            break
    if target is None and accounts:
        target = accounts[0]
    if target is not None:
        try:
            ensure_catalog(target, force=True, timeout=20.0)
        except RaccoonError:
            pass
    entries = model_entries(config)
    catalog = models_meta(config)
    ok = catalog.get("source") == "raccoon"
    return {
        "ok": ok,
        "models": len(entries),
        "catalog": catalog,
        "message": (
            "模型目录已刷新" if ok else (catalog.get("warning") or "上游模型目录不可用")
        ),
    }


# ---------------------------------------------------------------------------
# 浏览器回环 OAuth
# ---------------------------------------------------------------------------


_OAUTH_SESSIONS = {}
_OAUTH_LOCK = threading.Lock()


class RaccoonOAuthSession(object):
    """One browser authorization attempt.

    `state` is the browser-visible single-use nonce; `id` is the panel handle
    used for polling. Neither `state` nor the exchange code is ever returned by
    `public()`, so an admin response cannot leak the credential.
    """

    def __init__(self, session_id, state, port):
        self.id = session_id
        self.state = state
        self.port = int(port)
        self.created_at = time.time()
        self.status = "pending"
        self.message = "等待浏览器完成授权…"
        self.auth_url = ""
        self.account = None

    def public(self):
        status = "pending" if self.status == "exchanging" else self.status
        out = {
            "ok": status != "error",
            "status": status,
            "message": self.message,
            "session_id": self.id,
            "auth_url": self.auth_url,
            "expires_in": max(
                0, int(OAUTH_TTL_SECONDS - (time.time() - self.created_at))
            ),
        }
        if self.account:
            out["account"] = dict(self.account)
        return out


def _gc_oauth(now=None):
    now = time.time() if now is None else now
    with _OAUTH_LOCK:
        for key, session in list(_OAUTH_SESSIONS.items()):
            if now - session.created_at > OAUTH_TTL_SECONDS:
                _OAUTH_SESSIONS.pop(key, None)


def _find_oauth_session(session_id=None):
    _gc_oauth()
    with _OAUTH_LOCK:
        return _OAUTH_SESSIONS.get(str(session_id or ""))


def _state_matches(left, right):
    """Constant-time compare; non-ASCII garbage simply does not match."""
    try:
        return secrets.compare_digest(str(left or ""), str(right or ""))
    except (TypeError, ValueError):
        return False


def _find_pending_oauth_state(state):
    _gc_oauth()
    text = str(state or "").strip()
    if not text:
        return None
    with _OAUTH_LOCK:
        for session in _OAUTH_SESSIONS.values():
            if session.status == "pending" and _state_matches(
                session.state, text
            ):
                return session
    return None


def _claim_oauth_session(session_id=None, state=None):
    """Atomically move a pending session to `exchanging`.

    The state is single-use: a second callback (or a callback against a done
    or error session) gets `None` and can no longer reach the exchange.
    """
    _gc_oauth()
    with _OAUTH_LOCK:
        if session_id:
            session = _OAUTH_SESSIONS.get(str(session_id or ""))
        else:
            session = None
            for candidate in _OAUTH_SESSIONS.values():
                if candidate.status == "pending" and _state_matches(
                    candidate.state, str(state or "")
                ):
                    session = candidate
                    break
        if session is None or session.status != "pending":
            return None
        if state and not _state_matches(session.state, state):
            return None
        session.status = "exchanging"
        return session


def _loopback_host(host):
    return str(host or "").lower() in ("127.0.0.1", "localhost", "::1")


def _parse_loopback_callback(url):
    """Validate a manually pasted callback URL; return (parsed, port)."""
    try:
        parsed = urllib.parse.urlsplit(str(url or "").strip())
    except ValueError:
        raise RaccoonBadRequest("invalid callback URL")
    if parsed.scheme != "http":
        raise RaccoonBadRequest("callback URL must use http")
    if parsed.username or parsed.password:
        raise RaccoonBadRequest("callback URL must not contain credentials")
    if not _loopback_host(parsed.hostname):
        raise RaccoonBadRequest("callback URL must point at loopback")
    if parsed.path != OAUTH_REDIRECT_PATH:
        raise RaccoonBadRequest("callback URL path must be %s" % OAUTH_REDIRECT_PATH)
    try:
        port = parsed.port
    except ValueError:
        raise RaccoonBadRequest("callback URL has an invalid port")
    if port is None:
        raise RaccoonBadRequest("callback URL must include the gateway port")
    return parsed, port


def _query_value(query, key):
    raw = query.get(key) if hasattr(query, "get") else None
    if isinstance(raw, list):
        return str(raw[0] if raw else "")
    return str(raw or "")


def _oauth_html(message):
    return "<h3>%s</h3>" % html_module.escape(str(message), quote=True)


def _redact_oauth_error(exc, code="", state=""):
    secrets = [value for value in (code, state) if value]
    return _redact(str(exc), secrets)[:500]


def exchange_code(config, code, timeout=None):
    """Redeem a browser authorization code for a Raccoon account.

    The code is passed to `_request_json` as a literal redaction secret: a
    hostile upstream error body that echoes `{"code": "<auth code>"}` (which
    `_error_text` would otherwise render as `error_code=...`) cannot leak it
    into a panel response, HTML callback page, poll message, or account state.
    """
    config = _config_or_load(config)
    accounts_dir = config.get("accounts_dir")
    if not accounts_dir:
        raise RaccoonBadRequest("raccoon config is missing accounts_dir")
    code = _clean_token(code)
    if not code:
        raise RaccoonBadRequest("authorization code is required")
    if timeout is None:
        timeout = float(config.get("timeout_seconds") or DEFAULTS["timeout_seconds"])
    body = json.dumps({"authorization_code": code}).encode("utf-8")
    headers = {
        "Content-Type": "application/json",
        "Accept": "application/json",
    }
    try:
        payload = _request_json(
            "POST",
            BASE_URL + EP_LOGIN,
            headers,
            body,
            timeout=timeout,
            secrets=[code],
        )
    except RaccoonError as exc:
        message = _redact_oauth_error(exc, code)
        raise type(exc)(
            message,
            status=getattr(exc, "status", None),
            code=getattr(exc, "code", None),
        )
    auth = auth_from_payload(payload)
    auth = _save_and_enable(accounts_dir, auth)
    acct = pool_for(accounts_dir).add(auth)
    invalidate_health()
    return acct


def _exchange_session(config, session, code):
    try:
        acct = exchange_code(config, code)
    except RaccoonError as exc:
        message = _redact_oauth_error(exc, code, session.state)
        session.status = "error"
        session.message = message
        if message != str(exc):
            exc.args = (message,)
        raise
    except Exception:
        session.status = "error"
        session.message = "OAuth exchange failed"
        raise RaccoonUpstreamError(session.message)
    session.status = "done"
    session.message = "登录成功：%s" % acct.nickname
    session.account = {"uid": acct.uid, "nickname": acct.nickname}
    return acct


def oauth_start(config, port=8788):
    """Start a loopback OAuth session and return the exact browser URL."""
    _config_or_load(config)
    try:
        port = int(port)
    except (TypeError, ValueError, OverflowError):
        raise RaccoonBadRequest("gateway port must be an integer")
    if port < 1024 or port > 65535:
        raise RaccoonBadRequest("gateway port must be between 1024 and 65535")
    _gc_oauth()
    with _OAUTH_LOCK:
        while len(_OAUTH_SESSIONS) >= OAUTH_MAX_SESSIONS:
            pending = [
                item
                for item in _OAUTH_SESSIONS.values()
                if item.status == "pending"
            ]
            if not pending:
                raise RaccoonBadRequest(
                    "OAuth 会话过多：请稍后重试或等待现有授权结束"
                )
            oldest = min(pending, key=lambda item: item.created_at)
            _OAUTH_SESSIONS.pop(oldest.id, None)
        state = secrets.token_hex(32)
        while any(item.state == state for item in _OAUTH_SESSIONS.values()):
            state = secrets.token_hex(32)
        session_id = secrets.token_hex(8)
        while session_id in _OAUTH_SESSIONS:
            session_id = secrets.token_hex(8)
        redirect = "http://127.0.0.1:%d%s" % (port, OAUTH_REDIRECT_PATH)
        auth_url = (
            "%s?login_source=desktop&appname=办公小浣熊客户端"
            "&desktop_callback=loopback&redirect=%s&state=%s"
            % (AUTHORIZE_URL, redirect, state)
        )
        session = RaccoonOAuthSession(session_id, state, port)
        session.auth_url = auth_url
        _OAUTH_SESSIONS[session_id] = session
    return {
        "ok": True,
        "session_id": session_id,
        "auth_url": auth_url,
        "expires_in": int(OAUTH_TTL_SECONDS),
        "message": "请在浏览器打开授权链接，完成后等待自动检测",
    }


def oauth_poll(config, session_id):
    """Panel poll; done/error sessions stay readable until the TTL expires."""
    _config_or_load(config)
    session = _find_oauth_session(session_id)
    if session is None:
        raise RaccoonNotFound("OAuth session not found or expired; start again")
    return session.public()


def oauth_callback_get(config, query):
    """Handle `GET /raccoon/oauth/callback`; return `(status, html)`.

    Only a pending, exactly matching state is accepted. The claim happens
    before the exchange so two concurrent callbacks cannot both redeem the
    same state, and the state stays consumed after success or failure.
    """
    config = _config_or_load(config)
    state = _query_value(query, "state").strip()
    code = _clean_token(_query_value(query, "code"))
    if not state:
        return 400, _oauth_html("回调缺少 state，请回到看板重新发起授权。")
    pending = _find_pending_oauth_state(state)
    if pending is None:
        return 400, _oauth_html("登录会话不存在或已过期，请回到看板重新发起。")
    if not code:
        return 400, _oauth_html("登录失败：回调缺少 code。")
    session = _claim_oauth_session(state=state)
    if session is None:
        return 400, _oauth_html("登录会话不存在或已过期，请回到看板重新发起。")
    try:
        _exchange_session(config, session, code)
    except RaccoonError as exc:
        return 400, _oauth_html("登录失败：%s" % exc)
    except Exception:
        session.status = "error"
        session.message = "OAuth exchange failed"
        return 400, _oauth_html("登录失败：OAuth exchange failed")
    return 200, _oauth_html("Raccoon 登录成功，可以关闭此页面。")


def oauth_callback(config, session_id=None, callback_url="", code="", state=""):
    """Panel-side manual callback: a full loopback URL or a raw code.

    A URL must be `http://127.0.0.1:<session-port>/raccoon/oauth/callback`,
    carry the session's exact `state`, and include a code. No redirect target
    is ever returned, so the callback cannot become an open redirect.
    """
    config = _config_or_load(config)
    session_id = str(session_id or "").strip()
    callback_url = str(callback_url or "").strip()
    code = _clean_token(code)
    state = str(state or "").strip()
    parsed = None
    url_state = ""

    if callback_url:
        parsed, callback_port = _parse_loopback_callback(callback_url)
        query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
        url_state = _query_value(query, "state").strip()
        url_code = _clean_token(_query_value(query, "code"))
        if not url_state:
            raise RaccoonBadRequest("callback URL is missing state")
        if state and not _state_matches(state, url_state):
            raise RaccoonBadRequest("callback state does not match the session")
        state = url_state
        if not code:
            code = url_code

    if session_id:
        session = _find_oauth_session(session_id)
        if session is None:
            raise RaccoonNotFound("OAuth session not found or expired; start again")
    elif state:
        session = _find_pending_oauth_state(state)
        if session is None:
            raise RaccoonNotFound("OAuth session not found or expired; start again")
        session_id = session.id
    else:
        raise RaccoonBadRequest("session_id or state is required")

    if parsed is not None:
        if callback_port != session.port:
            raise RaccoonBadRequest(
                "callback port does not match the authorization session"
            )
        if not _state_matches(url_state, session.state):
            raise RaccoonBadRequest("callback state does not match the session")
    if state and not _state_matches(state, session.state):
        raise RaccoonBadRequest("callback state does not match the session")
    if not code:
        raise RaccoonBadRequest("callback is missing code")

    claimed = _claim_oauth_session(session_id=session_id, state=session.state)
    if claimed is None:
        raise RaccoonNotFound(
            "OAuth session is no longer pending; start a new authorization"
        )
    _exchange_session(config, claimed, code)
    return claimed.public()


# ---------------------------------------------------------------------------
# 管理操作
# ---------------------------------------------------------------------------


def invalidate_overview():
    invalidate_health()


def enable(config, uid):
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    if not pool.enable(uid):
        raise RaccoonNotFound("account not found")
    invalidate_health()
    return {"ok": True, "uid": uid, "message": "已启用 %s" % uid}


def disable(config, uid, reason=""):
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    if not pool.disable(uid, reason or "manual disable"):
        raise RaccoonNotFound("account not found")
    invalidate_health()
    return {"ok": True, "uid": uid, "message": "已停用 %s" % uid}


def remove(config, uid):
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    if pool.get(uid) is None:
        raise RaccoonNotFound("account not found")
    if not pool.remove(uid):
        raise RaccoonDeleteError(
            "账号文件删除失败，账号仍保留；请检查 accounts/raccoon 的文件权限"
        )
    invalidate_health()
    return {"ok": True, "deleted": True, "message": "已删除 %s" % uid}


def clear_cooldown(config, uid):
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    if not pool.clear_cooldown(uid):
        raise RaccoonNotFound("account not found")
    invalidate_health()
    return {"ok": True, "uid": uid, "message": "已清冷却 %s" % uid}


def set_note(config, uid, note):
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    if not pool.set_note(uid, note):
        raise RaccoonNotFound("account not found")
    invalidate_health()
    return {"ok": True, "uid": uid, "note": pool.get(uid).note}


def set_tasks_enabled(config, uid, enabled):
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    if not pool.set_tasks_enabled(uid, enabled):
        raise RaccoonNotFound("account not found")
    invalidate_health()
    return {
        "ok": True,
        "uid": uid,
        "tasks_enabled": bool(pool.get(uid).tasks_enabled),
    }


def set_proxy_slot(config, uid, slot_id):
    """Bind one account to a proxy slot (empty string = direct)."""
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    acct = pool.get(uid)
    if acct is None:
        raise RaccoonNotFound("account not found")
    pool.set_proxy_slot(uid, slot_id)
    acct = pool.get(uid)
    invalidate_health()
    return {
        "ok": True,
        "uid": acct.uid,
        "proxy_slot": acct.proxy_slot,
        "proxy": _mask_proxy_url(acct.proxy),
        "message": "出口已更新",
    }


# ---------------------------------------------------------------------------
# 导入 / 概览 / 健康
# ---------------------------------------------------------------------------


def _save_and_enable(accounts_dir, auth):
    """Persist an auth file and enable the realm, rolling the file back if enable fails."""
    target = os.path.join(accounts_path(accounts_dir), _auth_filename(auth["uid"]))
    backup = None
    if os.path.exists(target):
        try:
            with open(target, "rb") as fh:
                backup = fh.read()
        except OSError:
            backup = None
    _clear_auth_tombstones(auth.get("uid") or "")
    saved = save_auth(accounts_dir, auth)
    try:
        save_config(accounts_dir, {"enabled": True})
    except Exception:
        path = saved.get("_path")
        try:
            if backup is not None:
                _atomic_write(path, json.loads(backup.decode("utf-8")))
            elif path:
                os.remove(path)
        except Exception:
            pass
        raise
    return saved


def import_auth(config, payload):
    """Import an `auth.json`, save it, auto-enable the realm, and add the pool."""
    if not isinstance(config, dict) or not config.get("accounts_dir"):
        raise RaccoonBadRequest("raccoon config is missing accounts_dir")
    accounts_dir = config["accounts_dir"]
    if not isinstance(payload, dict):
        raise RaccoonBadRequest("import payload must be an object")
    if isinstance(payload.get("auth"), dict):
        payload = payload["auth"]
    auth = auth_from_payload(payload)
    auth = _save_and_enable(accounts_dir, auth)
    acct = pool_for(accounts_dir).add(auth)
    invalidate_health()
    return {
        "ok": True,
        "uid": acct.uid,
        "account": acct.view(),
        "enabled": True,
    }


_health_cache = {"at": 0.0, "value": {}}
_health_lock = threading.Lock()


def overview(config, force=False):
    """Admin-facing snapshot: accounts, stats, models, base URL; no secrets."""
    if not isinstance(config, dict) or not config.get("accounts_dir"):
        config = load_config(
            config.get("accounts_dir") if isinstance(config, dict) else "."
        )
    pool = pool_for(config["accounts_dir"])
    models = [
        {"id": item["id"], "name": item.get("name") or item["id"]}
        for item in model_entries(config)
    ]
    return {
        "provider": "raccoon",
        "service": "raccoon",
        "online": bool(config.get("enabled")),
        "base_url": BASE_URL,
        "stats": pool.stats(),
        "accounts": pool.list(),
        "health": health(config, force=bool(force)),
        "models": models,
    }


def health(config, force=False):
    """Small cached health block consumed by the gateway `/health`."""
    if not isinstance(config, dict) or not config.get("accounts_dir"):
        config = load_config(
            config.get("accounts_dir") if isinstance(config, dict) else "."
        )
    pool = pool_for(config["accounts_dir"])
    stats = pool.stats()
    catalog = models_meta(config)
    catalog_models, _ = _catalog_union(pool)
    model_count = (
        len(catalog_models) if catalog["source"] == "raccoon" else len(SEED_MODELS)
    )
    now = time.time()
    key = (
        os.path.abspath(str(config["accounts_dir"])),
        BASE_URL,
        bool(config.get("enabled")),
        config.get("timeout_seconds"),
        config.get("stream_timeout_seconds"),
        config.get("models_ttl_seconds"),
        config.get("max_concurrent"),
        stats["total"],
        stats["healthy"],
        stats["disabled"],
        stats["cooling"],
        catalog["source"],
        catalog["fetched_at"],
        model_count,
    )
    with _health_lock:
        if (
            not force
            and _health_cache.get("key") == key
            and now - _health_cache["at"] < 5.0
        ):
            return dict(_health_cache["value"])
    value = {
        "ok": bool(config.get("enabled")),
        "configured": bool(config.get("enabled")),
        "accounts": stats["total"],
        "healthy": stats["healthy"],
        "base_url": BASE_URL,
        "catalog_source": catalog["source"],
        "models": model_count,
    }
    with _health_lock:
        _health_cache.update({"at": now, "key": key, "value": value})
    return dict(value)


def invalidate_health():
    with _health_lock:
        _health_cache.update({"at": 0.0, "key": None, "value": {}})


# ---------------------------------------------------------------------------
# 聊天传输 / SSE
# ---------------------------------------------------------------------------

#: OpenAI chat fields the Raccoon gateway accepts. Everything else (client
#: metadata, legacy `functions`, `stream_options`, ...) is dropped before it
#: reaches upstream; `stream` is always forced on.
CHAT_ALLOWED_FIELDS = frozenset(
    (
        "model",
        "messages",
        "tools",
        "tool_choice",
        "parallel_tool_calls",
        "temperature",
        "top_p",
        "stream",
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
    )
)


def build_chat_body(payload, session_key=None):
    """Reduce an OpenAI chat payload to the Raccoon allowlist.

    `stream` is forced to True because the gateway always consumes SSE and
    aggregates it for non-streaming clients. `session_key` is accepted for
    parity with the other providers; Raccoon has no client-side session field.
    """
    if not isinstance(payload, dict):
        raise RaccoonBadRequest("request body must be a JSON object")
    body = {
        str(key): value
        for key, value in payload.items()
        if str(key) in CHAT_ALLOWED_FIELDS
    }
    model = str(body.get("model") or "").strip()
    if not model:
        raise RaccoonBadRequest("model is required")
    messages = body.get("messages")
    if not isinstance(messages, list) or not messages:
        raise RaccoonBadRequest("messages is empty")
    body["model"] = model
    body["messages"] = messages
    body["stream"] = True
    return body


def _chat_headers(acct):
    """Desktop-client SSE headers; `X-Org-Code` only for a real org identity."""
    auth = (acct.auth if acct is not None else None) or {}
    headers = {
        "Authorization": "Bearer " + _clean_token(auth.get("access_token")),
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
        "X-Client-Platform": CLIENT_PLATFORM,
        "X-Client-Version": CLIENT_VERSION,
        "X-Raccoon-Language": CLIENT_LANGUAGE,
        "Cache-Control": "no-store",
    }
    identity = str(auth.get("office_identity") or "").strip()
    if identity and identity.lower() not in ("personal", "none"):
        headers["X-Org-Code"] = identity
    return headers


def _model_output_limit(acct, model):
    """Catalog output limit for `model`; 0 when the model is unknown.

    Read-only: account and shared cache lists are never mutated here.
    """
    key = str(model or "").strip().lower()
    if not key:
        return 0
    sources = []
    if acct is not None:
        sources.append(list(acct.models or ()))
    sources.append(list(_cached_catalog().get("models") or ()))
    for items in sources:
        for item in items:
            if not isinstance(item, dict):
                continue
            if str(item.get("id") or "").strip().lower() != key:
                continue
            limit = _int_value(item.get("max_tokens"))
            if limit <= 0:
                limit = _int_value(item.get("max_completion_tokens"))
            if limit > 0:
                return limit
    seed = _SEED_BY_ID.get(key) or {}
    return _int_value(seed.get("max_tokens"))


def _clamp_output_tokens(acct, model, body):
    """Validate and clamp both OpenAI output-token spellings."""
    limit = _model_output_limit(acct, model)
    for key in ("max_tokens", "max_completion_tokens"):
        if body.get(key) is None:
            continue
        raw = body[key]
        if isinstance(raw, bool):
            raise RaccoonBadRequest("%s must be a positive integer" % key, status=400)
        try:
            requested = int(raw)
        except (TypeError, ValueError, OverflowError):
            raise RaccoonBadRequest("%s must be a positive integer" % key, status=400)
        if requested <= 0:
            raise RaccoonBadRequest("%s must be a positive integer" % key, status=400)
        # Normalize numeric strings (and integral floats) before they reach the
        # upstream: the allowlist forwards the body verbatim otherwise.
        body[key] = requested
        if limit > 0 and requested > limit:
            body[key] = limit


class RaccoonStream(object):
    """Pass Raccoon SSE data lines through without re-encoding them.

    The gateway's `_chat_stream_response` / `aggregate_stream` already own
    chunk normalization, usage aggregation and terminal error frames. This
    wrapper only caches the first data line during `preflight()` (so an HTTP
    200 with an embedded error fails before headers are sent) and releases the
    account slot exactly once.
    """

    def __init__(self, raw, model, acct, pool):
        self.raw = raw
        self.model = model
        self.acct = acct
        self.pool = pool
        self._wb_billed_model = model
        self._pending = b""
        self._closed = False
        self._saw_done = False
        self._preflighted = False

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        self.close()
        return False

    def _close_raw(self):
        try:
            self.raw.close()
        except Exception:
            pass

    def abort(self):
        """Close the response without releasing the slot; the caller owns it."""
        if getattr(self, "_closed", False):
            return
        self._closed = True
        self._close_raw()

    def close(self):
        """Close the response and release the account slot, exactly once."""
        if getattr(self, "_closed", False):
            return
        self._closed = True
        self._close_raw()
        try:
            if self.pool is not None:
                self.pool.release(self.acct)
        except Exception:
            pass

    @staticmethod
    def _payload(line):
        stripped = (line or b"").strip()
        if not stripped.startswith(b"data:"):
            return b""
        return stripped[5:].strip()

    def _raise_embedded(self, data):
        """Map an error object sent inside a 200 SSE stream to Raccoon*."""
        text = data.decode("utf-8", "replace").strip()
        if not text or text == "[DONE]":
            return
        try:
            payload = json.loads(text)
        except Exception:
            return
        if not isinstance(payload, dict):
            return

        # Normal chat/usage chunks are not errors even though they share the
        # stream. A frame only counts as error-shaped when it carries an
        # `error` object/message or a non-zero `error_code`/`code`.
        error = payload.get("error")
        raw_code = payload.get("error_code")
        if raw_code in (None, "", 0, "0"):
            raw_code = payload.get("code")
        if (
            (payload.get("choices") or payload.get("usage") is not None)
            and not error
            and raw_code in (None, "", 0, "0")
        ):
            return

        code = payload.get("error_code")
        if code in (None, "", 0, "0"):
            code = payload.get("code")
        error = payload.get("error")
        message = ""
        if isinstance(error, dict):
            message = str(
                error.get("message")
                or error.get("error_msg")
                or error.get("detail")
                or ""
            ).strip()
            if code in (None, "", 0, "0"):
                code = error.get("code") or error.get("type")
        elif isinstance(error, str) and error.strip():
            message = error.strip()
        if not message:
            for key in ("error_msg", "message", "detail", "error_description"):
                value = payload.get(key)
                if isinstance(value, str) and value.strip():
                    message = value.strip()
                    break
        if not message:
            if code in (None, "", 0, "0"):
                return
            message = "error_code=%s" % code

        secrets = _auth_secrets(self.acct.auth) if self.acct is not None else ()
        message = _redact(message, secrets)[:500]

        status = None
        for key in ("status", "status_code", "http_status"):
            try:
                value = int(payload.get(key) or 0)
            except (TypeError, ValueError, OverflowError):
                value = 0
            if value:
                status = value
                break
        if status is None:
            try:
                numeric = int(code)
            except (TypeError, ValueError, OverflowError):
                numeric = 0
            if numeric in (400, 401, 403, 429) or 500 <= numeric < 600:
                status = numeric

        normalized = str(code or "").strip().lower()
        if status in (401, 403):
            raise RaccoonAuthError(message, status=status)
        if status == 400 or normalized in (
            "400",
            "bad_request",
            "invalid_request",
            "invalid_request_error",
            "invalid_param",
            "invalid_parameter",
        ):
            raise RaccoonBadRequest(message, status=400)
        if status == 429 or normalized == "429":
            raise RaccoonRateLimited(message, status=429)
        raise RaccoonUpstreamError(message, status=status)

    def preflight(self):
        """Read the first data event; raise an embedded error before headers."""
        if self._preflighted:
            return
        while True:
            line = self.raw.readline()
            if not line:
                self._preflighted = True
                return
            data = self._payload(line)
            if not data:
                continue
            self._raise_embedded(data)
            self._pending = line if line.endswith(b"\n") else line + b"\n"
            self._preflighted = True
            return

    def __iter__(self):
        try:
            if self._pending:
                line, self._pending = self._pending, b""
                data = self._payload(line)
                if data == b"[DONE]":
                    self._saw_done = True
                if data:
                    self._raise_embedded(data)
                    yield line
            while True:
                line = self.raw.readline()
                if not line:
                    break
                data = self._payload(line)
                if not data:
                    continue
                if data == b"[DONE]":
                    self._saw_done = True
                else:
                    # A 200 stream can carry an error object after some normal
                    # chunks. Raise it here as well; otherwise the gateway would
                    # treat the truncated answer as a clean completion.
                    self._raise_embedded(data)
                yield line
            if not self._saw_done:
                raise RaccoonUpstreamError(
                    "Raccoon stream ended before the [DONE] event"
                )
        finally:
            self.close()


def _open_stream_response(req, timeout, proxy):
    """Open a streaming response through the shared pool.

    A pooled redirect (`PoolBypass`) replays through the account's proxy-aware
    opener, matching `_request_json` so an account never changes exit IP.
    """
    try:
        return wb_upstream_pool.urlopen(req, timeout=timeout, proxy=proxy)
    except wb_upstream_pool.PoolBypass:
        import wb_accounts

        opener = wb_accounts.opener_for_proxy(proxy)
        if opener is None:
            return urllib.request.urlopen(req, timeout=timeout)
        return opener.open(req, timeout=timeout)


def _client_chat(acct, body, session_key):
    """POST one chat request and return `(RaccoonStream, model)`.

    A 401/403 forces exactly one token refresh and retries the same account;
    the second auth failure propagates as `RaccoonAuthError`.
    """
    body = build_chat_body(body, session_key)
    model = body["model"]
    upstream_body = dict(body)
    upstream_body["stream"] = True
    _clamp_output_tokens(acct, model, upstream_body)
    raw = json.dumps(upstream_body, ensure_ascii=False).encode("utf-8")
    timeout = float(
        acct.pool.config.get("stream_timeout_seconds")
        or DEFAULTS["stream_timeout_seconds"]
    )

    for attempt in (0, 1):
        expected_generation = acct.auth_generation
        headers = _chat_headers(acct)
        secrets = _auth_secrets(acct.auth)
        req = urllib.request.Request(
            BASE_URL + EP_CHAT, data=raw, headers=headers, method="POST"
        )
        # Preserve the exact header spelling on the wire (Request.capitalize()
        # would turn X-Client-Platform into X-client-platform).
        req.wb_exact_headers = list(headers.items())
        try:
            resp = _open_stream_response(req, timeout, acct.proxy)
        except urllib.error.HTTPError as exc:
            try:
                err = exc.read(ERROR_BODY_MAX)
            except Exception:
                err = b""
            try:
                exc.close()
            except Exception:
                pass
            if exc.code in (401, 403) and attempt == 0:
                ensure_token(acct, force=True, expected_generation=expected_generation)
                continue
            _raise_for_status(exc.code, err, secrets)
            raise
        except (
            urllib.error.URLError,
            socket.timeout,
            TimeoutError,
            ConnectionError,
            OSError,
        ) as exc:
            reason = getattr(exc, "reason", None) or exc
            raise RaccoonUnavailable(
                _redact("cannot reach Raccoon: %s" % (reason,), secrets)
            )

        stream = RaccoonStream(resp, model, acct, acct.pool)
        try:
            stream.preflight()
        except RaccoonAuthError:
            stream.abort()
            if attempt == 0:
                ensure_token(acct, force=True, expected_generation=expected_generation)
                continue
            raise
        except RaccoonError:
            stream.abort()
            raise
        except Exception as exc:
            stream.abort()
            raise RaccoonUpstreamError(
                _redact(
                    "Raccoon chat stream failed: %s" % (exc,),
                    _auth_secrets(acct.auth),
                )
            )
        return stream, model

    raise RaccoonAuthError("Raccoon 拒绝访问：token 刷新后仍未授权")


def open_chat(config, body, session_key=None, allowed_accounts=None):
    """Pick an account, refresh/fail over as needed, and open the SSE stream."""
    config = _config_or_load(config)
    if not config.get("enabled"):
        raise RaccoonBadRequest(
            "Raccoon 出口未启用：请到设置页打开「启用 Raccoon 出口」。"
        )
    body = build_chat_body(body, session_key)
    model = body["model"]
    pool = pool_for(config["accounts_dir"])
    exclude = set()
    last_error = None
    attempts = max(2, len(pool.accounts) + 1)

    for _ in range(attempts):
        acct = pool.pick(
            model,
            session_key=session_key,
            exclude=exclude,
            allowed_accounts=allowed_accounts,
        )
        if acct is None:
            busy = any(
                a.healthy() and a.active_concurrent >= a.max_concurrent
                for a in list(pool.accounts)
            )
            if busy and len(exclude) < len(pool.accounts):
                time.sleep(0.05)
                continue
            break
        if not pool.acquire(acct):
            exclude.add(acct.uid)
            continue
        try:
            ensure_token(acct)
            stream, used_model = _client_chat(acct, body, session_key)
            pool.note_success(acct)
            pool.bind_session(session_key, acct.uid)
            return stream, acct, used_model
        except RaccoonBadRequest:
            pool.release(acct)
            raise
        except RaccoonAuthError as exc:
            pool.note_error(acct, str(exc), terminal=True)
            pool.release(acct)
            exclude.add(acct.uid)
            last_error = exc
        except RaccoonRateLimited as exc:
            pool.note_error(acct, str(exc), soft=True)
            pool.release(acct)
            exclude.add(acct.uid)
            last_error = exc
        except RaccoonUpstreamError as exc:
            soft = exc.status is None or exc.status == 429 or exc.status >= 500
            pool.note_error(acct, str(exc), soft=soft)
            pool.release(acct)
            exclude.add(acct.uid)
            last_error = exc
        except RaccoonUnavailable as exc:
            pool.note_error(acct, str(exc), soft=True)
            pool.release(acct)
            exclude.add(acct.uid)
            last_error = exc
        except RaccoonError as exc:
            pool.note_error(acct, str(exc))
            pool.release(acct)
            exclude.add(acct.uid)
            last_error = exc
        except Exception:
            pool.release(acct)
            raise

    if last_error is not None:
        raise last_error

    reasons = []
    now = time.time()
    for acct in list(pool.accounts):
        secrets = _auth_secrets(acct.auth)
        if acct.disabled:
            reasons.append(
                "%s 已停用：%s"
                % (
                    acct.nickname,
                    _redact(acct.disabled_reason or "手动停用", secrets),
                )
            )
        elif acct.cool_until > now:
            reasons.append(
                "%s 冷却中：%s"
                % (acct.nickname, _redact(acct.last_error or "稍后重试", secrets))
            )
        elif acct.active_concurrent >= acct.max_concurrent:
            reasons.append("%s 并发已满" % acct.nickname)
    if reasons:
        raise RaccoonUnavailable("没有可用的 Raccoon 账号：" + "；".join(reasons[:2]))
    raise RaccoonUnavailable(
        "没有可用的 Raccoon 账号：请在看板「账号」页导入并启用至少一个账号。"
    )


def test_account(config, uid):
    """Force a catalog refresh and report the account's token/model counts."""
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    acct = pool.get(uid)
    if acct is None:
        raise RaccoonNotFound("account not found")
    try:
        fetch_catalog(acct, timeout=15.0, force=True)
        remaining = max(0, int(acct.token_remaining()))
        return {
            "ok": True,
            "uid": acct.uid,
            "models": len(acct.models),
            "token_remaining": _fmt_duration(remaining),
            "token_seconds": remaining,
            "message": "连接正常",
        }
    except RaccoonError as exc:
        return {
            "ok": False,
            "uid": acct.uid,
            "message": _redact(str(exc), _auth_secrets(acct.auth))[:300],
        }


def keepalive(config):
    """Refresh expiring tokens and the shared catalog. There is no check-in here."""
    config = _config_or_load(config)
    pool = pool_for(config["accounts_dir"])
    refreshed = 0
    errors = []
    accounts = list(pool.accounts)
    for acct in accounts:
        if acct.disabled:
            continue
        if not (acct.expiring_soon() or acct.expired()):
            continue
        generation = acct.auth_generation
        try:
            ensure_token(acct)
        except RaccoonError as exc:
            errors.append(
                "%s: %s"
                % (
                    acct.nickname,
                    _redact(str(exc), _auth_secrets(acct.auth))[:160],
                )
            )
            continue
        if acct.auth_generation != generation:
            refreshed += 1

    # The catalog is account-independent; refresh it once after the token pass
    # so an expired token has just been rotated before this request.
    target = None
    for acct in accounts:
        if not acct.disabled and acct.healthy():
            target = acct
            break
    if target is None:
        for acct in accounts:
            if not acct.disabled:
                target = acct
                break
    models = len(SEED_MODELS)
    catalog = {"source": "seed", "models": models}
    if target is not None:
        try:
            fetch_catalog(target, timeout=20.0, force=True)
            models = len(target.models)
            catalog = {"source": "raccoon", "models": models}
        except RaccoonError as exc:
            errors.append(
                "catalog: %s" % _redact(str(exc), _auth_secrets(target.auth))[:160]
            )
            if getattr(target, "models_source", "") == "raccoon" and target.models:
                models = len(target.models)
                catalog = {"source": "raccoon", "models": models}
    return {
        "ok": not errors,
        "accounts": len(accounts),
        "refreshed": refreshed,
        "models": models,
        "catalog": catalog,
        "errors": errors[:3],
        "message": "已检查 %d 个账号，续期 %d 个，模型 %d 个"
        % (len(accounts), refreshed, models),
    }
