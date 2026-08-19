#!/usr/bin/env python3
"""cgo-shim-build.py — CGO_ENABLED=0 build/vet/test for c-shared CPA plugins.

CPA plugins (workbuddy/qoderwork/token-usage-tracker) use -buildmode=c-shared
and import "C", so they cannot be compiled on machines without a C toolchain
(typical local Windows). This script copies the plugin dir to a temp location,
rewrites main.go by stripping the cgo preamble and replacing C.* references
with Go stubs, then runs go build/vet/test with CGO_ENABLED=0.

Usage:
    python scripts/cgo-shim-build.py <plugin-dir> [--go <go-binary>] [--no-test]

Exit code 0 = shim build+vet+test all green.
"""
from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

SHIM_BLOCK = '''// cgo shim — replaces the C preamble for CGO_ENABLED=0 local verification.
// Type-check/build/test only; the C ABI entry points are stubs. Never ships.
// Appended at the END of main.go (imports must stay at the top; "unsafe" is
// expected in the plugin's own import block — see ensure_unsafe_import()).

type cliproxy_buffer struct {
	ptr unsafe.Pointer
	len uintptr
}

type cliproxy_host_call_fn func(hostCtx unsafe.Pointer, method *int8, request *uint8, requestLen uintptr, response *cliproxy_buffer) int32
type cliproxy_host_free_fn func(ptr unsafe.Pointer, len uintptr)

type cliproxy_host_api struct {
	abi_version uint32
	host_ctx    unsafe.Pointer
	call        cliproxy_host_call_fn
	free_buffer cliproxy_host_free_fn
}

type cliproxy_plugin_call_fn func(method *int8, request *uint8, requestLen uintptr, response *cliproxy_buffer) int32
type cliproxy_plugin_free_fn func(ptr unsafe.Pointer, len uintptr)
type cliproxy_plugin_shutdown_fn func()

type cliproxy_plugin_api struct {
	abi_version uint32
	call        cliproxy_plugin_call_fn
	free_buffer cliproxy_plugin_free_fn
	shutdown    cliproxy_plugin_shutdown_fn
}

func cgoGoBytes(p unsafe.Pointer, l int32) []byte {
	if p == nil || l <= 0 {
		return nil
	}
	return unsafe.Slice((*byte)(p), int(l))
}

func cgoGoString(p *int8) string {
	if p == nil {
		return ""
	}
	return string(cgoGoBytes(unsafe.Pointer(p), 1<<20))
}

func cgoCString(string) *int8              { return nil }
func cgoCBytes([]byte) unsafe.Pointer      { return nil }
func cgoFree(unsafe.Pointer)               {}
func cgoCallHost(*cliproxy_host_api, *int8, *uint8, uintptr, *cliproxy_buffer) int32 { return 0 }
func cgoFreeHostBuffer(*cliproxy_host_api, unsafe.Pointer, uintptr)                  {}
'''

# Ordered longest-first so typed names replace before generic ones.
C_REPLACEMENTS = [
    ("C.cliproxy_plugin_call_fn", "cliproxy_plugin_call_fn"),
    ("C.cliproxy_plugin_free_fn", "cliproxy_plugin_free_fn"),
    ("C.cliproxy_plugin_shutdown_fn", "cliproxy_plugin_shutdown_fn"),
    ("C.cliproxy_host_api", "cliproxy_host_api"),
    ("C.cliproxy_plugin_api", "cliproxy_plugin_api"),
    ("C.cliproxy_buffer", "cliproxy_buffer"),
    ("C.cliproxyPluginCall", "cliproxyPluginCall"),
    ("C.cliproxyPluginFree", "cliproxyPluginFree"),
    ("C.cliproxyPluginShutdown", "cliproxyPluginShutdown"),
    ("C.wb_call_host", "cgoCallHost"),
    ("C.wb_free_host_buffer", "cgoFreeHostBuffer"),
    ("C.GoBytes", "cgoGoBytes"),
    ("C.GoString", "cgoGoString"),
    ("C.CString", "cgoCString"),
    ("C.CBytes", "cgoCBytes"),
    ("C.free", "cgoFree"),
    ("C.uint32_t", "uint32"),
    ("C.uint8_t", "uint8"),
    ("C.size_t", "uintptr"),
    ("C.int", "int32"),
    ("C.char", "int8"),
]

