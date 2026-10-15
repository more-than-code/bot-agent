# bot-agent

Telegraf snippets and the SQL for the bot-console Services page. Umbrella: `/home/joe/workspace/deploybot`.

This repo does not install Telegraf, does not apply SQL, and does not restart anything. Those steps stay with Joe.

## Manifests

A bot-agent compatible service is one JSON file in `services/<name>.json`. The stem matches `name`. `nodes.json` is the cluster member list and whether the host has systemd. grok-bot-box does not.

```json
{
  "name": "tra-backend",
  "project": "tour-agent",
  "host": "pi1",
  "port": 3031,
  "health_url": "http://127.0.0.1:3031/health",
  "systemd_unit": "tra-backend.service",
  "systemd_scope": "user",
  "process_match": "/home/joe/workspace/tour-agent/tra-backend"
}
```

`health_url` is loopback only, and its port must match `port`. `process_match` is a cmdline substring (letters, digits, `/._:@-` and spaces). It is the procstat pattern. Node reports every process as `MainThread`, so a workspace path is what tells the services apart. The field list is in `docs/spec-v1.md` (Services).

Hosts and units below were checked read-only on 2026-10-12. Twelve manifests:

| Service | Host | Port / health | Unit | Scope | process_match |
|---------|------|---------------|------|-------|---------------|
| tra-backend | pi1 | 3031 `/health` | tra-backend.service | user | `/home/joe/workspace/tour-agent/tra-backend` |
| tra-webapp | pi1 | 5176 `/` | tra-webapp.service | user | `/home/joe/workspace/tour-agent/tra-webapp` |
| ttd-backend | pi1 | 3001 `/health` | ttd-backend.service | user | `/home/joe/workspace/tutored/ttd-backend` |
| ttd-webapp | pi1 | 5173 `/` | ttd-webapp.service | user | `/home/joe/workspace/tutored/ttd-webapp` |
| bfd-backend | pi1 | 3021 `/health` | none (tmux) | | `/home/joe/workspace/briefed/bfd-backend` |
| bfd-webapp | pi1 | 5174 `/` | none (tmux) | | `/home/joe/workspace/briefed/bfd-webapp` |
| bot-console | pi2 | 5180 `/` | bot-console.service | user | none |
| bot-agent-pi2 | pi2 | 8081, no `/health` | bot_agent.service | system | `bot_agent` |
| bot-agent-pi3 | pi3 | 8081, no `/health` | bot_agent.service | system | `bot_agent` |
| bot-agent-pi4 | pi4 | 8081, no `/health` | bot_agent.service | system | `bot_agent` |
| postgres | pi4 | 5432, no HTTP | postgresql@18-main.service | system | `postgres` |
| pgadmin | pi4 | 5050 `/` | none (hand-started) | | `pgadmin4` |

`bot-console` has no `process_match`. Its MainPID is already `node`, and the command line is `node build`. The workspace path is the working directory, not an argument, so a path pattern would drop the only process in that cgroup. The cgroup filter identifies it. `postgresql.service` is an exited wrapper (MainPID 0); the real unit is `postgresql@18-main.service`. pgAdmin and Briefed have no unit. `bot_agent` has no health route, so those three rows have no `health_url`. Names are distinct because `metrics.services` is keyed by name only.

```bash
go run ./cmd/render-telegraf
go run ./cmd/render-telegraf --check
go test ./...
```

`cmd/render-telegraf` writes `telegraf/<host>.conf` and does not talk to a node. Run the commands above from the repo root. `--check` exits non-zero when a file is missing, stale, or extra. The snippet banner still says `render_telegraf.py` so the checked-in files stay byte-identical.

## What each snippet collects

Every host gets one procstat scan: `patterns = ['.+']`, filter name `user`, no `recursion_depth`. `processors.topk` keeps 20 series for CPU and 20 for RSS (60s). Processor `order` is 1 (rename `procstat` to `procstat_scan` when `filter = user`), 2 (`topk`), 3 (rename back). Declared service filters skip topk. `pid`, `user`, and `cmdline` are tags. The field list is `fieldinclude`. `fieldpass` is deprecated on Telegraf 1.40 and logs DeprecationError.

Service filters set `recursion_depth = -1` so the node grandchild of pnpm is kept. The scan filter does not. A `process_match` is AND-ed with the service selector.

A template instance (`@` in the unit name) is not selected with procstat `systemd_units`. Telegraf 1.40.1 lists unit files, and `postgresql@18-main.service` is only an instance of `postgresql@.service`, so that selector returns no processes and no error. The filter uses the cmdline pattern alone and does not set `recursion_depth`: the backends already match, and recursion emits each one a second time. The cgroup is not used. A backend that exits while `cgroup.procs` is read drops the whole filter. `inputs.systemd_units` still lists the instance.

Telegraf runs as user `telegraf`. `inputs.systemd_units` with `scope = "user"` reads telegraf's own bus, and procstat's `systemd_units` selector uses the system bus only. User-scope services therefore have no systemd input. Their procstat filter uses an absolute cgroup:

`/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/<unit>`

