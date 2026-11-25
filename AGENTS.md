# bot-agent

The Deploybot host agent (`cmd/bot-agent`, `internal/agent/`) and its socket-activated root helper `bot-agent-runner`, with the installer and units in `packaging/`. It runs on each host as the unprivileged `bot-agent` user, dials out to the API (`wss://<api-host>/agent/v1/ws`), sends metrics, processes and discovered services, and runs catalog programs and container images only as the host's root-owned `/etc/bot-agent/runner.toml` allows. Design: `docs/design.md`. Contracts: `docs/contracts/agent-protocol-v1.md` (this repo's own copy) and `docs/contracts/api-v1.md`. Install guide: `docs/install.md`. Cross-repo design and the contract source live in the umbrella workspace (Joe's machine, not part of this repository): `docs/design-bot-agent-deploy.md`. Server side: `more-than-code/bot-control-plane`.

## Rules

- Do not print, log or commit secrets: join tokens, `/etc/bot-agent/env` values, registry logins or env values. Log only a short redacted error line.
- Do not commit unless the user asks. Do not push or add a remote.
- Do not install, upgrade or restart the agent, the runner or Telegraf on any host, and do not change pi4, Postgres or systemd from an agent session. Host steps are done by the ops session with Joe.
- A change to what the helper may do as root (`runner.toml` parsing, the socket peer check, the systemd and Docker calls) is Tier 3: its blast radius is root on every enrolled host.

## Gates (bot-agent)

Change gates, for each pull request in bot-agent; paste each command and its result into the pull request:

- `gofmt -l .` prints nothing, then `go build ./...`, `go vet ./...` and `go test -count=1 ./...` (this includes the installer and packaging tests in `packaging/`).
- Run the tests as a non-root user: as root, the permission and file-mode tests fail or test the wrong thing. Cloud sessions run as root, so use `su`/`runuser` to a test user.
- `go test -race` on the changed packages, and always on `./internal/agent/run/...`.
- A change to `packaging/` or the helper: also `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/...` and the same with `GOARCH=arm64`. Hosts run both.

Release gates, run by Main Dev on `git archive` of the version commit: `go test -count=1 ./...` (with the packaging and installer suite), `go test -race ./internal/agent/run/...`, and the four linux binaries (`bot-agent` and `bot-agent-runner`, amd64 and arm64) built twice with `CGO_ENABLED=0 -trimpath -ldflags "-s -w"` and `cmp`-equal. A change gate failure blocks the commit; a release gate failure blocks the tag.

Releases (`docs/spec-release-workflow.md`): a release thread's version commit sets `internal/agent/protocol.AgentVersion` and adds `release-notes/vX.Y.Z.md` (the release text). When it merges into `main`, `.github/workflows/release.yml` runs the release gates, builds the bundle twice with `scripts/release-bundle.sh`, waits for Joe's approval in the `release` environment, signs `SHA256SUMS` with the CI release key in AWS KMS (GitHub OIDC; no signing secret in GitHub), tags the commit and publishes the release. The keys hosts trust are `internal/release.PublicKeys` (Joe's offline recovery key and the CI key); changing that list is Tier 3.

- Sessions never read the signing keys, never approve a pending deployment, never use AWS credentials for the signing account, and never change environments, environment secrets or variables, rulesets, repository settings, or the KMS key and IAM role. A change to `.github/workflows/`, `scripts/release-bundle.sh`, `cmd/release-sign` or `internal/release` is Tier 3 and goes through a reviewed PR.

A change to `runner.toml` is Tier 3, like the agent protocol, the installer and the root helper.

## Legacy Telegraf renderer

Kept until Telegraf is retired on every host (`cmd/render-telegraf`, `internal/render`, `services/`, `telegraf/`, `sql/`).

Telegraf snippets and Services SQL for Deploybot. Spec: umbrella `docs/spec-v1.md` (Services).

Do not apply SQL. Do not install, edit, or restart Telegraf on any machine. Do not change pi4, Postgres, or systemd from an agent session. Do not print secrets. Leave `sql/010_procstat_and_services.sql` for Joe to run.

```
services/*.json          one manifest per service; stem matches name
nodes.json               cluster members; grok-bot-box has systemd false
cmd/render-telegraf/     writes telegraf/<host>.conf; --check compares disk
internal/render/         manifest checks and snippet rendering
telegraf/<host>.conf     generated; not installed
sql/010_procstat_and_services.sql
                         aligns the existing procstat hypertable; creates
                         systemd_units and http_response; seeds metrics.services.
                         Not applied.
internal/render/*_test.go
                         go test ./...
```

A new service is a new manifest, then `go run ./cmd/render-telegraf`. Health URLs stay on `127.0.0.1` or `localhost` with an explicit port. The scan filter name `user` is reserved. The generated banner still says `render_telegraf.py`. Leave that text; `--check` compares the snippets byte for byte.

User-scope units are not `inputs.systemd_units`. Telegraf runs as user `telegraf`, so `scope = "user"` is telegraf's bus, and procstat `systemd_units` uses the system bus only. Those services use an absolute cgroup under `/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/<unit>`. A relative cgroup path does not match on Telegraf 1.40.1. Service filters set `recursion_depth = -1`. The scan filter does not. Processor `order` is rename (1), topk (2), rename (3).

A template instance (`@` in the unit name) is not a procstat `systemd_units` filter. Telegraf 1.40.1 lists unit files, so `postgresql@18-main.service` matches nothing. Use the cmdline pattern and do not set `recursion_depth` (it would emit each backend twice). Do not use the cgroup: a PID that vanishes while `cgroup.procs` is read drops the whole filter. The field list is `fieldinclude`, not `fieldpass`.

`metrics.procstat` already exists on pi4. The SQL file adds columns and a 7-day retention policy. It also creates `metrics.systemd_units` and `metrics.http_response`. Telegraf does not already have those tables, and the writers cannot create them. Do not grant `metrics_grokbox` separately; it inherits `INSERT` from `metrics_writer`.

On grok-bot-box, `grokbox-metrics-start` runs `telegraf --config /etc/telegraf/telegraf.conf` with no `--config-directory` and skips when Telegraf is already running. Do not edit that script. The exact argv change is in `README.md` and waits for Joe.
