#!/bin/sh
# bot-agent installer. Run from a release bundle as root. No downloads.
# Test-only: --root DIR is refused unless BOT_AGENT_INSTALLER_TEST=1,
# which also skips the uid 0 check and then requires --root.
# Outside test mode PATH is fixed. Test mode keeps PATH so stubs work.
# umask 022 so a new directory is at most 0755 before a tighter chmod.
# xtrace stays off: a join token must not be written to a trace.
set -eu
set +x
if [ "${BOT_AGENT_INSTALLER_TEST:-}" != "1" ]; then
  PATH=/usr/sbin:/usr/bin:/sbin:/bin
  export PATH
fi
umask 022

STAGE=
STTY_SAVED=
# Set after upgrade or rollback has a complete .prev set to restore.
# Cleared after verification. The EXIT trap rolls back while this is 1.
ROLLBACK_ARMED=0
ROLLBACK_KIND=upgrade
REMOVE_INCOMPLETE=0
# Set when this command enabled the runner socket and verification must see it active.
SOCKET_VERIFY=0
SOCKET_UNIT=bot-agent-runner.socket
SOCKET_PATH=/etc/systemd/system/bot-agent-runner.socket
# enable, disable, or none. Set before a rollback restore so disable can run
# while an added unit file is still on disk.
RB_SOCKET_ACT=none

# stty -g prints a list of arguments. They must be split, not passed as one word.
restore_stty() {
  if [ -z "$STTY_SAVED" ]; then
    return 0
  fi
  # shellcheck disable=SC2086
  stty $STTY_SAVED || true
  STTY_SAVED=
}

cleanup() {
  _exit_rc=$?
  trap - EXIT INT TERM
  restore_stty
  if [ "$ROLLBACK_ARMED" = "1" ] && [ "$_exit_rc" != "0" ]; then
    ROLLBACK_ARMED=0
    rollback_armed_now "$ROLLBACK_KIND" || true
    if [ "$_exit_rc" != "130" ] && [ "$_exit_rc" != "143" ]; then
      _exit_rc=1
    fi
  fi
  if [ -n "$STAGE" ]; then
    case "$STAGE" in
      /|/tmp|/var/tmp) ;;
      *bot-agent-inst.*) rm -rf "$STAGE" ;;
    esac
    STAGE=
  fi
  exit "$_exit_rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

usage() {
  cat >&2 <<'EOF'
usage: install.sh check|install|enroll|upgrade|rollback|remove [flags]
  check [--runner] [--runner-config FILE | --project SLUG --image PATTERN... [--enable]]
  install [--runner] [--runner-config FILE | --project SLUG --image PATTERN... [--enable]] [--insecure-dev]
  enroll --server URL [--runner] [--insecure-dev]
  upgrade [--force] [--runner] [--insecure-dev]
  rollback
  remove [--purge] [--remove-workloads]
  --project SLUG, --image PATTERN (1-20), --enable: need --runner; write
              /etc/bot-agent/runner.toml for these images in this project
  --dry-run   print every file to write or delete; change nothing
  --root DIR  test-only prefix for every filesystem path
EOF
  exit 2
}

say() { printf '%s\n' "$*"; }

fail() {
  printf '%s\n' "$*" >&2
  exit 1
}

precheck_fail() {
  printf '%s\n' "$*" >&2
  if [ "${MUTATED:-0}" != "1" ]; then
    say "nothing changed"
  fi
  exit 1
}

DRY=0
ROOT=
RUNNER=0
FORCE=0
PURGE=0
REMOVE_WL=0
INSECURE=0
SERVER=
RUNNER_CONFIG=
RUNNER_CONFIG_ORIG=
RUNNER_CONFIG_LABEL=--runner-config
PROJECT=
IMAGES=
IMAGE_N=0
ENABLE=0
GEN_POLICY=0
LAST_POLICY_CHECK=
MUTATED=0
WANT_RUNNER=0
VER=
ARCH=
AGENT_SRC=
RUNNER_SRC=
BUNDLE=
SETTLE=${BOT_AGENT_INSTALLER_SETTLE:-15}

case "$SETTLE" in
  ''|*[!0-9]*) fail "BOT_AGENT_INSTALLER_SETTLE must be a non-negative integer" ;;
esac

# How long enroll waits for the credential keys. Outside test mode this
# stays 30s even if the variable is set.
ENROLL_WAIT=30
if [ "${BOT_AGENT_INSTALLER_TEST:-}" = "1" ] && [ -n "${BOT_AGENT_ENROLL_WAIT:-}" ]; then
  case "$BOT_AGENT_ENROLL_WAIT" in
    *[!0-9]*) fail "BOT_AGENT_ENROLL_WAIT must be a non-negative integer" ;;
  esac
  ENROLL_WAIT=$BOT_AGENT_ENROLL_WAIT
fi

# Policy values are checked here, before any file is written, so nothing
# outside this grammar can reach the generated runner.toml. Explicit letter
# lists, because a range like a-z can match uppercase in some locales.
valid_slug() {
  case "$1" in
    ''|[!abcdefghijklmnopqrstuvwxyz0123456789]*|*[!abcdefghijklmnopqrstuvwxyz0123456789-]*) return 1 ;;
  esac
  [ "${#1}" -le 64 ]
}

valid_image_pattern() {
  case "$1" in
    ''|-*|*[!abcdefghijklmnopqrstuvwxyz0123456789._:/*-]*) return 1 ;;
  esac
  case "$1" in
    */*) ;;
    *) return 1 ;;
  esac
  [ "${#1}" -le 255 ]
}

if [ "$#" -lt 1 ]; then
  usage
fi
CMD=$1
shift
case "$CMD" in
  check|install|enroll|upgrade|rollback|remove) ;;
  *) usage ;;
esac

while [ "$#" -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1; shift ;;
    --runner) RUNNER=1; shift ;;
    --force) FORCE=1; shift ;;
    --purge) PURGE=1; shift ;;
    --remove-workloads) REMOVE_WL=1; shift ;;
    --insecure-dev) INSECURE=1; shift ;;
    --root)
      [ "$#" -ge 2 ] || usage
      ROOT=$2
      shift 2
      ;;
    --runner-config)
      [ "$#" -ge 2 ] || usage
      RUNNER_CONFIG=$2
      shift 2
      ;;
    --server)
      [ "$#" -ge 2 ] || usage
      SERVER=$2
      shift 2
      ;;
    --project)
      [ "$#" -ge 2 ] || usage
      [ -z "$PROJECT" ] || usage
      if ! valid_slug "$2"; then
        printf '%s\n' "bad --project value" >&2
        usage
      fi
      PROJECT=$2
      shift 2
      ;;
    --image)
      [ "$#" -ge 2 ] || usage
      if ! valid_image_pattern "$2"; then
        printf '%s\n' "bad --image value" >&2
        usage
      fi
      IMAGE_N=$((IMAGE_N + 1))
      [ "$IMAGE_N" -le 20 ] || usage
      # Patterns have no spaces, so a space-separated list is safe.
      # Expand it only with set -f: a pattern may contain *.
      IMAGES="${IMAGES:+$IMAGES }$2"
      shift 2
      ;;
    --enable) ENABLE=1; shift ;;
    *) usage ;;
  esac
done

# --project, --image, and --enable: check and install only, with --runner,
# not with --runner-config. --project and --image come together; --enable needs both.
if [ -n "$PROJECT" ] || [ -n "$IMAGES" ] || [ "$ENABLE" != "0" ]; then
  case "$CMD" in
    check|install) ;;
    *) usage ;;
  esac
  if [ "$RUNNER" != "1" ] || [ -n "$RUNNER_CONFIG" ]; then
    usage
  fi
  if [ -z "$PROJECT" ] || [ -z "$IMAGES" ]; then
    usage
  fi
fi

case "$CMD" in
  check)
    if [ "$FORCE" != "0" ] || [ "$PURGE" != "0" ] || [ "$REMOVE_WL" != "0" ] || [ "$INSECURE" != "0" ] || [ -n "$SERVER" ]; then
      usage
    fi
    ;;
  install)
    if [ "$FORCE" != "0" ] || [ "$PURGE" != "0" ] || [ "$REMOVE_WL" != "0" ] || [ -n "$SERVER" ]; then
      usage
    fi
    ;;
  enroll)
    if [ "$FORCE" != "0" ] || [ "$PURGE" != "0" ] || [ "$REMOVE_WL" != "0" ] || [ -n "$RUNNER_CONFIG" ]; then
      usage
    fi
    if [ -z "$SERVER" ]; then
      usage
    fi
    ;;
  upgrade)
    if [ "$PURGE" != "0" ] || [ "$REMOVE_WL" != "0" ] || [ -n "$SERVER" ] || [ -n "$RUNNER_CONFIG" ]; then
      usage
    fi
    ;;
  rollback)
    if [ "$RUNNER" != "0" ] || [ "$FORCE" != "0" ] || [ "$PURGE" != "0" ] || [ "$REMOVE_WL" != "0" ] || [ "$INSECURE" != "0" ] || [ -n "$SERVER" ] || [ -n "$RUNNER_CONFIG" ]; then
      usage
    fi
    ;;
  remove)
    if [ "$RUNNER" != "0" ] || [ "$FORCE" != "0" ] || [ "$INSECURE" != "0" ] || [ -n "$SERVER" ] || [ -n "$RUNNER_CONFIG" ]; then
      usage
    fi
    ;;
esac

if [ -n "$ROOT" ] && [ "${BOT_AGENT_INSTALLER_TEST:-}" != "1" ]; then
  printf '%s\n' "--root is test-only" >&2
  exit 2
fi
if [ "${BOT_AGENT_INSTALLER_TEST:-}" = "1" ] && [ -z "$ROOT" ]; then
  fail "test mode requires --root"
fi
if [ "${BOT_AGENT_INSTALLER_TEST:-}" != "1" ]; then
  _uid=$(id -u) || fail "id -u failed"
  if [ "$_uid" != "0" ]; then
    fail "must run as root"
  fi