# NOTE: never use `.*` inside a repeated group under DOTALL here — `(?://.*\n)*`
# backtracks catastrophically on large main.go files (observed 100% CPU hang).
# `[^\n]*` keeps each iteration to a single line => linear time.
PREAMBLE_RE = re.compile(r'(?s)^((?://[^\n]*\n)*package main\s*\n)\s*/\*.*?\*/\s*\nimport "C"\s*\n')


def shim_main(path: Path) -> None:
    src = path.read_text(encoding="utf-8")
    preamble = PREAMBLE_RE.match(src)
    if preamble is None:
        raise SystemExit(f"{path}: could not strip cgo preamble (pattern mismatch)")
    # Sanity-check the standard cliproxy ABI extern declarations: cgo resolves
    # C.cliproxyPluginCall etc. against these, and a missing extern only fails
    # in a REAL cgo build (never under the shim, which strips the preamble).
    # Verified in production: the token-usage-tracker CI build failed exactly
    # this way ("could not determine what C.cliproxyPluginCall refers to").
    for required in (
        "extern int cliproxyPluginCall(",
        "extern void cliproxyPluginFree(",
        "extern void cliproxyPluginShutdown(",
    ):
        if required not in preamble.group(0):
            raise SystemExit(
                f"{path}: cgo preamble is missing {required!r} — the real "
                f"cgo build will fail with 'could not determine what C.* refers to'"
            )
    new, count = PREAMBLE_RE.subn(r"\1\n", src, count=1)
    if count != 1:
        raise SystemExit(f"{path}: could not strip cgo preamble (pattern mismatch)")
    for old, new_name in C_REPLACEMENTS:
        new = new.replace(old, new_name)
    # Append the shim declarations AFTER the import block (Go requires all
    # imports before any declaration).
    new = new.rstrip() + "\n\n" + SHIM_BLOCK + "\n"
    # Plugins build with -buildmode=c-shared (no main); the default build mode
    # used by the shim requires one for type-check/test. Some plugins already
    # declare a no-op main() — don't duplicate it.
    if "\nfunc main(" not in new:
        new += "func main() {}\n"
    if '"unsafe"' not in new:
        # Add "unsafe" to the first import block so shim types compile.
        if 'import (\n' in new:
            new = new.replace('import (\n', 'import (\n\t"unsafe"\n', 1)
        else:
            raise SystemExit(f"{path}: no block import found; cannot inject unsafe import")
    path.write_text(new, encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("plugin_dir", help="plugin directory (e.g. workbuddy)")
    parser.add_argument("--go", default=os.environ.get("GO_BIN", "go"), help="go binary")
    parser.add_argument("--no-test", action="store_true", help="skip go test")
    parser.add_argument("--keep", action="store_true", help="keep temp dir on failure")
    parser.add_argument("--timeout", type=int, default=900, help="per-command timeout in seconds")
    args = parser.parse_args()

    root = Path(args.plugin_dir).resolve()
    if not (root / "go.mod").exists():
        raise SystemExit(f"{root}: not a Go module (no go.mod)")

    workdir = Path(tempfile.mkdtemp(prefix="cpa-shim-", dir=root.parent)) / root.name
    shutil.copytree(root, workdir, ignore=shutil.ignore_patterns(".git", "dist"))
    shim_main(workdir / "main.go")
    print(f"[cgo-shim] shim dir: {workdir}", flush=True)

    env = dict(os.environ, CGO_ENABLED="0")
    cmds = [["build", "./..."], ["vet", "./..."]]
    if not args.no_test:
        cmds.append(["test", "./..."])
    ok = True
    for cmd in cmds:
        print(f"[cgo-shim] running: go {' '.join(cmd)} ...", flush=True)
        try:
            proc = subprocess.run(
                [args.go, *cmd], cwd=workdir, env=env, timeout=args.timeout
            )
        except subprocess.TimeoutExpired:
            print(f"[cgo-shim] FAILED: go {' '.join(cmd)} timed out after {args.timeout}s", file=sys.stderr, flush=True)
            ok = False
            break
        if proc.returncode != 0:
            ok = False
            print(f"[cgo-shim] FAILED: go {' '.join(cmd)} (exit {proc.returncode})", file=sys.stderr, flush=True)
            break
        print(f"[cgo-shim] OK: go {' '.join(cmd)}", flush=True)
    if ok:
        shutil.rmtree(workdir, ignore_errors=True)
        print(f"[cgo-shim] all green ({root.name})", flush=True)
        return 0
    if args.keep:
        print(f"[cgo-shim] shim dir kept at {workdir}", flush=True)
    else:
        shutil.rmtree(workdir, ignore_errors=True)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
