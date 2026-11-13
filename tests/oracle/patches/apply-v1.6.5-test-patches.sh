#!/bin/sh
# Apply the test-only patches to the ignored v1.6.5 copy.
#
# The source tree /root/opencode/mix2api is never modified. This script only
# edits tests/oracle/v1.6.5, which tests/oracle/.gitignore excludes.
#
# Why each patch exists:
#   * wb_accounts.REALM_CONFIGS hardcodes the intl chat/billing origins and the
#     CN billing origin. v1.6.5 already reads WB_CN_CHAT_UPSTREAM for the CN
#     chat origin; the new WB_INTL_CHAT_UPSTREAM/WB_BILLING_UPSTREAM hooks let
#     the oracle point the remaining origins at the loopback fake.
#   * wb_tasks hardcodes the CN billing/web origins; the same billing hook is
#     applied there so check-in/task calls cannot escape to the real network.
#   * _fake_raccoon has no way to send a custom HTTP error body. The 400
#     credit/concurrency scenarios need deterministic bodies so the recorded
#     error envelope is meaningful.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../../.." && pwd)
V1="$ROOT/tests/oracle/v1.6.5"

python3 - "$V1" <<'PY'
import sys
from pathlib import Path

root = Path(sys.argv[1])
accounts = root / "app" / "wb_accounts.py"
tasks = root / "app" / "wb_tasks.py"
identity = root / "app" / "wb_identity.py"
fake = root / "app" / "tests" / "_fake_raccoon.py"

def replace_once(path, old, new):
    text = path.read_text(encoding="utf-8")
    if new in text:
        return
    if old not in text:
        raise SystemExit("patch target not found in %s" % path)
    path.write_text(text.replace(old, new, 1), encoding="utf-8")

replace_once(
    accounts,
    '        "chat_upstream": "https://www.workbuddy.ai",\n'
    '        "billing_upstream": "https://www.workbuddy.ai",',
    '        "chat_upstream": os.environ.get("WB_INTL_CHAT_UPSTREAM")\n'
    '        or "https://www.workbuddy.ai",\n'
    '        "billing_upstream": os.environ.get("WB_BILLING_UPSTREAM")\n'
    '        or "https://www.workbuddy.ai",',
)
replace_once(
    accounts,
    '        "chat_upstream": wb_identity.CN_CHAT_BASE,\n'
    '        "billing_upstream": "https://www.codebuddy.cn",',
    '        "chat_upstream": wb_identity.CN_CHAT_BASE,\n'
    '        "billing_upstream": os.environ.get("WB_BILLING_UPSTREAM")\n'
    '        or "https://www.codebuddy.cn",',
)
replace_once(
    identity,
    'def endpoint_for(realm, product):\n'
    '    """回传 (chat base URL, X-Domain)。身分换了，端点也要跟着换。"""\n'
    '    key = ("cn" if realm == "cn" else "intl", normalize_product(product))\n'
    '    return _ENDPOINTS[key]',
    'def endpoint_for(realm, product):\n'
    '    """回传 (chat base URL, X-Domain)。身分换了，端点也要跟着换。"""\n'
    '    key = ("cn" if realm == "cn" else "intl", normalize_product(product))\n'
    '    base, domain = _ENDPOINTS[key]\n'
    '    if key[0] == "intl":\n'
    '        base = os.environ.get("WB_INTL_CHAT_UPSTREAM") or base\n'
    '    return base, domain',
)
replace_once(
    tasks,
    'import json\nimport re\nimport time',
    'import json\nimport os\nimport re\nimport time',
)
replace_once(
    tasks,
    'BILL_BASE = "https://www.codebuddy.cn"',
    'BILL_BASE = os.environ.get("WB_BILLING_UPSTREAM") or "https://www.codebuddy.cn"',
)
replace_once(
    fake,
    "        self.chat_status = 200\n        self.chat_status_remaining = -1",
    "        self.chat_status = 200\n        self.chat_status_body = None\n        self.chat_status_remaining = -1",
)
replace_once(
    fake,
    '            if status != 200:\n'
    '                self._send_json(\n'
    '                    status, {"message": "chat failed with HTTP %s" % status}\n'
    '                )\n'
    '                return',
    '            if status != 200:\n'
    '                self._send_json(\n'
    '                    status,\n'
    '                    state.chat_status_body\n'
    '                    or {"message": "chat failed with HTTP %s" % status},\n'
    '                )\n'
    '                return',
)
print("patched", accounts, identity, tasks, fake)
PY