fi
if [ -n "$ROOT" ]; then
  case "$ROOT" in
    /*) ;;
    *) fail "--root must be absolute" ;;
  esac
  ROOT=$(CDPATH='' cd -P -- "$ROOT" && pwd) || fail "--root is not a directory"
  if [ "$ROOT" = "/" ]; then
    fail "--root must not be /"
  fi
fi

case "$SERVER" in
  *[![:print:]]*) usage ;;
esac
if [ -n "$RUNNER_CONFIG" ]; then
  case "$RUNNER_CONFIG" in
    *[![:print:]]*) usage ;;
  esac
fi

hostpath() {
  printf '%s%s\n' "$ROOT" "$1"
}

hash_file() {
  _hf=$1
  if command -v sha256sum >/dev/null 2>&1; then
    _hout=$(sha256sum "$_hf") || return 1
  else
    _hout=$(shasum -a 256 "$_hf") || return 1
  fi
  printf '%s\n' "$_hout" | awk '{print $1}'
}

# Permission bits, including setuid, setgid, and sticky.
# Linux stat -c %a prints those bits. macOS stat -f %p prints the whole
# st_mode, so keep the last four octal digits.
file_mode() {
  if _m=$(stat -c %a "$1" 2>/dev/null); then
    :
  else
    _m=$(stat -f %p "$1" 2>/dev/null) || return 1
  fi
  printf '%s\n' "$_m" | awk '{
    s=$1
    n=length(s)
    if (n>4) s=substr(s, n-3)
    print s+0
  }'
}

file_uid() {
  _u=$(stat -c %u "$1" 2>/dev/null) || _u=$(stat -f %u "$1" 2>/dev/null) || return 1
  case "$_u" in
    ''|*[!0-9]*) return 1 ;;
  esac
  printf '%s\n' "$_u"
}

# 0 when the numeric mode has the group or other write bit.
mode_group_or_other_writable() {
  printf '%s\n' "$1" | awk '{
    n=$1+0
    o=n%10
    g=int(n/10)%10
    bad=0
    if (o==2 || o==3 || o==6 || o==7) bad=1
    if (g==2 || g==3 || g==6 || g==7) bad=1
    if (bad) exit 0
    exit 1
  }'
}

require_mode() {
  _got=$(file_mode "$1") || fail "cannot stat $1"
  _want=$(printf '%s\n' "$2" | awk '{print $1+0}')
  if [ "$_got" != "$_want" ]; then
    fail "mode of $1 is ${_got}, want ${_want}"
  fi
}

ORIG_BUNDLE=$(CDPATH='' cd -- "$(dirname "$0")" && pwd -P) || fail "cannot locate bundle"
case "$ORIG_BUNDLE" in
  *[[:space:]]*) precheck_fail "bundle path contains whitespace" ;;
esac

# The bundle path is printed in the enroll command for people and agents to
# paste into a shell, so it may hold only these characters. No ranges: a
# range can match more than ASCII in some locales.
safe_bundle_path() {
  case "$1" in
    ''|*[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._/+-]*) return 1 ;;
  esac
  return 0
}

if [ "${BOT_AGENT_INSTALL_REEXEC:-}" != "1" ] && ! safe_bundle_path "$ORIG_BUNDLE"; then
  precheck_fail "bundle path has characters that are unsafe to paste into a shell; move the bundle to a plain path"
fi
BUNDLE=$ORIG_BUNDLE
# The operator's bundle directory, for the printed enroll command. The staged
# copy gets it from the first run, because its own directory is the stage.
INSTALL_BUNDLE=$ORIG_BUNDLE

# sum_path prints the relative path from one SHA256SUMS line, or nothing.
sum_path() {
  _line=$1
  case "$_line" in
    ''|\#*) return 0 ;;
  esac
  _h=$(printf '%s\n' "$_line" | awk '{print $1}')
  _p=$(printf '%s\n' "$_line" | awk '{print $2}')
  if ! printf '%s\n' "$_h" | awk 'length($0)==64 && $0 ~ /^[0-9a-f]+$/ {ok=1} END{exit ok?0:1}'; then
    return 1
  fi
  case "$_p" in
    \**) _p=${_p#\*} ;;
  esac
  case "$_p" in
    ''|/*|*..*) return 1 ;;
  esac
  printf '%s\n' "$_p"
}

verify_sums() {
  _sums=$BUNDLE/SHA256SUMS
  if [ ! -f "$_sums" ]; then
    precheck_fail "SHA256SUMS missing"
  fi
  _saw_install=0
  while IFS= read -r _line || [ -n "$_line" ]; do
    case "$_line" in
      ''|\#*) continue ;;
    esac
    _p=$(sum_path "$_line") || precheck_fail "bad SHA256SUMS line"
    if [ -z "$_p" ]; then
      precheck_fail "bad SHA256SUMS line"
    fi
    case "$_p" in
      install.sh) _saw_install=1 ;;
    esac
    if [ ! -f "$BUNDLE/$_p" ]; then
      precheck_fail "bundle file missing: ${_p}"
    fi
    _want=$(printf '%s\n' "$_line" | awk '{print $1}')
    _got=$(hash_file "$BUNDLE/$_p") || precheck_fail "cannot hash ${_p}"
    if [ "$_got" != "$_want" ]; then
      precheck_fail "sha256 mismatch: ${_p}"
    fi
  done < "$_sums"
  if [ "$_saw_install" != "1" ]; then
    precheck_fail "SHA256SUMS does not list install.sh"
  fi
}

# Copy every listed file into a private directory and only use that copy.
# A symlink, ".." segment, or absolute path is refused before any install.
stage_bundle() {
  _sums=$ORIG_BUNDLE/SHA256SUMS
  if [ -h "$_sums" ] || [ ! -f "$_sums" ]; then
    precheck_fail "SHA256SUMS missing"
  fi
  STAGE=$(mktemp -d "${TMPDIR:-/tmp}/bot-agent-inst.XXXXXX") || precheck_fail "mktemp failed"
  chmod 0700 "$STAGE" || precheck_fail "cannot chmod stage"
  if [ "${BOT_AGENT_INSTALLER_TEST:-}" != "1" ]; then
    chown root:root "$STAGE" || precheck_fail "cannot chown stage"
  fi
  cp -P "$_sums" "$STAGE/SHA256SUMS" || precheck_fail "cannot copy SHA256SUMS"
  if [ -h "$STAGE/SHA256SUMS" ] || [ ! -f "$STAGE/SHA256SUMS" ]; then
    precheck_fail "SHA256SUMS missing"
  fi
  while IFS= read -r _line || [ -n "$_line" ]; do
    _p=$(sum_path "$_line") || precheck_fail "bad SHA256SUMS line"
    if [ -z "$_p" ]; then
      continue
    fi
    if [ -h "$ORIG_BUNDLE/$_p" ]; then
      precheck_fail "bundle symlink: ${_p}"
    fi
    if [ ! -f "$ORIG_BUNDLE/$_p" ]; then
      precheck_fail "bundle file missing: ${_p}"
    fi
    _sd=$(dirname "$STAGE/$_p")
    mkdir -p "$_sd" || precheck_fail "cannot stage ${_p}"
    cp -P "$ORIG_BUNDLE/$_p" "$STAGE/$_p" || precheck_fail "cannot copy ${_p}"
    if [ -h "$STAGE/$_p" ] || [ ! -f "$STAGE/$_p" ]; then
      precheck_fail "bundle file is not regular: ${_p}"
    fi
    case "$_p" in
      install.sh|bot-agent-v*|bot-agent-runner-v*)
        chmod 0755 "$STAGE/$_p" || precheck_fail "cannot chmod ${_p}"
        ;;
    esac
  done < "$STAGE/SHA256SUMS"
  BUNDLE=$STAGE
}

# Snapshot --runner-config into the private stage before any policy check.
# Later steps install this copy, not the operator path.
stage_runner_config() {
  if [ -z "$RUNNER_CONFIG" ]; then
    return 0
  fi
  if [ -h "$RUNNER_CONFIG" ] || [ ! -f "$RUNNER_CONFIG" ]; then
    precheck_fail "--runner-config is not a regular file"
  fi
  _rcmode=$(file_mode "$RUNNER_CONFIG") || precheck_fail "cannot stat --runner-config"
  if mode_group_or_other_writable "$_rcmode"; then
    precheck_fail "--runner-config is writable by group or other"
  fi
  cp -P "$RUNNER_CONFIG" "$STAGE/runner-config.toml" || precheck_fail "cannot stage --runner-config"
  if [ -h "$STAGE/runner-config.toml" ] || [ ! -f "$STAGE/runner-config.toml" ]; then
    precheck_fail "--runner-config is not a regular file"
  fi
  _rcmode=$(file_mode "$STAGE/runner-config.toml") || precheck_fail "cannot stat --runner-config"
  if mode_group_or_other_writable "$_rcmode"; then
    precheck_fail "--runner-config is writable by group or other"
  fi
  chmod 0644 "$STAGE/runner-config.toml" || precheck_fail "cannot chmod staged --runner-config"
  RUNNER_CONFIG=$STAGE/runner-config.toml
}

# The shell may read this script in chunks. After the hash check, keep running
# the staged copy so a later rewrite of the original path is not parsed.
# Arguments are rebuilt from the flags already accepted. The guard stops a second exec.
exec_staged_installer() {
  if [ "${BOT_AGENT_INSTALL_REEXEC:-}" = "1" ]; then
    return 0
  fi
  if [ "$0" = "$STAGE/install.sh" ]; then
    return 0
  fi
  BOT_AGENT_INSTALL_REEXEC=1
  export BOT_AGENT_INSTALL_REEXEC
  set -- "$CMD"
  if [ "$DRY" = "1" ]; then
    set -- "$@" --dry-run
  fi
  if [ "$RUNNER" = "1" ]; then
    set -- "$@" --runner
  fi
  if [ "$FORCE" = "1" ]; then
    set -- "$@" --force
  fi
  if [ "$PURGE" = "1" ]; then
    set -- "$@" --purge
  fi
  if [ "$REMOVE_WL" = "1" ]; then
    set -- "$@" --remove-workloads
  fi
  if [ "$INSECURE" = "1" ]; then
    set -- "$@" --insecure-dev
  fi
  if [ -n "$ROOT" ]; then
    set -- "$@" --root "$ROOT"
  fi
  if [ -n "$RUNNER_CONFIG_ORIG" ]; then
    set -- "$@" --runner-config "$RUNNER_CONFIG_ORIG"
  fi
  if [ -n "$SERVER" ]; then
    set -- "$@" --server "$SERVER"
  fi
  if [ -n "$PROJECT" ]; then
    set -- "$@" --project "$PROJECT"
  fi
  set -f
  for _img in $IMAGES; do
    set -- "$@" --image "$_img"
  done
  set +f
  if [ "$ENABLE" = "1" ]; then
    set -- "$@" --enable
  fi
  BOT_AGENT_INSTALL_BUNDLE=$INSTALL_BUNDLE
  export BOT_AGENT_INSTALL_BUNDLE
  # shellcheck disable=SC2093 # exec replaces this process; the next line runs only if it fails
  exec "$STAGE/install.sh" "$@"
  precheck_fail "cannot re-exec staged install.sh"
}

sum_listed() {
  _want=$1
  _found=0
  while IFS= read -r _line || [ -n "$_line" ]; do
    _p=$(sum_path "$_line") || return 1
    if [ "$_p" = "$_want" ]; then
      _found=1
      break
    fi
  done < "$BUNDLE/SHA256SUMS"
  [ "$_found" = "1" ]
}

RUNNER_CONFIG_ORIG=$RUNNER_CONFIG
if [ "${BOT_AGENT_INSTALL_REEXEC:-}" = "1" ]; then
  STAGE=$ORIG_BUNDLE
  case "$STAGE" in
    *bot-agent-inst.*) ;;
    *) precheck_fail "refusing staged re-exec" ;;
  esac
  BUNDLE=$STAGE
  INSTALL_BUNDLE=${BOT_AGENT_INSTALL_BUNDLE:-}
  case "$INSTALL_BUNDLE" in
    /*) ;;
    *) precheck_fail "refusing staged re-exec" ;;
  esac
  if ! safe_bundle_path "$INSTALL_BUNDLE"; then
    precheck_fail "refusing staged re-exec"
  fi
  # The printed path must be the bundle this stage was copied from.
  if [ ! -f "$INSTALL_BUNDLE/install.sh" ] || ! cmp -s "$INSTALL_BUNDLE/install.sh" "$STAGE/install.sh"; then
    precheck_fail "refusing staged re-exec"
  fi
else
  stage_bundle
fi
stage_runner_config
verify_sums
exec_staged_installer

# Write the policy from --project and --image into the stage. From here it
# takes the --runner-config path: policy check, then install as root:root 0644.
stage_generated_policy() {
  if [ -z "$PROJECT" ]; then
    return 0
  fi
  _gp=$STAGE/generated-runner.toml
  _gimgs=
  set -f
  for _img in $IMAGES; do
    _gimgs="${_gimgs:+$_gimgs, }\"${_img}\""
  done
  set +f
  _genabled=false
  if [ "$ENABLE" = "1" ]; then
    _genabled=true
  fi
  {
    printf '%s\n' "# Written by install.sh from --project and --image. Edit by hand or re-run install with new flags."
    printf 'enabled = %s\n' "$_genabled"
    printf 'projects = ["%s"]\n' "$PROJECT"
    printf 'images = [%s]\n' "$_gimgs"
  } > "$_gp" || precheck_fail "cannot write generated policy"
  chmod 0644 "$_gp" || precheck_fail "cannot chmod generated policy"
  RUNNER_CONFIG=$_gp
  RUNNER_CONFIG_LABEL="generated policy"
  GEN_POLICY=1
}
stage_generated_policy
if [ "${BOT_AGENT_INSTALLER_TEST:-}" = "1" ] && [ -n "${BOT_AGENT_STUB_DIR:-}" ]; then
  printf '%s\n' "$0" > "$BOT_AGENT_STUB_DIR/installer-argv0"
fi

find_bins() {
  AGENT_SRC=
  for _f in "$BUNDLE"/bot-agent-v*-linux-"$ARCH"; do
    [ -f "$_f" ] || continue
    _b=$(basename "$_f")
    case "$_b" in
      bot-agent-runner-*) continue ;;
    esac
    if [ -n "$AGENT_SRC" ]; then
      precheck_fail "multiple agent binaries for ${ARCH}"
    fi
    AGENT_SRC=$_f
  done
  if [ -z "$AGENT_SRC" ]; then
    precheck_fail "no agent binary for ${ARCH}"
  fi
  if [ ! -x "$AGENT_SRC" ]; then
    precheck_fail "agent binary is not executable"
  fi
  VER=$("$AGENT_SRC" version) || precheck_fail "cannot run agent version"
  case "$VER" in
    ''|*[!0-9.]*) precheck_fail "bad agent version" ;;
  esac
  _base=$(basename "$AGENT_SRC")
  if [ "$_base" != "bot-agent-v${VER}-linux-${ARCH}" ]; then
    precheck_fail "agent binary name does not match version"
  fi
  sum_listed "$_base" || precheck_fail "SHA256SUMS does not list ${_base}"
  say "bundle: ${VER}"

  RUNNER_SRC=
  for _f in "$BUNDLE"/bot-agent-runner-v*-linux-"$ARCH"; do
    [ -f "$_f" ] || continue
    if [ -n "$RUNNER_SRC" ]; then
      precheck_fail "multiple runner binaries for ${ARCH}"
    fi
    RUNNER_SRC=$_f
  done
  if [ -n "$RUNNER_SRC" ]; then
    _rb=$(basename "$RUNNER_SRC")
    if [ "$_rb" != "bot-agent-runner-v${VER}-linux-${ARCH}" ]; then
      precheck_fail "runner binary name does not match version"
    fi
    sum_listed "$_rb" || precheck_fail "SHA256SUMS does not list ${_rb}"
  fi
}

mach=$(uname -m) || fail "uname failed"
case "$mach" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) precheck_fail "unsupported arch ${mach}" ;;
esac
say "arch: ${ARCH}"

if ! command -v systemctl >/dev/null 2>&1; then
  precheck_fail "systemd: absent"
fi
if ! systemctl --version >/dev/null 2>&1; then
  precheck_fail "systemd: absent"
fi
say "systemd: present"

find_bins

STATE=$(hostpath /var/lib/bot-agent-installer)
RECORD=$STATE/installed
PREV=$STATE/prev/record

record_version() {
  _rf=$1
  if [ ! -f "$_rf" ]; then
    return 0
  fi
  awk '$1=="version"{print $2; exit}' "$_rf"
}

record_hash() {
  _rf=$1
  _path=$2
  if [ ! -f "$_rf" ]; then
    return 0
  fi
  awk -v p="$_path" 'length($1)==64 && $1 ~ /^[0-9a-f]+$/ && $2==p {print $1; exit}' "$_rf"
}

installed_ver=$(record_version "$RECORD")
# No installer record. check and upgrade report `bot-agent version` instead of "none".
HAND_VER=
if [ -z "$installed_ver" ] && [ ! -f "$RECORD" ]; then
  if [ "$CMD" = "check" ] || [ "$CMD" = "upgrade" ]; then
    _hbin=$(hostpath /usr/local/bin/bot-agent)
    # Do not execute an untrusted binary as root. Test mode skips only the
    # uid 0 check, because the fake root is not owned by root. A group or
    # other write bit still means there is no version.
    _htrust=0
    if [ -f "$_hbin" ] && [ ! -h "$_hbin" ] && [ -x "$_hbin" ]; then
      _hmode=$(file_mode "$_hbin") || _hmode=
      if [ -n "$_hmode" ] && ! mode_group_or_other_writable "$_hmode"; then
        if [ "${BOT_AGENT_INSTALLER_TEST:-}" = "1" ]; then
          _htrust=1
        else
          _huid=$(file_uid "$_hbin") || _huid=
          if [ "$_huid" = "0" ]; then
            _htrust=1
          fi
        fi
      fi
    fi
    if [ "$_htrust" = "1" ]; then
      _hv=$("$_hbin" version 2>/dev/null) || _hv=
      # Trailing newlines are already gone. Reject a second line.
      case "$_hv" in
        *'
'*) _hv= ;;
      esac
      # One token, so the prev record's "version <word>" stays one field.
      case "$_hv" in
        ''|*[!0-9A-Za-z._+-]*|-*) _hv= ;;
      esac
      if [ -n "$_hv" ]; then
        _hlen=$(printf '%s' "$_hv" | awk '{print length($0); exit}')
        if [ "$_hlen" -gt 64 ]; then
          _hv=
        fi
      fi
      if [ -n "$_hv" ]; then
        HAND_VER="$_hv"
        installed_ver=$_hv
      fi
    fi
  fi
fi
if [ -n "$HAND_VER" ]; then
  say "installed: ${installed_ver} (not installed by the installer)"
elif [ -n "$installed_ver" ]; then
  say "installed: ${installed_ver}"
else
  say "installed: none"
fi

if [ -n "$installed_ver" ] && record_hash "$RECORD" /usr/local/libexec/bot-agent-runner >/dev/null; then
  _rh=$(record_hash "$RECORD" /usr/local/libexec/bot-agent-runner)
  if [ -n "$_rh" ]; then
    WANT_RUNNER=1
  fi
fi
if [ "$RUNNER" = "1" ]; then
  WANT_RUNNER=1
fi
if [ "$WANT_RUNNER" = "1" ] && [ -z "$RUNNER_SRC" ]; then
  precheck_fail "bundle has no runner binary for ${ARCH}"
fi

require_unit() {
  for _rel in units/bot-agent.service units/bot-agent-runner.socket units/bot-agent-runner@.service units/tmpfiles-bot-agent-runner.conf; do
    if [ ! -f "$BUNDLE/$_rel" ]; then
      precheck_fail "bundle file missing: ${_rel}"
    fi
    sum_listed "$_rel" || precheck_fail "SHA256SUMS does not list ${_rel}"
  done
}
require_unit

write_disabled_template() {
  cat > "$1" <<'EOF'
# Disabled template installed by packaging/install.sh.
# Replace with a reviewed policy before enabling programs or images.
enabled = false
EOF
}

write_dropin() {
  cat > "$1" <<'EOF'
# Dev only. Written by install.sh --insecure-dev.
[Service]
Environment=BOT_AGENT_INSECURE_DEV=1
EOF
}

# policy_check prints the bot-agent policy check output.
# Returns 0 when the first line is valid.
policy_check() {
  _pc_file=$1
  _pc_rc=0
  _pc_out=$("$AGENT_SRC" policy check "$_pc_file") || _pc_rc=$?
  printf '%s\n' "$_pc_out"
  _first=$(printf '%s\n' "$_pc_out" | awk 'NR==1{print; exit}')
  if [ "$_pc_rc" != "0" ] || [ "$_first" != "valid" ]; then
    return 1
  fi
  return 0
}

policy_patterns() {
  printf '%s\n' "$1" | awk -F= '$1=="image_patterns"{print $2; exit}'
}

note_docker() {
  _patterns=$1
  if [ -z "$_patterns" ] || [ "$_patterns" = "0" ]; then
    say "docker: not checked"
    return 0
  fi
  if command -v docker >/dev/null 2>&1 && docker version >/dev/null 2>&1; then
    say "docker: present"
    return 0
  fi
  say "docker: absent"
  return 0
}

# Optional third argument is a temp path to remove before any exit.
check_policy_file() {
  _pf=$1
  _label=$2
  _cleanup=${3:-}
  if [ ! -f "$_pf" ] || [ -L "$_pf" ]; then
    [ -z "$_cleanup" ] || rm -f "$_cleanup"
    precheck_fail "${_label} is not a regular file"
  fi
  _bad=0
  _pc=$(policy_check "$_pf") || _bad=1
  printf '%s\n' "$_pc"
  LAST_POLICY_CHECK=$_pc
  if [ "$_bad" = "1" ]; then
    [ -z "$_cleanup" ] || rm -f "$_cleanup"
    precheck_fail "policy check failed for ${_label}"
  fi
  note_docker "$(policy_patterns "$_pc")"
  [ -z "$_cleanup" ] || rm -f "$_cleanup"
}

# Mode of an existing runner.toml: not group- or world-writable.
# Owner is checked only outside test mode (tests cannot chown to root).
check_policy_install_mode() {
  _pf=$1
  _mode=$(file_mode "$_pf") || precheck_fail "cannot stat policy"
  if [ "$_mode" != "644" ]; then
    precheck_fail "runner.toml mode must be 644"
  fi
  if [ "${BOT_AGENT_INSTALLER_TEST:-}" != "1" ]; then
    if command -v stat >/dev/null 2>&1; then
      _ou=$(stat -c %u "$_pf" 2>/dev/null) || _ou=$(stat -f %u "$_pf" 2>/dev/null) || _ou=
      _og=$(stat -c %g "$_pf" 2>/dev/null) || _og=$(stat -f %g "$_pf" 2>/dev/null) || _og=
      if [ "$_ou" != "0" ] || [ "$_og" != "0" ]; then
        precheck_fail "runner.toml must be root:root"
      fi
    fi
  fi
}

POLICY_HOST=/etc/bot-agent/runner.toml
POLICY_DEST=$(hostpath "$POLICY_HOST")

# A generated policy never replaces a different runner.toml.
refuse_different_policy() {
  if [ "$GEN_POLICY" != "1" ]; then
    return 0
  fi
  if [ -h "$POLICY_DEST" ]; then
    precheck_fail "refusing symlink ${POLICY_HOST}"
  fi
  if [ -e "$POLICY_DEST" ] && ! cmp -s "$RUNNER_CONFIG" "$POLICY_DEST"; then
    precheck_fail "runner.toml exists and differs; remove it or edit it by hand"
  fi
}

# With --enable, the last policy check must report images=true.
require_images_enabled() {
  if [ "$ENABLE" != "1" ]; then
    return 0
  fi
  if ! printf '%s\n' "$LAST_POLICY_CHECK" | awk '$0=="images=true"{f=1} END{exit f?0:1}'; then
    precheck_fail "policy would not enable images"
  fi
}

# The command to run next, with the absolute bundle path, so nobody has to
# work out where the bundle is.
say_next_enroll() {
  if [ "$WANT_RUNNER" = "1" ]; then
    say "next: sudo ${INSTALL_BUNDLE}/install.sh enroll --server '<server URL>' --runner"
  else
    say "next: sudo ${INSTALL_BUNDLE}/install.sh enroll --server '<server URL>'"
  fi
}

ENV_HOST=/etc/bot-agent/env
ENV_DEST=$(hostpath "$ENV_HOST")

report_installed_files() {
  _any=0
  for _path in \
    /usr/local/bin/bot-agent \
    /etc/systemd/system/bot-agent.service \
    /usr/local/libexec/bot-agent-runner \
    /etc/systemd/system/bot-agent-runner.socket \
    /etc/systemd/system/bot-agent-runner@.service \
    /etc/tmpfiles.d/bot-agent-runner.conf
  do
    _dest=$(hostpath "$_path")
    if [ ! -e "$_dest" ]; then
      say "absent: ${_path}"
      continue
    fi
    _any=1
    _cur=$(hash_file "$_dest") || _cur=
    _rec=$(record_hash "$RECORD" "$_path")
    if [ -z "$installed_ver" ]; then
      say "not installed by the installer: ${_path}"
    elif [ -z "$_rec" ]; then
      say "not installed by the installer: ${_path}"
    elif [ "$_cur" != "$_rec" ]; then
      say "modified: ${_path}"
    else
      say "stock: ${_path}"
    fi
  done
  if [ "$_any" = "0" ] && [ -z "$installed_ver" ]; then
    say "units: none"
  fi
}

do_check() {
  report_installed_files
  if [ -n "$RUNNER_CONFIG" ]; then
    refuse_different_policy
    check_policy_file "$RUNNER_CONFIG" "$RUNNER_CONFIG_LABEL"
    require_images_enabled
  elif [ -f "$POLICY_DEST" ]; then
    check_policy_file "$POLICY_DEST" "$POLICY_HOST"
  elif [ "$WANT_RUNNER" = "1" ]; then
    _tmp=$(mktemp) || precheck_fail "mktemp failed"
    write_disabled_template "$_tmp"
    check_policy_file "$_tmp" "disabled template" "$_tmp"
  else
    say "policy: not checked"
  fi
  if [ -f "$ENV_DEST" ]; then
    say "kept: ${ENV_HOST}"
  fi
  say "summary: check ok"
}

# Paths the installer itself may install. Rollback will not delete any other
# path, even if a prev record names it as added.
managed_install_path() {
  case "$1" in
    /usr/local/bin/bot-agent) return 0 ;;
    /etc/systemd/system/bot-agent.service) return 0 ;;
    /usr/local/libexec/bot-agent-runner) return 0 ;;
    /etc/systemd/system/bot-agent-runner.socket) return 0 ;;
    /etc/systemd/system/bot-agent-runner@.service) return 0 ;;
    /etc/tmpfiles.d/bot-agent-runner.conf) return 0 ;;
    /etc/bot-agent/runner.toml) return 0 ;;
    /etc/systemd/system/bot-agent.service.d/insecure-dev.conf) return 0 ;;
  esac
  return 1
}

# Paths the installer may write. kind is bin, unit, file, policy, or dropin.
each_managed() {
  printf '%s\n' "/usr/local/bin/bot-agent 0755 root:root bin ${BUNDLE}/$(basename "$AGENT_SRC")"
  printf '%s\n' "/etc/systemd/system/bot-agent.service 0644 root:root unit ${BUNDLE}/units/bot-agent.service"
  if [ "$WANT_RUNNER" = "1" ]; then
    printf '%s\n' "/usr/local/libexec/bot-agent-runner 0755 root:root bin ${BUNDLE}/$(basename "$RUNNER_SRC")"
    printf '%s\n' "/etc/systemd/system/bot-agent-runner.socket 0644 root:root unit ${BUNDLE}/units/bot-agent-runner.socket"
    printf '%s\n' "/etc/systemd/system/bot-agent-runner@.service 0644 root:root unit ${BUNDLE}/units/bot-agent-runner@.service"
    printf '%s\n' "/etc/tmpfiles.d/bot-agent-runner.conf 0644 root:root file ${BUNDLE}/units/tmpfiles-bot-agent-runner.conf"
    printf '%s\n' "/etc/bot-agent/runner.toml 0644 root:root policy -"
  fi
  if [ "$INSECURE" = "1" ]; then
    printf '%s\n' "/etc/systemd/system/bot-agent.service.d/insecure-dev.conf 0644 root:root dropin -"
  fi
}

ensure_user() {
  if getent group bot-agent >/dev/null 2>&1; then
    say "group bot-agent: present"
  else
    if [ "$DRY" = "1" ]; then
      say "dry-run: groupadd --system bot-agent"
    else
      groupadd --system bot-agent || return 1
      say "groupadd: bot-agent"
    fi
  fi
  if id bot-agent >/dev/null 2>&1; then
    say "user bot-agent: present"
  else
    if [ "$DRY" = "1" ]; then
      say "dry-run: useradd --system --gid bot-agent --no-create-home --shell /usr/sbin/nologin bot-agent"
    else
      _shell=/usr/sbin/nologin
      if [ ! -x "$_shell" ] && [ -x /sbin/nologin ]; then
        _shell=/sbin/nologin
      fi
      useradd --system --gid bot-agent --no-create-home --shell "$_shell" --comment bot-agent bot-agent || return 1
      say "useradd: bot-agent"
    fi
  fi
}

# A symlink is refused before mkdir, so chmod cannot follow it onto another directory.
refuse_dir_symlink() {
  if [ -h "$1" ]; then
    printf '%s\n' "refusing symlink $2" >&2
    return 1
  fi
  return 0
}

# Shared directories keep whatever mode and owner they already have.
# chmod only when this installer creates the directory.
ensure_shared_dir() {
  _p=$1
  _label=$2
  refuse_dir_symlink "$_p" "$_label" || return 1
  if [ -d "$_p" ]; then
    return 0
  fi
  mkdir -p "$_p" || return 1
  if [ -h "$_p" ]; then
    printf '%s\n' "refusing symlink ${_label}" >&2
    return 1
  fi
  chmod 0755 "$_p" || return 1
  return 0
}

# Owned directories get an explicit mode and owner every time.
ensure_owned_dir() {
  _p=$1
  _label=$2
  _mode=$3
  _owner=$4
  refuse_dir_symlink "$_p" "$_label" || return 1
  mkdir -p "$_p" || return 1
  if [ -h "$_p" ]; then
    printf '%s\n' "refusing symlink ${_label}" >&2
    return 1
  fi
  chmod "$_mode" "$_p" || return 1
  chown "$_owner" "$_p" || return 1
  return 0
}

prepare_dirs() {
  _etc=$(hostpath /etc/bot-agent)
  _libexec=$(hostpath /usr/local/libexec)
  _bin=$(hostpath /usr/local/bin)
  _unit=$(hostpath /etc/systemd/system)
  _tmpfiles=$(hostpath /etc/tmpfiles.d)
  _dd=$(hostpath /etc/systemd/system/bot-agent.service.d)
  refuse_dir_symlink "$_bin" "/usr/local/bin" || return 1
  refuse_dir_symlink "$_unit" "/etc/systemd/system" || return 1
  refuse_dir_symlink "$_etc" "/etc/bot-agent" || return 1
  refuse_dir_symlink "$STATE" "/var/lib/bot-agent-installer" || return 1
  if [ "$WANT_RUNNER" = "1" ]; then
    refuse_dir_symlink "$_libexec" "/usr/local/libexec" || return 1
    refuse_dir_symlink "$_tmpfiles" "/etc/tmpfiles.d" || return 1
  fi
  if [ "$INSECURE" = "1" ]; then
    refuse_dir_symlink "$_dd" "/etc/systemd/system/bot-agent.service.d" || return 1
  fi
  if [ "$DRY" = "1" ]; then
    say "dry-run: mkdir /usr/local/bin mode 0755 owner root:root"
    say "dry-run: mkdir /etc/bot-agent mode 1770 owner root:bot-agent"
    say "dry-run: mkdir /var/lib/bot-agent-installer mode 0700 owner root:root"
    if [ "$WANT_RUNNER" = "1" ]; then
      say "dry-run: mkdir /usr/local/libexec mode 0755 owner root:root"
      say "dry-run: mkdir /etc/tmpfiles.d mode 0755 owner root:root"
    fi
    if [ "$INSECURE" = "1" ]; then
      say "dry-run: mkdir /etc/systemd/system/bot-agent.service.d mode 0755 owner root:root"
    fi
    return 0
  fi
  ensure_shared_dir "$_bin" "/usr/local/bin" || return 1
  ensure_shared_dir "$_unit" "/etc/systemd/system" || return 1
  # 1770 root:bot-agent, sticky. The agent can create its temp file and
  # rename it over env. It cannot unlink the root-owned runner.toml.
  ensure_owned_dir "$_etc" "/etc/bot-agent" 1770 root:bot-agent || return 1
  ensure_owned_dir "$STATE" "/var/lib/bot-agent-installer" 0700 root:root || return 1
  if [ "$WANT_RUNNER" = "1" ]; then
    ensure_owned_dir "$_libexec" "/usr/local/libexec" 0755 root:root || return 1
    ensure_owned_dir "$_tmpfiles" "/etc/tmpfiles.d" 0755 root:root || return 1
  fi
  if [ "$INSECURE" = "1" ]; then
    ensure_owned_dir "$_dd" "/etc/systemd/system/bot-agent.service.d" 0755 root:root || return 1
  fi
  require_mode "$_etc" 1770
  require_mode "$STATE" 0700
}

# atomic_start makes a temp file in the destination directory.
# atomic_finish chmods, chowns, and renames it onto the destination.
# A symlink at the destination is refused. Rename replaces a running binary
# without opening it for write (ETXTBSY).
atomic_start() {
  ATOMIC_DEST=$1
  ATOMIC_LABEL=$2
  ATOMIC_TMP=
  if [ -h "$ATOMIC_DEST" ]; then
    printf '%s\n' "refusing symlink ${ATOMIC_LABEL}" >&2
    return 1
  fi
  if [ -e "$ATOMIC_DEST" ] && [ ! -f "$ATOMIC_DEST" ]; then
    printf '%s\n' "refusing non-regular ${ATOMIC_LABEL}" >&2
    return 1
  fi
  _adir=$(dirname "$ATOMIC_DEST")
  mkdir -p "$_adir" || return 1
  ATOMIC_TMP=$(mktemp "$_adir/.bot-agent-inst.XXXXXX") || return 1
  return 0
}

atomic_finish() {
  _amode=$1
  _aowner=$2
  if [ -z "$ATOMIC_TMP" ] || [ ! -f "$ATOMIC_TMP" ]; then
    return 1
  fi
  if ! chmod "$_amode" "$ATOMIC_TMP"; then
    rm -f "$ATOMIC_TMP"
    ATOMIC_TMP=
    return 1
  fi
  if ! chown "$_aowner" "$ATOMIC_TMP"; then
    rm -f "$ATOMIC_TMP"
    ATOMIC_TMP=
    return 1
  fi
  if [ -h "$ATOMIC_DEST" ]; then
    rm -f "$ATOMIC_TMP"
    ATOMIC_TMP=
    printf '%s\n' "refusing symlink ${ATOMIC_LABEL}" >&2
    return 1
  fi
  if ! mv -f "$ATOMIC_TMP" "$ATOMIC_DEST"; then
    rm -f "$ATOMIC_TMP"
    ATOMIC_TMP=
    return 1
  fi
  ATOMIC_TMP=
  return 0
}

install_bytes() {
  _path=$1
  _mode=$2
  _owner=$3
  _src=$4
  _dest=$(hostpath "$_path")
  if [ "$DRY" = "1" ]; then
    say "dry-run: write ${_path} mode ${_mode} owner ${_owner}"
    return 0
  fi
  if [ -h "$_dest" ]; then
    fail "refusing symlink ${_path}"
  fi
  atomic_start "$_dest" "$_path" || fail "cannot stage ${_path}"
  if ! cp "$_src" "$ATOMIC_TMP"; then
    rm -f "$ATOMIC_TMP"
    ATOMIC_TMP=
    fail "copy failed ${_path}"
  fi
  atomic_finish "$_mode" "$_owner" || fail "rename failed ${_path}"
  require_mode "$_dest" "$_mode"
}

refuse_managed_symlinks() {
  while IFS=' ' read -r _path _mode _owner _kind _src; do
    [ -n "$_path" ] || continue
    _dest=$(hostpath "$_path")
    if [ -h "$_dest" ] || [ -h "${_dest}.prev" ]; then
      precheck_fail "refusing symlink ${_path}"
    fi
  done <<EOF
$(each_managed)
EOF
}

# Refuse install replacements that are not byte-identical.
guard_install_file() {
  _path=$1
  _kind=$2
  _src=$3
  _dest=$(hostpath "$_path")
  if [ ! -f "$_dest" ]; then
    return 0
  fi
  if [ "$_kind" = "policy" ]; then
    return 0
  fi
  if [ -z "$_src" ] || [ "$_src" = "-" ]; then
    return 0
  fi
  _cur=$(hash_file "$_dest") || precheck_fail "cannot hash ${_path}"
  _ship=$(hash_file "$_src") || precheck_fail "cannot hash bundle file ${_path}"
  if [ "$_cur" = "$_ship" ]; then
    return 0
  fi
  _rec=$(record_hash "$RECORD" "$_path")
  if [ "$_kind" = "unit" ] && [ -n "$_rec" ] && [ "$_cur" != "$_rec" ]; then
    if command -v diff >/dev/null 2>&1; then
      diff -u "$_dest" "$_src" || true
    fi
    precheck_fail "refusing to replace modified unit ${_path}"
  fi
  if [ -z "$_rec" ]; then
    precheck_fail "not installed by the installer: ${_path}"
  fi
  precheck_fail "differs from the bundle, use upgrade: ${_path}"
}

guard_upgrade_file() {
  _path=$1
  _kind=$2
  _src=$3
  _dest=$(hostpath "$_path")
  if [ "$_kind" != "unit" ]; then
    return 0
  fi
  if [ ! -f "$_dest" ]; then
    return 0
  fi
  _cur=$(hash_file "$_dest") || precheck_fail "cannot hash ${_path}"
  _rec=$(record_hash "$RECORD" "$_path")
  if [ -z "$_rec" ]; then
    if [ "$FORCE" != "1" ]; then
      precheck_fail "not installed by the installer: ${_path}"
    fi
    return 0
  fi
  if [ "$_cur" != "$_rec" ] && [ "$FORCE" != "1" ]; then
    if [ -n "$_src" ] && [ "$_src" != "-" ] && command -v diff >/dev/null 2>&1; then
      diff -u "$_dest" "$_src" || true
    fi
    precheck_fail "refusing to replace modified unit ${_path}"
  fi
  if [ -n "$_src" ] && [ "$_src" != "-" ] && [ -f "$_src" ] && command -v cmp >/dev/null 2>&1 && command -v diff >/dev/null 2>&1; then
    if ! cmp -s "$_dest" "$_src"; then
      say "unit diff: ${_path}"
      diff -u "$_dest" "$_src" || true
    fi
  fi
}

write_record() {
  _ver=$1
  if [ "$DRY" = "1" ]; then
    return 0
  fi
  mkdir -p "$STATE" || return 1
  if [ -h "$RECORD" ]; then
    fail "refusing symlink installed record"
  fi
  atomic_start "$RECORD" "installed record" || return 1
  {
    printf 'version %s\n' "$_ver"
    while IFS=' ' read -r _path _mode _owner _kind _src; do
      [ -n "$_path" ] || continue
      if [ "$_kind" = "policy" ]; then
        if [ ! -f "$(hostpath "$_path")" ]; then
          continue
        fi
        # Record the policy only when this installer wrote it.
        if [ -f "$STATE/policy-owned" ]; then
          _h=$(hash_file "$(hostpath "$_path")") || return 1
          printf '%s  %s\n' "$_h" "$_path"
        fi
        continue
      fi
      if [ ! -f "$(hostpath "$_path")" ]; then
        continue
      fi
      _h=$(hash_file "$(hostpath "$_path")") || return 1
      printf '%s  %s\n' "$_h" "$_path"
    done <<EOF
$(each_managed)
EOF
  } > "$ATOMIC_TMP" || {
    rm -f "$ATOMIC_TMP"
    ATOMIC_TMP=
    return 1
  }
  atomic_finish 0600 root:root || return 1
  return 0
}

mark_mutated() {
  if [ "$DRY" = "1" ]; then
    return 0
  fi
  MUTATED=1
}

apply_managed() {
  while IFS=' ' read -r _path _mode _owner _kind _src; do
    [ -n "$_path" ] || continue
    _dest=$(hostpath "$_path")
    if [ "$_kind" = "policy" ]; then
      if [ -e "$ENV_DEST" ]; then
        say "kept: ${ENV_HOST}"
      fi
      # [ -f ] follows a symlink. Refuse one, and check again immediately
      # before keeping the file: the directory is group-writable by then.
      if [ -h "$_dest" ]; then
        fail "refusing symlink ${_path}"
      fi
      if [ -f "$_dest" ] && [ ! -h "$_dest" ]; then
        if [ -h "$_dest" ] || [ ! -f "$_dest" ]; then
          fail "refusing symlink ${_path}"
        fi
        say "kept: ${_path}"
        continue
      fi
      if [ -n "$RUNNER_CONFIG" ]; then
        install_bytes "$_path" "$_mode" "$_owner" "$RUNNER_CONFIG" || return 1
        # Dry-run did not create the file. Hashing it fails the command.
        if [ "$DRY" = "1" ]; then
          continue
        fi
        _ih=$(hash_file "$_dest") || return 1
        _sh=$(hash_file "$RUNNER_CONFIG") || return 1
        if [ "$_ih" != "$_sh" ]; then
          fail "runner.toml does not match staged --runner-config"
        fi
      else
        if [ "$DRY" = "1" ]; then
          say "dry-run: write ${_path} mode ${_mode} owner ${_owner}"
        else
          if [ -h "$_dest" ]; then
            fail "refusing symlink ${_path}"
          fi
          atomic_start "$_dest" "$_path" || return 1
          write_disabled_template "$ATOMIC_TMP" || return 1
          atomic_finish "$_mode" "$_owner" || return 1
          require_mode "$_dest" "$_mode" || return 1
        fi
      fi
      if [ "$DRY" != "1" ]; then
        _powned=$STATE/policy-owned
        if [ -h "$_powned" ]; then
          fail "refusing symlink policy-owned"
        fi
        atomic_start "$_powned" "policy-owned" || return 1
        : > "$ATOMIC_TMP" || return 1
        atomic_finish 0600 root:root || return 1
      fi
      continue
    fi
    if [ "$_kind" = "dropin" ]; then
      if [ "$DRY" = "1" ]; then
        say "dry-run: write ${_path} mode ${_mode} owner ${_owner}"
      else
        if [ -h "$_dest" ]; then
          fail "refusing symlink ${_path}"
        fi
        atomic_start "$_dest" "$_path" || return 1
        write_dropin "$ATOMIC_TMP" || return 1
        atomic_finish "$_mode" "$_owner" || return 1
        require_mode "$_dest" "$_mode" || return 1
      fi
      continue
    fi
    if [ -f "$_dest" ]; then
      _cur=$(hash_file "$_dest") || fail "cannot hash ${_path}"
      _ship=$(hash_file "$_src") || fail "cannot hash ${_src}"
      if [ "$_cur" = "$_ship" ]; then
        say "unchanged: ${_path}"
        continue
      fi
    fi
    install_bytes "$_path" "$_mode" "$_owner" "$_src" || return 1
    # "wrote:" means the file is on disk. Dry-run only printed the plan.
    if [ "$DRY" = "1" ]; then
      continue
    fi
    say "wrote: ${_path}"
  done <<EOF
$(each_managed)
EOF
}

precheck_policy_for_install() {
  if [ "$WANT_RUNNER" != "1" ]; then
    say "policy: not checked"
    return 0
  fi
  refuse_different_policy
  if [ -f "$POLICY_DEST" ]; then
    check_policy_install_mode "$POLICY_DEST"
    check_policy_file "$POLICY_DEST" "$POLICY_HOST"
    require_images_enabled
    return 0
  fi
  if [ -n "$RUNNER_CONFIG" ]; then
    check_policy_file "$RUNNER_CONFIG" "$RUNNER_CONFIG_LABEL"
    require_images_enabled
    return 0
  fi
  _tmp=$(mktemp) || precheck_fail "mktemp failed"
  write_disabled_template "$_tmp"
  check_policy_file "$_tmp" "disabled template" "$_tmp"
}

same_as_record() {
  if [ ! -f "$RECORD" ]; then
    return 1
  fi
  if [ "$installed_ver" != "$VER" ]; then
    return 1
  fi
  while IFS=' ' read -r _path _mode _owner _kind _src; do
    [ -n "$_path" ] || continue
    _dest=$(hostpath "$_path")
    case "$_kind" in
      policy)
        if [ ! -f "$_dest" ]; then
          return 1
        fi
        ;;
      dropin)
        if [ ! -f "$_dest" ]; then
          return 1
        fi
        _tmp=$(mktemp) || return 1
        write_dropin "$_tmp"
        if ! cmp -s "$_tmp" "$_dest"; then
          rm -f "$_tmp"
          return 1
        fi
        rm -f "$_tmp"
        ;;
      *)
        if [ ! -f "$_dest" ] || [ ! -f "$_src" ]; then
          return 1
        fi
        _cur=$(hash_file "$_dest") || return 1
        _ship=$(hash_file "$_src") || return 1
        _rec=$(record_hash "$RECORD" "$_path")
        if [ "$_cur" != "$_ship" ] || [ "$_cur" != "$_rec" ]; then
          return 1
        fi
        ;;
    esac
  done <<EOF
$(each_managed)
EOF
  return 0
}

do_install() {
  if [ -f "$ENV_DEST" ]; then
    say "kept: ${ENV_HOST}"
  fi
  precheck_policy_for_install
  while IFS=' ' read -r _path _mode _owner _kind _src; do
    [ -n "$_path" ] || continue
    guard_install_file "$_path" "$_kind" "$_src"
  done <<EOF
$(each_managed)
EOF
  refuse_managed_symlinks
  if same_as_record && id bot-agent >/dev/null 2>&1 && getent group bot-agent >/dev/null 2>&1; then
    if [ "$GEN_POLICY" = "1" ]; then
      say "kept: ${POLICY_HOST}"
    fi
    say "already installed, nothing changed"
    say_next_enroll
    say "summary: already installed ${VER}"
    exit 0
  fi
  if [ "$DRY" = "1" ]; then
    ensure_user
    prepare_dirs || fail "directories failed"
    apply_managed
    say "dry-run: systemctl daemon-reload"
    if [ "$WANT_RUNNER" = "1" ]; then
      say "dry-run: systemd-tmpfiles --create /etc/tmpfiles.d/bot-agent-runner.conf"
    fi
    say "summary: dry-run install ${VER}"
    exit 0
  fi
  mark_mutated
  ensure_user || fail "user setup failed"
  prepare_dirs || fail "directories failed"
  apply_managed || fail "install failed"
  write_record "$VER" || fail "record failed"
  systemctl daemon-reload || fail "daemon-reload failed"
  say "daemon-reload"
  if [ "$WANT_RUNNER" = "1" ]; then
    systemd-tmpfiles --create "$(hostpath /etc/tmpfiles.d/bot-agent-runner.conf)" || fail "tmpfiles failed"
    say "tmpfiles: applied"
  fi
  say "units: disabled"
  say_next_enroll
  say "summary: installed ${VER}"
}

# Enrolled means /etc/bot-agent/env exists and is not a symlink.
host_enrolled() {
  if [ -f "$ENV_DEST" ] && [ ! -h "$ENV_DEST" ]; then
    return 0
  fi
  return 1
}

# enable, disable, or none from the prev record. The socket is included when
# a hash line names it, and removed when the upgrade recorded it as added.
socket_action_from_prev() {
  if [ ! -f "$PREV" ] || [ -h "$PREV" ]; then
    printf '%s\n' "none"
    return 0
  fi
  _sact=none
  while IFS= read -r _sline || [ -n "$_sline" ]; do
    case "$_sline" in
      "added ${SOCKET_PATH}")
        _sact=disable
        ;;
      *)
        _spath=$(printf '%s\n' "$_sline" | awk '{print $2}')
        if [ "$_spath" = "$SOCKET_PATH" ]; then
          _sact=enable
        fi
        ;;
    esac
  done < "$PREV"
  printf '%s\n' "$_sact"
}

# enable or disable. Disable of a unit that is already gone is success:
# systemd 252 exits non-zero from `disable --now` once the unit file is gone.
run_socket_action() {
  case "$1" in
    enable)
      if [ "$DRY" = "1" ]; then
        say "dry-run: systemctl enable --now ${SOCKET_UNIT}"
        return 0
      fi
      say "systemctl enable --now ${SOCKET_UNIT}"
      systemctl enable --now "$SOCKET_UNIT" || return 1
      SOCKET_VERIFY=1
      ;;
    disable)
      if [ "$DRY" = "1" ]; then
        say "dry-run: systemctl disable --now ${SOCKET_UNIT}"
        return 0
      fi
      _su=$(hostpath "$SOCKET_PATH")
      if [ ! -e "$_su" ] && [ ! -h "$_su" ]; then
        if ! systemctl cat "$SOCKET_UNIT" >/dev/null 2>&1; then
          return 0
        fi
      fi
      say "systemctl disable --now ${SOCKET_UNIT}"
      systemctl disable --now "$SOCKET_UNIT" || return 1
      ;;
    *) return 1 ;;
  esac
  return 0
}

# After daemon-reload and before the agent restart. The agent probes the
# helper only at start-up. Upgrade enables only with --runner. A host
# without env enables nothing. Rollback does not use this: a disable has
# to run before restore deletes an added unit file.
sync_runner_socket() {
  _swhen=$1
  SOCKET_VERIFY=0
  if ! host_enrolled; then
    return 0
  fi
  if [ "$_swhen" != "upgrade" ] || [ "$RUNNER" != "1" ]; then
    return 0
  fi
  run_socket_action enable || return 1
  return 0
}

# Disable while the added unit file is still installed. Enable waits until
# restore and daemon-reload have put the unit file back.
begin_rollback_socket() {
  SOCKET_VERIFY=0
  RB_SOCKET_ACT=none
  if ! host_enrolled; then
    return 0
  fi
  RB_SOCKET_ACT=$(socket_action_from_prev) || return 1
  case "$RB_SOCKET_ACT" in
    enable|none) return 0 ;;
    disable)
      run_socket_action disable || return 1
      return 0
      ;;
    *) return 1 ;;
  esac
}

finish_rollback_socket() {
  if [ "$RB_SOCKET_ACT" = "enable" ]; then
    run_socket_action enable || return 1
  fi
  return 0
}

# Active, NRestarts exactly 0, the bundle version, and the socket when this
# command enabled it. Prints the success lines. Does not restart.
# Every status is checked explicitly: callers run under `||` where set -e is off.
require_running_version() {
  _expect=$1
  _st=$(systemctl is-active bot-agent.service 2>/dev/null || true)
  if [ "$_st" != "active" ]; then
    say "verify: agent is ${_st:-unknown}"
    return 1
  fi
  _re=$(systemctl show -p NRestarts --value bot-agent.service 2>/dev/null)
  _rc=$?
  if [ "$_rc" != "0" ] || [ "$_re" != "0" ]; then
    say "verify: NRestarts is not 0"
    return 1
  fi
  if [ "$SOCKET_VERIFY" = "1" ]; then
    _sst=$(systemctl is-active "$SOCKET_UNIT" 2>/dev/null || true)
    if [ "$_sst" != "active" ]; then
      say "verify: runner socket is ${_sst:-unknown}"
      return 1
    fi
  fi
  _bin=$(hostpath /usr/local/bin/bot-agent)
  _got=$("$_bin" version)
  _rc=$?
  if [ "$_rc" != "0" ]; then
    return 1
  fi
  if [ "$_got" != "$_expect" ]; then
    say "verify: version ${_got} != ${_expect}"
    return 1
  fi
  say "verify: active, NRestarts 0, version ${_got}"
  if [ "$SOCKET_VERIFY" = "1" ]; then
    say "verify: runner socket active"
  fi
  return 0
}

# Restart and require NRestarts exactly 0 only when the host is enrolled.
verify_active_version() {
  _expect=$1
  if host_enrolled; then
    systemctl reset-failed bot-agent.service >/dev/null 2>&1 || true
    say "systemctl restart bot-agent.service"
    if ! systemctl restart bot-agent.service; then
      say "verify: restart failed"
      return 1
    fi
    if [ "$SETTLE" != "0" ]; then
      if ! sleep "$SETTLE"; then
        return 1
      fi
    fi
    require_running_version "$_expect" || return 1
    return 0
  fi
  say "not enrolled: agent not restarted"
  _bin=$(hostpath /usr/local/bin/bot-agent)
  _got=$("$_bin" version)
  _rc=$?
  if [ "$_rc" != "0" ]; then
    return 1
  fi
  if [ "$_got" != "$_expect" ]; then
    say "verify: version ${_got} != ${_expect}"
    return 1
  fi
  return 0
}

say_restart_plan() {
  if host_enrolled; then
    say "dry-run: systemctl restart bot-agent.service"
    say "dry-run: sleep ${SETTLE} then check active and NRestarts"
  else
    say "not enrolled: agent not restarted"
  fi
}

# Copy one installed file to <file>.prev via rename. The .prev file stays
# until a later restore has been verified.
backup_one() {
  _bpath=$1
  _bowner=$2
  _bdest=$(hostpath "$_bpath")
  _bprev=${_bdest}.prev
  if [ -h "$_bdest" ] || [ -h "$_bprev" ]; then
    fail "refusing symlink ${_bpath}"
  fi
  if [ ! -f "$_bdest" ]; then
    return 0
  fi
  _bmode=$(file_mode "$_bdest") || return 1
  atomic_start "$_bprev" "${_bpath}.prev" || return 1
  if ! cp "$_bdest" "$ATOMIC_TMP"; then
    rm -f "$ATOMIC_TMP"
    ATOMIC_TMP=
    return 1
  fi
  atomic_finish "$_bmode" "$_bowner" || return 1
  return 0
}

backup_tree() {
  if [ -h "$STATE" ]; then
    printf '%s\n' "refusing symlink /var/lib/bot-agent-installer" >&2
    return 1
  fi
  if [ ! -d "$STATE" ]; then
    mkdir -p "$STATE" || return 1
  fi
  if [ -h "$STATE" ]; then
    printf '%s\n' "refusing symlink /var/lib/bot-agent-installer" >&2
    return 1
  fi
  # Tighten state before anything is written under prev.
  chmod 0700 "$STATE" || return 1
  chown root:root "$STATE" || return 1
  if [ -h "$STATE/prev" ]; then
    printf '%s\n' "refusing symlink /var/lib/bot-agent-installer/prev" >&2
    return 1
  fi
  _prevdir=$STATE/prev
  mkdir -p "$_prevdir" || return 1
  chmod 0700 "$_prevdir" || return 1
  chown root:root "$_prevdir" || return 1
  if [ -h "$PREV" ]; then
    fail "refusing symlink prev record"
  fi
  if host_enrolled; then
    _en=1
  else
    _en=0
  fi
  _lines=$(mktemp "$STATE/.bot-agent-inst.XXXXXX") || return 1
  _bfail=0
  {
    printf 'version %s\n' "$installed_ver"
    printf 'enrolled %s\n' "$_en"
    while IFS=' ' read -r _path _mode _owner _kind _src; do
      [ -n "$_path" ] || continue
      if [ "$_kind" = "policy" ]; then
        continue
      fi
      _dest=$(hostpath "$_path")
      if [ -f "$_dest" ]; then
        backup_one "$_path" "$_owner" || _bfail=1
        if [ "$_bfail" != "0" ]; then
          break
        fi
        _h=$(hash_file "$_dest") || _bfail=1
        if [ "$_bfail" != "0" ]; then
          break
        fi
        printf '%s  %s\n' "$_h" "$_path"
      else
        printf 'added %s\n' "$_path"
      fi
    done <<EOF
$(each_managed)
EOF
  } > "$_lines"
  if [ "$_bfail" != "0" ]; then
    rm -f "$_lines"
    return 1
  fi
  atomic_start "$PREV" "prev record" || {
    rm -f "$_lines"
    return 1
  }
  if ! cp "$_lines" "$ATOMIC_TMP"; then
    rm -f "$_lines" "$ATOMIC_TMP"
    ATOMIC_TMP=
    return 1
  fi
  rm -f "$_lines"
  atomic_finish 0600 root:root || return 1
  return 0
}

# Put .prev bytes back. .prev files and the prev record stay until discard_prevs.
restore_prev() {
  if [ ! -f "$PREV" ] || [ -h "$PREV" ]; then
    return 1
  fi
  _old=$(record_version "$PREV")
  while IFS= read -r _line || [ -n "$_line" ]; do
    case "$_line" in
      ''|version\ *|was-active\ *|enrolled\ *) continue ;;
      added\ *)
        _path=${_line#added }
        if ! managed_install_path "$_path"; then
          printf '%s\n' "refusing to delete unmanaged path ${_path}" >&2
          return 1
        fi
        _dest=$(hostpath "$_path")
        if [ "$DRY" = "1" ]; then
          say "dry-run: delete ${_path}"
        else
          if [ -h "$_dest" ]; then
            printf '%s\n' "refusing symlink ${_path}" >&2
            return 1
          fi
          rm -f "$_dest" || return 1
        fi
        ;;
      *)
        _path=$(printf '%s\n' "$_line" | awk '{print $2}')
        [ -n "$_path" ] || continue
        _dest=$(hostpath "$_path")
        if [ -h "$_dest" ] || [ -h "${_dest}.prev" ]; then
          printf '%s\n' "refusing symlink ${_path}" >&2
          return 1
        fi
        if [ ! -f "${_dest}.prev" ]; then
          printf '%s\n' "missing backup ${_path}.prev" >&2
          return 1
        fi
        if [ "$DRY" = "1" ]; then
          say "dry-run: restore ${_path} from ${_path}.prev"
        else
          _rmode=$(file_mode "${_dest}.prev") || return 1
          atomic_start "$_dest" "$_path" || return 1
          if ! cp "${_dest}.prev" "$ATOMIC_TMP"; then
            rm -f "$ATOMIC_TMP"
            ATOMIC_TMP=
            return 1
          fi
          atomic_finish "$_rmode" root:root || return 1
        fi
        ;;
    esac
  done < "$PREV"
  if [ "$DRY" = "1" ]; then
    return 0
  fi
  _lines=$(mktemp "$STATE/.bot-agent-inst.XXXXXX") || return 1
  {
    printf 'version %s\n' "$_old"
    awk '$1 ~ /^[0-9a-f]+$/ && length($1)==64 {printf "%s  %s\n", $1, $2}' "$PREV"
  } > "$_lines" || {
    rm -f "$_lines"
    return 1
  }
  if [ -h "$RECORD" ]; then
    rm -f "$_lines"
    printf '%s\n' "refusing symlink installed record" >&2
    return 1
  fi
  atomic_start "$RECORD" "installed record" || {
    rm -f "$_lines"
    return 1
  }
  if ! cp "$_lines" "$ATOMIC_TMP"; then
    rm -f "$_lines" "$ATOMIC_TMP"
    ATOMIC_TMP=
    return 1
  fi
  rm -f "$_lines"
  atomic_finish 0600 root:root || return 1
  return 0
}

discard_prevs() {
  if [ ! -f "$PREV" ] || [ -h "$PREV" ]; then
    return 0
  fi
  while IFS= read -r _line || [ -n "$_line" ]; do
    case "$_line" in
      ''|version\ *|was-active\ *|enrolled\ *|added\ *) continue ;;
    esac
    _path=$(printf '%s\n' "$_line" | awk '{print $2}')
    [ -n "$_path" ] || continue
    _prev=$(hostpath "$_path").prev
    if [ -h "$_prev" ]; then
      printf '%s\n' "refusing symlink ${_path}.prev" >&2
      return 1
    fi
    rm -f "$_prev" || return 1
  done < "$PREV"
  rm -f "$PREV" || return 1
  return 0
}

# Called from the EXIT trap after upgrade or rollback has already failed.
# Restores .prev and returns. It must not exit.
rollback_armed_now() {
  _kind=$1
  _back=
  say "rolling back"
  # A failed socket stop must not skip the file restore.
  begin_rollback_socket || true
  _rb=0
  restore_prev || _rb=$?
  if [ "$_rb" = "0" ]; then
    systemctl daemon-reload || true
    finish_rollback_socket || _rb=$?
  fi
  if [ "$_rb" = "0" ]; then
    _back=$(record_version "$RECORD") || _back=
    verify_active_version "$_back" || _rb=$?
  fi
  if [ "$_rb" = "0" ]; then
    discard_prevs || _rb=$?
  fi
  if [ "$_rb" != "0" ]; then
    printf '%s\n' "rollback failed" >&2
    return 1
  fi
  printf '%s\n' "${_kind} failed, restored ${_back}" >&2
  return 0
}

# Restore the previous set, verify it, and only then delete .prev.
rollback_unverified() {
  say "$1"
  # Stay armed until restore_prev returns. A socket-stop failure still restores.
  begin_rollback_socket || true
  restore_prev || fail "rollback failed"
  ROLLBACK_ARMED=0
  systemctl daemon-reload || fail "daemon-reload failed"
  finish_rollback_socket || fail "rollback failed"
  _back=$(record_version "$RECORD")
  _rc=0
  verify_active_version "$_back" || _rc=$?
  if [ "$_rc" != "0" ]; then
    fail "rollback verify failed"
  fi
  discard_prevs || fail "could not remove backups"
  fail "upgrade failed, restored ${_back}"
}

do_upgrade() {
  if [ ! -f "$RECORD" ] && [ "$FORCE" != "1" ]; then
    precheck_fail "not installed by the installer"
  fi
  refuse_managed_symlinks
  if [ -n "$installed_ver" ] && [ "$installed_ver" = "$VER" ] && [ "$FORCE" != "1" ]; then
    say "already at ${VER}, nothing changed"
    say "summary: already at ${VER}"
    exit 0
  fi
  if [ -f "$ENV_DEST" ]; then
    say "kept: ${ENV_HOST}"
  fi
  if [ -f "$POLICY_DEST" ]; then
    say "kept: ${POLICY_HOST}"
    check_policy_file "$POLICY_DEST" "$POLICY_HOST" || return 1
  fi
  while IFS=' ' read -r _path _mode _owner _kind _src; do
    [ -n "$_path" ] || continue
    guard_upgrade_file "$_path" "$_kind" "$_src"
  done <<EOF
$(each_managed)
EOF
  if [ -z "$installed_ver" ]; then
    installed_ver=unknown
  fi
  if [ "$DRY" = "1" ]; then
    say "dry-run: backup replaced files to <file>.prev"
    apply_managed
    say "dry-run: systemctl daemon-reload"
    sync_runner_socket upgrade
    say_restart_plan
    say "summary: dry-run upgrade ${installed_ver} -> ${VER}"
    exit 0
  fi
  mark_mutated
  _rc=0
  backup_tree || _rc=$?
  if [ "$_rc" != "0" ]; then
    fail "backup failed"
  fi
  # .prev is complete. Any later exit restores it before the process ends.
  ROLLBACK_KIND=upgrade
  ROLLBACK_ARMED=1
  ensure_user || _rc=$?
  if [ "$_rc" = "0" ]; then
    prepare_dirs || _rc=$?
  fi
  if [ "$_rc" = "0" ]; then
    apply_managed || _rc=$?
  fi
  if [ "$_rc" = "0" ]; then
    write_record "$VER" || _rc=$?
  fi
  if [ "$_rc" = "0" ]; then
    systemctl daemon-reload || _rc=$?
  fi
  if [ "$_rc" = "0" ]; then
    say "daemon-reload"
  fi
  if [ "$_rc" = "0" ] && [ "$WANT_RUNNER" = "1" ]; then
    systemd-tmpfiles --create "$(hostpath /etc/tmpfiles.d/bot-agent-runner.conf)" || _rc=$?
  fi
  if [ "$_rc" != "0" ]; then
    rollback_unverified "apply failed, rolling back"
  fi
  _rc=0
  sync_runner_socket upgrade || _rc=$?
  if [ "$_rc" != "0" ]; then
    rollback_unverified "runner socket update failed, rolling back"
  fi
  _rc=0
  verify_active_version "$VER" || _rc=$?
  if [ "$_rc" != "0" ]; then
    rollback_unverified "verify failed, rolling back"
  fi
  ROLLBACK_ARMED=0
  say "summary: upgraded ${installed_ver} -> ${VER}"
}

do_rollback() {
  if [ ! -f "$PREV" ]; then
    say "nothing to roll back"
    say "summary: nothing to roll back"
    exit 0
  fi
  _old=$(record_version "$PREV")
  refuse_managed_symlinks
  if [ "$DRY" = "1" ]; then
    begin_rollback_socket || fail "runner socket update failed"
    restore_prev || fail "rollback failed"
    prepare_dirs || fail "directories failed"
    say "dry-run: systemctl daemon-reload"
    finish_rollback_socket || fail "runner socket update failed"
    say_restart_plan
    say "summary: dry-run rollback ${_old}"
    exit 0
  fi
  mark_mutated
  # The first restore write is the first rollback write. A later exit
  # runs restore again so a partial write does not stay mixed.
  # Disable runs before that restore: the unit file has to still exist.
  ROLLBACK_KIND=rollback
  ROLLBACK_ARMED=1
  _sock=0
  begin_rollback_socket || _sock=$?
  restore_prev || fail "rollback failed"
  ROLLBACK_ARMED=0
  prepare_dirs || fail "directories failed"
  systemctl daemon-reload || fail "daemon-reload failed"
  finish_rollback_socket || fail "runner socket update failed"
  _rc=0
  verify_active_version "$_old" || _rc=$?
  if [ "$_rc" != "0" ]; then
    fail "rollback verify failed"
  fi
  if [ "$_sock" != "0" ]; then
    fail "runner socket update failed"
  fi
  discard_prevs || fail "could not remove backups"
  say "summary: rolled back ${_old}"
}

safe_name() {
  case "$1" in
    *[!A-Za-z0-9._-]*) return 1 ;;
    '') return 1 ;;
  esac
  return 0
}

unit_owned() {
  _p=0
  _e=0
  while IFS= read -r _line || [ -n "$_line" ]; do
    case "$_line" in
      X-Deploybot-Project=*) _p=1 ;;
      X-Deploybot-Entry=*) _e=1 ;;
    esac
  done < "$1"
  [ "$_p" = "1" ] && [ "$_e" = "1" ]
}

each_workload_unit() {
  _out=$1
  : > "$_out"
  for _dir in "$(hostpath /etc/systemd/system)" "$(hostpath /run/systemd/system)"; do
    [ -d "$_dir" ] || continue
    for _f in "$_dir"/deploybot-*.service; do
      [ -e "$_f" ] || continue
      if [ -L "$_f" ]; then
        say "left in place: ${_f} (symlink)"
        continue
      fi
      [ -f "$_f" ] || continue
      _base=$(basename "$_f")
      if ! safe_name "$_base"; then
        say "left in place: ${_f} (name)"
        continue
      fi
      case "$_base" in
        deploybot-*.service) ;;
        *) continue ;;
      esac
      if unit_owned "$_f"; then
        printf '%s\n' "$_f" >> "$_out"
      else
        say "left in place: ${_f} (no unit markers)"
      fi
    done
  done
}

remove_owned_units() {
  _tmp=$(mktemp) || fail "mktemp failed"
  each_workload_unit "$_tmp"
  while IFS= read -r _f || [ -n "${_f:-}" ]; do
    [ -n "$_f" ] || continue
    _base=$(basename "$_f")
    if [ "$REMOVE_WL" != "1" ]; then
      say "left in place: ${_base}"
      continue
    fi
    if [ "$DRY" = "1" ]; then
      say "dry-run: systemctl disable --now ${_base}"
      say "dry-run: delete ${_f}"
      continue
    fi
    if ! systemctl disable --now "$_base"; then
      say "systemctl failed: disable --now ${_base}"
      REMOVE_INCOMPLETE=1
    fi
    rm -f "$_f" || return 1
    say "removed unit: ${_base}"
  done < "$_tmp"
  rm -f "$_tmp" || return 1
  return 0
}

# Stop helper instances before the template unit is deleted.
# A pattern that matches nothing is non-zero on some systemd versions.
# That is not a failure. A stop that fails while an instance exists is.
stop_runner_instances() {
  if [ "$DRY" = "1" ]; then
    say "dry-run: systemctl stop bot-agent-runner@*.service"
    return 0
  fi
  if systemctl stop 'bot-agent-runner@*.service'; then
    return 0
  fi
  _listed=$(systemctl list-units --all --no-legend --plain 'bot-agent-runner@*.service' 2>/dev/null) || _listed=
  if [ -n "$_listed" ]; then
    say "systemctl failed: stop bot-agent-runner@*.service"
    REMOVE_INCOMPLETE=1
  fi
  return 0
}

remove_containers() {
  if ! command -v docker >/dev/null 2>&1; then
    if [ "$REMOVE_WL" = "1" ]; then
      say "containers could not be listed"
      return 1
    fi
    say "left in place: containers not listed (docker absent)"
    return 0
  fi
  _lines=$(docker ps -a --filter label=dev.deploybot.owner=bot-agent --format '{{.ID}} {{.Names}}') || {
    if [ "$REMOVE_WL" = "1" ]; then
      say "containers could not be listed"
      return 1
    fi
    say "left in place: could not list containers"
    return 0
  }
  if [ -z "$_lines" ]; then
    say "containers: none labelled"
    return 0
  fi
  _oldifs=$IFS
  IFS='
'
  for _line in $_lines; do
    [ -n "$_line" ] || continue
    _id=${_line%% *}
    if ! safe_name "$_id"; then
      say "left in place: container (id refused)"
      continue
    fi
    if [ "$REMOVE_WL" != "1" ]; then
      say "left in place: container ${_line}"
      continue
    fi
    if [ "$DRY" = "1" ]; then
      say "dry-run: docker rm -f ${_id}"
      continue
    fi
    docker rm -f "$_id" >/dev/null || return 1
    say "removed container: ${_id}"
  done
  IFS=$_oldifs
}

# Volumes are removed only with both --remove-workloads and --purge.
# Networks are removed with --remove-workloads alone. Names still have to
# come from the deploybot owner label.
remove_labelled() {
  _kind=$1
  _need_purge=$2
  shift 2
  if [ "$REMOVE_WL" != "1" ]; then
    return 0
  fi
  if [ "$_need_purge" = "1" ] && [ "$PURGE" != "1" ]; then
    say "left in place: ${_kind}s (need --purge)"
    return 0
  fi
  if ! command -v docker >/dev/null 2>&1; then
    say "${_kind}s could not be listed"
    return 1
  fi
  _lines=$(docker "$@" ) || {
    say "${_kind}s could not be listed"
    return 1
  }
  if [ -z "$_lines" ]; then
    return 0
  fi
  _oldifs=$IFS
  IFS='
'
  for _name in $_lines; do
    [ -n "$_name" ] || continue
    if ! safe_name "$_name"; then
      say "left in place: ${_kind} (name refused)"
      continue
    fi
    if [ "$DRY" = "1" ]; then
      say "dry-run: docker ${_kind} rm ${_name}"
      continue
    fi
    if [ "$_kind" = "volume" ]; then
      docker volume rm "$_name" >/dev/null || return 1
    else
      docker network rm "$_name" >/dev/null || return 1
    fi
    say "removed ${_kind}: ${_name}"
  done
  IFS=$_oldifs
}

delete_managed_files() {
  for _path in \
    /usr/local/bin/bot-agent \
    /usr/local/libexec/bot-agent-runner \
    /etc/systemd/system/bot-agent.service \
    /etc/systemd/system/bot-agent-runner.socket \
    /etc/systemd/system/bot-agent-runner@.service \
    /etc/tmpfiles.d/bot-agent-runner.conf \
    /etc/systemd/system/bot-agent.service.d/insecure-dev.conf
  do
    _dest=$(hostpath "$_path")
    if [ -f "$_dest" ] || [ -f "$_dest.prev" ]; then
      if [ "$DRY" = "1" ]; then
        if [ -f "$_dest" ]; then
          _mode=$(file_mode "$_dest" || true)
          say "dry-run: delete ${_path} mode ${_mode} owner root:root"
        fi
        if [ -f "$_dest.prev" ]; then
          say "dry-run: delete ${_path}.prev"
        fi
      else
        rm -f "$_dest" "$_dest.prev"
        say "deleted: ${_path}"
      fi
    fi
  done
  _dd=$(hostpath /etc/systemd/system/bot-agent.service.d)
  if [ -d "$_dd" ]; then
    if [ "$DRY" = "1" ]; then
      say "dry-run: rmdir /etc/systemd/system/bot-agent.service.d"
    else
      rmdir "$_dd" 2>/dev/null || true
    fi
  fi
}

purge_one() {
  _host=$1
  case "$_host" in
    /etc/bot-agent|/var/lib/bot-agent|/var/lib/bot-agent-runner|/var/lib/bot-agent-installer) ;;
    *) fail "refusing to delete ${_host}" ;;
  esac
  _abs=$(hostpath "$_host")
  if [ -n "$ROOT" ]; then
    case "$_abs" in
      "$ROOT"/*) ;;
      *) fail "refusing to delete ${_abs}" ;;
    esac
  fi
  if [ "$DRY" = "1" ]; then
    say "dry-run: delete tree ${_host}"
    return 0
  fi
  rm -rf "$_abs"
  say "purged: ${_host}"
}

do_remove() {
  remove_owned_units || fail "could not remove workload units"
  stop_runner_instances || fail "could not stop runner instances"
  _wl=0
  remove_containers || _wl=1
  remove_labelled volume 1 volume ls --filter label=dev.deploybot.owner=bot-agent --format '{{.Name}}' || _wl=1
  remove_labelled network 0 network ls --filter label=dev.deploybot.owner=bot-agent --format '{{.Name}}' || _wl=1
  if [ "$_wl" != "0" ]; then
    fail "could not remove workloads"
  fi
  if [ "$DRY" != "1" ]; then
    mark_mutated
    if ! systemctl disable --now bot-agent.service; then
      say "systemctl failed: disable --now bot-agent.service"
      REMOVE_INCOMPLETE=1
    fi
  else
    say "dry-run: systemctl disable --now bot-agent.service"
  fi
  # Absent socket unit is success, including systemd 252's non-zero disable.
  if ! run_socket_action disable; then
    say "systemctl failed: disable --now ${SOCKET_UNIT}"
    REMOVE_INCOMPLETE=1
  fi
  delete_managed_files
  if [ "$PURGE" = "1" ]; then
    purge_one /etc/bot-agent
    purge_one /var/lib/bot-agent
    purge_one /var/lib/bot-agent-runner
  else
    if [ -d "$(hostpath /etc/bot-agent)" ]; then
      say "left in place: /etc/bot-agent"
    fi
    if [ -d "$(hostpath /var/lib/bot-agent-runner)" ]; then
      say "left in place: /var/lib/bot-agent-runner"
    fi
    if [ -d "$(hostpath /var/lib/bot-agent)" ]; then
      say "left in place: /var/lib/bot-agent"
    fi
  fi
  if [ "$DRY" = "1" ]; then
    say "dry-run: delete tree /var/lib/bot-agent-installer"
  else
    purge_one /var/lib/bot-agent-installer
    systemctl daemon-reload || true
  fi
  if [ "$PURGE" = "1" ]; then
    if id bot-agent >/dev/null 2>&1; then
      if [ "$DRY" = "1" ]; then
        say "dry-run: userdel bot-agent"
      else
        userdel bot-agent
        say "userdel: bot-agent"
      fi
    fi
    if getent group bot-agent >/dev/null 2>&1; then
      say "left in place: group bot-agent"
    fi
  fi
  say "revoke the host in the console"
  if [ "$REMOVE_INCOMPLETE" = "1" ]; then
    if [ "$DRY" = "1" ]; then
      say "summary: dry-run remove incomplete"
    else
      say "summary: remove incomplete"
    fi
    exit 1
  fi
  if [ "$DRY" = "1" ]; then
    say "summary: dry-run remove"
  else
    say "summary: removed"
  fi
}

# Same drop-in install --insecure-dev writes. Refuses a symlink.
install_insecure_dropin() {
  _path=/etc/systemd/system/bot-agent.service.d/insecure-dev.conf
  _dd=$(hostpath /etc/systemd/system/bot-agent.service.d)
  _dest=$(hostpath "$_path")
  refuse_dir_symlink "$_dd" "/etc/systemd/system/bot-agent.service.d" || return 1
  if [ "$DRY" = "1" ]; then
    say "dry-run: mkdir /etc/systemd/system/bot-agent.service.d mode 0755 owner root:root"
    say "dry-run: write ${_path} mode 0644 owner root:root"
    return 0
  fi
  ensure_owned_dir "$_dd" "/etc/systemd/system/bot-agent.service.d" 0755 root:root || return 1
  if [ -h "$_dest" ]; then
    fail "refusing symlink ${_path}"
  fi
  atomic_start "$_dest" "$_path" || return 1
  write_dropin "$ATOMIC_TMP" || return 1
  atomic_finish 0644 root:root || return 1
  require_mode "$_dest" 0644
  return 0
}

# Restart when the unit is already active. Otherwise enable and start it.
# Dry-run only reads is-active.
enroll_start_agent() {
  _st=$(systemctl is-active bot-agent.service 2>/dev/null || true)
  if [ "$_st" = "active" ]; then
    if [ "$DRY" = "1" ]; then
      say "dry-run: systemctl restart bot-agent.service"
      return 0
    fi
    say "systemctl restart bot-agent.service"
    systemctl restart bot-agent.service || return 1
    return 0
  fi
  if [ "$DRY" = "1" ]; then
    say "dry-run: systemctl enable --now bot-agent.service"
    return 0
  fi
  say "systemctl enable --now bot-agent.service"
  systemctl enable --now bot-agent.service || return 1
  return 0
}

# Trim spaces and tabs on $_trim. Matches strings.TrimSpace for those bytes.
trim_spaces() {
  _tab=$(printf '\t')
  while :; do
    case "$_trim" in
      ' '*|"$_tab"*) _trim=${_trim#?} ;;
      *) break ;;
    esac
  done
  while :; do
    case "$_trim" in
      *' '|*"$_tab") _trim=${_trim%?} ;;
      *) break ;;
    esac
  done
  _tab=
}

# $_q includes the surrounding quotes. Result is $_uq. Trailing backslash fails.
# One backslash drops itself and keeps the next byte, same as parseEnv's unquote.
env_unquote() {
  _inner=${_q#\"}
  _inner=${_inner%\"}
  _uq=
  _esc=0
  # shellcheck disable=SC1003 # the byte is one backslash, the env escape character
  _bs=$(printf '\\')
  while [ -n "$_inner" ]; do
    _rest=${_inner#?}
    _c=${_inner%"$_rest"}
    _inner=$_rest
    if [ "$_esc" = "1" ]; then
      _uq=${_uq}${_c}
      _esc=0
      continue
    fi
    if [ "$_c" = "$_bs" ]; then
      _esc=1
      continue
    fi
    _uq=${_uq}${_c}
  done
  _bs=
  _inner=
  _rest=
  _c=
  if [ "$_esc" = "1" ]; then
    _uq=
    _esc=
    return 1
  fi
  _esc=
  return 0
}

# Same rules as parseEnv: trim, one leading "export ", trim the key, unquote.
# A join-token key counts whatever its value. Host and credential ids must be
# non-empty after unquoting. A line the agent would reject is not ready.
# The line and value variables are cleared before return. A symlink is not ready.
env_credential_ready() {
  _f=$1
  if [ -h "$_f" ] || [ ! -f "$_f" ]; then
    return 1
  fi
  _host=0
  _cid=0
  _join=0
  _bad=0
  while IFS= read -r _line || [ -n "$_line" ]; do
    _trim=$_line
    trim_spaces
    _line=$_trim
    case "$_line" in
      ''|\#*) continue ;;
    esac
    case "$_line" in
      'export '*) _line=${_line#export } ;;
    esac
    case "$_line" in
      *=*) ;;
      *) _bad=1; break ;;
    esac
    _key=${_line%%=*}
    _val=${_line#*=}
    _trim=$_key
    trim_spaces
    _key=$_trim
    _trim=$_val
    trim_spaces
    _val=$_trim
    _tab=$(printf '\t')
    case "$_key" in
      ''|*' '*|*"$_tab"*) _bad=1 ;;
    esac
    _tab=
    if [ "$_bad" = "1" ]; then
      break
    fi
    case "$_val" in
      \"*\")
        _q=$_val
        if env_unquote; then
          _val=$_uq
        else
          _bad=1
          break
        fi
        ;;
    esac
    case "$_key" in
      BOT_AGENT_HOST_ID)
        if [ -n "$_val" ]; then
          _host=1
        else
          _host=0
        fi
        ;;
      BOT_AGENT_CREDENTIAL_ID)
        if [ -n "$_val" ]; then
          _cid=1
        else
          _cid=0
        fi
        ;;
      BOT_AGENT_JOIN_TOKEN) _join=1 ;;
    esac
  done < "$_f"
  _line=
  _trim=
  _key=
  _val=
  _q=
  _uq=
  _inner=
  _rest=
  _c=
  if [ "$_bad" = "1" ]; then
    return 1
  fi
  if [ "$_host" = "1" ] && [ "$_cid" = "1" ] && [ "$_join" = "0" ]; then
    return 0
  fi
  return 1
}

wait_enroll_persisted() {
  _i=0
  while :; do
    if env_credential_ready "$ENV_DEST"; then
      return 0
    fi
    if [ "$_i" -ge "$ENROLL_WAIT" ]; then
      return 1
    fi
    sleep 1 || return 1
    _i=$((_i + 1))
  done
}

do_enroll() {
  _agent=$(hostpath /usr/local/bin/bot-agent)
  if [ ! -x "$_agent" ]; then
    precheck_fail "bot-agent is not installed"
  fi
  case "$SERVER" in
    wss://*|ws://*) ;;
    *) precheck_fail "server url must use wss://" ;;
  esac
  if [ "$DRY" = "1" ]; then
    say "dry-run: exec bot-agent init --server <url> --join-token-stdin --env-file ${ENV_HOST}"
    if [ "$INSECURE" = "1" ]; then
      install_insecure_dropin || fail "drop-in failed"
    fi
    enroll_start_agent || fail "enroll failed"
    if [ "$RUNNER" = "1" ] || [ "$WANT_RUNNER" = "1" ]; then
      run_socket_action enable || fail "enroll failed"
    fi
    say "summary: dry-run enroll"
    exit 0
  fi
  # init is the first write. A refusal here leaves the host unchanged.
  _init_env=
  if [ "$INSECURE" = "1" ] || [ -f "$(hostpath /etc/systemd/system/bot-agent.service.d/insecure-dev.conf)" ]; then
    _init_env=1
  fi
  _enr=0
  if [ -t 0 ]; then
    printf '%s' "join token (not echoed): " >&2
    STTY_SAVED=$(stty -g) || fail "stty failed"
    stty -echo || fail "stty failed"
    IFS= read -r _tok || true
    restore_stty
    printf '\n' >&2
    # printf is a shell builtin in dash, so the token is not in any process's argv.
    if [ "$_init_env" = "1" ]; then
      printf '%s\n' "$_tok" | BOT_AGENT_INSECURE_DEV=1 "$_agent" init --server "$SERVER" --join-token-stdin --env-file "$ENV_DEST" || _enr=$?
    else
      printf '%s\n' "$_tok" | "$_agent" init --server "$SERVER" --join-token-stdin --env-file "$ENV_DEST" || _enr=$?
    fi
    _tok=
  else
    if [ "$_init_env" = "1" ]; then
      BOT_AGENT_INSECURE_DEV=1 "$_agent" init --server "$SERVER" --join-token-stdin --env-file "$ENV_DEST" || _enr=$?
    else
      "$_agent" init --server "$SERVER" --join-token-stdin --env-file "$ENV_DEST" || _enr=$?
    fi
  fi
  if [ "$_enr" != "0" ]; then
    precheck_fail "enroll failed"
  fi
  mark_mutated
  if [ "$INSECURE" = "1" ]; then
    install_insecure_dropin || fail "drop-in failed"
  fi
  if [ -h "$ENV_DEST" ]; then
    fail "refusing symlink ${ENV_HOST}"
  fi
  if [ -f "$ENV_DEST" ]; then
    # Mode while the inode is still root-owned. After chown, do not chmod or
    # stat this path: both follow a symlink the new owner can plant.
    chmod 0600 "$ENV_DEST" || fail "env chmod failed"
    if [ -h "$ENV_DEST" ] || [ ! -f "$ENV_DEST" ]; then
      fail "refusing symlink ${ENV_HOST}"
    fi
    chown -h bot-agent:bot-agent "$ENV_DEST" || fail "env chown failed"
  fi
  systemctl daemon-reload || fail "daemon-reload failed"
  enroll_start_agent || fail "enroll failed"
  if [ "$RUNNER" = "1" ] || [ "$WANT_RUNNER" = "1" ]; then
    run_socket_action enable || fail "enroll failed"
  fi
  if ! wait_enroll_persisted; then
    fail "enroll failed: credential not persisted"
  fi
  systemctl reset-failed bot-agent.service >/dev/null 2>&1 || true
  if [ "$SETTLE" != "0" ]; then
    if ! sleep "$SETTLE"; then
      fail "enroll failed"
    fi
  fi
  if ! require_running_version "$VER"; then
    fail "enroll failed: agent did not stay up. Run systemctl stop bot-agent, move ${ENV_HOST} aside, then enroll again"
  fi
  say "summary: enrolled"
}

case "$CMD" in
  check) do_check ;;
  install) do_install ;;
  enroll) do_enroll ;;
  upgrade) do_upgrade ;;
  rollback) do_rollback ;;
  remove) do_remove ;;
  *) usage ;;
esac
