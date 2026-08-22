#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""Download release assets for workbuddy-provider and verify checksums.

Usage: python scripts/download-release-assets.py <version>
Downloads into release-assets/workbuddy-provider-<version>/ using direct
releases/download/<tag>/<name> URLs (public repo, no auth).
"""
import hashlib
import json
import os
import re
import sys
import urllib.request

REPO = "luode0320/cpa-workbuddy-plugin"
PLUGIN = "workbuddy-provider"


def http_get(url, retries=5):
    last = None
    for i in range(retries):
        try:
            req = urllib.request.Request(url, headers={"Accept": "application/vnd.github+json"})
            with urllib.request.urlopen(req, timeout=60) as r:
                return r.read()
        except Exception as e:  # noqa: BLE001
            last = e
    raise last


def main():
    if len(sys.argv) != 2:
        print("usage: download-release-assets.py <version>")
        return 2
    version = sys.argv[1].lstrip("v")
    tag = f"{PLUGIN}-v{version}"
    out_dir = os.path.join("release-assets", f"{PLUGIN}-{version}")
    os.makedirs(out_dir, exist_ok=True)

    # Resolve release + asset names via API
    api = f"https://api.github.com/repos/{REPO}/releases/tags/{tag}"
    # public repo: token optional, but auth raises rate limits
    token = os.environ.get("GH_TOKEN")
    if token:
        req = urllib.request.Request(
            api, headers={"Authorization": f"token {token}", "Accept": "application/vnd.github+json"}
        )
    else:
        req = urllib.request.Request(api, headers={"Accept": "application/vnd.github+json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        release = json.load(r)
    print("release", release["id"], release["tag_name"])
    assets = [(a["name"], a["size"]) for a in release["assets"]]
    print("assets:", len(assets))

    for name, size in assets:
        url = f"https://github.com/{REPO}/releases/download/{tag}/{name}"
        dest = os.path.join(out_dir, name)
        if os.path.exists(dest) and os.path.getsize(dest) == size:
            print("cached", name)
            continue
        print("downloading", name, "...", flush=True)
        data = http_get(url)
        with open(dest, "wb") as f:
            f.write(data)
        assert len(data) == size, f"size mismatch {name}: {len(data)} != {size}"
        print("  ok", len(data), "bytes")

    # Verify checksums if present
    csum_path = os.path.join(out_dir, "checksums.txt")
    if os.path.exists(csum_path):
        with open(csum_path, encoding="utf-8") as f:
            lines = [ln.strip() for ln in f if ln.strip()]
        bad = 0
        for ln in lines:
            m = re.match(r"^([0-9a-f]{64})\s+(.+)$", ln)
            if not m:
                continue
            want, fn = m.groups()
            fp = os.path.join(out_dir, os.path.basename(fn))
            if not os.path.exists(fp):
                print("MISSING", fn)
                bad += 1
                continue
            h = hashlib.sha256(open(fp, "rb").read()).hexdigest()
            status = "OK" if h == want else "FAIL"
            if h != want:
                bad += 1
            print(status, fn)
        if bad:
            print(f"CHECKSUM FAILURES: {bad}")
            return 1
        print("ALL CHECKSUMS OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
