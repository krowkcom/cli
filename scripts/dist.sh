#!/usr/bin/env bash
# dist.sh — how a version becomes release archives. The one home of the
# platform table and of the release layout: CI builds one target per runner
# with it, in parallel, and `make dist` builds every target here in turn.
#
#   scripts/dist.sh targets                 the table: triple goos goarch runner
#   scripts/dist.sh matrix                  the table as a GitHub Actions matrix
#   scripts/dist.sh archive-name GOOS GOARCH VERSION
#   scripts/dist.sh build TRIPLE VERSION    both binaries for one target, archived
#   scripts/dist.sh assemble VERSION        checksums.txt, metadata.json, artifacts.json
#   scripts/dist.sh all VERSION             build every target, then assemble
#
# The layout is what GoReleaser wrote, because npm/build.mjs, install.sh and the
# upgrader read it: dist/krowk_<version>_<goos>_<goarch>.tar.gz (.zip on Windows)
# holding krowk and krowk-mcp at its root, dist/checksums.txt in `sha256sum`
# form, and dist/<triple>/ holding the loose binaries.
#
# KROWK_FAST_BUILD=1 builds without LTO and with parallel codegen: the same
# binaries in every way a packaging test can see, minutes sooner. Releases
# never set it.

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# triple goos goarch runner. Linux is musl, and static, because the binary lands
# in containers we did not build. Darwin builds on macOS, the only place its
# SDK is; the rest cross-compile with zig as the C toolchain for the bundled
# SQLite and ring. Nobody runs an agent container on Windows ARM yet.
TARGETS="x86_64-unknown-linux-musl linux amd64 ubuntu-latest
aarch64-unknown-linux-musl linux arm64 ubuntu-latest
x86_64-apple-darwin darwin amd64 macos-latest
aarch64-apple-darwin darwin arm64 macos-latest
x86_64-pc-windows-gnu windows amd64 ubuntu-latest"

die() { echo "dist.sh: $*" >&2; exit 1; }

row() {
  local found
  found=$(awk -v t="$1" '$1 == t' <<<"$TARGETS")
  [[ -n "$found" ]] || die "no target $1 — scripts/dist.sh targets lists them"
  echo "$found"
}

archive_name() {
  local ext="tar.gz"
  [[ "$1" == windows ]] && ext="zip"
  echo "krowk_${3}_${1}_${2}.${ext}"
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

build() {
  local triple="$1" version="$2" goos goarch ext="" tool=zigbuild out
  read -r _ goos goarch _ <<<"$(row "$triple")"
  [[ "$goos" == windows ]] && ext=".exe"
  # Apple's own toolchain for Apple targets on a Mac; zig everywhere else.
  [[ "$goos" == darwin && "$(uname -s)" == Darwin ]] && tool=build
  if [[ "${KROWK_FAST_BUILD:-}" == 1 ]]; then
    export CARGO_PROFILE_RELEASE_LTO=false CARGO_PROFILE_RELEASE_CODEGEN_UNITS=16
  fi
  # The device build for krowk, the agent build for krowk-mcp: the MCP server
  # never touches the session store, so it does not carry SQLite.
  KROWK_VERSION="$version" cargo "$tool" --release --locked --target "$triple" -p krowk --bin krowk --features sessions
  KROWK_VERSION="$version" cargo "$tool" --release --locked --target "$triple" -p krowk --bin krowk-mcp

  out="dist/$triple"
  rm -rf "$out" && mkdir -p "$out"
  for bin in krowk krowk-mcp; do
    cp "target/$triple/release/$bin$ext" "$out/"
  done
  local archive
  archive="dist/$(archive_name "$goos" "$goarch" "$version")"
  rm -f "$archive"
  if [[ "$goos" == windows ]]; then
    (cd "$out" && zip -q -X "../$(basename "$archive")" "krowk$ext" "krowk-mcp$ext")
  else
    tar -czf "$archive" -C "$out" krowk krowk-mcp
  fi
  echo "dist.sh: $archive"
}

assemble() {
  local version="$1" archives=() triple goos goarch ext
  while read -r triple goos goarch _; do
    local archive
    archive=$(archive_name "$goos" "$goarch" "$version")
    [[ -f "dist/$archive" ]] || die "dist/$archive is missing — build $triple first"
    archives+=("$archive")
  done <<<"$TARGETS"
  (cd dist && sha256 "${archives[@]}" >checksums.txt)

  printf '{"project_name":"krowk","tag":"v%s","version":"%s"}\n' "$version" "$version" >dist/metadata.json
  {
    echo "["
    local first=1
    while read -r triple goos goarch _; do
      ext=""
      [[ "$goos" == windows ]] && ext=".exe"
      for bin in krowk krowk-mcp; do
        [[ $first == 1 ]] || echo ","
        first=0
        printf '  {"name":"%s","path":"dist/%s/%s%s","goos":"%s","goarch":"%s","type":"Binary","extra":{"ID":"%s"}}' \
          "$bin" "$triple" "$bin" "$ext" "$goos" "$goarch" "$bin"
      done
    done <<<"$TARGETS"
    echo
    echo "]"
  } >dist/artifacts.json
  echo "dist.sh: dist/checksums.txt, dist/metadata.json, dist/artifacts.json"
}

case "${1:-}" in
  targets) echo "$TARGETS" ;;
  matrix)
    printf '{"include":['
    awk 'NR > 1 { printf "," } { printf "{\"target\":\"%s\",\"runner\":\"%s\"}", $1, $4 }' <<<"$TARGETS"
    printf ']}\n'
    ;;
  archive-name) archive_name "${2:?goos}" "${3:?goarch}" "${4:?version}" ;;
  build) build "${2:?triple}" "${3:?version}" ;;
  assemble) assemble "${2:?version}" ;;
  all)
    rm -rf dist && mkdir -p dist
    while read -r triple _; do build "$triple" "${2:?version}"; done <<<"$TARGETS"
    assemble "$2"
    ;;
  *) sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
esac
