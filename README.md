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

A services report is the host's full set, so it is sent only from a complete discovery. If the process scan fails, or the program policy is enabled and the owned-program lookup fails, or images are enabled and `container_list` fails, that cycle sends nothing and the previous report stays. `hello` carries `services_complete`. It is false, with `services` empty, when the first discovery did not finish, so the server does not treat that hello as the full set. See agent-protocol v1.3.2.

## Programs (agent-protocol v1.3.1)

`bot-agent` stays an unprivileged user. `packaging/bot-agent.service` keeps `NoNewPrivileges=yes`, an empty `CapabilityBoundingSet=` and `AmbientCapabilities=`, `ProtectSystem=strict`, and `ProtectHome=yes`. It hides the Docker sockets and the managed-file store with `InaccessiblePaths=-/run/docker.sock -/var/run/docker.sock -/var/lib/bot-agent-runner`. The leading `-` lets a host that has no such path start. That sandbox cannot see `/home` and cannot run a useful `sudo`. The agent therefore does no filesystem checks on a program directory. It dials `/run/bot-agent-runner.sock`. The root helper re-reads `/etc/bot-agent/runner.toml`, resolves the directory and the executable, and writes an installed system unit under `/etc/systemd/system`.

The helper is socket-activated. There is no sudoers line.

- `bot-agent-runner.socket` listens on `/run/bot-agent-runner.sock` with `SocketUser=root`, `SocketGroup=bot-agent`, `SocketMode=0660`, and `Accept=yes`.
- Each connection starts `bot-agent-runner@.service` as root. `ExecStart` is `/usr/local/libexec/bot-agent-runner serve-conn`. The process reads one newline-terminated request from the socket and writes one JSON response.
- `serve-conn` checks `SO_PEERCRED` on that socket. The peer uid must be the `bot-agent` account. Anyone else is refused.

A compromised API cannot widen what this host will run: the policy file is root-owned and is not writable by `bot-agent`. A compromised agent can ask only for what that policy already allows, and only by connecting to that socket. Polkit is not a substitute. It can limit unit names, not the command a unit runs. Shell mode (`/bin/sh -c`) is off unless `allow_shell` is true. Request env values are written to the root-only env file and are not logged. Keys that change which code runs (`LD_*`, `DYLD_*`, `NODE_OPTIONS`, and the rest of the v1.6.1 list) are refused, including a hoisted `KEY=value` argument.

`ProtectHome=read-only` on a program unit leaves other home directories readable. That follows from the policy model: the host owner chooses `run_as`, and that account can read whatever its own permissions already allow. An allowlisted interpreter such as `/usr/bin/node` runs the caller's arguments as `run_as`. The host owner chooses `exec_allow`. The helper does not try to decide which of those arguments are safe.

The helper unit is hardened only as far as its job allows. It must write `/etc/systemd/system` and `/var/lib/bot-agent-runner/programs`, keep `/run/systemd/system` writable while a wave-1 unit is still there, create `/run/bot-agent` (the helper lock, the image locks, and the old env directory), run `systemctl` and `journalctl`, read `/home` and `/proc`, and, when image containers are enabled, open `/var/run/docker.sock`. `packaging/bot-agent-runner@.service` sets `ProtectHome=read-only`, `PrivateTmp=yes`, `ProtectKernelModules=yes`, `RestrictAddressFamilies=AF_UNIX`, `NoNewPrivileges=yes`, `RuntimeMaxSec=900`, and `ReadWritePaths` including `/etc/systemd/system` and `/var/run/docker.sock`. It also sets `StateDirectory=bot-agent-runner` and `StateDirectoryMode=0700`, which creates `/var/lib/bot-agent-runner` where `ProtectSystem=strict` would otherwise hide it. The helper creates `programs/` under that directory. The comments in that file say why a stricter sandbox is omitted. Opening that socket is root-equivalent.

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

