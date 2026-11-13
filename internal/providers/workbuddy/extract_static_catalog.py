#!/usr/bin/env python3
"""Regenerate the embedded WorkBuddy static catalogs from v1.

This is a development-time extraction helper, not a runtime dependency. The
Go provider reads the committed static_intl.json / static_cn.json files through
go:embed; it never invokes Python.

Usage:
    python3 extract_static_catalog.py [path-to-v1-app]

The default source is /root/opencode/mix2api/app. The JSON is written next to
this script. Model IDs and every v1 field are preserved verbatim; metadata the
gateway later derives (free, aliases, wire capability flags) is not injected
here.
"""
import json
import os
import sys

DEFAULT_V1_APP = "/root/opencode/mix2api/app"


def main():
    source_app = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_V1_APP
    sys.path.insert(0, source_app)
    import wb_catalog  # noqa: E402

    here = os.path.dirname(os.path.abspath(__file__))
    for name, attr in (("static_intl.json", "STATIC_INTL_MODELS"),
                       ("static_cn.json", "STATIC_CN_MODELS")):
        models = getattr(wb_catalog, attr)
        path = os.path.join(here, name)
        with open(path, "w", encoding="utf-8", newline="\n") as fh:
            json.dump(models, fh, ensure_ascii=False, indent=2)
            fh.write("\n")
        print("%s: %d models" % (path, len(models)))


if __name__ == "__main__":
    main()
