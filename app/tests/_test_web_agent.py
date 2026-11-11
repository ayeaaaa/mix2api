"""网页打卡要真的把会话驱动起来，不能只建会话（issue #90）。

POST /console/as/conversations/ 只是排队；agent 要等客户端接上沙箱（ACP over
HTTP + SSE）并请求这一轮才会跑。这里钉住那个调用顺序，以及两种结果怎么上报：
跑完（completed）和没跑完。
"""
import base64
import io
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import wb_webagent as W


class FakeChannel(object):
    """顶替 AcpChannel：记录 JSON-RPC 调用，不碰网络。"""

    def __init__(self, link, token, user_agent, log=None, proxy="", timeout=30):
        self.link = link
        self.token = token
        self.user_agent = user_agent
        self.proxy = proxy
        self.calls = []
        self.updates = 0
        self.chunks = 0
        self.closed = False
        self.fail_on = None

    def open(self):
        self.calls.append(("open",))
        return "conn-1"

    def request(self, method, params, request_id):
        if self.fail_on == method:
            raise W.AcpError("%s 返回 HTTP 500" % method)
        self.calls.append((method, params, request_id))
        if method == "session/prompt":
            self.updates, self.chunks = 7, 3
        return 202

    def close(self):
        self.closed = True


def run(statuses, channel_attrs=None, pause=None, **kwargs):
    """用一段状态脚本跑 run_turn，返回 (结果, 通道)。"""
    made = []

    def factory(*args, **factory_kwargs):
        channel = FakeChannel(*args, **factory_kwargs)
        if channel_attrs:
            channel.__dict__.update(channel_attrs)
        made.append(channel)
        return channel

    remaining = list(statuses)

    def poll():
        return remaining.pop(0) if remaining else "working"

    old_pause = W._human_pause
    W._human_pause = pause or (lambda: None)
    try:
        result = W.run_turn("https://box.example/acp", "tok", "sess-1",
                            "/workspace", "Hi", "ua", poll_status=poll,
                            poll_interval=0, channel_factory=factory, **kwargs)
    finally:
        W._human_pause = old_pause
    return result, made[0]


class RunTurnTests(unittest.TestCase):
    def test_the_turn_is_driven_in_order(self):
        result, channel = run(["working", "completed"])
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["status"], "completed")
        self.assertEqual([call[0] for call in channel.calls],
                         ["open", "initialize", "session/load", "session/new",
                          "session/set_mode", "session/set_config_option",
                          "session/prompt"])
        load = channel.calls[2][1]
        self.assertEqual(load["sessionId"], "sess-1")
        self.assertEqual(load["cwd"], "/workspace")
        self.assertEqual(load["mcpServers"], [])
        new = channel.calls[3][1]
        self.assertEqual(new["cwd"], "/workspace")
        self.assertEqual(new["mcpServers"], [])
        mode = channel.calls[4][1]
        self.assertEqual(mode["sessionId"], "sess-1")
        self.assertEqual(mode["modeId"], "default")
        config = channel.calls[5][1]
        self.assertEqual(config["sessionId"], "sess-1")
        self.assertEqual(config["configId"], "language")
        self.assertEqual(config["value"], "English")
        prompt = channel.calls[6][1]
        self.assertEqual(prompt["sessionId"], "sess-1")
        self.assertEqual(prompt["prompt"][0]["text"], "Hi")
        meta = prompt["_meta"]["codebuddy.ai"]
        self.assertEqual(meta["conversationId"], "sess-1")
        self.assertEqual(meta["mode"], "craft")
        self.assertEqual(meta["extra"]["sourceContentBlocks"],
                         [{"type": "text", "text": "Hi"}])
        self.assertRegex(meta["traceId"], r"^[0-9a-f]{32}$")
        self.assertRegex(meta["traceparent"],
                         r"^00-[0-9a-f]{32}-[0-9a-f]{16}-01$")
        self.assertTrue(channel.closed)
        self.assertEqual(result["events"], 7)
        self.assertEqual(result["chunks"], 3)

    def test_a_human_pause_happens_before_the_prompt(self):
        events = []
        result, channel = run(["completed"], pause=lambda: events.append("pause"))
        self.assertTrue(result["ok"], result)
        self.assertEqual(events, ["pause"], "提示词前应有一次拟人停顿")
        self.assertEqual(
            [call[0] for call in channel.calls],
            ["open", "initialize", "session/load", "session/new",
             "session/set_mode", "session/set_config_option", "session/prompt"],
        )

    def test_a_conversation_that_never_finishes_is_reported(self):
        result, channel = run(["working"], wait_seconds=1)
        self.assertFalse(result["ok"])
        self.assertIn("没有跑完", result["error"])
        self.assertTrue(channel.closed)

    def test_a_failed_conversation_stops_early(self):
        result, _channel = run(["working", "failed"])
        self.assertFalse(result["ok"])
        self.assertIn("failed", result["error"])

    def test_a_transport_error_is_reported_not_raised(self):
        result, channel = run(["working"], channel_attrs={"fail_on": "session/prompt"})
        self.assertFalse(result["ok"])
        self.assertIn("session/prompt", result["error"])
        self.assertTrue(channel.closed)


