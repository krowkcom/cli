#!/usr/bin/env bash
# install.sh — put krowk and krowk-mcp on this machine.
#
#   curl -fsSL https://krowk.com/install | bash
#
# Both binaries come down together because .goreleaser.yaml ships them in one
# archive: an agent container that wants krowk usually wants krowk-mcp too, and
# one download is one thing to get wrong.
#
# There is no wizard at the end and nothing to log into. A keyless push works the
# moment the binary lands, so the last thing this script does is say what to run,
# not ask a question nobody is there to answer — the common caller is `curl |
# bash` inside a container build, where stdin is not a terminal.
#
# Options, all through the environment because a piped script has no argv:
#   KROWK_BIN_DIR     Where the binaries go (default: ~/bin if it is on PATH,
#                     else ~/.local/bin if it is; otherwise ~/bin on Windows and
#                     ~/.local/bin everywhere else)
#   KROWK_VERSION     A version to install, e.g. 0.1.0 (default: the latest release)
#   KROWK_SKIP_SKILL  1 to leave the agent skill alone
#
#   KROWK_INSTALL_BASE_URL
#                     Test-only. A directory holding the archives, checksums.txt
#                     and SKILL.md, instead of the GitHub release. It exists so
#                     scripts/install_test.sh can run this file end to end
#                     against a local server; it is not a supported knob, and it
#                     requires KROWK_VERSION since there is no release to ask.

set -euo pipefail

REPO="krowkcom/cli"
BIN_DIR="${KROWK_BIN_DIR:-}"
VERSION="${KROWK_VERSION:-}"
BASE_URL_OVERRIDE="${KROWK_INSTALL_BASE_URL:-}"
CURL_SCHANNEL_FALLBACK_FLAG=""
# Both of these are files rather than variables, and main fills them in. See
# curl_run for why.
CURL_ERROR_FILE=""
CURL_FALLBACK_NOTED_FILE=""
# The SHA-256 command, as an array because `shasum -a 256` is three words.
# resolve_sha256 fills it once, before anything is downloaded.
SHA256_CMD=()

# The binaries this installs. krowk is first because it is the one that gets
# checked afterwards, and the one the next steps talk about.
BINARIES=(krowk krowk-mcp)

# Color helpers — NO_COLOR is honoured (https://no-color.org), and so is a stdout
# that is not a terminal, which is the usual case under `curl | bash` in CI.
if [[ -z "${NO_COLOR:-}" ]] && [[ -t 1 ]]; then
  bold()  { printf '\033[1m%s\033[0m' "$1"; }
  green() { printf '\033[32m%s\033[0m' "$1"; }
  red()   { printf '\033[31m%s\033[0m' "$1"; }
else
  bold()  { printf '%s' "$1"; }
  green() { printf '%s' "$1"; }
  red()   { printf '%s' "$1"; }
fi

info()  { echo "  $(green "✓") $1"; }
step()  { echo "  $(bold "→") $1"; }
note()  { echo "    $1"; }
error() { echo "  $(red "✗") $1" >&2; exit 1; }

# resolve_sha256 assigns SHA256_CMD rather than printing it, and main calls it
# before the first download. Printing it would put this failure inside a command
# substitution, where error's exit ends the subshell and nothing else: the caller
# would carry on with an empty command, compute an empty digest, and report a
# checksum mismatch that never happened. Assigning to a global keeps the failure
# where the reader is — at top level, under set -e — and keeps it early, before
# any bytes have been fetched to verify.
resolve_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    SHA256_CMD=(sha256sum)
  elif command -v shasum >/dev/null 2>&1; then
    SHA256_CMD=(shasum -a 256)
  else
    error "No SHA-256 tool here, so the download could not be verified and nothing was installed: install sha256sum or shasum, then run this again."
  fi
}

path_contains_dir() {
  [[ ":$PATH:" == *":$1:"* ]]
}

# default_bin_dir prefers a directory already on PATH, so the install ends with a
# working command rather than an instruction to edit a shell profile.
default_bin_dir() {
  local platform="$1"

  if path_contains_dir "$HOME/bin"; then
    echo "$HOME/bin"
    return 0
  fi
  if path_contains_dir "$HOME/.local/bin"; then
    echo "$HOME/.local/bin"
    return 0
  fi
  if [[ "$platform" == windows_* ]]; then
    echo "$HOME/bin"
  else
    echo "$HOME/.local/bin"
  fi
}

