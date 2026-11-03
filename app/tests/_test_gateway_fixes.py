"""Tests for gateway_hint 413, GPT max_tokens clamping and 429 reset headers."""
import io
import json
import time
import unittest
import sys
import os
from unittest import mock
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
import wb_proxy
import wb_codearts

class GatewayFixesTest(unittest.TestCase):
    def test_gateway_hint_413(self):
        hint = wb_proxy.gateway_hint(413, "payload too large (20375735 bytes > 16777216 limit)")
        self.assertIn("WB_MAX_PAYLOAD_BYTES", hint)
        self.assertIn("panel settings", hint)

    def test_gpt_max_tokens_clamping(self):
        body = {"model": "gpt-6-luna", "messages": [], "max_tokens": 1}
        upstream = wb_proxy.build_upstream_body(body)
        self.assertEqual(upstream["max_tokens"], 16)

        body_ds = {"model": "deepseek-v4.1-flash", "messages": [], "max_tokens": 1}
        upstream_ds = wb_proxy.build_upstream_body(body_ds)
        self.assertEqual(upstream_ds["max_tokens"], 1)

        body_ok = {"model": "gpt-6.1-sol", "messages": [], "max_tokens": 32}
        upstream_ok = wb_proxy.build_upstream_body(body_ok)
        self.assertEqual(upstream_ok["max_tokens"], 32)

    def test_rate_limit_headers_carry_an_absolute_instant(self):
        reset = int(time.time()) + 60
        headers = dict(wb_proxy.rate_limit_headers(reset))
        self.assertEqual(headers["X-RateLimit-Reset"], str(reset))
        self.assertTrue(headers["X-RateLimit-Reset-At"].endswith("Z"))
        self.assertEqual(wb_proxy.rate_limit_headers(None), [])

class ErrorResponseTests(unittest.TestCase):
    """429 error replies carry the absolute reset instant in headers and body."""

    class _Handler(wb_proxy.Handler):
        def __init__(self):
            self.path = "/v1/chat/completions"
            self.status = None
            self.sent_headers = []
            self.wfile = io.BytesIO()
            self._json_etag = None
            self._json_extra_headers = None

        def send_response(self, code, message=None):
            self.status = code

        def send_header(self, key, value):
            self.sent_headers.append((key, value))

        def end_headers(self):
            pass

        def _handle_expect_continue(self):
            pass

        def _discard_body(self):
            pass

        def _key_id(self):
            return None

    def test_429_error_carries_absolute_reset_headers(self):
        reset = int(time.time()) + 60
        handler = self._Handler()
        handler._error(429, "too many login attempts", "rate_limit_error",
                       reset_at=reset)
        headers = dict(handler.sent_headers)
        self.assertEqual(headers["X-RateLimit-Reset"], str(reset))
        self.assertTrue(headers["X-RateLimit-Reset-At"].endswith("Z"))
        body = json.loads(handler.wfile.getvalue())
        self.assertEqual(body["error"]["reset_at"], reset)
        self.assertIn("resets at", body["error"]["message"])

    def test_429_error_parses_a_wall_clock_from_the_message(self):
        handler = self._Handler()
        handler._error(
            429,
            "usage exceeds frequency limit, reset at 2026-11-03 12:00:00 UTC+8",
            "rate_limit_error")
        headers = dict(handler.sent_headers)
        self.assertIn("X-RateLimit-Reset", headers)
        self.assertIn("X-RateLimit-Reset-At", headers)

class CodeArtsErrorTests(unittest.TestCase):
    """A CodeArts upstream 429 must keep its 429 status and reset instant."""

    def test_codearts_429_maps_to_rate_limit(self):
        handler = ErrorResponseTests._Handler()
        exc = wb_codearts.CodeArtsUpstreamError(
            "usage exceeds frequency limit, reset at 2026-11-03 12:00:00 UTC+8",
            status=429)
        with mock.patch.object(wb_proxy, "record_error", lambda *a, **k: None):
            handler._codearts_error("glm-5.2", exc, time.time(), "codearts")
        self.assertEqual(handler.status, 429)
        headers = dict(handler.sent_headers)
        self.assertIn("X-RateLimit-Reset", headers)
        self.assertIn("X-RateLimit-Reset-At", headers)
        body = json.loads(handler.wfile.getvalue())
        self.assertIn("reset_at", body["error"])


if __name__ == "__main__":
    unittest.main()
