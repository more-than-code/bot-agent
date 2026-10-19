#!/bin/sh
# Orchestrator gate for the bot-agent installer on a disposable podman VM.
# Starts a debian:12 container with systemd as PID 1, copies linux/arm64
# bundles built from this tree, and runs AC1, AC2, AC4, AC5, AC6, and AC7,
# plus a hand-installed enrolled host upgraded with --force --runner,
# and an enrolled host with no socket whose upgrade --runner fails
# verification and rolls the socket back off.
# AC3 checks that enroll feeds the token to `bot-agent init` on stdin and
# that the token is not in the script output or process arguments.
# This script removes only the container it creates. It does not touch the
# Mac's /etc, /usr, systemd, or Docker.
set -eu

here=$(CDPATH='' cd -- "$(dirname "$0")" && pwd)
repo=$(CDPATH='' cd -- "$here/.." && pwd)
work=$(mktemp -d)
name="bot-agent-inst-$$"

cleanup() {
  if [ -n "${name:-}" ]; then
    podman rm -f "$name" >/dev/null 2>&1 || true
  fi
  if [ -n "${work:-}" ]; then
    rm -rf "$work"
  fi
}
trap cleanup EXIT INT TERM

if ! command -v podman >/dev/null 2>&1; then
  printf '%s\n' "podman is not installed" >&2
  exit 1
fi
if ! podman info >/dev/null 2>&1; then
  podman machine start
  i=0
  while [ "$i" -lt 30 ]; do
    if podman info >/dev/null 2>&1; then
      break
    fi
    i=$((i + 1))
    sleep 1
  done
fi
podman info >/dev/null

