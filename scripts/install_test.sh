#!/usr/bin/env bash
# install_test.sh — run scripts/install.sh against a release that exists.
#
# No krowk release has been published yet, so the installer cannot be tried the
# way a user will try it. What it *can* be tried against is a release that
# exists on this machine: a local HTTP server stands in for GitHub and
# KROWK_INSTALL_BASE_URL points the installer at it. That exercises the archive
# naming, the checksum verification, the extraction of both binaries, the bin
# directory, the version check and the skill copy — everything except resolving
# the latest tag, which needs a tag to resolve.
#
# Where the release comes from depends on what is already here. In CI the
# packaging workflow has just run `goreleaser release --snapshot`, so dist/
# holds GoReleaser's own archives and its own checksums.txt: those are served
# as they are, which is the point — the archiving and the checksums are then
# what gets tested, rather than a tarball this script rolled by hand. Only when
# there is no such archive does it build one, and then without --clean, because
# wiping dist/ would throw away the release it was supposed to be installing.
#
#   scripts/install_test.sh
#
# Needs bash, python3 and either goreleaser or the Go toolchain. Run from
# anywhere; it finds the repository from its own path.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
REPO_ROOT=$(pwd)

host_os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) host_arch=amd64 ;;
  aarch64|arm64) host_arch=arm64 ;;
  *) echo "  ✗ this test only installs for the host, and $(uname -m) is not one of the release's architectures" >&2; exit 1 ;;
esac

pass() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1" >&2; exit 1; }

WORK=$(mktemp -d)
SERVER_PID=""
cleanup() {
  [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "Syntax"
bash -n scripts/install.sh || fail "scripts/install.sh does not parse"
pass "scripts/install.sh parses"

# Sourced, not re-implemented: the helpers below are the installer's own, so the
# checksum this test writes is made by whatever tool the installer would have
# reached for, and the platform table it checks is the one detect_platform
# actually routes on. Sourcing runs nothing — install.sh only calls main when it
# is the program being run.
# shellcheck source=scripts/install.sh
source scripts/install.sh
resolve_sha256

if command -v shellcheck >/dev/null 2>&1; then
  shellcheck --severity=warning scripts/install.sh || fail "shellcheck"
  pass "shellcheck is happy"
else
  echo "  – shellcheck not installed, skipped"
fi

# The archive name and checksum file are read straight out of .goreleaser.yaml's
# templates rather than reproduced from memory: this is the one place the
# installer and the release pipeline have to agree, so a change to either that
# the other does not follow should break here.
echo
echo "Holding the installer to .goreleaser.yaml"
name_template=$(sed -n 's/^ *name_template: *"\(.*\)"$/\1/p' .goreleaser.yaml | head -1)
[[ "$name_template" == '{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}' ]] \
  || fail ".goreleaser.yaml's archive name_template is now '$name_template', which scripts/install.sh does not build"
grep -q 'name_template: checksums.txt' .goreleaser.yaml \
  || fail ".goreleaser.yaml no longer writes checksums.txt, which scripts/install.sh downloads"
pass "the archive and checksum names still match what the installer builds"

# The other half of that agreement is the platform table. install.sh writes it
# out as case arms and .goreleaser.yaml as goos/goarch lists, and a table kept
# in two places drifts: a platform added to the release that the installer will
# not name 404s, and a platform the installer names that the release never
# built 404s the same way, with the same wrong-looking error. So the lists are
# read out of the config and diffed against the arms.
yaml_goos=$(sed -n 's/^ *goos: *\[\(.*\)\].*/\1/p' .goreleaser.yaml | tr -d ' ' | tr ',' '\n' | sort -u)
yaml_goarch=$(sed -n 's/^ *goarch: *\[\(.*\)\].*/\1/p' .goreleaser.yaml | tr -d ' ' | tr ',' '\n' | sort -u)
[[ -n "$yaml_goos" && -n "$yaml_goarch" ]] \
  || fail "no goos/goarch lists found in .goreleaser.yaml, so this check is reading nothing"

# What the case arms in detect_platform can answer with, as opposed to what they
# accept: `mingw*|msys*|cygwin*) os="windows"` is three spellings of one goos.
sh_goos=$(sed -n 's/^ *[^ ]*) *os="\([a-z0-9]*\)".*/\1/p' scripts/install.sh | sort -u)
sh_goarch=$(sed -n 's/^ *[^ ]*) *arch="\([a-z0-9]*\)".*/\1/p' scripts/install.sh | sort -u)

[[ "$yaml_goos" == "$sh_goos" ]] \
  || fail ".goreleaser.yaml builds for [$(echo "$yaml_goos" | tr '\n' ' ')] and scripts/install.sh names [$(echo "$sh_goos" | tr '\n' ' ')]"
[[ "$yaml_goarch" == "$sh_goarch" ]] \
  || fail ".goreleaser.yaml builds for [$(echo "$yaml_goarch" | tr '\n' ' ')] and scripts/install.sh names [$(echo "$sh_goarch" | tr '\n' ' ')]"
pass "the platform lists still agree: $(echo "$yaml_goos" | tr '\n' ' ')× $(echo "$yaml_goarch" | tr '\n' ' ')"

# The ignore list is the third piece: a combination the config refuses to build
# must be one the installer refuses to offer, and every combination it does
# build must be one detect_platform can name.
yaml_ignored=$(awk '
  $1 == "ignore:" { in_ignore = 1; next }
  in_ignore && $1 == "-" && $2 == "goos:" { os = $3; next }
  in_ignore && $1 == "goarch:" { print os "_" $2; next }
  in_ignore && /^ *[a-z_]+:/ { in_ignore = 0 }
' .goreleaser.yaml | sort -u)

# detect_platform reads uname, so uname is what this stands in for. The stub is
# only live while FAKE_UNAME_* are set, and the installer itself runs in its own
# process, where it is not.
FAKE_UNAME_S=""
FAKE_UNAME_M=""
uname() {
  if [[ "${1:-}" == "-s" && -n "$FAKE_UNAME_S" ]]; then echo "$FAKE_UNAME_S"; return 0; fi
  if [[ "${1:-}" == "-m" && -n "$FAKE_UNAME_M" ]]; then echo "$FAKE_UNAME_M"; return 0; fi
  command uname "$@"
}

# How each goos and goarch reaches a machine as `uname -s` and `uname -m`. A
# platform the config gains and this table has not heard of fails loudly rather
# than going unchecked.
uname_s_for() {
  case "$1" in
    linux) echo "Linux" ;;
    darwin) echo "Darwin" ;;
    windows) echo "MINGW64_NT-10.0-22631" ;;
    *) return 1 ;;
  esac
}
uname_m_for() {
  case "$1" in
    amd64) echo "x86_64" ;;
    arm64) echo "aarch64" ;;
    *) return 1 ;;
  esac
}