`/run` is empty at boot. The tmpfiles snippet creates `/run/bot-agent` and `/run/bot-agent/env` as `0700` `root:root`. The helper unit lists `-/run/bot-agent` so a missing directory does not fail namespace setup, and the helper creates those directories itself when it can. Program units are installed, not transient. The helper writes `/etc/systemd/system/deploybot-<project>-<entry>.service` as root `0644`, ending with `[Install]` and `WantedBy=multi-user.target`, and writes the env file and the validated start request as root `0600` under `/var/lib/bot-agent-runner/programs/` (the directory is `0700`). The unit's `EnvironmentFile=` is that programs path. `start` writes the env file, then the spec, then the unit, then runs `daemon-reload`, `enable`, and `restart`. A `\` in the directory, the executable, or the `run_as` home is `policy_denied` before any write. A drop-in at `/etc/systemd/system/<unit>.d` or `/run/systemd/system/<unit>.d` (any type) is `io_error` before `enable` on `start` and `restart`. The helper never creates or removes a drop-in. A `restart` of a unit already in `/etc` does not rewrite it, and still runs `daemon-reload`, `enable`, and `restart`. systemd starts an enabled unit at boot, so a program that was running comes back after a reboot with no agent involved. `stop` runs `disable`, then `stop`, then removes every regular file among the `/etc` unit, env, and spec and an owned `/run` copy. It keeps going when one of those cannot be removed, never unlinks a symlink or other non-regular file, then runs `daemon-reload`. If a file it tried to remove is still there, the reply is `io_error`. A later stop with no unit file removes a leftover spec whose stored project and entry match, and the regular env beside it, and does not report success while that spec remains. Otherwise a stopped program leaves no file in those directories and does not come back at boot. A start whose `daemon-reload` or `enable` fails, or whose unit does not become freshly active, disables and stops that unit, removes the `/etc` unit, env, and spec and an owned `/run` copy, then runs `daemon-reload`. A failed start leaves no file when that cleanup finishes. If a file cannot be removed, or `disable` fails, the reply is `io_error`. If `daemon-reload` fails while `restart` is moving a unit off `/run`, the helper removes the `/etc` files it just wrote and leaves the `/run` files, with no `disable`, `stop`, or second reload. If `enable` fails after that reload, the helper removes those `/etc` files and runs `daemon-reload` again so the `/run` unit is what systemd has loaded. Both replies are `start_failed` with `fresh` false. `max_programs` counts every unit file still on disk, including inactive ones. A name present under both `/etc` and `/run` counts once. Restart reads the stored request and refuses it, stopping the unit and removing its files, when the current policy would no longer allow that command. The next `start` of a unit this helper still owns in `/run/systemd/system` writes the `/etc` copy and then removes the `/run` unit and `/run/bot-agent/env/<unit>.{env,json}`. `restart` writes that `/etc` copy only when it is missing, and removes the `/run` files only after `enable` succeeds. `stop` removes that `/run` copy together with any `/etc` copy. `status`, `logs`, `sockets`, and `list` look in `/etc/systemd/system` first, then `/run/systemd/system`. A unit file with our name but without our markers is not written, enabled, disabled, or removed. With our unit in `/etc`, a foreign same-name file in `/run` is left untouched and does not block the verb; a foreign file in `/etc` always refuses with `not_created`, and an owned `/run` unit behind it still counts for `max_programs` but `list` does not return it. Program units also set `PrivateDevices=yes`, `ProtectKernelTunables=yes`, `ProtectKernelModules=yes`, and `ProtectControlGroups=yes`, and they hide the docker, containerd, and podman sockets.

Rollback for programs: `systemctl disable --now bot-agent-runner.socket`, or set `enabled = false` and restart `bot-agent` (the agent reads the policy only at startup). Either one stops new program tasks. Units that are already enabled keep running, and they come back at the next boot, until an administrator runs `systemctl disable --now 'deploybot-*'` and removes the unit, env, and spec files. After `enabled = false` the agent stops reporting those units as owned services.

## Images (agent-protocol v1.4)