A relative `user.slice/...` path does not match on Telegraf 1.40.1 (`telegraf --test` on pi2: only the absolute path emitted a metric). Health for those services is HTTP plus process presence, not a unit state.

System-scope units use procstat `systemd_units` plus one `inputs.systemd_units` plugin, `scope = "system"`, `details = false`, except a template instance, which uses the cmdline pattern above. `details = true` would send fields the SQL table does not have, and writers cannot create columns (`create_templates = []`).

| Host | Procstat service filters | systemd_units input | http_response |
|------|--------------------------|---------------------|---------------|
| pi1 | cgroup + workspace pattern + recursion for tra-backend, tra-webapp, ttd-backend, ttd-webapp; pattern + recursion for bfd-backend, bfd-webapp | none | `:3031/health`, `:5176/`, `:3001/health`, `:5173/`, `:3021/health`, `:5174/` |
| pi2 | cgroup + recursion for bot-console (no pattern); system unit + `bot_agent` + recursion for bot-agent-pi2 | `bot_agent.service` (system) | `http://127.0.0.1:5180/` |
| pi3 | system unit + `bot_agent` + recursion for bot-agent-pi3 | `bot_agent.service` (system) | none |
| pi4 | pattern `postgres` for postgres (template instance, no `systemd_units`, no recursion); system unit + `bot_agent` + recursion for bot-agent-pi4; pattern `pgadmin4` + recursion | `bot_agent.service` and `postgresql@18-main.service` (system) | `http://127.0.0.1:5050/` |
| grok-bot-box | scan only | none | none |

Copy a reviewed file to `/etc/telegraf/telegraf.d/deploybot-services.conf` on that host, then restart Telegraf on the Pis. Do not do that from an agent session. The Pis already start Telegraf with `-config-directory /etc/telegraf/telegraf.d`.

## grok-bot-box install (needs Joe's approval; not applied)

The start script was not read from this machine. SSH to `box@100.101.8.28` failed host-key verification, and there is no local copy of `/home/box/.local/bin/grokbox-metrics-start`. The review observed that the script runs:

```
telegraf --config /etc/telegraf/telegraf.conf
```

and that it skips when Telegraf is already running. Copying a file into `telegraf.d` does nothing until Joe changes that argv and restarts the process. Proposed change, for Joe to make on the box:

```
telegraf --config /etc/telegraf/telegraf.conf --config-directory /etc/telegraf/telegraf.d
```

Then stop the running Telegraf and start it again. Running the script while Telegraf is already up does nothing, so a rerun does not load `telegraf.d`. Do not edit that script from an agent session.

## SQL

`sql/010_procstat_and_services.sql` is idempotent and unapplied. On pi4, as a superuser, against `cluster_metrics`:

- `metrics.procstat` already exists (empty, owner `metrics_owner`, 7-day chunks, an older column set, no retention). The file adds missing columns (`filter`, `cmdline`, and the renderer columns), leaves the old columns, sets the chunk interval to 1 day, and adds a 7-day retention policy. No other raw metrics table has a retention policy. `CREATE TABLE IF NOT EXISTS` does not alter the live table; the `ADD COLUMN` statements do.
- `metrics.systemd_units` and `metrics.http_response` do not exist. Telegraf will not create them (`create_templates = []`). The file creates both as hypertables owned by `metrics_owner`, 1-day chunks, 7-day retention. `http_response` includes `result_type text` because Telegraf 1.40.1 sends that field on every sample. `systemd_units` is the `details = false` column set only.
- Grants: `INSERT` to `metrics_writer`, `SELECT` to `metrics_reader`. No grant to `metrics_grokbox`; that login is a member of `metrics_writer` and inherits `INSERT`. No `INSERT` on `metrics.services`.
- `metrics.services` is the registry, seeded from the twelve manifests. Re-running updates those rows and deletes registry names that are not in the seed (the old `briefed` row). That `DELETE` removes registry rows only.

Destructive once Joe runs it: the 7-day retention policies drop chunks older than 7 days (`procstat` is empty today; the new tables start empty), and the seed `DELETE` removes registry names not in the list. The file does not `DROP` a table. The header comment has a rollback (`remove_retention_policy` and `DROP TABLE`) that is not run. Dropping `metrics.procstat` would destroy the existing table.

About 2 to 3 million procstat rows if all five hosts run the snippet for the full 7-day retention. The other raw metrics tables still have no retention policy.

## bot-agent daemon

`bot-agent run` is the default and takes no arguments. `bot-agent version` prints the agent version. `init` reads the join token from stdin so it does not show up in process arguments:

```bash
printf '%s\n' '<token>' | bot-agent init --server '<wss-url>' --join-token-stdin
```

The token is one trimmed line, 16–128 characters of `[A-Za-z0-9_-]`. `--join-token <tok>` is rejected. Optional `--env-file` selects the env file (default `/etc/bot-agent/env`). Nothing from the env file is printed.

Discovered services are re-sent at least every 5 minutes while connected; see agent-protocol v1.2.

A services report is the host's full set, so it is sent only from a complete discovery. If the process scan fails, or the program policy is enabled and the owned-program lookup fails, that cycle sends nothing and the previous report stays. `hello` carries `services_complete`. It is false, with `services` empty, when the first discovery did not finish, so the server does not treat that hello as the full set. See agent-protocol v1.3.2.

