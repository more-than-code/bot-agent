# bot-agent: per-repo design (v2)

**Date:** 2026-10-13. **Status:** Draft, design only.
**Cross-repo design (the source of truth for the protocol, permissions, and schema):** [../../docs/design-bot-agent-deploy.md](../../docs/design-bot-agent-deploy.md). This file covers only what lives inside this repo.

Nothing here is installed on a host. Host steps are Joe's.

## What this repo will hold

| Path | Today | v2 |
|---|---|---|
| `cmd/render-telegraf`, `internal/render`, `services/*.json`, `nodes.json`, `telegraf/`, `sql/010_…` | Telegraf snippets and Services SQL for `cluster_metrics` | Kept unchanged **during the overlap period**. Retired after Telegraf is removed (umbrella design §9). |
| `cmd/bot-agent` (new) | — | The agent daemon. |
| `cmd/bot-agent-runner` | — | Root helper for program units. The unprivileged agent reaches it over `/run/bot-agent-runner.sock`. |
| `internal/agent/...` (new) | — | `conn` (wss client, enroll/hello, heartbeat, backoff), `collect` (host metrics, processes), `discover` (ports, units, containers), `probe` (HTTP/TCP health), `spool` (on-disk ingest buffer), `tasks` (allowlist and dispatch), and from phase 2 `docker` (proxy client) and `owned` (local owned list). |
| `cmd/bot-agent-docker-proxy` (new, phase 2) | — | The custom Docker socket proxy (umbrella D26). |

The new code does not import `internal/render`, and the renderer does not import the agent. Neither imports `reference/deploybot-service-agent`.

## Process model

- One static Go binary, `linux/arm64` (Pis) and `linux/amd64` (grok-bot-box). It is deterministic and runs no LLM.
- It opens no listen socket of any kind. Its only network peers are bot-api-server (outbound `wss://`) and, for probes, `127.0.0.1:<exposed port>`.
- It refuses to start if: euid is 0; `BOT_AGENT_SERVER_URL` is not `wss://`; the env file mode is not 0600; or, in phase 2, `DOCKER_HOST` is empty, `tcp://`, or the raw engine socket.
- `bot-agent version` prints the version. There is no `env` subcommand and nothing else prints configuration.

## Configuration

`/etc/bot-agent/env` (mode 0600, owner `bot-agent`, directory 0750):

```
BOT_AGENT_SERVER_URL=wss://<api-host>/agent/v1/ws
BOT_AGENT_JOIN_TOKEN=<one-time; removed after enroll>
BOT_AGENT_HOST_ID=<written on enroll>
BOT_AGENT_CREDENTIAL_ID=<written on enroll>
BOT_AGENT_CREDENTIAL=<written on enroll; rotated by the server>
```

Rewrites are atomic: temp file in the same directory, mode 0600, fsync, rename. Collection intervals and health checks come from the server in `hello_ok.config`. The agent clamps them (host metrics 5–60 s, processes and health 30–300 s, at most 50 checks).

## Phase 1: read-only

| Area | What | Matches Telegraf today |
|---|---|---|
| Host compute | cpu (`cpu-total`, `usage_idle`, …), mem, disk (`/`), diskio (whole disks), net (no `lo`), system (load1/5/15, uptime), temp (`cpu_thermal` where present) | Same names and units as `docs/api-contract.md`, so the overlap comparison is like for like. |
| Processes | Top 20 by cpu and top 20 by RSS each interval, plus every process of a known service. Irix cpu (100 = one core). The agent truncates `cmdline` to 1024 characters and redacts secrets (agent-protocol §Redaction, `internal/agent/redact`) before sending; the server redacts again on ingest and shortens it for display. | procstat scan + topk |
| Services | Discovery: listening TCP sockets → pid → systemd unit (via `/proc/<pid>/cgroup`) or process. Reported in `hello`, when they change, and at least every 5 minutes while connected; see agent-protocol v1.2. | `services/*.json` (hand-written) |
| Health | `probe_health` per configured check: `http://127.0.0.1:<port><path>` or a TCP connect. The port must be one the service is listening on (D12). No redirects; timeout ≤ 10 s; body not stored, only status, latency, and result. | `inputs.http_response` |
| Live tasks | `host_snapshot`, `probe_health` | — |