# detect_platform names the archive, so it may only answer with combinations
# .goreleaser.yaml actually builds. Anything else fails here with the reason,
# rather than 404ing on a download and looking like a network problem.
detect_platform() {
  local os arch
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$os" in
    darwin) os="darwin" ;;
    linux) os="linux" ;;
    mingw*|msys*|cygwin*) os="windows" ;;
    *) error "krowk has no build for $os. Linux, macOS and Windows are what the release carries; from source: go install github.com/${REPO}/cmd/krowk@latest" ;;
  esac

  arch=$(uname -m)
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) error "krowk has no build for $arch. amd64 and arm64 are what the release carries; from source: go install github.com/${REPO}/cmd/krowk@latest" ;;
  esac

  # Windows ARM is deliberately not built. Say so, rather than offering a
  # download that was never uploaded.
  if [[ "$os" == "windows" && "$arch" == "arm64" ]]; then
    error "No Windows ARM build is published. Install inside WSL2, or build from source: go install github.com/${REPO}/cmd/krowk@latest"
  fi

  echo "${os}_${arch}"
}

# detect_curl_fallback looks for the one curl failure that is not a real failure:
# Windows' Schannel backend cannot always reach a CRL, and refuses the download
# over a certificate it has no complaint about otherwise.
detect_curl_fallback() {
  local version_output help_output

  version_output=$(curl --version 2>/dev/null || true)
  if [[ "$version_output" != *[Ss]channel* ]]; then
    return 0
  fi

  help_output=$(curl --help all 2>/dev/null || true)
  if [[ "$help_output" == *"--ssl-revoke-best-effort"* ]]; then
    CURL_SCHANNEL_FALLBACK_FLAG="--ssl-revoke-best-effort"
  elif [[ "$help_output" == *"--ssl-no-revoke"* ]]; then
    CURL_SCHANNEL_FALLBACK_FLAG="--ssl-no-revoke"
  fi
}

# curl_run keeps curl's own words. Every failure this script reports is a failure
# somebody has to act on, and "failed to download" without the reason sends them
# to the wrong place.
#
# The reason goes to a file, not to a variable. Every caller here runs curl_run
# inside `$(…)` to capture what came back, and a variable assigned in that
# subshell dies with it — so a global would be empty in exactly the callers that
# want to quote it. The file outlives the subshell; curl_reason reads it back.
# The same goes for having said the Schannel line once: a marker file, because
# the counter would reset with every subshell and repeat the line each time.
# curl's status is taken on the same line as curl, with `|| status=$?`. Reading
# it after an `if` would read the `if` instead: a compound command whose
# condition was false and which has no else branch succeeds, so $? is 0 there
# however curl exited, and every failed download would be reported as a working
# one.
curl_run() {
  local status=0

  curl --show-error "$@" 2>"$CURL_ERROR_FILE" || status=$?
  if ((status == 0)); then
    : >"$CURL_ERROR_FILE"
    return 0
  fi

  if [[ -n "$CURL_SCHANNEL_FALLBACK_FLAG" ]] &&
    grep -q 'CRYPT_E_NO_REVOCATION_CHECK' "$CURL_ERROR_FILE"; then
    if [[ ! -e "$CURL_FALLBACK_NOTED_FILE" ]]; then
      step "Windows cannot check certificate revocation here; retrying with ${CURL_SCHANNEL_FALLBACK_FLAG}" >&2
      : >"$CURL_FALLBACK_NOTED_FILE"
    fi
    status=0
    curl --show-error "$CURL_SCHANNEL_FALLBACK_FLAG" "$@" 2>"$CURL_ERROR_FILE" || status=$?
    if ((status == 0)); then
      : >"$CURL_ERROR_FILE"
      return 0
    fi
  fi

  return "$status"
}

