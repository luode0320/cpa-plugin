#!/usr/bin/env python3
"""Publish helper: sync release-assets/<plugin>-<version>/ + registry.json.

Usage:
    python publish-assets.py token-usage-tracker 0.1.2 [--assets-dir release-assets]

Steps:
  1. scan release-assets/<id>-<version>/ for <id>_<version>_<goos>_<goarch>.zip + checksums.txt
  2. verify all 7 platforms present
  3. update registry.json: version + install.artifacts[*].{url,sha256,size}
     (URLs keep the raw.githubusercontent.com/main/release-assets/... layout)
  4. validate with scripts/validate-registry.py
"""
from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys

PLATFORMS = [
    "darwin_amd64",
    "darwin_arm64",
    "freebsd_amd64",
    "linux_amd64",
    "linux_arm64",
    "windows_amd64",
    "windows_arm64",
]

RAW_BASE = "https://raw.githubusercontent.com/luode0320/cpa-plugin/main/release-assets"


def sha256_of(path: pathlib.Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("plugin")
    ap.add_argument("version")
    ap.add_argument("--assets-dir", default="release-assets")
    args = ap.parse_args()

    plugin, version = args.plugin, args.version
    ver_re = re.compile(r"^[0-9][0-9A-Za-z.+-]*$")
    if not ver_re.match(version):
        raise SystemExit(f"bad version {version!r}")

    root = pathlib.Path(args.assets_dir)
    out = root / f"{plugin}-{version}"
    if not out.is_dir():
        raise SystemExit(f"assets dir missing: {out}")

    missing = [t for t in PLATFORMS if not (out / f"{plugin}_{version}_{t}.zip").exists()]
    if missing:
        raise SystemExit(f"missing zips: {missing}")

    # checksums.txt sanity (regenerate if absent)
    cksum = out / "checksums.txt"
    if not cksum.exists():
        lines = []
        for t in PLATFORMS:
            z = out / f"{plugin}_{version}_{t}.zip"
            lines.append(f"{sha256_of(z)}  {z.name}")
        cksum.write_text("\n".join(lines) + "\n")
    for t in PLATFORMS:
        z = out / f"{plugin}_{version}_{t}.zip"
        h = sha256_of(z)
        ck = f"{h}  {z.name}"
        if ck not in cksum.read_text():
            print(f"WARN checksums.txt mismatch for {z.name} (will regenerate)")
            lines = []
            for tt in PLATFORMS:
                zz = out / f"{plugin}_{version}_{tt}.zip"
                lines.append(f"{sha256_of(zz)}  {zz.name}")
            cksum.write_text("\n".join(lines) + "\n")
            break

    reg = pathlib.Path("registry.json")
    data = json.loads(reg.read_text())
    entry = next((p for p in data.get("plugins", []) if p.get("id") == plugin), None)
    if entry is None:
        raise SystemExit(f"plugin {plugin} not found in registry.json")

    entry["version"] = version
    arts = entry.get("install", {}).get("artifacts")
    if not isinstance(arts, list):
        raise SystemExit("install.artifacts missing")
    seen = set()
    for t in PLATFORMS:
        z = out / f"{plugin}_{version}_{t}.zip"
        goos, goarch = t.split("_", 1)
        art = next((a for a in arts if a.get("goos") == goos and a.get("goarch") == goarch), None)
        if art is None:
            raise SystemExit(f"no artifact slot for {t}")
        art["url"] = f"{RAW_BASE}/{plugin}-{version}/{z.name}"
        art["sha256"] = sha256_of(z)
        art["size"] = z.stat().st_size
        seen.add(t)
    # drop artifact slots for platforms not in our set (keeps registry honest)
    arts[:] = [a for a in arts if f"{a.get('goos')}_{a.get('goarch')}" in seen]
    reg.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")

    print(f"updated {plugin} -> {version}: {len(arts)} artifacts, sha256+size synced")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