A services report is the host's full set, so the agent sends one only from a complete discovery: the process scan succeeded and, when the program policy is enabled, the helper's owned-program lookup succeeded. If either fails, that cycle sends no `services` frame and the previous report stays. `hello` includes `services_complete`, which is false with `services` empty when the first discovery did not finish, so the server does not store that hello as the full set (agent-protocol v1.3.2).

The user-scope systemd caveat from spec-v1 still applies: a system user cannot read another user's bus. Units are inferred from the cgroup path, which needs no bus.

## Phase 2: logs, deploy, run/stop

- **Docker socket proxy:** a separate unit, the only process that opens the engine socket (raw socket access is root-equivalent). It listens on `/run/bot-agent/docker.sock` (mode 0600, owner `bot-agent`). Allowed calls:
  - `GET /containers/json`, `GET /containers/{id}/json`, `GET /containers/{id}/logs`;
  - `POST /containers/{id}/start`, `POST /containers/{id}/stop`;
  - `GET /images/{name}/json`, `POST /images/create` (pull, no registry auth);
  - `POST /containers/create`, only after the body is validated. It rejects `Privileged`, `CapAdd`, `Devices`, `Binds`, bind-type `Mounts`, `NetworkMode=host`, `PidMode=host`, `IpcMode=host`, `UsernsMode=host`, and `User` 0/root. It requires the label `dev.deploybot.owner=bot-agent`.

  - *For redeploy (umbrella §5.3, D30):* `POST /containers/{id}/rename` and `DELETE /containers/{id}` (never `v=1`), only for containers labelled `dev.deploybot.owner=bot-agent`.

  Everything else is denied: build, exec, attach, kill, commit, volumes and networks endpoints, and swarm. Body validation is why a small custom proxy is recommended (umbrella D26).
- **Owned list:** `/var/lib/bot-agent/owned` (mode 0600), one container name per line. Only a successful `deploy` adds a name and only undeploy removes one. `logs`, `run`, and `stop` refuse names that are not on it (`not_owned`), whatever the server says.
- `deploy`: if a container with `container_name` exists, adopt it (owned list only; the container is untouched). Otherwise pull the image if needed, create the container from the closed spec (umbrella §5.2), start it, and add it to the list.
- `redeploy` (umbrella §5.3, D30): created containers only. Pull first, stop and rename to `<name>-prev`, create and start the new container, wait for health, and on failure roll back to `-prev`.
- Secrets (umbrella §5.4, D31): values arrive only inside a deploy/redeploy task frame as `secret_env`. They go straight into the Docker create body. They are never written to `tasks/`, the spool, or the journal, and are redacted by exact value from logs and results. `status` results never include `Config.Env`.
- `logs`: tail 1–1000 lines, stdout/stderr flags, no follow. Redacted with the shared regex and capped at 64 KiB. Viewers may request it (umbrella D19).
- One mutating task per service at a time. Task files are kept under `/var/lib/bot-agent/tasks/<task_id>.json` so a result survives a reconnect. A mutating task that was not finished when the process restarted is reported `failed`/`agent_restarted` and is not re-run.

## Every task, before running

1. `host_id` matches the enrolled host.
2. `expires_at` has not passed (clock corrected by the `hello_ok.server_time` offset).
3. `kind` is in this phase's allowlist and in the build's capabilities.
4. The service is known; for logs/run/stop it is also owned.
5. `spec` keys are from that kind's closed set.

## State on disk

| Path | Mode | Contents |
|---|---|---|
| `/etc/bot-agent/env` | 0600 | URL, join token or credential |
| `/var/lib/bot-agent/spool/` | 0700 | Unacked metric batches (≤ 24 h or 50 MiB, oldest dropped first) |
| `/var/lib/bot-agent/tasks/` | 0700 | Phase 2 task results until acked |
| `/var/lib/bot-agent/owned` | 0600 | Phase 2 owned container names |

## systemd unit (sketch; Joe installs)