cat > "$work/Containerfile" <<'EOF'
FROM docker.io/library/debian:12
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
  && apt-get install -y --no-install-recommends \
    systemd systemd-sysv ca-certificates passwd login diffutils procps docker.io \
  && rm -rf /var/lib/apt/lists/* \
  && ln -sf /dev/null /etc/systemd/system/systemd-logind.service \
  && ln -sf /dev/null /etc/systemd/system/console-getty.service \
  && ln -sf /dev/null /etc/systemd/system/getty.target
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
EOF

podman build -t bot-agent-installer-test:debian12 -f "$work/Containerfile" "$work"

hash_tree() {
  dest=$1
  list=$(mktemp)
  find "$dest" -type f ! -name SHA256SUMS -print | sed "s|^${dest}/||" | sort > "$list"
  (
    cd "$dest"
    : > SHA256SUMS
    while IFS= read -r p; do
      [ -n "$p" ] || continue
      line=$(shasum -a 256 "$p") || exit 1
      printf '%s\n' "$line" >> SHA256SUMS
    done < "$list"
  )
  rm -f "$list"
  grep -q 'install.sh$' "$dest/SHA256SUMS"
}

build_bundle() {
  src=$1
  ver=$2
  dest=$3
  arch=arm64
  mkdir -p "$dest/units"
  (
    cd "$src"
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$dest/bot-agent-v${ver}-linux-${arch}" ./cmd/bot-agent
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$dest/bot-agent-runner-v${ver}-linux-${arch}" ./cmd/bot-agent-runner
  )
  cp "$repo/packaging/bot-agent.service" "$dest/units/bot-agent.service"
  cp "$repo/packaging/bot-agent-runner.socket" "$dest/units/bot-agent-runner.socket"
  cp "$repo/packaging/bot-agent-runner@.service" "$dest/units/bot-agent-runner@.service"
  cp "$repo/packaging/tmpfiles.d/bot-agent-runner.conf" "$dest/units/tmpfiles-bot-agent-runner.conf"
  cp "$repo/packaging/install.sh" "$dest/install.sh"
  chmod 0755 "$dest/install.sh" \
    "$dest/bot-agent-v${ver}-linux-${arch}" \
    "$dest/bot-agent-runner-v${ver}-linux-${arch}"
  printf 'bot-agent %s\n' "$ver" > "$dest/RELEASE.md"
  hash_tree "$dest"
}

# The test uses its own version labels (0.2.0 old, 0.2.1 new, 0.2.2 broken),
# independent of the repo's current AgentVersion: each bundle is built from a
# copy of the tree with the version set explicitly.
versioned_src() {
  _vs=$1
  _dir=$2
  mkdir -p "$_dir"
  tar -C "$repo" -cf - go.mod go.sum cmd internal | tar -C "$_dir" -xf -
  _proto=$_dir/internal/agent/protocol/protocol.go
  sed "s/AgentVersion = \"[^\"]*\"/AgentVersion = \"$_vs\"/" "$_proto" > "$_proto.new"
  mv "$_proto.new" "$_proto"
  grep -q "AgentVersion = \"$_vs\"" "$_proto"
}

versioned_src "0.2.0" "$work/src020"
build_bundle "$work/src020" "0.2.0" "$work/b020"
versioned_src "0.2.1" "$work/src021"
build_bundle "$work/src021" "0.2.1" "$work/b021"
# The 0.2.1 unit differs from 0.2.0 so upgrade and rollback can be compared.
printf '\n# bundle-0.2.1-unit\n' >> "$work/b021/units/bot-agent.service"
hash_tree "$work/b021"

mkdir -p "$work/b022/units"
cp "$work/b021/units/bot-agent.service" "$work/b022/units/bot-agent.service"
cp "$work/b021/units/bot-agent-runner.socket" "$work/b022/units/bot-agent-runner.socket"
cp "$work/b021/units/bot-agent-runner@.service" "$work/b022/units/bot-agent-runner@.service"
cp "$work/b021/units/tmpfiles-bot-agent-runner.conf" "$work/b022/units/tmpfiles-bot-agent-runner.conf"
cp "$work/b021/install.sh" "$work/b022/install.sh"
cp "$work/b021/bot-agent-runner-v0.2.1-linux-arm64" "$work/b022/bot-agent-runner-v0.2.2-linux-arm64"
cp "$work/b021/bot-agent-v0.2.1-linux-arm64" "$work/real-bot-agent"
cat > "$work/b022/bot-agent-v0.2.2-linux-arm64" <<'EOF'
#!/bin/sh
set -eu
if [ "${1:-}" = "version" ] || [ "${1:-}" = "--version" ] || [ "${1:-}" = "-version" ]; then
  printf '%s\n' "0.2.2"
  exit 0
fi
if [ "${1:-}" = "policy" ] || [ "${1:-}" = "init" ]; then
  exec /opt/bot-agent-real "$@"
fi
exit 1
EOF
chmod 0755 "$work/b022/install.sh" \
  "$work/b022/bot-agent-v0.2.2-linux-arm64" \
  "$work/b022/bot-agent-runner-v0.2.2-linux-arm64" \
  "$work/real-bot-agent"
printf '%s\n' "bot-agent 0.2.2 bad" > "$work/b022/RELEASE.md"
hash_tree "$work/b022"

cat > "$work/inner.sh" <<'EOF'
#!/bin/sh
# Runs inside the debian:12 systemd container as root.
set -eu
token=EnrollTokenValue1234

die() {
  printf '%s\n' "$*" >&2
  exit 1
}

snap() {
  {
    for p in \
      /usr/local/bin/bot-agent \
      /usr/local/libexec/bot-agent-runner \
      /etc/systemd/system/bot-agent.service \
      /etc/systemd/system/bot-agent-runner.socket \
      "/etc/systemd/system/bot-agent-runner@.service" \
      /etc/tmpfiles.d/bot-agent-runner.conf \
      /etc/systemd/system/bot-agent.service.d \
      /etc/bot-agent \
      /var/lib/bot-agent \
      /var/lib/bot-agent-runner \
      /var/lib/bot-agent-installer
    do
      if [ -e "$p" ]; then
        find "$p" -print | sort | while IFS= read -r f; do
          if [ -f "$f" ]; then
            sha256sum "$f"
            stat -c '%n %a' "$f"
          else
            stat -c '%n %a %F' "$f"
          fi
        done
      else
        printf 'absent %s\n' "$p"
      fi
    done
  } | sha256sum
}

have() {
  case "$1" in
    *"$2"*) ;;
    *) die "$3" ;;
  esac
}

run_ok() {
  # $1 is a label. The rest is the command.
  _label=$1
  shift
  set +e
  _out=$("$@" 2>&1)
  _code=$?
  set -e
  if [ "$_code" != 0 ]; then
    printf '%s\n' "$_out" >&2
    die "$_label failed ($_code)"
  fi
  printf '%s\n' "$_out"
}

chmod 0755 \
  /opt/bundles/b020/install.sh \
  /opt/bundles/b020/bot-agent-v0.2.0-linux-arm64 \
  /opt/bundles/b020/bot-agent-runner-v0.2.0-linux-arm64 \
  /opt/bundles/b021/install.sh \
  /opt/bundles/b021/bot-agent-v0.2.1-linux-arm64 \
  /opt/bundles/b021/bot-agent-runner-v0.2.1-linux-arm64 \
  /opt/bundles/b022/install.sh \
  /opt/bundles/b022/bot-agent-v0.2.2-linux-arm64 \
  /opt/bundles/b022/bot-agent-runner-v0.2.2-linux-arm64 \
  /opt/bot-agent-real

printf '%s\n' "enabled = false" > /opt/ok.toml
chmod 0644 /opt/ok.toml
cat > /opt/bad.toml <<'BAD'
enabled = true
images = [
  "ghcr.io/acme/*"
]
BAD
chmod 0644 /opt/bad.toml

# AC7 before anything is installed.
# want is the exit code. A finished dry-run is 0. A failed precheck is 1
# and must say "nothing changed". No dry-run may print "wrote:".
before=$(snap)
dry_case() {
  _want=$1
  shift
  set +e
  "$@" >/tmp/dry.out 2>&1
  _code=$?
  set -e
  after=$(snap)
  if [ "$before" != "$after" ]; then
    cat /tmp/dry.out >&2 || true
    die "AC7 dry-run changed the host: $*"
  fi
  if grep -F 'wrote:' /tmp/dry.out >/dev/null 2>&1; then
    cat /tmp/dry.out >&2 || true
    die "AC7 dry-run printed wrote: $*"
  fi
  if [ "$_code" != "$_want" ]; then
    cat /tmp/dry.out >&2 || true
    die "AC7 dry-run exit ${_code} want ${_want}: $*"
  fi
  if [ "$_want" = "1" ]; then
    if ! grep -F 'nothing changed' /tmp/dry.out >/dev/null 2>&1; then
      cat /tmp/dry.out >&2 || true
      die "AC7 dry-run precheck did not say nothing changed: $*"
    fi
  fi
}

dry_case 0 /opt/bundles/b020/install.sh check --dry-run
dry_case 0 /opt/bundles/b020/install.sh check --dry-run --runner
dry_case 0 /opt/bundles/b020/install.sh install --dry-run --runner --runner-config /opt/ok.toml
for line in \
  "dry-run: write /usr/local/bin/bot-agent mode 0755 owner root:root" \
  "dry-run: write /etc/systemd/system/bot-agent.service mode 0644 owner root:root" \
  "dry-run: write /usr/local/libexec/bot-agent-runner mode 0755 owner root:root" \
  "dry-run: write /etc/systemd/system/bot-agent-runner.socket mode 0644 owner root:root" \
  "dry-run: write /etc/systemd/system/bot-agent-runner@.service mode 0644 owner root:root" \
  "dry-run: write /etc/tmpfiles.d/bot-agent-runner.conf mode 0644 owner root:root" \
  "dry-run: write /etc/bot-agent/runner.toml mode 0644 owner root:root" \
  "summary: dry-run install"
do
  grep -F "$line" /tmp/dry.out >/dev/null || die "AC7 fresh install missing: $line"
done
[ ! -e /usr/local/bin/bot-agent ] || die "AC7 fresh install created the agent binary"
dry_case 1 /opt/bundles/b020/install.sh enroll --dry-run --server wss://example.invalid/agent/v1/ws
dry_case 1 /opt/bundles/b020/install.sh upgrade --dry-run
dry_case 0 /opt/bundles/b020/install.sh rollback --dry-run
dry_case 0 /opt/bundles/b020/install.sh remove --dry-run
dry_case 0 /opt/bundles/b020/install.sh remove --dry-run --purge --remove-workloads

# AC2. A multi-line array is refused before any install.
before=$(snap)
set +e
out=$(/opt/bundles/b020/install.sh install --runner --runner-config /opt/bad.toml 2>&1)
code=$?
set -e
[ "$code" != 0 ] || die "AC2 accepted a bad policy"
have "$out" "invalid: parse" "AC2 did not report invalid: parse"
have "$out" "nothing changed" "AC2 did not say nothing changed"
[ "$(snap)" = "$before" ] || die "AC2 changed the host"
[ ! -e /usr/local/bin/bot-agent ] || die "AC2 installed a binary"

# AC1.
run_ok "AC1 check" /opt/bundles/b020/install.sh check --runner --runner-config /opt/ok.toml >/tmp/check.out
run_ok "AC1 install" /opt/bundles/b020/install.sh install --runner --runner-config /opt/ok.toml >/tmp/install.out
[ "$(stat -c %a /usr/local/bin/bot-agent)" = "755" ] || die "agent mode"
[ "$(stat -c %U:%G /usr/local/bin/bot-agent)" = "root:root" ] || die "agent owner"
[ "$(stat -c %a /usr/local/libexec/bot-agent-runner)" = "755" ] || die "runner mode"
[ "$(stat -c %a /etc/systemd/system/bot-agent.service)" = "644" ] || die "unit mode"
[ "$(stat -c %a /etc/bot-agent/runner.toml)" = "644" ] || die "policy mode"
[ "$(stat -c %U:%G /etc/bot-agent/runner.toml)" = "root:root" ] || die "policy owner"
[ "$(stat -c %a /etc/bot-agent)" = "750" ] || die "etc mode"
[ "$(stat -c %U:%G /etc/bot-agent)" = "root:bot-agent" ] || die "etc owner"
[ "$(stat -c %a /var/lib/bot-agent-installer)" = "700" ] || die "state mode"
[ "$(stat -c %U:%G /var/lib/bot-agent-installer)" = "root:root" ] || die "state owner"
en=$(systemctl is-enabled bot-agent.service 2>/dev/null || true)
[ "$en" = "disabled" ] || die "agent unit is $en"
en=$(systemctl is-enabled bot-agent-runner.socket 2>/dev/null || true)
[ "$en" = "disabled" ] || die "socket is $en"
[ "$(/usr/local/bin/bot-agent version)" = "0.2.0" ] || die "installed version"
out=$(run_ok "AC1 rerun" /opt/bundles/b020/install.sh install --runner --runner-config /opt/ok.toml)
have "$out" "already installed, nothing changed" "AC1 rerun"

# AC7 after install.
before=$(snap)
dry_case 0 /opt/bundles/b020/install.sh check --dry-run --runner
dry_case 0 /opt/bundles/b020/install.sh install --dry-run --runner --runner-config /opt/ok.toml
grep -F 'already installed, nothing changed' /tmp/dry.out >/dev/null || die "AC7 reinstall dry-run"
dry_case 0 /opt/bundles/b020/install.sh enroll --dry-run --server wss://example.invalid/agent/v1/ws
grep -F 'summary: dry-run enroll' /tmp/dry.out >/dev/null || die "AC7 enroll dry-run plan"
dry_case 0 /opt/bundles/b021/install.sh upgrade --dry-run
grep -F 'dry-run: write /usr/local/bin/bot-agent mode 0755 owner root:root' /tmp/dry.out >/dev/null || die "AC7 upgrade dry-run plan"
grep -F 'summary: dry-run upgrade' /tmp/dry.out >/dev/null || die "AC7 upgrade dry-run summary"
dry_case 0 /opt/bundles/b020/install.sh rollback --dry-run
grep -F 'nothing to roll back' /tmp/dry.out >/dev/null || die "AC7 rollback dry-run before upgrade"
dry_case 0 /opt/bundles/b020/install.sh remove --dry-run --purge --remove-workloads
grep -F 'dry-run: delete /usr/local/bin/bot-agent' /tmp/dry.out >/dev/null || die "AC7 remove dry-run plan"
grep -F 'summary: dry-run remove' /tmp/dry.out >/dev/null || die "AC7 remove dry-run summary"

# AC4 modified unit, then put the stock bytes back so a later upgrade can run.
printf '\n# local\n' >> /etc/systemd/system/bot-agent.service
set +e
out=$(/opt/bundles/b021/install.sh upgrade 2>&1)
code=$?
set -e
[ "$code" != 0 ] || die "AC4 replaced a modified unit"
have "$out" "nothing changed" "AC4 modified unit"
grep -q '# local' /etc/systemd/system/bot-agent.service || die "local edit disappeared"
[ "$(/usr/local/bin/bot-agent version)" = "0.2.0" ] || die "modified-unit upgrade changed the binary"
cp /opt/bundles/b020/units/bot-agent.service /etc/systemd/system/bot-agent.service
chmod 0644 /etc/systemd/system/bot-agent.service

# AC3. The server is unreachable. init must consume the token; the agent
# does not have to complete a handshake. ps is argv only: systemd's
# EnvironmentFile puts the token in the process environment on purpose.
set +e
out=$(printf '%s\n' "$token" | /opt/bundles/b020/install.sh enroll --server 'wss://example.invalid/agent/v1/ws' 2>&1)
code=$?
set -e
case "$out" in
  *"$token"*) die "token in enroll output" ;;
esac
ps -eo args > /tmp/ps.args
if grep -q "$token" /tmp/ps.args; then
  rm -f /tmp/ps.args
  die "token in process arguments"
fi
rm -f /tmp/ps.args
[ -f /etc/bot-agent/env ] || die "enroll did not write the env file"
[ "$(stat -c '%U:%G %a' /etc/bot-agent/env)" = "bot-agent:bot-agent 600" ] || die "env owner mode"
cp /etc/bot-agent/env /opt/enrolled.env
chmod 0600 /opt/enrolled.env
if ! grep -q 'BOT_AGENT_SERVER_URL=wss://example.invalid/agent/v1/ws' /etc/bot-agent/env; then
  die "env missing server url"
fi
if [ "$code" != 0 ]; then
  printf '%s\n' "$out" >&2
  die "enroll exited $code"
fi
st=$(systemctl is-active bot-agent.service 2>/dev/null || true)
[ "$st" = "active" ] || die "agent is $st after enroll"

# AC4 upgrade while the agent is running. The installer must rename the
# binary (a write in place returns ETXTBSY). NRestarts must be exactly 0.
id1=$(systemctl show -p InvocationID --value bot-agent.service || true)
run_ok "AC4 upgrade" /opt/bundles/b021/install.sh upgrade >/tmp/up.out
[ "$(/usr/local/bin/bot-agent version)" = "0.2.1" ] || die "upgrade version"
[ -x /usr/local/bin/bot-agent.prev ] || die "missing .prev"
[ "$(/usr/local/bin/bot-agent.prev version)" = "0.2.0" ] || die "prev version"
cmp -s /etc/systemd/system/bot-agent.service /opt/bundles/b021/units/bot-agent.service || die "upgraded unit bytes"
cmp -s /etc/systemd/system/bot-agent.service.prev /opt/bundles/b020/units/bot-agent.service || die "prev unit bytes"
st=$(systemctl is-active bot-agent.service 2>/dev/null || true)
[ "$st" = "active" ] || die "agent is $st after upgrade"
re=$(systemctl show -p NRestarts --value bot-agent.service) || die "NRestarts show failed"
[ "$re" = "0" ] || die "NRestarts after upgrade: ${re:-empty}"
id2=$(systemctl show -p InvocationID --value bot-agent.service || true)
if [ -z "$id2" ] || [ "$id1" = "$id2" ]; then
  die "upgrade did not restart the agent"
fi

# Rollback dry-run while .prev exists lists restores and changes nothing.
before=$(snap)
dry_case 0 /opt/bundles/b021/install.sh rollback --dry-run
grep -F 'dry-run: restore /usr/local/bin/bot-agent from /usr/local/bin/bot-agent.prev' /tmp/dry.out >/dev/null || die "AC5 rollback dry-run plan"
[ -x /usr/local/bin/bot-agent.prev ] || die "rollback dry-run removed .prev"
[ "$(/usr/local/bin/bot-agent version)" = "0.2.1" ] || die "rollback dry-run changed the binary"

# AC5.
run_ok "AC5 rollback" /opt/bundles/b021/install.sh rollback >/tmp/back.out
[ "$(/usr/local/bin/bot-agent version)" = "0.2.0" ] || die "rollback version"
cmp -s /etc/systemd/system/bot-agent.service /opt/bundles/b020/units/bot-agent.service || die "rolled-back unit bytes"
[ ! -e /usr/local/bin/bot-agent.prev ] || die ".prev remained"
st=$(systemctl is-active bot-agent.service 2>/dev/null || true)
[ "$st" = "active" ] || die "agent is $st after rollback"
out=$(run_ok "AC5 second" /opt/bundles/b020/install.sh rollback)
have "$out" "nothing to roll back" "second rollback"

# AC4 automatic rollback. 0.2.2 exits on `run`, so NRestarts climbs.
set +e
out=$(/opt/bundles/b022/install.sh upgrade 2>&1)
code=$?
set -e
[ "$code" != 0 ] || die "bad upgrade succeeded"
case "$out" in
  *rolling\ back*|*restored*) ;;
  *) printf '%s\n' "$out" >&2; die "bad upgrade did not roll back" ;;
esac
[ "$(/usr/local/bin/bot-agent version)" = "0.2.0" ] || die "auto-rollback version"

# AC6 needs dockerd. Objects come from a local import, not a registry.
systemctl start docker
i=0
ok=0
while [ "$i" -lt 30 ]; do
  if docker info >/dev/null 2>&1; then
    ok=1
    break
  fi
  i=$((i + 1))
  sleep 1
done
[ "$ok" = 1 ] || die "docker did not start"
mkdir -p /tmp/tiny
printf '%s\n' "placeholder" > /tmp/tiny/README
cp /opt/sleeper /tmp/tiny/sleep
chmod 0755 /tmp/tiny/sleep
tar -C /tmp/tiny -cf /tmp/tiny.tar .
docker import /tmp/tiny.tar local-tiny:test >/dev/null

plant() {
  mkdir -p /etc/systemd/system /run/systemd/system /var/lib/bot-agent-runner
  printf '%s\n' "[Service]" "X-Deploybot-Project=demo" "X-Deploybot-Entry=web" "ExecStart=/bin/sleep infinity" \
    > /etc/systemd/system/deploybot-demo-web.service
  printf '%s\n' "[Service]" "X-Deploybot-Project=demo" "X-Deploybot-Entry=job" "ExecStart=/bin/true" \
    > /run/systemd/system/deploybot-demo-job.service
  printf '%s\n' "[Service]" "ExecStart=/bin/true" > /etc/systemd/system/deploybot-foreign.service
  printf '%s\n' "[Service]" "ExecStart=/usr/sbin/nginx" > /etc/systemd/system/nginx.service
  printf '%s\n' "keep" > /var/lib/bot-agent-runner/keep
  systemctl daemon-reload || die "daemon-reload before workload start"
  systemctl start deploybot-demo-web.service || die "could not start owned unit"
  [ "$(systemctl is-active deploybot-demo-web.service)" = "active" ] || die "owned unit did not stay active"
  docker rm -f owned other >/dev/null 2>&1 || true
  docker volume rm vol-owned vol-other >/dev/null 2>&1 || true
  docker network rm net-owned net-other >/dev/null 2>&1 || true
  # Docker nested in the podman VM cannot start containers (runc: "can't get
  # final child's PID"), so the owned container is created, not started. The
  # Go stub test asserts remove never calls docker stop/rm without
  # --remove-workloads; the owned systemd unit above really runs.
  docker create --name owned --label dev.deploybot.owner=bot-agent local-tiny:test /sleep >/dev/null
  docker create --name other local-tiny:test /sleep >/dev/null
  docker volume create --label dev.deploybot.owner=bot-agent vol-owned >/dev/null
  docker volume create vol-other >/dev/null
  docker network create --label dev.deploybot.owner=bot-agent net-owned >/dev/null
  docker network create net-other >/dev/null
}

reinstall() {
  run_ok "reinstall" /opt/bundles/b020/install.sh install --runner --runner-config /opt/ok.toml >/tmp/re.out
}

plant
out=$(run_ok "AC6 remove" /opt/bundles/b020/install.sh remove)
have "$out" "left in place" "AC6 left in place"
have "$out" "deploybot-demo-web" "AC6 unit list"
have "$out" "owned" "AC6 container list"
have "$out" "revoke the host in the console" "AC6 revoke"
[ ! -e /usr/local/bin/bot-agent ] || die "binary remained"
[ ! -e /etc/systemd/system/bot-agent.service ] || die "unit remained"
[ -f /etc/bot-agent/env ] || die "env was removed"
[ -f /var/lib/bot-agent-runner/keep ] || die "runner state was removed"
[ -f /etc/systemd/system/deploybot-demo-web.service ] || die "owned unit removed"
[ -f /run/systemd/system/deploybot-demo-job.service ] || die "run unit removed"
[ -f /etc/systemd/system/deploybot-foreign.service ] || die "foreign unit removed"
[ -f /etc/systemd/system/nginx.service ] || die "nginx removed"
[ "$(systemctl is-active deploybot-demo-web.service)" = "active" ] || die "owned unit stopped"
docker ps -a --format '{{.Names}}' | grep -qx owned || die "owned container removed"
docker ps -a --format '{{.Names}}' | grep -qx other || die "other container missing"

reinstall
plant
run_ok "AC6 workloads" /opt/bundles/b020/install.sh remove --remove-workloads >/tmp/wl.out
[ ! -e /etc/systemd/system/deploybot-demo-web.service ] || die "owned unit remained"
[ ! -e /run/systemd/system/deploybot-demo-job.service ] || die "run unit remained"
[ -f /etc/systemd/system/deploybot-foreign.service ] || die "foreign unit removed"
[ -f /etc/systemd/system/nginx.service ] || die "nginx removed"
[ -d /etc/bot-agent ] || die "config removed without --purge"
if docker ps -a --format '{{.Names}}' | grep -qx owned; then
  die "owned container remained"
fi
docker ps -a --format '{{.Names}}' | grep -qx other || die "other container removed"
docker volume ls | awk 'NR>1 {print $2}' | grep -qx vol-owned || die "volume removed without --purge"
if docker network ls | awk 'NR>1 {print $2}' | grep -qx net-owned; then
  die "network remained"
fi
docker network ls | awk 'NR>1 {print $2}' | grep -qx net-other || die "other network removed"

reinstall
plant
run_ok "AC6 purge" /opt/bundles/b020/install.sh remove --purge >/tmp/purge.out
[ ! -e /etc/bot-agent ] || die "config remained"
[ ! -e /var/lib/bot-agent-runner ] || die "runner state remained"
[ -f /etc/systemd/system/deploybot-demo-web.service ] || die "purge removed a unit"
docker ps -a --format '{{.Names}}' | grep -qx owned || die "purge removed a container"
if id bot-agent >/dev/null 2>&1; then
  die "user remained"
fi

reinstall
plant
run_ok "AC6 both" /opt/bundles/b020/install.sh remove --purge --remove-workloads >/tmp/both.out
[ ! -e /etc/systemd/system/deploybot-demo-web.service ] || die "owned unit remained"
[ -f /etc/systemd/system/nginx.service ] || die "nginx removed"
[ -f /etc/systemd/system/deploybot-foreign.service ] || die "foreign unit removed"
if docker ps -a --format '{{.Names}}' | grep -qx owned; then
  die "owned container remained"
fi
docker ps -a --format '{{.Names}}' | grep -qx other || die "other container removed"
if docker volume ls | awk 'NR>1 {print $2}' | grep -qx vol-owned; then
  die "owned volume remained"
fi
docker volume ls | awk 'NR>1 {print $2}' | grep -qx vol-other || die "other volume removed"
if docker network ls | awk 'NR>1 {print $2}' | grep -qx net-owned; then
  die "owned network remained"
fi
docker network ls | awk 'NR>1 {print $2}' | grep -qx net-other || die "other network removed"

# Hand-installed enrolled host: 0.2.0 binary, unit, and the env file from the
# enroll above, copied without install.sh. No helper and no installer record.
# upgrade --force --runner must enable the socket before the agent restarts.
if ! getent group bot-agent >/dev/null 2>&1; then
  groupadd --system bot-agent || die "hand setup: groupadd"
fi
if ! id bot-agent >/dev/null 2>&1; then
  _shell=/usr/sbin/nologin
  if [ ! -x "$_shell" ] && [ -x /sbin/nologin ]; then
    _shell=/sbin/nologin
  fi
  useradd --system --gid bot-agent --no-create-home --shell "$_shell" --comment bot-agent bot-agent || die "hand setup: useradd"
fi
install -d -m 0755 /usr/local/bin
install -d -m 0750 -o root -g bot-agent /etc/bot-agent
install -d -m 0755 /etc/systemd/system
cp /opt/bundles/b020/bot-agent-v0.2.0-linux-arm64 /usr/local/bin/bot-agent
chmod 0755 /usr/local/bin/bot-agent
cp /opt/bundles/b020/units/bot-agent.service /etc/systemd/system/bot-agent.service
chmod 0644 /etc/systemd/system/bot-agent.service
cp /opt/enrolled.env /etc/bot-agent/env
chown bot-agent:bot-agent /etc/bot-agent/env
chmod 0600 /etc/bot-agent/env
[ ! -e /usr/local/libexec/bot-agent-runner ] || die "hand setup: helper present"
[ ! -e /etc/systemd/system/bot-agent-runner.socket ] || die "hand setup: socket unit present"
[ ! -e /var/lib/bot-agent-installer ] || die "hand setup: installer record present"
[ "$(/usr/local/bin/bot-agent version)" = "0.2.0" ] || die "hand setup: version"
systemctl daemon-reload || die "hand setup: daemon-reload"
systemctl enable --now bot-agent.service || die "hand setup: agent did not start"
st=$(systemctl is-active bot-agent.service 2>/dev/null || true)
[ "$st" = "active" ] || die "hand setup: agent is ${st:-unknown}"
id_before=$(systemctl show -p InvocationID --value bot-agent.service || true)
[ -n "$id_before" ] || die "hand setup: no InvocationID"

out=$(run_ok "hand check" /opt/bundles/b021/install.sh check)
have "$out" "installed: 0.2.0 (not installed by the installer)" "hand check installed line"
out=$(run_ok "hand upgrade" /opt/bundles/b021/install.sh upgrade --force --runner)
have "$out" "installed: 0.2.0 (not installed by the installer)" "hand upgrade installed line"
have "$out" "summary: upgraded 0.2.0 -> 0.2.1" "hand upgrade summary"
[ "$(/usr/local/bin/bot-agent version)" = "0.2.1" ] || die "hand upgrade version"
en=$(systemctl is-enabled bot-agent-runner.socket 2>/dev/null || true)
[ "$en" = "enabled" ] || die "hand socket is-enabled: ${en:-empty}"
st=$(systemctl is-active bot-agent-runner.socket 2>/dev/null || true)
[ "$st" = "active" ] || die "hand socket is-active: ${st:-empty}"
id_after=$(systemctl show -p InvocationID --value bot-agent.service || true)
if [ -z "$id_after" ] || [ "$id_before" = "$id_after" ]; then
  die "hand upgrade did not restart the agent"
fi
# Microseconds since boot. The new agent invocation must start after the socket.
sock_mono=$(systemctl show -p ActiveEnterTimestampMonotonic --value bot-agent-runner.socket || true)
agent_mono=$(systemctl show -p ActiveEnterTimestampMonotonic --value bot-agent.service || true)
case "$sock_mono" in
  ''|*[!0-9]*) die "hand socket ActiveEnterTimestampMonotonic: ${sock_mono:-empty}" ;;
esac
case "$agent_mono" in
  ''|*[!0-9]*) die "hand agent ActiveEnterTimestampMonotonic: ${agent_mono:-empty}" ;;
esac
if [ "$agent_mono" -le "$sock_mono" ]; then
  die "agent ActiveEnterTimestampMonotonic $agent_mono is not after socket $sock_mono"
fi

# Enrolled host, no socket unit. upgrade --runner of the broken bundle
# fails verification. Rollback must disable the socket before deleting the
# unit file, then leave 0.2.0 running.
systemctl disable --now bot-agent-runner.socket || die "socket reset: disable"
systemctl disable --now bot-agent.service || die "socket reset: agent"
rm -f \
  /etc/systemd/system/bot-agent-runner.socket \
  "/etc/systemd/system/bot-agent-runner@.service" \
  /etc/tmpfiles.d/bot-agent-runner.conf \
  /usr/local/libexec/bot-agent-runner \
  /etc/systemd/system/sockets.target.wants/bot-agent-runner.socket
rm -rf /var/lib/bot-agent-installer
cp /opt/bundles/b020/bot-agent-v0.2.0-linux-arm64 /usr/local/bin/bot-agent
chmod 0755 /usr/local/bin/bot-agent
cp /opt/bundles/b020/units/bot-agent.service /etc/systemd/system/bot-agent.service
chmod 0644 /etc/systemd/system/bot-agent.service
[ -f /etc/bot-agent/env ] || die "socket reset: env missing"
[ ! -e /etc/systemd/system/bot-agent-runner.socket ] || die "socket reset: unit remained"
systemctl daemon-reload || die "socket reset: daemon-reload"
systemctl enable --now bot-agent.service || die "socket reset: agent did not start"
st=$(systemctl is-active bot-agent.service 2>/dev/null || true)
[ "$st" = "active" ] || die "socket reset: agent is ${st:-unknown}"
[ "$(/usr/local/bin/bot-agent version)" = "0.2.0" ] || die "socket reset: version"
set +e
out=$(/opt/bundles/b022/install.sh upgrade --force --runner 2>&1)
code=$?
set -e
[ "$code" != 0 ] || die "no-socket bad upgrade succeeded"
case "$out" in
  *rolling\ back*|*restored*) ;;
  *) printf '%s\n' "$out" >&2; die "no-socket bad upgrade did not roll back" ;;
esac
[ "$(/usr/local/bin/bot-agent version)" = "0.2.0" ] || die "no-socket rollback version"
st=$(systemctl is-active bot-agent.service 2>/dev/null || true)
[ "$st" = "active" ] || die "no-socket rollback: agent is ${st:-unknown}"
[ ! -e /etc/systemd/system/bot-agent-runner.socket ] || die "no-socket rollback left the unit file"
[ ! -h /etc/systemd/system/bot-agent-runner.socket ] || die "no-socket rollback left a unit symlink"
st=$(systemctl is-active bot-agent-runner.socket 2>/dev/null || true)
case "$st" in
  active) die "no-socket rollback left the socket active (${st})" ;;
esac
en=$(systemctl is-enabled bot-agent-runner.socket 2>/dev/null || true)
case "$en" in
  enabled) die "no-socket rollback left the socket enabled (${en})" ;;
esac
if [ -e /etc/systemd/system/sockets.target.wants/bot-agent-runner.socket ] || [ -h /etc/systemd/system/sockets.target.wants/bot-agent-runner.socket ]; then
  die "no-socket rollback left sockets.target.wants"
fi
printf '%s\n' "inner checks passed"
EOF

mkdir -p "$work/sleeper-src"
cat > "$work/sleeper-src/main.go" <<'EOF'
package main

import "time"

func main() {
	for {
		time.Sleep(time.Hour)
	}
}
EOF
(
  cd "$work/sleeper-src"
  go mod init example.com/sleeper
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$work/sleeper" .
)

podman run -d --name "$name" --systemd=always --privileged bot-agent-installer-test:debian12 >/dev/null

ready=0
i=0
st=
while [ "$i" -lt 40 ]; do
  st=$(podman exec "$name" systemctl is-system-running 2>/dev/null || true)
  case "$st" in
    running|degraded) ready=1; break ;;
  esac
  i=$((i + 1))
  sleep 1
done
if [ "$ready" != 1 ]; then
  podman logs "$name" >&2 || true
  printf '%s\n' "systemd did not become ready (last: ${st:-})" >&2
  exit 1
fi

podman exec "$name" mkdir -p /opt/bundles
podman cp "$work/b020" "$name:/opt/bundles/b020"
podman cp "$work/b021" "$name:/opt/bundles/b021"
podman cp "$work/b022" "$name:/opt/bundles/b022"
podman cp "$work/real-bot-agent" "$name:/opt/bot-agent-real"
podman cp "$work/sleeper" "$name:/opt/sleeper"
podman exec "$name" chmod 0755 /opt/sleeper
podman cp "$work/inner.sh" "$name:/opt/inner.sh"
podman exec "$name" /bin/sh /opt/inner.sh
printf '%s\n' "installer systemd test passed"