# curl_reason is what curl last complained about, on one line so it can be read
# inside a sentence, or nothing at all when the last call succeeded.
curl_reason() {
  [[ -n "$CURL_ERROR_FILE" && -s "$CURL_ERROR_FILE" ]] || return 0
  tr '\n' ' ' <"$CURL_ERROR_FILE" | sed 's/  */ /g; s/ *$//'
}

is_semver() {
  [[ $1 =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]
}

# latest_version follows the releases/latest redirect and reads the tag off the
# URL it lands on. That is a plain redirect rather than an API call, so it is not
# rate limited and needs no token; the API is the fallback for when GitHub
# changes the redirect.
latest_version() {
  local url version api_json

  if url=$(curl_run -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest"); then
    version="${url##*/}"
    version="${version#v}"
    if is_semver "$version"; then
      echo "$version"
      return 0
    fi
  fi

  if api_json=$(curl_run -fsSL -H 'Accept: application/vnd.github+json' \
    -H 'User-Agent: krowk-installer' \
    "https://api.github.com/repos/${REPO}/releases/latest"); then
    if [[ $api_json =~ \"tag_name\"[[:space:]]*:[[:space:]]*\"v?([^\"]+)\" ]]; then
      version="${BASH_REMATCH[1]}"
      if is_semver "$version"; then
        echo "$version"
        return 0
      fi
    fi
  fi

  local why
  why=$(curl_reason)
  error "Could not find the latest release of ${REPO}. ${why:+curl said: ${why}. }Name one with KROWK_VERSION, or install from source: go install github.com/${REPO}/cmd/krowk@latest"
}

release_base_url() {
  if [[ -n "$BASE_URL_OVERRIDE" ]]; then
    echo "${BASE_URL_OVERRIDE%/}"
  else
    echo "https://github.com/${REPO}/releases/download/v$1"
  fi
}

# verify_checksum is not best-effort. These bytes are about to be made executable
# and put on PATH, so a checksums.txt that will not download, does not name the
# archive, or names a different digest all end the install — there is no path
# through this function that installs an unverified binary.
verify_checksum() {
  local base_url="$1" tmp_dir="$2" archive="$3"
  local expected actual why

  step "Verifying the download"

  if ! curl_run -fsSL "${base_url}/checksums.txt" -o "${tmp_dir}/checksums.txt"; then
    why=$(curl_reason)
    error "checksums.txt would not download from ${base_url}${why:+ (${why})}. Nothing is installed: an archive nobody can check is not one to run."
  fi

  # GoReleaser writes `<digest>  <name>`; a binary-mode digest writes `*<name>`.
  expected=$(awk -v f="$archive" '$2 == f || $2 == ("*" f) {print $1; exit}' "${tmp_dir}/checksums.txt")
  if [[ -z "$expected" ]]; then
    error "checksums.txt does not mention ${archive}, so there is nothing to check it against. Report this at https://github.com/${REPO}/issues"
  fi

  actual=$(cd "$tmp_dir" && "${SHA256_CMD[@]}" "$archive" | awk '{print $1}')

  if [[ "$expected" != "$actual" ]]; then
    error "${archive} is not the file the release signed for (expected ${expected}, got ${actual}). Nothing is installed. Retry, and if it happens again report it at https://github.com/${REPO}/issues"
  fi
  info "Checksum matches"
}

# download_binaries fetches one archive and installs both binaries out of it.
download_binaries() {
  local version="$1" platform="$2" tmp_dir="$3"
  local ext="tar.gz" archive base_url why suffix=""

  if [[ "$platform" == windows_* ]]; then
    ext="zip"
    suffix=".exe"
  fi

  # The name .goreleaser.yaml builds: krowk_<version>_<os>_<arch>.<ext>.
  archive="krowk_${version}_${platform}.${ext}"
  base_url=$(release_base_url "$version")

  step "Downloading krowk ${version} for ${platform//_/ }"
  if ! curl_run -fsSL "${base_url}/${archive}" -o "${tmp_dir}/${archive}"; then
    why=$(curl_reason)
    error "Could not download ${base_url}/${archive}${why:+ (${why})}. Check that ${version} is a released version: https://github.com/${REPO}/releases"
  fi

  verify_checksum "$base_url" "$tmp_dir" "$archive"

  if [[ "$ext" == "zip" ]]; then
    command -v unzip >/dev/null 2>&1 || error "unzip is needed to open ${archive} and is not installed"
    unzip -q "${tmp_dir}/${archive}" -d "$tmp_dir"
  else
    tar -xzf "${tmp_dir}/${archive}" -C "$tmp_dir"
  fi

  mkdir -p "$BIN_DIR"
  local name
  for name in "${BINARIES[@]}"; do
    if [[ ! -f "${tmp_dir}/${name}${suffix}" ]]; then
      error "${archive} does not contain ${name}${suffix}. Report this at https://github.com/${REPO}/issues"
    fi
    # Move then chmod, so the file is never briefly executable under a name
    # something else might pick up.
    mv -f "${tmp_dir}/${name}${suffix}" "${BIN_DIR}/${name}${suffix}"
    chmod +x "${BIN_DIR}/${name}${suffix}"
    info "Installed ${BIN_DIR}/${name}${suffix}"
  done
}

# setup_path only writes to a shell profile when it has to. If the chosen
# directory is already on PATH — which default_bin_dir tries hard to arrange —
# this script leaves the user's dotfiles alone.
setup_path() {
  if path_contains_dir "$BIN_DIR"; then
    return 0
  fi

  local shell_rc
  case "${SHELL:-}" in
    */zsh)  shell_rc="$HOME/.zshrc" ;;
    */bash) shell_rc="$HOME/.bashrc" ;;
    *)      shell_rc="$HOME/.profile" ;;
  esac

  # The profile's directory is $HOME, which normally exists — but an installer
  # that aborts here has already put working binaries on disk, and failing after
  # the work is done is the worst place to fail.
  mkdir -p "$(dirname "$shell_rc")"

  # PATH itself has already answered "is this directory reachable?" above. All
  # the profile can answer is the narrower question of whether this installer has
  # written this line before, so that is the only thing looked for: the whole
  # line, exactly. Searching the file for the directory name instead would count
  # a comment, an alias, or a longer path that merely starts the same way, and
  # then skip the export while PATH stays broken.
  local export_line="export PATH=\"$BIN_DIR:\$PATH\""

  if [[ -f "$shell_rc" ]] && grep -qxF "$export_line" "$shell_rc" 2>/dev/null; then
    info "$BIN_DIR is already exported in $shell_rc"
    note "This shell has not read that yet: source $shell_rc"
    return 0
  fi

  {
    echo ""
    echo "# Added by the krowk installer"
    echo "$export_line"
  } >>"$shell_rc"
  info "Added $BIN_DIR to PATH in $shell_rc"
  note "This shell has not read that yet: source $shell_rc"
}

# verify_install runs the thing that was just installed. A binary that landed but
# will not execute is the failure worth catching here — the wrong architecture,
# or Windows refusing an unsigned executable.
verify_install() {
  local platform="$1" suffix="" installed err_file
  [[ "$platform" == windows_* ]] && suffix=".exe"

  err_file=$(mktemp "${TMPDIR:-/tmp}/krowk-verify.XXXXXX")
  if installed=$("${BIN_DIR}/krowk${suffix}" --version 2>"$err_file"); then
    rm -f "$err_file"
    info "krowk ${installed} works"
    return 0
  fi

  local why
  why=$(<"$err_file")
  rm -f "$err_file"

  local detail="krowk was installed to ${BIN_DIR} but will not run"
  [[ -n "$why" ]] && detail="${detail}: ${why}"
  if [[ "$platform" == windows_* ]]; then
    detail="$detail
    Windows may have blocked it: Smart App Control runs code-signed binaries only.
    Installing inside WSL2 avoids that."
  fi
  error "$detail"
}

# skills_dir is where an agent looks for skills on this machine, or empty when
# nothing here uses them. CLAUDE_CONFIG_DIR wins, since a user who moved their
# config has said where it lives.
skills_dir() {
  local config="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
  if [[ -d "${config}/skills" ]]; then
    echo "${config}/skills"
  fi
}

# install_skill is best-effort on purpose. krowk works without it; the skill only
# teaches an agent which command to reach for. So a machine with no agent config
# gets a sentence saying where the skill lives, not an error and not a directory
# created speculatively under someone's home.
install_skill() {
  local version="$1" dir url tmp

  if [[ "${KROWK_SKIP_SKILL:-}" == "1" ]]; then
    step "Skipping the agent skill (KROWK_SKIP_SKILL=1)"
    return 0
  fi

  dir=$(skills_dir)
  if [[ -z "$dir" ]]; then
    note "No agent skills directory here, so none was written."
    note "For Claude Code: mkdir -p ~/.claude/skills and re-run, or copy"
    note "https://github.com/${REPO}/blob/main/skills/krowk/SKILL.md yourself."
    return 0
  fi

  if [[ -n "$BASE_URL_OVERRIDE" ]]; then
    url="${BASE_URL_OVERRIDE%/}/SKILL.md"
  else
    # From the tag that was installed, so the skill describes the binary that is
    # on this machine rather than whatever main says today.
    url="https://raw.githubusercontent.com/${REPO}/v${version}/skills/krowk/SKILL.md"
  fi

  tmp=$(mktemp "${TMPDIR:-/tmp}/krowk-skill.XXXXXX")
  if ! curl_run -fsSL "$url" -o "$tmp"; then
    rm -f "$tmp"
    note "Could not fetch the agent skill from ${url} — krowk itself is installed and working."
    return 0
  fi

  mkdir -p "${dir}/krowk"
  mv -f "$tmp" "${dir}/krowk/SKILL.md"
  chmod 0644 "${dir}/krowk/SKILL.md"
  info "Agent skill written to ${dir}/krowk/SKILL.md"
}

next_steps() {
  echo ""
  echo "  Next:"
  echo "    $(bold "krowk push screenshot.png")     Upload without a key — the link is live, and lasts a day"
  echo "    $(bold "krowk auth login --token …")    Add a key, and uploads keep, group under runs and stay yours"
  echo "    $(bold "krowk help")                    Everything else — add --json for the surface as data"
  echo ""
}

main() {
  echo ""
  echo "  $(bold "krowk") — permalinks for agent output"
  echo ""

  command -v curl >/dev/null 2>&1 || error "curl is needed and is not installed"
  # Before the network, not after it: a machine with no way to check a checksum
  # should hear that instead of downloading an archive it cannot verify.
  resolve_sha256

  local platform version tmp_dir
  platform=$(detect_platform)
  detect_curl_fallback

  # One directory for everything this run writes, removed on the way out. curl's
  # complaints live here too, because curl_run is called from inside command
  # substitutions and a file is the only place a reason survives one.
  tmp_dir=$(mktemp -d)
  # shellcheck disable=SC2064  # expand tmp_dir now: the trap must name this run's directory.
  trap "rm -rf '${tmp_dir}'" EXIT
  CURL_ERROR_FILE="${tmp_dir}/curl.err"
  CURL_FALLBACK_NOTED_FILE="${tmp_dir}/curl.noted"
  : >"$CURL_ERROR_FILE"

  if [[ -z "$BIN_DIR" ]]; then
    BIN_DIR=$(default_bin_dir "$platform")
  fi

  if [[ -n "$VERSION" ]]; then
    version="${VERSION#v}"
    is_semver "$version" || error "KROWK_VERSION=${VERSION} is not a version. It looks like 0.1.0, or 0.1.0-rc.1."
  elif [[ -n "$BASE_URL_OVERRIDE" ]]; then
    error "KROWK_INSTALL_BASE_URL has no release to ask for the latest version. Set KROWK_VERSION too."
  else
    version=$(latest_version)
  fi

  download_binaries "$version" "$platform" "$tmp_dir"
  setup_path
  verify_install "$platform"
  install_skill "$version"
  next_steps
}

# Sourcing this file — which scripts/install_test.sh does, to reach the helpers
# without installing anything — must not run the installer. The if-form is
# required: `[[ … ]] && main` returns 1 when sourced and trips the sourcing
# shell's set -e. So is `:-$0`: bash reading from stdin, which is exactly what
# `curl | bash` is, leaves BASH_SOURCE unset and set -u would abort on it.
if [[ "${BASH_SOURCE[0]:-$0}" == "$0" ]]; then
  main "$@"
fi
