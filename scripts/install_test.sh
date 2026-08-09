#!/usr/bin/env bash
# install_test.sh — run scripts/install.sh against a release that exists.
#
# No krowk release has been published yet, so the installer cannot be tried the
# way a user will try it. What it *can* be tried against is a release built here:
# GoReleaser produces the archives and checksums.txt from the same
# .goreleaser.yaml a tag will use, a local HTTP server stands in for GitHub, and
# KROWK_INSTALL_BASE_URL points the installer at it. That exercises the archive
# naming, the checksum verification, the extraction of both binaries, the bin
# directory, the version check and the skill copy — everything except resolving
# the latest tag, which needs a tag to resolve.
#
#   scripts/install_test.sh
#
# Needs bash, python3 and either goreleaser or the Go toolchain. Run from
# anywhere; it finds the repository from its own path.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
REPO_ROOT=$(pwd)

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

if command -v shellcheck >/dev/null 2>&1; then
  shellcheck --severity=warning scripts/install.sh || fail "shellcheck"
  pass "shellcheck is happy"
else
  echo "  – shellcheck not installed, skipped"
fi

# The release, built the way a tag would build it.
echo
echo "Building a release to install from"
RELEASE="$WORK/release"
mkdir -p "$RELEASE"

host_os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) host_arch=amd64 ;;
  aarch64|arm64) host_arch=arm64 ;;
  *) fail "this test only builds for the host, and $(uname -m) is not one of the release's architectures" ;;
esac

mkdir -p "$WORK/build"
if command -v goreleaser >/dev/null 2>&1; then
  # --snapshot so it needs no tag, --skip=publish so it touches nothing remote.
  # Single-target keeps it to the host's platform: this test is about the
  # installer, and `goreleaser check` in `make release-check` is what holds the
  # other nine platforms to the config.
  goreleaser build --snapshot --clean --single-target -o "$WORK/build/krowk" --id krowk >"$WORK/goreleaser.log" 2>&1
  goreleaser build --snapshot --clean --single-target -o "$WORK/build/krowk-mcp" --id krowk-mcp >>"$WORK/goreleaser.log" 2>&1
  SOURCE="goreleaser"
else
  mkdir -p "$WORK/build"
  go build -o "$WORK/build/krowk" ./cmd/krowk
  go build -o "$WORK/build/krowk-mcp" ./cmd/krowk-mcp
  SOURCE="go build"
fi
pass "built both binaries with $SOURCE"

# The archive name and checksum file are read straight out of .goreleaser.yaml's
# templates rather than reproduced from memory: this is the one place the
# installer and the release pipeline have to agree, so a change to either that
# the other does not follow should break here.
name_template=$(sed -n 's/^ *name_template: *"\(.*\)"$/\1/p' .goreleaser.yaml | head -1)
[[ "$name_template" == '{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}' ]] \
  || fail ".goreleaser.yaml's archive name_template is now '$name_template', which scripts/install.sh does not build"
grep -q 'name_template: checksums.txt' .goreleaser.yaml \
  || fail ".goreleaser.yaml no longer writes checksums.txt, which scripts/install.sh downloads"
pass "the archive and checksum names still match what the installer builds"

VERSION="9.9.9"
ARCHIVE="krowk_${VERSION}_${host_os}_${host_arch}.tar.gz"
tar -czf "$RELEASE/$ARCHIVE" -C "$WORK/build" krowk krowk-mcp
(cd "$RELEASE" && sha256sum "$ARCHIVE" >checksums.txt)
cp skills/krowk/SKILL.md "$RELEASE/SKILL.md"
pass "release laid out: $ARCHIVE + checksums.txt"

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