for goos in $yaml_goos; do
  for goarch in $yaml_goarch; do
    want="${goos}_${goarch}"
    fake_s=$(uname_s_for "$goos") || fail "this test has no uname -s spelling for $goos; add one beside the others"
    fake_m=$(uname_m_for "$goarch") || fail "this test has no uname -m spelling for $goarch; add one beside the others"

    got=""
    if got=$(FAKE_UNAME_S="$fake_s" FAKE_UNAME_M="$fake_m" detect_platform 2>/dev/null); then
      named=yes
    else
      named=no
    fi

    if grep -qx "$want" <<<"$yaml_ignored"; then
      [[ "$named" == "no" ]] \
        || fail ".goreleaser.yaml does not build $want, but scripts/install.sh offers it as $got"
    else
      [[ "$named" == "yes" ]] \
        || fail ".goreleaser.yaml builds $want, but scripts/install.sh refuses to name it"
      [[ "$got" == "$want" ]] \
        || fail ".goreleaser.yaml builds $want, but scripts/install.sh calls that platform $got"
    fi
  done
done
pass "every platform the release builds is one the installer names, and no others"

# The release to install from.
echo
echo "Laying out a release to install from"
RELEASE="$WORK/release"
mkdir -p "$RELEASE"

# dist/ first. In CI the packaging workflow has already run a full snapshot two
# steps up, so GoReleaser's own archive and its own checksums.txt are sitting
# there — serving those is what makes this a test of the release rather than of
# a tarball assembled here. They are copied rather than linked because the
# tampering test below writes a byte into the archive it serves.
dist_archive=""
if [[ -f dist/checksums.txt ]]; then
  for candidate in dist/krowk_*_"${host_os}"_"${host_arch}".tar.gz; do
    if [[ -f "$candidate" ]]; then
      dist_archive="$candidate"
      break
    fi
  done
fi