class SseParsingTests(unittest.TestCase):
    def test_updates_and_chunks_are_counted(self):
        channel = W.AcpChannel("https://box.example/acp", "tok", "ua")
        lines = []
        for message in (
            {"jsonrpc": "2.0", "method": "session/update",
             "params": {"update": {"sessionUpdate": "session_info_update"}}},
            {"jsonrpc": "2.0", "method": "session/update",
             "params": {"update": {"sessionUpdate": "agent_message_chunk",
                                   "content": {"type": "text", "text": "hi"}}}},
            {"jsonrpc": "2.0", "id": 3, "result": {"stopReason": "end_turn"}},
        ):
            lines.append(("event: message\ndata: " + json.dumps(message) + "\n\n")
                          .encode("utf-8"))
        lines.append(b": heartbeat\n\n")

        class FakeResponse(object):
            fp = io.BytesIO(b"".join(lines))

        channel._read_events(FakeResponse())
        drained = []
        while True:
            item = channel.events.get_nowait()
            if item is None:
                break
            drained.append(item)
        self.assertEqual(channel.updates, 2)
        self.assertEqual(channel.chunks, 1)
        self.assertEqual(len(drained), 3)
        self.assertEqual(drained[-1]["id"], 3)

    def test_the_client_capabilities_stay_honest(self):
        # 打卡不需要文件系统/终端回调：fs 能力必须是关的，否则沙箱会等我们回调；
        # _meta / elicitation 是官方客户端的声明，按抓包补齐。
        self.assertFalse(W.CLIENT_CAPABILITIES["fs"]["readTextFile"])
        self.assertFalse(W.CLIENT_CAPABILITIES["fs"]["writeTextFile"])
        self.assertTrue(W.CLIENT_CAPABILITIES["_meta"]["codebuddy.ai"]["question"])
        self.assertIn("elicitation", W.CLIENT_CAPABILITIES)
        self.assertEqual(W.PROTOCOL_VERSION, 1)


class ProxyTunnelTests(unittest.TestCase):
    """账号绑了「要认证的代理」时，CONNECT 必须带 Proxy-Authorization。

    urllib 的 ProxyHandler 会替 urlopen 那条路径加这个头，裸 http.client +
    set_tunnel 不会：少了它，认证代理一律回 407，而桌面通道却正常——表现就是
    「每日活跃对话成功完成（网页通道失败：OSError: Tunnel connection failed:
    407 Proxy Authentication Required）」。
    """

    def test_credentials_are_parsed_from_both_url_shapes(self):
        for url in ("http://bob:secret@proxy.example:8080",
                    "bob:secret@proxy.example:8080"):
            host, port, headers = W._proxy_parts(url)
            self.assertEqual(host, "proxy.example", url)
            self.assertEqual(port, 8080, url)
            self.assertEqual(headers["Proxy-Authorization"],
                             "Basic " + base64.b64encode(b"bob:secret").decode(), url)

    def test_escaped_credentials_match_urllib(self):
        # 与 urllib.request.ProxyHandler.proxy_open 同一口径：先 unquote 再 base64。
        _host, _port, headers = W._proxy_parts("http://us%40er:pa%3Ass@proxy.example:8080")
        self.assertEqual(headers["Proxy-Authorization"],
                         "Basic " + base64.b64encode(b"us@er:pa:ss").decode())

    def test_a_proxy_without_credentials_sends_no_header(self):
        _host, _port, headers = W._proxy_parts("http://proxy.example:8080")
        self.assertEqual(headers, {})

    def test_the_tunnel_carries_the_header(self):
        captured = {}

        class FakeConn(object):
            def __init__(self, host, port, timeout=None):
                captured["host"] = host
                captured["port"] = port

            def set_tunnel(self, host, port, headers=None):
                captured["tunnel"] = (host, port, headers)

        real = W.http.client.HTTPSConnection
        W.http.client.HTTPSConnection = FakeConn
        try:
            channel = W.AcpChannel("https://box.example/acp", "tok", "ua",
                                   proxy="http://bob:secret@proxy.example:8080")
            channel._connect()
        finally:
            W.http.client.HTTPSConnection = real
        self.assertEqual(captured["host"], "proxy.example")
        self.assertEqual(captured["port"], 8080)
        self.assertEqual(captured["tunnel"][0], "box.example")
        self.assertEqual(captured["tunnel"][1], 443)
        self.assertEqual(captured["tunnel"][2]["Proxy-Authorization"],
                         "Basic " + base64.b64encode(b"bob:secret").decode())

    def test_a_direct_connection_never_tunnels(self):
        captured = {}

        class FakeConn(object):
            def __init__(self, host, port, timeout=None):
                captured["host"] = host

            def set_tunnel(self, host, port, headers=None):
                captured["tunnel"] = True

        real = W.http.client.HTTPSConnection
        W.http.client.HTTPSConnection = FakeConn
        try:
            W.AcpChannel("https://box.example/acp", "tok", "ua")._connect()
        finally:
            W.http.client.HTTPSConnection = real
        self.assertEqual(captured["host"], "box.example")
        self.assertNotIn("tunnel", captured)


if __name__ == "__main__":
    unittest.main()

