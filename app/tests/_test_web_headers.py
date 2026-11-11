"""网页通道的浏览器画像：每账号稳定、头部自洽，且不再所有账号同一个 UA。"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import wb_accounts  # noqa: E402
import wb_fingerprint  # noqa: E402


class WebProfileTests(unittest.TestCase):
    def test_profile_is_stable(self):
        self.assertEqual(
            wb_fingerprint.web_profile("u-1"),
            wb_fingerprint.web_profile("u-1"),
        )

    def test_profile_fields_are_consistent(self):
        for uid in ("a", "b", "c", "d", "e", "f"):
            p = wb_fingerprint.web_profile(uid)
            self.assertIn("Chrome/" + p["chrome"], p["ua"])
            self.assertIn('"Chromium";v="' + p["chrome"] + '"', p["ch_ua"])
            self.assertIn('"%s";v="' % p["brand"] + p["chrome"] + '"', p["ch_ua"])
            self.assertIn(p["brand"], ("Microsoft Edge", "Google Chrome"))
            self.assertEqual(p["mobile"], "?0")
            self.assertIn(p["platform"], ("Windows", "macOS"))

    def test_profiles_are_not_all_the_same(self):
        uas = {wb_fingerprint.web_profile("u-%d" % i)["ua"] for i in range(30)}
        self.assertGreater(len(uas), 1, "所有账号不应共用同一个浏览器 UA")

    def test_headers_carry_browser_fingerprint(self):
        h = wb_fingerprint.web_browser_headers("u-1", "intl")
        for key in (
            "User-Agent",
            "Accept-Language",
            "sec-ch-ua",
            "sec-ch-ua-mobile",
            "sec-ch-ua-platform",
            "sec-fetch-site",
            "sec-fetch-mode",
            "sec-fetch-dest",
        ):
            self.assertIn(key, h)
        p = wb_fingerprint.web_profile("u-1")
        self.assertEqual(h["User-Agent"], p["ua"])
        self.assertEqual(h["sec-ch-ua-platform"], '"%s"' % p["platform"])
        self.assertIn("en-US", h["Accept-Language"])

    def test_cn_uses_chinese_language(self):
        h = wb_fingerprint.web_browser_headers("u-1", "cn")
        self.assertIn("zh-CN", h["Accept-Language"])


class AccountWebHeadersTests(unittest.TestCase):
    def test_web_headers_merge_identity_and_browser(self):
        acc = wb_accounts.Account({"uid": "u-1", "realm": "intl", "accessToken": "tok"})
        h = acc.web_headers()
        self.assertEqual(h["Authorization"], "Bearer tok")
        self.assertEqual(h["X-User-Id"], "u-1")
        self.assertIn("sec-ch-ua", h)
        self.assertIn("sec-fetch-mode", h)
        self.assertEqual(h["User-Agent"], wb_fingerprint.web_profile("u-1")["ua"])
        self.assertEqual(h["Accept"], "application/json")
        self.assertRegex(h["X-Request-ID"], r"^[0-9a-f]{32}$")


if __name__ == "__main__":
    unittest.main()