The v1 hardening carries over ([v1 §10](../../docs/design-bot-agent-deploy.v1.md)): `User=bot-agent`, no supplementary groups, `NoNewPrivileges`, `ProtectSystem=strict`, `ReadWritePaths=/var/lib/bot-agent /etc/bot-agent`, empty `CapabilityBoundingSet=`, `InaccessiblePaths=/run/docker.sock /var/run/docker.sock`, and `EnvironmentFile=/etc/bot-agent/env` (no leading `-`). Phase 1 additionally needs read access to `/proc` (for other users' processes: `ProtectProc=default`, not `invisible`) and to `/sys/class/thermal`. Unit `bot-agent.service`, binary `/usr/local/bin/bot-agent` (accepted). The legacy `bot_agent.service` and `/usr/local/bin/bot_agent` are removed by Joe on pi2, pi3, and pi4 after the new agent is up (umbrella D24). The agent's installer never touches them.

bot-agent replaces Telegraf completely (umbrella D23). After a host's overlap ends, Joe removes Telegraf there.

grok-bot-box has no systemd. It starts from a script that Joe reruns after every restart or wake, replacing `grokbox-metrics-start` (umbrella D27).

## Tests (when implemented)

- A fake server that speaks the umbrella §4 protocol: enroll, hello, rotation, ingest ack and replay, and tasks with wrong host, expired, forbidden kind, not owned, and bad spec.
- A collector golden test against fixture `/proc` and `/sys` trees.
- Probe targets: rejects ports not exposed, other hosts, and redirects.
- Phase 2: a stub Docker API asserting that only the allowed calls are made. The proxy rejects every forbidden create field. `logs`, `run`, and `stop` refuse names not on the owned list. Deploy adopts an existing name without changing it.
- `go test ./...` keeps covering `internal/render` until it is retired.

## Programs (v1.3)

Wave 1 of `docs/spec-project-catalog.md`, amended by agent-protocol v1.3.1, is a root helper, not a capability on `bot-agent.service`. `cmd/bot-agent-runner` is installed at `/usr/local/libexec/bot-agent-runner` (`root:root`, `0755`). systemd accepts `/run/bot-agent-runner.sock` and starts one `serve-conn` process per connection. The agent dials that socket. The connect budget is 5 seconds. The rest of the call uses the task timeout (`timeout_sec`, clamped to 15–60 seconds); hello and discovery keep a 5 second limit. It does not resolve paths under `/home`. The helper re-validates the root-owned policy and the task, including realpath and exec resolution, writes `/run/systemd/system/deploybot-<project>-<entry>.service` (`root`, `0644`), `/run/bot-agent/env/<unit>.env` (`root`, `0600`), and the same request at `/run/bot-agent/env/<unit>.json` (`root`, `0600`), and reloads systemd. `packaging/tmpfiles.d/bot-agent-runner.conf` creates `/run/bot-agent` and `/run/bot-agent/env` at boot (`0700` `root:root`); the helper creates them too. A non-zero `systemctl restart`, a timestamp that does not move, or a failed `daemon-reload` removes that start's files and is `start_failed`. `max_programs` counts unit files, not only active units. Restart re-checks the stored request against the current policy and stops a unit that is no longer allowed. The helper refuses a unit file it did not create. Tests drive systemd, journalctl, and the socket through fakes. See `NOTES.md` for what still has to be confirmed on pi1.

Threat model, from the catalog spec: a compromised API cannot change `/etc/bot-agent/runner.toml`; a compromised agent stays unprivileged and cannot exceed that file, because the helper checks every request again and the peer uid must be `bot-agent`. Polkit can constrain unit names and still let a transient unit run as root, which is why the helper is the privileged step. There is no shell unless `allow_shell` is true. Env values are not logged. `bot-agent.service` gains no capability. `ProtectHome=read-only` on a program unit leaves other homes readable, and an allowlisted interpreter runs the caller's arguments as `run_as`. Both follow from the host owner choosing `run_as` and `exec_allow`.

Rollback is local. Set `enabled = false` and restart the agent, or `systemctl disable --now bot-agent-runner.socket`. New tasks stop. Units already started keep running until an administrator runs `systemctl stop 'deploybot-*'`. The agent reads the policy only at startup, so `enabled = false` does not take effect until that restart. After that it no longer reports those units as owned services, and it will not stop them itself.
