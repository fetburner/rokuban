#!/usr/bin/env bash
# Build crates/mpeg2toh264-wasm from the library clone into dist/, and transpile
# the yadif package source (optional WebGL deinterlacer) into dist/yadif/.
#
#   LIB=/path/to/mpeg2toh264 ./build-wasm.sh
#
# Needs: rustup target wasm32-unknown-unknown, and a wasm-bindgen CLI whose
# version equals the wasm-bindgen crate version in the clone's Cargo.lock
# (cargo install wasm-bindgen-cli --version <that> --locked).
# Yadif is transpiled with the rokuban web/ tree's tsc; no npm install happens.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
lib="${LIB:?set LIB to a clone of https://github.com/otya128/mpeg2toh264 at 7b008c6}"
out="$here/dist"
tsc_bin="${TSC:-$here/../../node_modules/.bin/tsc}"

wanted="$(sed -n '/^name = "wasm-bindgen"$/,/^version/s/^version = "\(.*\)"/\1/p' "$lib/Cargo.lock" | head -1)"
have="$(wasm-bindgen --version | awk '{print $2}')"
if [ "$wanted" != "$have" ]; then
  echo "wasm-bindgen CLI is $have but the crate is $wanted; they must match" >&2
  exit 1
fi

(cd "$lib" && cargo build --release --package mpeg2toh264-wasm --target wasm32-unknown-unknown >/dev/null 2>"$here/build-cargo.log") \
  || { echo "cargo build failed, see $here/build-cargo.log" >&2; exit 1; }
wasm-bindgen --target web --out-dir "$out" \
  "$lib/target/wasm32-unknown-unknown/release/mpeg2toh264_wasm.wasm"
echo "wasm: $(ls "$out" | tr '\n' ' ')"

# Yadif: plain ES modules out of tsc, no bundling. Type errors are reported
# but do not stop emission, since only the runtime code is wanted here.
rm -rf "$out/yadif"
"$tsc_bin" --module es2022 --target es2022 --moduleResolution bundler \
  --ignoreConfig --lib es2022,dom,dom.iterable --skipLibCheck --outDir "$out/yadif" --rootDir "$lib/packages/yadif/src" \
  "$lib"/packages/yadif/src/index.ts || echo "tsc reported errors (yadif emitted anyway)" >&2
echo "yadif: $(ls "$out/yadif" | wc -l | tr -d ' ') files"
