#!/bin/sh
# Builds codemode/quickjs.wasm: QuickJS-ng compiled to WASI with glue.c in
# place of quickjs-libc. Run through `go tool task wasm`; needs zig and curl.
# The source is pinned by version and checksum, and the output is committed,
# so malachi's own build never needs a C toolchain.
set -eu

VERSION=0.17.0
SHA256=559bc4c420475e55c7ab4510adbc562f55d7524d75e8e89d79ce4bb02f5687d9

here=$(cd "$(dirname "$0")" && pwd)
out="$here/../quickjs.wasm"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

curl -fsSL -o "$work/src.tar.gz" "https://github.com/quickjs-ng/quickjs/archive/refs/tags/v$VERSION.tar.gz"
echo "$SHA256  $work/src.tar.gz" | shasum -a 256 -c - >/dev/null
tar -xzf "$work/src.tar.gz" -C "$work"
src="$work/quickjs-$VERSION"

# The flags follow QuickJS-ng's own WASI build (CMakeLists.txt). The stack
# is 2 MB as there; QuickJS's own stack check stays at its 1 MB default.
zig cc -target wasm32-wasi -O2 -mexec-model=reactor \
	-D_GNU_SOURCE -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_SIGNAL \
	-I"$src" \
	-Wl,-z,stack-size=2097152 \
	-Wl,--strip-all \
	-lwasi-emulated-process-clocks -lwasi-emulated-signal \
	-o "$out" \
	"$here/glue.c" "$src/quickjs.c" "$src/libregexp.c" "$src/libunicode.c" "$src/dtoa.c"

ls -l "$out"
