#!/usr/bin/env bash
# Build the bot-agent release bundle for one version into dist/
# (spec docs/spec-release-workflow.md §4, job build):
#   bot-agent-vX.Y.Z.tar.gz   one directory bot-agent-vX.Y.Z/ with the four
#                             linux binaries, install.sh, units/ and SHA256SUMS
#   SHA256SUMS                byte-identical copy of the bundled one; the file
#                             that is signed (SHA256SUMS.sig is made elsewhere)
# The layout is the one docs/install.md lists and internal/upgrade checks.
# The build is reproducible: the same commit and Go toolchain give the same
# bytes (SOURCE_DATE_EPOCH is the commit time; tar and gzip carry no host
# times, owners or names).
#
# Usage: scripts/release-bundle.sh X.Y.Z [OUT_DIR]   (OUT_DIR defaults to dist)
# Refuses a version that is not internal/agent/protocol.AgentVersion and a
# dirty work tree. Needs go, git, GNU tar, gzip and sha256sum on PATH.
set -euo pipefail

version="${1:-}"
if [[ ! "$version" =~ ^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$ ]]; then
  echo "usage: $0 X.Y.Z [OUT_DIR]" >&2
  exit 2
fi
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
out="${2:-$root/dist}"

agent_version="$(sed -n 's/^[[:space:]]*AgentVersion[[:space:]]*=[[:space:]]*"\([0-9.]*\)"$/\1/p' internal/agent/protocol/protocol.go)"
if [[ "$agent_version" != "$version" ]]; then
  echo "version $version is not protocol.AgentVersion ($agent_version)" >&2
  exit 1
fi
if [[ -n "$(git status --porcelain --untracked-files=no)" ]]; then
  echo "work tree has uncommitted changes" >&2
  exit 1
fi

export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}"
export LC_ALL=C TZ=UTC
name="bot-agent-v$version"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
dir="$stage/$name"
mkdir -p "$dir/units"

for arch in amd64 arm64; do
  for cmd in bot-agent bot-agent-runner; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
      -ldflags "-s -w" -o "$dir/$cmd-v$version-linux-$arch" "./cmd/$cmd"
  done
done
install -m 0755 packaging/install.sh "$dir/install.sh"
install -m 0644 packaging/bot-agent.service "$dir/units/bot-agent.service"
install -m 0644 packaging/bot-agent-runner.socket "$dir/units/bot-agent-runner.socket"
install -m 0644 packaging/bot-agent-runner@.service "$dir/units/bot-agent-runner@.service"
install -m 0644 packaging/runner.toml.example "$dir/units/runner.toml.example"
install -m 0644 packaging/tmpfiles.d/bot-agent-runner.conf "$dir/units/tmpfiles-bot-agent-runner.conf"
chmod 0755 "$dir"/bot-agent-*-linux-* "$dir" "$dir/units"

(cd "$dir" && find . -type f ! -name SHA256SUMS | sed 's|^\./||' | sort | xargs sha256sum > SHA256SUMS)
chmod 0644 "$dir/SHA256SUMS"
find "$dir" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

mkdir -p "$out"
rm -f "$out/$name.tar.gz" "$out/SHA256SUMS"
tar -C "$stage" --sort=name --format=ustar --mtime="@$SOURCE_DATE_EPOCH" \
  --owner=0 --group=0 --numeric-owner -cf - "$name" | gzip -9 -n > "$out/$name.tar.gz"
cp "$dir/SHA256SUMS" "$out/SHA256SUMS"
ls -l "$out/$name.tar.gz" "$out/SHA256SUMS"