The same helper speaks the image verbs. The agent stays unprivileged and still dials `/run/bot-agent-runner.sock`. One root process, `bot-agent-runner`, opens `/var/run/docker.sock` (Engine API `/v1.47`). The helper builds every Engine request from typed fields. A request never arrives at Docker as a path, query, header, or body.

Threat model, from spec §3:

- One root process opens the socket. The agent unit keeps the Docker sockets inaccessible.
- No pass-through. Verbs are typed JSON, one object per connection. `exec`, `attach`, and archive are not reachable.
- `runner.toml` lists the project slugs this host may run. The helper writes `dev.deploybot.owner`, `host`, `project`, and `entry` itself. A container under `<project>-<entry>` or `<project>-<entry>-prev` with different labels is `conflict`, and nothing is changed.
- Env values, registry tokens, and secret file contents reach the agent in the task frame and are passed to the helper for that task. A compromised agent can read the secrets of the tasks it receives. That is the same trust the agent already has for its host (spec §3.4). The helper does not return them. List and status are projections. Results and log lines are redacted for the registry token and the secret-file values described under managed files, and for env values: every env value when `secret_keys` is absent, or only the listed keys when `secret_keys` is present. A listed key that is not in `env` is `bad_spec`. Values of at least 6 bytes are removed with the raw bytes, standard and URL base64 (padded and raw), and both JSON-escaped forms, and then with the v1.0.1 regex. `outcome`, `digest`, `exit_code`, and status fields are passed through as the helper returned them. Lines are cut to 2048 characters and 64 KiB after that. A deploy or rollback tail sends that set as `container_logs` `redact` (at most 2048 values, each 6–4096 bytes, at most 512 KiB, no NUL). The helper removes those forms from each raw line before the regex and the cut, and it does not log or return the list. `image_logs` sends no `redact`. That filter is best-effort, not a boundary: a command can print a value in another encoding or send it off the host (spec §12.9).
- A caller can ask only for the image patterns, the listed projects, project volumes, the project bridge network, and managed files. The only host bind mounts are the read-only file mounts the helper builds itself. There are no extra capabilities, devices, or host namespaces. A public port needs `allow_public_ports`.

Verbs: `image_pull`, `container_deploy`, `container_rollback`, `container_remove`, `container_start`, `container_stop`, `container_restart`, `container_status`, `container_logs`, `container_list`. Program verbs keep `{"ok":false,"reason":"..."}`. Image verbs return `{"error":"policy_denied"|"bad_spec"|"conflict"|"docker_unavailable"|"image_unavailable"}` or the success object in the v1.4 table. The agent accepts either refusal shape.

The agent does not import the helper. Task kinds are `image_deploy`, `image_rollback`, `image_remove`, `image_start`, `image_stop`, `image_restart`, `image_status`, and `image_logs`. `service` is null. An unknown spec key, a missing required key, or a non-null `service` is `bad_spec` before the policy is consulted. After the v1.0 host, expiry, and kind checks, an image task is refused in this order: images are not enabled (`policy_denied`); `project` is not in `projects` (`policy_denied`); the ImageSpec or the entry fields fail (`bad_spec`); the image does not match a pattern (`policy_denied`). Start, stop, restart, status, remove, and logs have no ImageSpec and no pattern check. `image_logs` checks `lines` (1..2000) after the policy and the project. An image task spec may be as large as the agent frame (1 MiB). Image verbs on the helper accept up to 2 MiB. Program verbs stay at 64 KiB. A helper body over its verb's limit finishes as outcome `failed`.

`hello.programs.images` is true when the policy is valid and enabled, both lists are non-empty, and no listed project is another plus `-` plus more characters. `hello.programs.docker` is true when `container_list` succeeded on the first discovery of this process. The first answer latches the flag. `hello.programs.files` is true whenever that object is sent: this agent and its helper implement managed files. It does not depend on the policy. Patterns and project lists are not sent. The flags are present whenever the policy file exists, including when `images` or `docker` is false. There is no images capability.