## Programs (agent-protocol v1.3.1)

`bot-agent` stays an unprivileged user. `packaging/bot-agent.service` is unchanged: `NoNewPrivileges=yes`, empty `CapabilityBoundingSet=` and `AmbientCapabilities=`, `ProtectSystem=strict`, and `ProtectHome=yes`. That sandbox cannot see `/home` and cannot run a useful `sudo`. The agent therefore does no filesystem checks on a program directory. It dials `/run/bot-agent-runner.sock`. The root helper re-reads `/etc/bot-agent/runner.toml`, resolves the directory and the executable, and writes a transient system unit under `/run/systemd/system`.

The helper is socket-activated. There is no sudoers line.

- `bot-agent-runner.socket` listens on `/run/bot-agent-runner.sock` with `SocketUser=root`, `SocketGroup=bot-agent`, `SocketMode=0660`, and `Accept=yes`.
- Each connection starts `bot-agent-runner@.service` as root. `ExecStart` is `/usr/local/libexec/bot-agent-runner serve-conn`. The process reads one newline-terminated request from the socket and writes one JSON response.
- `serve-conn` checks `SO_PEERCRED` on that socket. The peer uid must be the `bot-agent` account. Anyone else is refused.

A compromised API cannot widen what this host will run: the policy file is root-owned and is not writable by `bot-agent`. A compromised agent can ask only for what that policy already allows, and only by connecting to that socket. Polkit is not a substitute. It can limit unit names, not the command a unit runs. Shell mode (`/bin/sh -c`) is off unless `allow_shell` is true. Request env values are written to the root-only env file and are not logged. Keys that change which code runs (`LD_*`, `DYLD_*`, `NODE_OPTIONS`, and the rest of the v1.6.1 list) are refused, including a hoisted `KEY=value` argument.

`ProtectHome=read-only` on a program unit leaves other home directories readable. That follows from the policy model: the host owner chooses `run_as`, and that account can read whatever its own permissions already allow. An allowlisted interpreter such as `/usr/bin/node` runs the caller's arguments as `run_as`. The host owner chooses `exec_allow`. The helper does not try to decide which of those arguments are safe.

The helper unit is hardened only as far as its job allows. It must write `/run/systemd/system` and `/run/bot-agent/env`, run `systemctl` and `journalctl`, and read `/home` and `/proc`. `packaging/bot-agent-runner@.service` sets `ProtectHome=read-only`, `PrivateTmp=yes`, `ProtectKernelModules=yes`, `RestrictAddressFamilies=AF_UNIX`, and `NoNewPrivileges=yes`, and the comments in that file say why a stricter sandbox is omitted.

Install, as root:

| Path | Owner | Mode |
|---|---|---|
| `/usr/local/libexec/bot-agent-runner` | `root:root` | `0755` |
| `/etc/systemd/system/bot-agent-runner.socket` | `root:root` | `0644` |
| `/etc/systemd/system/bot-agent-runner@.service` | `root:root` | `0644` |
| `/etc/tmpfiles.d/bot-agent-runner.conf` | `root:root` | `0644` |
| `/etc/bot-agent` | `root:bot-agent` | `0750` |
| `/etc/bot-agent/env` | `bot-agent:bot-agent` | `0600` |
| `/etc/bot-agent/runner.toml` | `root:root` | `0644` |

`/etc/bot-agent` stays `0750` and group `bot-agent` so the agent can read the policy and cannot replace it. Copy `packaging/runner.toml.example` (it ships with `enabled = false`) and `packaging/tmpfiles.d/bot-agent-runner.conf`, install the two units, then:

```bash
systemd-tmpfiles --create /etc/tmpfiles.d/bot-agent-runner.conf
systemctl daemon-reload
systemctl enable --now bot-agent-runner.socket
```

`/run` is empty at boot. The tmpfiles snippet creates `/run/bot-agent` and `/run/bot-agent/env` as `0700` `root:root`. The helper unit lists `-/run/bot-agent` so a missing directory does not fail namespace setup, and the helper creates those directories itself when it can. The helper writes unit files as root `0644`, env files as root `0600`, and the validated start request as root `0600` at `/run/bot-agent/env/<unit>.json`. All of them disappear on reboot. A start whose `daemon-reload` fails, or whose unit does not become freshly active, stops that unit and removes the files. `max_programs` counts every unit file still on disk, including inactive ones. Restart reads the stored request and refuses it, stopping the unit, when the current policy would no longer allow that command. Program units also set `PrivateDevices=yes`, `ProtectKernelTunables=yes`, `ProtectKernelModules=yes`, and `ProtectControlGroups=yes`, and they hide the docker, containerd, and podman sockets.

Rollback: `systemctl disable --now bot-agent-runner.socket`, or set `enabled = false` and restart `bot-agent` (the agent reads the policy only at startup). Either one stops new program tasks. Units that are already running keep running until `systemctl stop 'deploybot-*'`. After `enabled = false` the agent stops reporting those units as owned services.