if [[ -n "$dist_archive" ]]; then
  ARCHIVE=$(basename "$dist_archive")
  VERSION=${ARCHIVE#krowk_}
  VERSION=${VERSION%"_${host_os}_${host_arch}.tar.gz"}
  cp "$dist_archive" "$RELEASE/$ARCHIVE"
  cp dist/checksums.txt "$RELEASE/checksums.txt"
  SOURCE="the release already in dist/"
else
  # No release here to install, so one gets built — and dist/ is left alone
  # while doing it, because --clean would empty a directory this script does not
  # own and a build without --clean refuses to write into one that is not empty.
  # The way out of that is a copy of the config with dist pointed somewhere
  # disposable: nothing to wipe, nothing to refuse, and the repository's dist/
  # is neither read nor written. One invocation builds both ids, so there is no
  # second one to find the first one's leftovers in the way.
  mkdir -p "$WORK/build"
  if command -v goreleaser >/dev/null 2>&1; then
    # --snapshot so it needs no tag. Single-target keeps it to the host's
    # platform: this test is about the installer, and `goreleaser check` in
    # `make release-check` is what holds the other nine platforms to the config.
    { cat .goreleaser.yaml; printf '\ndist: %s\n' "$WORK/dist"; } >"$WORK/goreleaser.yaml"
    goreleaser build --snapshot --single-target --config "$WORK/goreleaser.yaml" \
      >"$WORK/goreleaser.log" 2>&1 || { cat "$WORK/goreleaser.log"; fail "goreleaser could not build"; }
    for binary in krowk krowk-mcp; do
      built=$(find "$WORK/dist" -type f -name "$binary" -print -quit)
      [[ -n "$built" ]] || fail "goreleaser built no $binary"
      cp "$built" "$WORK/build/$binary"
    done
    SOURCE="goreleaser"
  else
    go build -o "$WORK/build/krowk" ./cmd/krowk
    go build -o "$WORK/build/krowk-mcp" ./cmd/krowk-mcp
    SOURCE="go build"
  fi
  VERSION="9.9.9"
  ARCHIVE="krowk_${VERSION}_${host_os}_${host_arch}.tar.gz"
  tar -czf "$RELEASE/$ARCHIVE" -C "$WORK/build" krowk krowk-mcp
  (cd "$RELEASE" && "${SHA256_CMD[@]}" "$ARCHIVE" >checksums.txt)
fi

cp skills/krowk/SKILL.md "$RELEASE/SKILL.md"
pass "serving $ARCHIVE + checksums.txt, from $SOURCE"

# The server. Port 0 so parallel runs do not collide.
python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$RELEASE" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
BASE=""
for _ in $(seq 1 50); do
  port=$(sed -n 's/.*port \([0-9]*\).*/\1/p' "$WORK/server.log" | head -1)
  if [[ -n "$port" ]]; then
    BASE="http://127.0.0.1:${port}"
    break
  fi
  sleep 0.1
done
[[ -n "$BASE" ]] || fail "the local release server never came up"
pass "serving the release at $BASE"

echo
echo "Installing"
BIN="$WORK/bin"
CLAUDE="$WORK/claude"
mkdir -p "$CLAUDE/skills"

# HOME is redirected so a failing test cannot write to the real one, and SHELL is
# named so the PATH branch picks a file inside the redirected HOME.
env -i PATH="$PATH" HOME="$WORK/home" SHELL=/bin/bash NO_COLOR=1 \
  CLAUDE_CONFIG_DIR="$CLAUDE" \
  KROWK_INSTALL_BASE_URL="$BASE" KROWK_VERSION="$VERSION" KROWK_BIN_DIR="$BIN" \
  bash "$REPO_ROOT/scripts/install.sh" >"$WORK/install.log" 2>&1 \
  || { cat "$WORK/install.log"; fail "the installer exited non-zero"; }

[[ -x "$BIN/krowk" ]]     || fail "krowk was not installed"
[[ -x "$BIN/krowk-mcp" ]] || fail "krowk-mcp was not installed"
pass "both binaries landed in $BIN"

"$BIN/krowk" --version >/dev/null || fail "the installed krowk does not run"
pass "the installed krowk runs"

grep -q "Checksum matches" "$WORK/install.log" || fail "the installer did not report verifying the checksum"
pass "the checksum was verified"

[[ -f "$CLAUDE/skills/krowk/SKILL.md" ]] || fail "the agent skill was not written"
diff -q skills/krowk/SKILL.md "$CLAUDE/skills/krowk/SKILL.md" >/dev/null \
  || fail "the installed skill differs from the one in the repository"
pass "the agent skill was written to CLAUDE_CONFIG_DIR"

grep -q "krowk push screenshot.png" "$WORK/install.log" || fail "the next steps were not printed"
pass "the next steps were printed"

# NO_COLOR was set above, so nothing may have emitted an escape sequence.
if grep -q $'\033' "$WORK/install.log"; then fail "the installer emitted colour with NO_COLOR set"; fi
pass "NO_COLOR was honoured"

echo
echo "Refusing a bad download"
rm -rf "${BIN:?}"/*
# One byte of the archive changed, checksums.txt untouched: exactly the shape of
# a tampered or truncated download.
printf 'x' >>"$RELEASE/$ARCHIVE"
if env -i PATH="$PATH" HOME="$WORK/home" SHELL=/bin/bash NO_COLOR=1 \
  KROWK_SKIP_SKILL=1 \
  KROWK_INSTALL_BASE_URL="$BASE" KROWK_VERSION="$VERSION" KROWK_BIN_DIR="$BIN" \
  bash "$REPO_ROOT/scripts/install.sh" >"$WORK/bad.log" 2>&1; then
  fail "the installer accepted an archive whose checksum does not match"
fi
if [[ -e "$BIN/krowk" ]]; then fail "the installer left a binary behind after a checksum failure"; fi
grep -q "not the file the release signed for" "$WORK/bad.log" \
  || { cat "$WORK/bad.log"; fail "the installer failed for some other reason than the checksum"; }
pass "a mismatched checksum stops the install, and installs nothing"

echo
echo "Refusing a missing version"
if env -i PATH="$PATH" HOME="$WORK/home" SHELL=/bin/bash NO_COLOR=1 \
  KROWK_INSTALL_BASE_URL="$BASE" KROWK_VERSION="not-a-version" KROWK_BIN_DIR="$BIN" \
  bash "$REPO_ROOT/scripts/install.sh" >"$WORK/version.log" 2>&1; then
  fail "the installer accepted KROWK_VERSION=not-a-version"
fi
pass "a version that is not one is refused"

echo
echo "All checks passed."