An image task honours the frame's `timeout_sec` for the whole sequence, clamped to at most 4020 seconds. Zero or negative uses 15 seconds. Service deploy pulls, then reads `container_status`. A running container with this digest, this `spec_hash`, and this `files_hash`, and `force` false, is `unchanged`. The agent computes the expected `files_hash` from the task. With no files that value is empty. Otherwise the agent deploys and waits. A configured health check whose service is the entry, whose protocol is `http` or `tcp`, and whose port is one of the spec's host ports is probed on `127.0.0.1`. One passing round while the container is `running` is success. With no such check, the container must stay `running` with `restart_count` 0 for `healthy_after_sec`, polled every 2 seconds. Unhealthy calls `container_rollback`: `rolled_back`, or `rollback_failed` when that call fails. A `container_status` error during that wait, including `docker_unavailable`, is outcome `failed` and does not roll back. A job pulls, removes the container when one is present, deploys, and polls every 2 seconds until `exited` or the spec's `timeout_sec`. Exit 0 is `succeeded`. Any other exit is `failed` with `exit_code`. A timeout stops the container and the outcome is `timeout`. Cancelling the task context is `failed` and does not stop the container. A service `image_rollback` whose digest equals `container_status` `prev.digest` does not pull or deploy: it calls `container_rollback` (the kept `-prev` container and its slot come back), waits healthy as a rollback deploy does (`healthy_after_sec`, at least 30 seconds), and reports `succeeded` with that digest, or `failed` when the swapped container is not healthy. Any other digest, and a job rollback, is still that service sequence, pulling `<name>@<digest>`. Start, stop, restart, status, remove, and logs are one verb each.

`task_result.status` is `succeeded` for outcomes `succeeded` and `unchanged`. Every other outcome is `failed` with `error_code` set to the outcome. `image_unavailable` and `conflict` stay those outcomes. `policy_denied` and `bad_spec` from the helper become `forbidden_spec`. `docker_unavailable` and a transport failure become `failed`.

When images are on, every discovery cycle calls `container_list`. A container with all four `dev.deploybot.*` labels, this host's label, and a project in `projects` is an owned service whose name is exactly its entry, with `owner.kind` `image` and `image_digest` set when the digest is `sha256:` plus 64 hex. A non-owned row that already uses that name is renamed. An owned row that already uses it, including an owned program, keeps the name, and the extra container is not given a suffixed name. The host label is the first 16 hex characters of SHA-256 of `/etc/machine-id` when the agent can read that file. If it cannot, any non-empty host label is accepted, and the agent logs that once without the path. A `<project>-<entry>-prev` container with the same four labels is not a second service; its published ports are still kept off the unmapped list. If only the `-prev` container exists, it is the owned service and `container_name` keeps that name. Every other container is `kind` `container`, named by its container name without one leading `/`, with `image_digest` null and `"owner":null`. A name outside the service name pattern is sanitized; `container_name` keeps the trimmed Docker name. Published host ports are not also reported as unmapped listeners. The helper returns at most 200 containers, deploybot-labelled rows first, and sets `truncated` when it cut. A truncated list is still complete, and the agent logs that once. A failed `container_list` makes that cycle incomplete.

Env values, `registry_auth.token`, and file contents are wiped from the task spec when the task ends. The pending result does not carry them. The merged env and the file contents sent to `container_deploy` hold the real values, and the token is sent on `image_pull`. `spec.secret_env` stays the `<set>.<KEY>` reference. `spec.files` stays names and targets.

`packaging/runner.toml.example` leaves these four keys commented out. Uncommenting them gives the helper root-equivalent container access.

| Key | Meaning |
|---|---|
| `images` | Anchored reference patterns. `*` is one path component. The host is literal. |
| `projects` | Project slugs this host may run. |
| `max_containers` | 1..200 deploybot-labelled containers, default 20. |
| `allow_public_ports` | When false, a `public` port is `policy_denied`. |

An invalid key or value makes the whole policy invalid. An empty `images` or `projects` list is valid and leaves image verbs off. When one listed project is another plus `-` plus more characters, as with `acme` and `acme-web`, the policy stays valid for programs and image verbs are refused.

Rollback for containers: set `images = []` (or remove `projects`) in `runner.toml`. The helper then refuses image verbs. Containers it already created keep running under their restart policy. Remove them by hand if they should stop. Taking `/var/run/docker.sock` out of `ReadWritePaths` is the other switch.

## Managed files (agent-protocol v1.5)

`image_deploy` and `image_rollback` carry a top-level `files` list, in the same order as `spec.files`. Each item is `name`, `target`, `content`, and `secret`. Content is not stored in `spec.files`. The agent checks the two lists one for one, and it checks the name, content, and target rules (api-v1 v1.9 rules 1–2 and 13, including a file target under a volume target and the reverse). That failure is `bad_spec`, the same task reject as any other bad image spec. The helper checks the same rules again on `container_deploy` and returns `bad_spec`.

The helper keeps the bytes at `/var/lib/bot-agent-runner/files/`. One slot directory per container: `<project>-<entry>`, `<project>-<entry>-prev`, and the transient `<project>-<entry>.new`. A file is `<slot>/<name>`. Slots are mode 0700 and files are mode 0444, both root-owned. The helper opens the store with `O_DIRECTORY|O_NOFOLLOW` and then only uses `openat`, `mkdirat`, `renameat`, and `unlinkat` on directory handles. It does not follow a symlink. Before every use it checks that the store and the slot are directories owned by root with mode 0700, and that each file is a regular file owned by root, mode 0444, link count 1. Anything else is `bad_spec`, and that verb does not change a container or delete the bad entry. A verb with no files treats a missing `files/` directory, or a missing parent of it, as "no store" and continues without creating either. A request that carries files creates `files/` when the parent already exists. A missing parent on that request is `docker_unavailable` and changes nothing; the helper does not create the parent. Any other I/O error opening the store is `docker_unavailable` and changes nothing.

Each file is one bind mount, `ReadOnly: true`, with no `BindOptions`. `Source` is always `/var/lib/bot-agent-runner/files/<slot>/<name>`. The helper builds that string. It is not taken from the request. When `files` is non-empty the container gets the label `dev.deploybot.files_hash`, the hex SHA-256 of the lines `<name>:<hex sha256 of content>`, sorted by name, each line ending in a newline. With no files the label is absent. `container_status` returns it as `files_hash`, or `""` when the container has none. Projections do not include content, slot paths, or mounts.

Trust boundary, from the catalog spec §3:

- The bind source is never a path from the request.
- A symlink, a wrong mode, a wrong owner, or a link count other than 1 refuses deploy, rollback, and remove before any container is touched.
- Mounts are read-only. There is no `BindOptions`, so propagation stays Docker's default.
- The agent unit cannot open the store (`InaccessiblePaths=-/var/lib/bot-agent-runner`). The agent sees secret contents only inside the task frame.
- Secret file contents are redacted by exact value. The whole content is included when it is 6–4096 bytes. Every trimmed line of 16–4096 bytes is included. When the content parses as JSON, every string value of 16–4096 bytes is included, and so is every trimmed line of that size inside a string value, which covers a PEM line printed on its own. A plain file adds nothing.

`container_logs` `redact` holds at most 2048 of those values, each 6–4096 bytes, at most 512 KiB altogether, and no NUL. A deploy tail clips the list to that cap. The exact set kept in the agent process still covers a value the list had to drop. Results and the agent's own logs do not contain file content.

`StateDirectory=bot-agent-runner` is what makes `/var/lib/bot-agent-runner` writable for the root helper. The helper creates `files/` only for a request that carries files, and only when that parent already exists. A missing parent on a request with files is `docker_unavailable` and does not change a container. A verb with no files still deploys, rolls back, or removes the container when `files/` or its parent is missing, and it does not create either directory. These two unit lines, and the agent's `InaccessiblePaths` entry, are host security settings. Review them before install. Install the helper unit, so the state directory exists, before deploying an entry that has files.
