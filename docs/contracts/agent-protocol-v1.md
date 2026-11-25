# Agent protocol v1 (bot-agent ↔ bot-api-server), phase 1 contract

**Status:** Frozen for wave 1 (2026-10-13), v1.0.1 (redaction regex fix, tag-type clarification). Source of truth: umbrella `docs/contracts/agent-protocol-v1.md`. Copies in each repo's `docs/contracts/` must not be edited. Report problems in `NOTES.md`.
**Design:** umbrella `docs/design-bot-agent-deploy.md` §3.2 and §4.

## Transport

- The agent dials `BOT_AGENT_SERVER_URL` (must start with `wss://`; `ws://` is accepted only when `BOT_AGENT_INSECURE_DEV=1` is set and the host is `127.0.0.1` or `localhost`, for tests).
- Before enrollment the upgrade has no `Authorization` header. After enrollment it sends `Authorization: Bearer <credential>`.
- Text frames, one JSON object each, UTF-8, at most 1 MiB (the receiver closes on oversize). Every object has `"v": 1` and `"type"`. Unknown `type` values are logged by type only and ignored.
- Server path: `/agent/v1/ws`.
- Heartbeat: the agent sends `{"v":1,"type":"ping","ts"}` every `heartbeat_sec` (20) and the server answers `pong`. The server marks the host offline after 60 s with no ping, measured by arrival time at the server.
- Agent reconnect backoff: 1 s, doubling, capped at 30 s, ±20 % jitter. It resets after a `hello_ok`.

## Enrollment

Agent → server, first frame on an unauthenticated connection:

```json
{"v":1,"type":"enroll","join_token":"<plaintext>","hostname":"pi4","agent_version":"0.1.0",
 "os":"linux","arch":"arm64","always_on":true}
```

Server → agent:

```json
{"v":1,"type":"enrolled","host_id":"<uuid>","credential_id":"<uuid>","credential":"<plaintext>"}
```

Then the server closes the socket (normal close 1000). The agent atomically rewrites its env file (removing `BOT_AGENT_JOIN_TOKEN`, adding `BOT_AGENT_HOST_ID`, `BOT_AGENT_CREDENTIAL_ID`, and `BOT_AGENT_CREDENTIAL`) and reconnects with Bearer.

- `hostname` is 1–63 characters `[a-z0-9-]`. If a live (not revoked) host in the organization already has that name, the server appends `-2`, `-3`, … (wording updated 2026-11-20 with api-v1 v1.38; the rule was per org since v1.24, and revoked hosts no longer count since v1.38).
- The join token must be unexpired, unused, and unrevoked. On success the server marks it used in the same transaction that creates the host and the credential. Credential: 32 random bytes as base64url (43 characters); only its SHA-256 is stored.
- A bad token gets `{"v":1,"type":"error","error_code":"unauthenticated","message":"..."}` and then a close.

## Hello

Agent → server, first frame on a Bearer connection:

```json
{"v":1,"type":"hello","host_id":"<uuid>","credential_id":"<uuid>","agent_version":"0.1.0",
 "os":"linux","arch":"arm64","capabilities":["metrics","probe"],"services":[Discovered, ...]}
```

`Discovered` = `{"name","kind","ports":[int],"systemd_unit":string|null,"systemd_scope":"system"|"user"|null,"container_name":null,"process_name","pid"}`
- `kind` is `systemd` (a unit found from `/proc/<pid>/cgroup`) or `process`. `container` is phase 2.
- `name` is the unit name without `.service` for systemd services, otherwise `process_name`. It must match `^[a-z0-9][a-z0-9._-]{0,62}$`; the agent lowercases it and replaces invalid characters with `-`.
- Only processes with a listening TCP socket (`0.0.0.0`, `::`, or `127.0.0.1`, any port) are reported. Several processes in one unit merge into one entry with the union of their ports.

Server → agent:

```json
{"v":1,"type":"hello_ok","host_id":"<uuid>","heartbeat_sec":20,"server_time":"<RFC3339>","config":Config}
```

`Config` = `{"metrics_interval_sec":10,"process_interval_sec":60,"health_checks":[{"id","service","protocol":"http|tcp","port","path","interval_sec","expect_status_min","expect_status_max"}]}`

- The server may send `{"v":1,"type":"config_update","config":Config}` at any time. The agent replaces its config.
- The agent clamps `metrics_interval_sec` to 5–60, `process_interval_sec` to 30–300, and each check's `interval_sec` to 30–300. It caps the list at 50 checks and drops any check whose `port` is not currently in that service's discovered `ports` (it logs the check id).
- The agent sends `{"v":1,"type":"services","services":[Discovered,...]}` whenever its discovered set changes (checked every 60 s). The server upserts these as `source='discovered'` (and never downgrades a `declared` row) and updates `last_seen_at`.
- Errors before `hello_ok`: `error_code` `unauthenticated` (bad credential), or `forbidden` (credential revoked, or `host_id` does not match the credential). Then the socket closes. On `forbidden` the agent does not retry until it restarts. On `unauthenticated` it backs off to 30 s and keeps retrying.

## Metrics ingest

Agent → server:

```json
{"v":1,"type":"metrics","batch_id":"<uuid>","samples":[{"m":"cpu","t":"<RFC3339>","tags":{...},"f":{...}}]}
```

Server → agent: `{"v":1,"type":"metrics_ack","batch_id":"<uuid>"}`. It is sent only after the samples are committed, and also for a replayed `batch_id` (which is not inserted twice). A batch holds at most 5000 samples. The agent sends a batch every `metrics_interval_sec`.

| `m` | tags (allowlist) | fields (allowlist, types) |
|---|---|---|
| `cpu` | `cpu` (only `cpu-total`) | `usage_user`, `usage_system`, `usage_idle`, `usage_iowait` (float %) |
| `mem` | — | `total`, `available`, `used` (int bytes), `used_percent` (float) |
| `disk` | `path`, `fstype` | `total`, `used`, `free` (int bytes), `used_percent` (float) |
| `diskio` | `name` (whole disks only: `mmcblkN`, `nvmeXnY`, `sdX`, `vdX`) | `read_bytes`, `write_bytes` (int, cumulative) |
| `net` | `interface` (not `lo`) | `bytes_recv`, `bytes_sent` (int, cumulative) |
| `system` | — | `load1`, `load5`, `load15` (float), `uptime` (int s), `n_cpus` (int) |
| `temp` | `sensor` | `celsius` (float) |
| `process` | `pid`, `process_name`, `user`, `cmdline` (≤ 1024 characters, raw), `service` (optional Discovered name) | `cpu_usage` (float, irix: 100 = one core), `memory_rss` (int bytes), `memory_usage` (float %), `num_threads` (int), `started_at` (RFC3339 string) |
| `health` | `service`, `check_id`, `protocol`, `port` | `result` (`success`, `status_mismatch`, `timeout`, `connection_refused`, `error`), `http_status` (int or null), `latency_ms` (float) |

- **Tag values are always JSON strings** (e.g. health `port` is `"8080"`); field types are as in the table. *(v1.0.1 clarification.)*
- The server rejects (logs, and does not ack) a batch containing an unknown `m`, tag, or field, or a wrong type. It ignores `host_id` claims inside samples; the host always comes from the connection.
- Processes: each `process_interval_sec`, the top 20 by `cpu_usage` plus the top 20 by `memory_rss` (union), plus every process of a discovered service.
- `disk` covers real filesystems only (no tmpfs, overlay, squashfs, proc, sysfs, devtmpfs).

## Tasks (phase 1 kinds)

Server → agent:

```json
{"v":1,"type":"task","task_id":"<uuid>","host_id":"<uuid>","kind":"host_snapshot|probe_health",
 "service":"<name>|null","expires_at":"<RFC3339>","timeout_sec":15,"spec":{...}}
```

The agent checks these in order. The first failure gives `{"v":1,"type":"task_reject","task_id","error_code"}` and nothing runs:
1. `host_id` equals its own (`wrong_host`).
2. Now (corrected by `server_time` − local time at `hello_ok`) is before `expires_at` (`expired`).
3. `kind` is `host_snapshot` or `probe_health` (`forbidden`).
4. `spec` keys are from the kind's closed set (`bad_spec`).
5. For `probe_health`: `service` is discovered locally (`unknown_service`) and `spec.port` is in its current ports (`forbidden`).

Specs:
- `host_snapshot`: `{}`.
- `probe_health`: `{"protocol":"http|tcp","port":int,"path":string|null,"expect_status_min":int,"expect_status_max":int}`. The target is always `127.0.0.1:<port>`. HTTP is a `GET`, does not follow redirects, and reads at most 64 KiB of body (discarded). The timeout is `min(timeout_sec, 10)` s.

Agent → server: `{"v":1,"type":"task_result","task_id","state":"succeeded|failed|timed_out","error_code":null|string,"payload":{...}}`. The agent resends it on reconnect until it gets `{"v":1,"type":"task_ack","task_id"}`. Phase 1 results are kept in memory only.

## Payloads

```jsonc
Snapshot {"sampled_at","cpu_percent","mem":{"total","used","available","used_percent"},
          "disks":[{"path","total","used","used_percent"}],"load":[l1,l5,l15],
          "temp_celsius":number|null,"uptime_sec","top":[{"pid","process_name","user","cmdline","cpu_usage","memory_rss"}]} // top ≤ 20 by cpu
ProbeResult {"service","protocol","port","path","result","http_status":int|null,"latency_ms","checked_at"}
```

## Redaction

The agent never logs a frame body, token, or credential. It truncates `cmdline` to 1024 characters and replaces any match of `(?i)(authorization|api[_-]?key|password|passwd|token|secret|credential)(\s*[=:]\s*|\s+)(bearer\s+)?\S+` with `$1=[REDACTED]` before sending. *(v1.0.1, 2026-10-13: the v1.0 pattern missed `Authorization: Bearer x` and `token: x`.)*

## v1.0.2 amendments (2026-10-14)

These take precedence over the sections above.

1. **Redaction (agent, and server on ingest and snapshots).** Apply in this order:
   - URL userinfo: `(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@]+:)[^@\s/]+@` → `$1[REDACTED]@`.
   - Then keywords: `(?i)([A-Za-z0-9_.-]*(?:authorization|api[_-]?key|password|passwd|token|secret|credential)[A-Za-z0-9_.-]*)(\s*[=:]\s*|\s+)(bearer\s+)?("[^"]*"|'[^']*'|\S+)` → `$1=[REDACTED]`.

   Required test vectors (input → output):

   | Input | Output |
   |---|---|
   | `Authorization: Bearer abc123` | `Authorization=[REDACTED]` |
   | `AWS_SECRET_ACCESS_KEY=AKIAxyz` | `AWS_SECRET_ACCESS_KEY=[REDACTED]` |
   | `password="two words" next` | `password=[REDACTED] next` |
   | `postgres://app:s3cret@127.0.0.1/db` | `postgres://app:[REDACTED]@127.0.0.1/db` |
   | `redis://localhost:6379` | unchanged |
   | `/usr/bin/node build` | unchanged |
   | `postgres: joe tour_agent 127.0.0.1(5000) idle` | unchanged |

   Over-redaction such as `--token-file=[REDACTED]` is accepted.
2. **`task_reject` code `busy`.** The agent runs host, expiry, kind, and spec checks before queuing. It runs at most 4 tasks and holds at most 16 waiting; beyond that it sends `task_reject` with `busy`. The server maps `busy` to `unavailable` (503).
3. **Outbound frame size.** The agent splits `metrics` batches so that each serialized frame is ≤ 1 MiB, as well as ≤ 5000 samples.
4. **Join token via stdin.** `bot-agent init --server <url> --join-token-stdin` reads the token from stdin (one line, trimmed). The `--join-token <tok>` argv form is removed, because argv is visible in `ps`.

## v1.0.3 amendment (2026-10-14)

The URL-userinfo pattern is replaced by `(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@]*:)[^\s/]*@` → `$1[REDACTED]@`. The keyword pattern is unchanged.

Additional required vectors:

| Input | Output |
|---|---|
| `redis://:s3cret@127.0.0.1:6379` | `redis://:[REDACTED]@127.0.0.1:6379` |
| `https://user:p@ss@host/db` | `https://user:[REDACTED]@host/db` |
| `ssh://git@host:22/repo` | unchanged |
| `http://127.0.0.1:3031/health` | unchanged |
| `a://u:p@h/x b://c:d@e/y` | `a://u:[REDACTED]@h/x b://c:[REDACTED]@e/y` |

## v1.1 discovered owner (2026-10-14, additive)

`Discovered` gains `"user": string | null`: the username of the uid that owns the listening socket. Read it from the `uid` column of `/proc/net/tcp{,6}` and resolve it via `/etc/passwd`; use the numeric uid as a string if the name is unknown. No privilege needed. It is sent for mapped and unmapped listeners alike. Servers that predate v1.1 ignore it. Servers that know v1.1 store it as `services.owner_user`.

## v1.2 periodic services (2026-10-14, approved by Joe)

Supersedes the "whenever its discovered set changes (checked every 60 s)" sentence in the session section, additively.

1. The agent sends `services` when its discovered set changes (checked every 60 s), **and also at least every 300 s while connected even if the set is unchanged**. A changed set is sent at once and restarts the 300 s timer.
2. The server treats every `services` message as proof that the listed services are present at that time and updates their `last_seen_at`. A service missing from the message is not changed; its `last_seen_at` stays at the last time it was reported.
3. No new fields. Servers that predate v1.2 already upsert `last_seen_at` on every message.

## v1.3 programs (2026-10-15, approved spec `docs/spec-project-catalog.md`, wave 1; additive)

### Host policy

`/etc/bot-agent/runner.toml`, written by the host's administrator, never by Deploybot:

```toml
enabled = true
run_as = ["joe"]                  # never root, never uid < 1000
dirs = ["/home/joe/workspace"]    # realpath of a program dir must be inside one of these
exec_allow = ["/usr/bin/node", "/usr/bin/npm", "/usr/bin/pnpm"]
allow_shell = false
max_programs = 20
```

- Unknown keys, a file not owned by root, or a file writable by group or others → the policy is **invalid** and treated as `enabled = false` (the agent logs why, without the file content).
- The agent reads it at start-up only. A policy change needs an agent restart.
- A `run_as` entry that is `root`, does not exist, or has uid < 1000 is dropped (logged).

### Hello

`capabilities` gains `"programs"` when the policy is valid and `enabled`. `hello` gains `"programs": {"enabled":bool,"run_as":[string],"allow_shell":bool,"policy_hash":"<hex sha256 of the file bytes>"}` whenever the file exists (invalid → `enabled:false`, empty `run_as`); it is absent when there is no file. Paths, `dirs` and `exec_allow` are never sent.

### Task kinds

`kind` may also be `program_start`, `program_stop`, `program_restart`, `program_status`, `program_logs`. `service` is null. Specs (closed key sets):

- `program_start`: `{"project":"<slug>","entry":"<name>","dir","exec","args":[...],"shell":bool,"run_as","env":{...},"restart":"on-failure"|"no"}`
- `program_stop`, `program_restart`, `program_status`: `{"project","entry"}`
- `program_logs`: `{"project","entry","lines":1..1000}`

Additional agent checks, after the v1.0 checks 1–4 (the first failure gives `task_reject` with that code; nothing runs):
5. The policy is valid and `enabled` (`policy_denied`).
6. For `program_start`: the same field rules as api-v1 v1.6 (`bad_spec`); `run_as` is in the policy; `shell=true` only if `allow_shell`; `dir` resolves (realpath, symlinks followed) inside a `dirs` entry; `exec` resolves as below (`policy_denied` for any of these).
7. Starting would exceed `max_programs` running deploybot units (`policy_denied`).

**exec resolution:** a bare name resolves to `<dir>/node_modules/.bin/<name>` if that exists and its realpath is inside `dir`, else to the `exec_allow` entry with that basename. An absolute path must equal an `exec_allow` entry or have a realpath inside `<dir>/node_modules/.bin/`'s target tree within `dir`. Shell mode always uses `/bin/sh`, which must be in `exec_allow`.

**Unit:** `deploybot-<project>-<entry>.service`, a system unit. The agent never edits units itself; it asks the helper.

**Results** (`task_result.payload`):
- start, stop, restart, status → `ProgramStatus {"unit","active_state","sub_state","main_pid","started_at","ports"}`. start and restart succeed once systemd reports the unit `active` (or `activating` after 10 s); a unit that fails within 10 s → `failed`, `error_code` `start_failed`, payload = ProgramStatus plus `"lines"`: the last 50 log lines (redacted). stop of a unit that does not exist succeeds.
- logs → `ProgramLogs {"unit","lines","truncated"}`: newest `lines` journal lines of the unit, each redacted with the v1.0.1 regex and truncated to 2048 chars, total ≤ 64 KiB (`truncated` when cut).
- A helper refusal → `failed` with `error_code` `policy_denied` and no payload detail beyond a fixed reason code.

### Owned services

`Discovered` gains `"owner": {"kind":"program","project":"<slug>","entry":"<name>"} | null`. For each running deploybot unit the agent asks the helper for its listening sockets (by cgroup) and reports one Discovered entry with `name` = the entry, `kind` = `systemd`, `systemd_unit` = the unit name, `systemd_scope` = `system`, the exact `ports`, and `owner` set. Those sockets are not also reported as unmapped listeners. Old servers ignore `owner`.

### Local helper (not on the wire; between bot-agent and root)

`/usr/local/libexec/bot-agent-runner`, root:root 0755, built from the bot-agent repo. The only privilege bot-agent has is one sudoers line:

```
bot-agent ALL=(root) NOPASSWD: /usr/local/libexec/bot-agent-runner
```

- Invoked as `sudo -n /usr/local/libexec/bot-agent-runner <verb>` with one JSON object on stdin (≤ 64 KiB) and one JSON object on stdout. Verbs: `start`, `stop`, `restart`, `status`, `logs`, `sockets`, `list`. Anything else → refusal.
- The helper re-reads and re-validates the policy and **every** field on every call, exactly as checks 5–7 above, independently of the agent. It acts only on units whose name matches `^deploybot-[a-z0-9-]{1,64}-[a-z0-9-]{1,48}\.service$` and that it created (its unit files carry `X-Deploybot-Project=` and `X-Deploybot-Entry=`).
- **start** writes `/run/systemd/system/<unit>` (root 0644) with: `User=<run_as>`, `Group=<run_as's primary group>`, `WorkingDirectory=<realpath dir>`, `ExecStart=` the resolved exec and args, each argument quoted for systemd (`%` doubled, `$` doubled, quotes and backslashes escaped), `Environment=HOME=<run_as home>` and `Environment=PATH=<dir>/node_modules/.bin:/usr/local/bin:/usr/bin:/bin`, `EnvironmentFile=/run/bot-agent/env/<unit>.env` (root 0600, the plain env), `Restart=`, `NoNewPrivileges=yes`, `PrivateTmp=yes`, `ProtectSystem=full`, `ProtectHome=read-only`, `ReadWritePaths=<realpath dir>`, `RestrictSUIDSGID=yes`, `LockPersonality=yes`; then `systemctl daemon-reload` and `systemctl restart <unit>`. Units are transient across reboot (`/run`).
- **stop** stops the unit and removes its unit and env files. **logs** runs `journalctl -u <unit> -n <lines> -o cat --no-pager`. **sockets** reads the unit's cgroup (`/sys/fs/cgroup/system.slice/<unit>/cgroup.procs`) and maps its pids' socket inodes to `/proc/net/tcp{,6}` listeners. **list** returns the deploybot units with their project and entry.
- The helper never runs a shell except `/bin/sh -c <line>` for an entry in shell mode on a policy with `allow_shell = true`, and never logs env values.

### v1.3.1 amendments (2026-10-15, from the wave 1 review; helper transport approved by Joe)

1. **Helper transport (replaces sudo and the sudoers line).** The agent's own unit (`NoNewPrivileges=yes`, empty `CapabilityBoundingSet=`, `ProtectSystem=strict`, `ProtectHome=yes`) makes a sudo child powerless, and that sandbox stays. The helper is a root **socket-activated** service instead:
   - `bot-agent-runner.socket`: `ListenStream=/run/bot-agent-runner.sock`, `SocketUser=root`, `SocketGroup=bot-agent`, `SocketMode=0660`, `Accept=yes`.
   - `bot-agent-runner@.service`: `User=root`, `StandardInput=socket`, `StandardOutput=socket`, `ExecStart=/usr/local/libexec/bot-agent-runner serve-conn`, `RuntimeMaxSec=60`.
   - One request per connection: `{"verb":"start|stop|restart|status|logs|sockets|list","request":{...}}` (≤ 64 KiB, newline-terminated), one JSON response, then close.
   - The helper checks the peer with `SO_PEERCRED`: the uid must be the `bot-agent` account's uid, else a refusal. The socket mode also limits callers to root and group `bot-agent`.
   - There is **no sudoers line**. Rollback: `systemctl disable --now bot-agent-runner.socket`, or `enabled = false`.
2. **Who checks paths.** The agent cannot see `/home`, so it performs only the policy and lexical checks (5, 7, and the non-filesystem parts of 6: field rules, `run_as` listed, shell gate, exec name syntax). The **helper** performs every filesystem check (realpath of `dir`, exec resolution) and refuses with `policy_denied`; the agent returns that as `task_result` `failed` / `policy_denied`.
3. **Environment denylist:** the agent and the helper both refuse the api-v1 v1.6.1 item 2 keys (`bad_spec` in the agent, `policy_denied` in the helper).
4. **Program unit hardening adds** `PrivateDevices=yes`, `ProtectKernelTunables=yes`, `ProtectKernelModules=yes`, `ProtectControlGroups=yes`.
5. **start/restart truth:** a non-zero `systemctl` exit is a failure (`start_failed`), and success needs the unit's `ActiveEnterTimestampMonotonic` to be newer than before the call.

### v1.3.2 note (2026-10-15, approved spec `docs/spec-services-view.md`)

1. A `services` frame (and `hello.services`) is sent only from a complete discovery: the process scan succeeded and, when the program policy is enabled, the owned-program lookup through the helper succeeded. If either fails, the agent sends no `services` frame for that cycle and keeps its previous canonical set and send time, so the next successful cycle compares against the last set actually sent. The server treats every received set as the full set (api-v1 v1.7 item 2).
2. At start, `hello.services` comes from the first discovery; if that discovery is incomplete, `hello.services` is an empty list **and** the server must not apply presence to it. To make that explicit, `hello` gains `"services_complete": bool` (absent = `true` for older agents). The server applies api-v1 v1.7 item 2 to `hello.services` only when it is `true`.

## v1.4 images (2026-10-15, approved spec `docs/spec-catalog-images.md` rev 3, wave 2a; additive)

### Host policy additions

```toml
images = ["ghcr.io/joe-and-his-friends/*"]  # reference patterns; empty or absent = no image entries
projects = ["mohiguide"]                     # project slugs this host may run containers for
max_containers = 20                          # 1..200, default 20
allow_public_ports = false
```

- Pattern match: the reference is parsed and normalised (registry host lowercase, `docker.io` and `library/` made explicit). A pattern is `<host>/<path>` where `*` matches exactly one path component; the host is matched literally; there is no other wildcard.
- `projects` entries: `^[a-z0-9][a-z0-9-]{0,63}$`. Invalid key or value → the whole policy is invalid (as v1.3).

### Hello

`hello.programs` gains `"images": bool` (policy valid, `enabled`, `images` and `projects` non-empty) and `"docker": bool` (the helper answered `container_list` at start-up). Patterns and project lists are not sent.

### Task kinds

`image_deploy`, `image_rollback`, `image_remove`, `image_start`, `image_stop`, `image_restart`, `image_status`, `image_logs`. `service` is null. Specs (closed key sets):

- `image_deploy`: `{"project","entry","spec":ImageSpec (api-v1 v1.8, as stored: inline env as written, env_sets and secret_env as references — v1.4.1),"env":{KEY:value} (the resolved, merged environment; the only place resolved values travel),"registry_auth":{"host","username","token"}|null,"force":bool,"spec_hash"}`
- `image_rollback`: as `image_deploy`, plus `"digest":"sha256:<64 hex>"` (the image is pulled by that digest).
- `image_remove`, `image_start`, `image_stop`, `image_restart`, `image_status`: `{"project","entry"}`
- `image_logs`: `{"project","entry","lines":1..2000}`

`env` values and `registry_auth.token` are secrets: the agent never logs them, never stores them beyond the task, and adds every value (raw, base64 std/url, and JSON-escaped forms, for values ≥ 6 bytes) to the redaction set for that task's logs and results, in addition to the v1.0.1 regex. Task records keep `env` key names only.

Agent checks after v1.0 checks 1–4: policy valid and `images` (`policy_denied`); `project` in `projects` (`policy_denied`); ImageSpec field rules as api-v1 v1.8 (`bad_spec`); image matches a pattern (`policy_denied`).

### Sequences and results

`task_result.payload` = `{"outcome":Outcome,"digest":string|null,"exit_code":int|null,"status":ImageStatus,"lines":[string]}` for deploy/rollback (lines = last ≤ 50 redacted log lines), `{"status":ImageStatus}` for start/stop/restart/status/remove, `{"container","lines","truncated"}` for logs (same caps as program logs). `status` = `succeeded` for outcomes `succeeded|unchanged`, `failed` otherwise with `error_code` = the outcome.

**Service deploy:** `image_pull` → if the current container (labels match) runs this digest with this `spec_hash` and not `force` → `unchanged` → else `container_deploy` → wait healthy (the service's configured health checks, else state `running` with `restart_count` 0 for `healthy_after_sec`, polled every 2 s) → `succeeded`; unhealthy → `container_rollback` → `rolled_back` (or `rollback_failed`).
**Job deploy:** `image_pull` → `container_remove` of the job container if present → `container_deploy` (restart `no`) → poll `container_status` every 2 s until `exited` or `timeout_sec` → exit 0 `succeeded`, else `failed` with `exit_code`; timeout → `container_stop` → `timeout`.
**Rollback:** the service sequence with `ref = <repo>@<digest>`.

### Discovery

Each discovery cycle calls `container_list`. A container with all four `dev.deploybot.*` labels whose `host` equals this host and `project` is in `projects` → `Discovered {"name":<entry>,"kind":"container","container_name","ports","image_digest","owner":{"kind":"image","project","entry"}}`. Every other container → `Discovered {"name":<container name without leading "/">,"kind":"container","container_name","ports","image_digest":null,"owner":null}`; its published host ports are not also reported as unmapped listeners. `kind` gains the value `container`. If `container_list` fails while `images` is true, discovery is incomplete (v1.3.2).

### Local helper: image verbs (same socket and framing as v1.3.1)

`RuntimeMaxSec` of `bot-agent-runner@.service` becomes **900**; `ReadWritePaths=` gains `/var/run/docker.sock`. Program verbs keep the helper-wide flock; image verbs take `flock` on `/run/bot-agent/image-<project>-<entry>.lock` for the whole verb. The helper talks to Docker Engine API `/v1.47` over `/var/run/docker.sock` with its own client; **nothing from the request is forwarded as raw HTTP**.

Request validation in the helper, before any Docker call (refusal = `{"error":"policy_denied"|"bad_spec"|"conflict"|"docker_unavailable"}`): `project` ∈ `projects`; `entry` `^[a-z0-9][a-z0-9-]{0,47}$`, not ending `-` or `-prev`; volume names `^[a-z0-9][a-z0-9-]{0,47}$`; every ImageSpec rule of api-v1 v1.8; `registry_auth` fields without CR/LF/NUL; Engine URL path segments built with path escaping; containers addressed by the full id from the helper's own lookup.

| Verb | Request | Response |
|---|---|---|
| `image_pull` | `{"project","ref","registry_auth"?}` | `{"digest","image_id"}` |
| `container_deploy` | `{"project","entry","spec","env","digest","spec_hash"}` | `{"container_id"}` |
| `container_rollback` | `{"project","entry"}` | `{"container_id"}` |
| `container_remove`, `container_start`, `container_stop`, `container_restart` | `{"project","entry"}` | `{"status":ImageStatus}` |
| `container_status` | `{"project","entry"}` | `{"status":ImageStatus,"spec_hash"}` |
| `container_logs` | `{"project","entry","lines"}` | `{"container","lines","truncated"}` |
| `container_list` | `{}` | `{"items":[{"id","name","state","ports":[{"host_ip","host_port","container_port"}],"digest","labels":{dev.deploybot.* only}}]}` |

Rules (normative; spec rev 3 §4.1 and §12 in full):
1. **Pull:** parse `ref`; must match `images`; Engine call `POST /images/create` with exactly `fromImage=<name>&tag=<non-empty tag>` or `fromImage=<name>@sha256:<digest>`; header `X-Registry-Auth` (base64url JSON `{"username","password","serveraddress"}`) only when `registry_auth.host` equals the parsed registry host. Response digest = the repo digest for that registry.
2. **Labels:** the helper sets `dev.deploybot.owner=bot-agent`, `dev.deploybot.host=<first 16 hex of sha256(/etc/machine-id)>` (the helper does not read the agent's credential file), `dev.deploybot.project`, `dev.deploybot.entry`, and `dev.deploybot.spec_hash`. Any verb that mutates or reads logs/status of `<project>-<entry>` or `<project>-<entry>-prev` first resolves each name; a container under either name whose owner/host/project/entry labels differ → `conflict`, nothing touched.
3. **Create body** contains exactly: `Image` (`<repo>@sha256:<64 hex>`, repo re-matched against `images`), `Cmd`, `Entrypoint`, `Env`, `Labels`, `ExposedPorts`, `User` (if set, rules as api-v1 v1.8 item 10), `HostConfig.{PortBindings (HostIp "127.0.0.1", or "0.0.0.0" only when allow_public_ports and the port is public), Mounts, RestartPolicy, NetworkMode "deploybot-<project>", CgroupnsMode "private", PidsLimit 1024, Memory and MemorySwap (= memory_mb MiB) when set}`, `NetworkingConfig.EndpointsConfig{"deploybot-<project>":{"Aliases":[<entry>]}}`. Nothing else, including no empty values of other fields.
4. **Volumes:** `Mounts[i] = {"Type":"volume","Source":"<project>-<name>","Target","ReadOnly"}` only. Before create: inspect each; missing → `POST /volumes/create` with driver `local`, no options, labels owner/host/project; then (whether created or existing) require driver `local`, empty options and label `project=<project>` → else `bad_spec`/`conflict`.
5. **Network:** inspect `deploybot-<project>`; missing → create with driver `bridge`, no options, default IPAM, labels owner/host/project; then require driver `bridge` and those labels → else `conflict`.
6. **Deploy order (service):** under the entry lock: check labels (rule 2) → remove `-prev` (stop if running, `DELETE ?v=1`) → stop current, rename to `-prev` → ensure network and volumes → create → start. **Rollback:** remove the current (`v=1`), rename `-prev` back, start. **Job:** remove the existing job container, create, start. `max_containers` counts deploybot-labelled containers on the host (including `-prev` and jobs); a create beyond it → `policy_denied`.
7. **Projections:** responses never contain env, command, entrypoint, mounts, host config, graph-driver data, log config or labels other than `dev.deploybot.*`. There is no image-inspect or container-inspect verb.
8. Logs: `GET /containers/{id}/logs?stdout=1&stderr=1&tail=<lines>&timestamps=0` (never `follow`), demultiplexed, each line redacted (v1.0.1 regex) and cut to 2048 chars, total ≤ 64 KiB.

### v1.4.1 amendment (2026-10-16, Joe approved; from the wave 2a round-2 security review)

1. In `image_deploy` and `image_rollback`, `spec` is the entry's ImageSpec **as stored**: inline `env` as the owner wrote it, `env_sets` and `secret_env` as references (`<set>.<KEY>`). Resolved values travel **only** in the top-level `env`, so each secret value appears once per frame. The helper refuses resolved values in `spec.secret_env` (`bad_spec`).
2. The API computes the marshalled task frame size before sending. A frame over the agent frame limit is never sent: the request fails with `bad_request` "the resolved environment is too large for one task", and the agent connection stays open. Neither side closes a connection because of an oversized frame the API built.

### v1.4.2 amendment (2026-10-16, Joe approved; from the wave 2a agent security reviews)

1. `container_logs` request: `{"project","entry","lines","redact"?:[string]}`. `redact` is optional, ≤ 64 values, each 6–4096 bytes, no NUL. The helper removes every value in raw, base64 std, base64 url (padded and raw) and JSON-escaped form from each raw log line **before** the v1.0.1 regex and the 2048-char cut, and never logs or returns the list.
2. The agent sends the task's exact set (env values and the registry token) in `redact` for the log tail it puts in `image_deploy` / `image_rollback` results. `image_logs` tasks carry no values, so their `container_logs` call has no `redact`.

## v1.5 managed files (2026-10-16, approved spec `docs/spec-catalog-files.md` rev 1, wave 2b; additive)

### Task specs

- `image_deploy` and `image_rollback` gain a top-level `"files":[{"name","target","content","secret":bool}]` (default `[]`), in the order of `spec.files`. `spec.files` stays as stored (`[{"file","target"}]`, api-v1 v1.9). Resolved content travels **only** in the top-level `files`. The agent refuses a task whose `files` names and targets do not equal `spec.files` one for one, or that breaks api-v1 v1.9 rules 1–2 and 13, with `task_reject` `bad_spec`, as for every other ImageSpec rule failure (v1.4).
- Task records and logs keep `name` and `target` only, never content.

### Exact-value redaction

For every **secret** file of the task, the agent adds to that task's exact set:
1. the whole content, when it is 6–4096 bytes;
2. every line of the content, trimmed of surrounding whitespace, of 16–4096 bytes;
3. when the content parses as JSON: every string value of 16–4096 bytes, and every trimmed line of 16–4096 bytes inside a string value (so the lines of a PEM key held in a JSON field are covered).

Plain files add nothing. The set applies as in v1.4.2 (results, log lines, and the `redact` list of `container_logs`).

**Only secret values (Joe 2026-10-16, from the pi3 mirror):** `image_deploy` and `image_rollback` gain `"secret_keys":[KEY]`: the keys of the top-level `env` whose resolved value comes from an env-set key with `is_secret: true` (through `env_sets` or `secret_env`, after the v1.8 item 12 precedence; a key overridden by inline `env` is not secret). The exact set is: the `env` values of `secret_keys`, the registry token, and the secret-file items above. Inline `env`, plain env-set keys and plain files are not redacted. The API sends `secret_keys` (possibly `[]`) **only to a host whose `hello.programs.files` is true**; to an older agent it omits the key, because a v1.4 agent refuses unknown keys. When `secret_keys` is absent, every `env` value joins the set, as in v1.4.

**Structured fields are never redacted:** in `task_result` payloads the exact set and the v1.0.1 regex apply only to free text: `lines`, log `lines`, and error messages. `outcome`, `digest`, `exit_code` and every `status` field (`container`, `state`, `exit_code`, `restart_count`, `started_at`, `finished_at`, `digest`, `ports`) are passed as the helper returned them. The helper builds them from labels, names and the Engine, never from env values.

`container_logs` `redact` limits become: ≤ **2048** values, each 6–4096 bytes, total ≤ **512 KiB**, no NUL.

### Hello

`hello.programs` gains `"files":bool`: true when this agent and its helper support v1.5 files. The API refuses a deploy of an entry with non-empty `files` to a host whose `files` is not true (`bad_request` "this host's agent does not support files"), before any task is sent.

### Local helper

1. **Request size:** image verbs accept requests up to **2 MiB**; program verbs keep 64 KiB.
2. **Units (host security files, Joe reviews before install):** `bot-agent-runner@.service` gains `StateDirectory=bot-agent-runner` and `StateDirectoryMode=0700`. `bot-agent.service` `InaccessiblePaths=` gains `-/var/lib/bot-agent-runner`.
3. **`container_deploy`** request gains `"files":[{"name","target","content"}]` (default `[]`); `container_status` response gains `"files_hash":string` (`""` when the container has none).
4. **Store:** `/var/lib/bot-agent-runner/files/`. One **slot** directory per container: `<project>-<entry>`, `<project>-<entry>-prev`, and the transient `<project>-<entry>.new`. Each file is `<slot>/<name>`.
5. **Writes:** the helper opens the store with `O_DIRECTORY|O_NOFOLLOW` and works only with `openat`/`mkdirat`/`renameat`/`unlinkat` relative to directory handles, never through a path string. Slot directories are created mode 0700, owner 0:0. Each file is created `O_CREAT|O_EXCL|O_NOFOLLOW|O_WRONLY`, written, `fsync`ed, set to mode 0444, owner 0:0. Then the slot directory is `fsync`ed.
6. **Checks before every use** (`fstatat` with `AT_SYMLINK_NOFOLLOW`): the store and each slot are directories owned by 0:0 with mode 0700 exactly; each file is a regular file owned by 0:0, mode 0444, link count 1; nothing else is in a slot. Any mismatch → `bad_spec`, and the deploy stops before any container is touched.
7. **Mounts:** for each file, `HostConfig.Mounts` gains `{"Type":"bind","Source":"/var/lib/bot-agent-runner/files/<slot>/<name>","Target","ReadOnly":true}`. No `BindOptions`, no other bind mounts. `Source` is built by the helper only.
8. **Label:** when `files` is non-empty, the container gets `dev.deploybot.files_hash` = hex SHA-256 of the lines `<name>:<hex sha256 of content>\n`, sorted by name. With no files the label is absent.
9. **Service deploy order** (extends v1.4 rule 6, under the entry lock): remove a leftover `.new` slot and any slot whose container no longer exists → write the `.new` slot (when `files` is non-empty) → check labels → remove `-prev` and its slot → stop the current container, rename it to `-prev` and its slot to `-prev` → rename `.new` to the current slot → ensure network and volumes → create → start.
10. **Rollback:** remove the current container and its slot, rename `-prev` and its slot back, start. **Job:** remove the existing job container and its slot, write the slot, create, start. **`container_remove`:** removes the container and its slot.
11. **Unchanged:** the agent reports `unchanged` only when digest, `spec_hash` and `files_hash` all match (the agent computes the expected `files_hash` from the task's `files` by rule 8; `""` when none).
12. Projections (v1.4 rule 7) never contain file content, slot paths or mounts.


## v1.5.1 persistent program units (2026-10-17, approved spec `docs/spec-program-persistence.md` rev 1; helper behaviour only, no wire change)

Replaces the v1.3 "Units are transient across reboot (`/run`)" rule. Frames, verbs, reasons and `hello` are unchanged.

1. **Paths:** **start** writes the unit to `/etc/systemd/system/<unit>` (root 0644), the env file to `/var/lib/bot-agent-runner/programs/<unit>.env` (root 0600) and the spec to `/var/lib/bot-agent-runner/programs/<unit>.json` (root 0600). The `programs` directory is root 0700. The unit's `EnvironmentFile=` is `/var/lib/bot-agent-runner/programs/<unit>.env`.
2. **`[Install]`:** the unit ends with `[Install]` and `WantedBy=multi-user.target`.
3. **start / restart:** write the files, `systemctl daemon-reload`, `systemctl enable <unit>`, `systemctl restart <unit>`. A restart of a unit already in `/etc/systemd/system` does not rewrite it: the stored spec is rechecked against the policy, then `daemon-reload`, `enable`, `restart`. Every value rendered into a unit (start, and the restart that moves a unit from `/run`) is refused when it contains CR, LF or NUL (and `dir`, `exec` and the run_as home also when they contain `\`, a systemd line continuation), before any write. A unit with a drop-in directory (`/etc/systemd/system/<unit>.d` or `/run/systemd/system/<unit>.d`) is refused (`io_error`) before `enable`; the helper never creates or removes drop-ins. A failed start leaves no file behind. A failed restart of an `/etc` unit keeps it enabled (reply `start_failed`, `fresh:false`).
4. **stop:** `systemctl disable <unit>`, `systemctl stop <unit>`, remove the unit, env and spec files, `systemctl daemon-reload`. A stopped program leaves no file behind. If a file cannot be removed (a symlink or non-regular file is never unlinked), the helper still removes every other file and reloads, then replies `io_error`.
5. **Ownership:** unchanged rules (`unitNameRE`, the `X-Deploybot-*` markers, `O_NOFOLLOW`, atomic writes). A unit in `/etc/systemd/system` with our name but without our markers is never written, enabled, disabled or removed (reason `not_created`, unchanged).
6. **Upgrade from `/run`:** when start, restart or stop finds our unit (markers checked) in `/run/systemd/system`, the helper writes the `/etc` copy first (start, restart) and then removes the `/run` unit and its `/run/bot-agent/env/<unit>.{env,json}` in the same verb. The move completes only once `enable` succeeded: if `daemon-reload` or `enable` fails on a restart, the helper removes the `/etc` copy it wrote and keeps the `/run` files (the program keeps running). `/etc/systemd/system` wins over `/run/systemd/system`, as in systemd's unit search path: with our unit in `/etc`, a foreign same-name file in `/run` is left untouched and does not block the verb; a foreign file in `/etc` always refuses (`not_created`). `status`, `logs`, `sockets` and `list` look in `/etc/systemd/system` first, then `/run/systemd/system`; `max_programs` counts every owned unit once.
7. **Units (host security file, Joe reviews before install):** `bot-agent-runner@.service` `ReadWritePaths=` gains `/etc/systemd/system`. `bot-agent.service` is unchanged (v1.5 already hides `/var/lib/bot-agent-runner`).

## v1.5.2 rollback to the kept container (2026-10-17, Joe: "Swap with -prev", from the pi3 wave 2b live check)

A content-only change could not be rolled back: `image_rollback` redeployed the target digest with the current spec and files, so it reported `unchanged`. Additive.

1. **`container_status`** response gains `"prev":{"digest":string|null,"spec_hash":string,"files_hash":string}|null`: the labels of the entry's `-prev` container, `null` when there is none. No other `-prev` data is returned.
2. **`image_rollback`, service:** when `prev` is not null and `prev.digest` equals the task's `digest`, the agent does not pull or deploy. It calls `container_rollback` (v1.5 rule 10: the current container and its slot are removed, `-prev` and its slot are renamed back and started), then waits healthy exactly as a rollback deploy does (the entry's health checks, else `healthy_after_sec`, 30 s when that is 0). Healthy → `succeeded`; not healthy → `failed` (there is no further generation to return to). The result `digest` is `prev.digest`. The previous spec and file content come back with that container.
3. Otherwise `image_rollback` is unchanged (v1.4/v1.5: pull by digest, deploy with the task's spec and current `files`). If the first `container_status` is refused, the refusal is the result (rule 4); if it fails in transport, the outcome is `failed` and nothing is pulled.
4. **Refusal codes:** a helper refusal for a bad file slot or store (v1.5 rule 6) is `bad_spec`, and the `max_containers` cap is `policy_denied`; the agent maps both to outcome `forbidden_spec` (`task_result.error_code` = `forbidden_spec`). They are never reported as a bare `failed`.
5. **Diagnostics:** the helper writes one line per refusal to the journal (`bot-agent-runner: refuse <verb> <reason>`; `StandardError=journal` in its unit, because stdout is the socket), and the agent logs a failed helper call as task kind, verb and reason or `transport`. Neither logs request data.
6. **Overlapping projects (Joe 2026-10-17, from the wave 2b files review):** container and slot names are `<project>-<entry>`, which is ambiguous when one listed project is another plus `-` (`acme` and `acme-web`). The policy is then not valid for images: `images:false` in `hello.programs`, every image verb is refused `policy_denied`, and the helper logs `bot-agent-runner: policy: overlapping projects <a> <b>` once per connection. Program verbs are unaffected.

## v1.6 Docker health status (2026-10-18, approved spec `docs/spec-docker-health.md` rev 1; additive, gated)

Containers whose image defines a Docker `HEALTHCHECK` report Docker's verdict; Deploybot checks remain the fallback (api-v1 v1.11).

1. **`container_list`** items gain `"health": "healthy"|"unhealthy"|"starting"|null`. The helper takes it from the Engine's structured `Health.Status` when present, otherwise from the `Status` text suffix `(healthy)`, `(unhealthy)` or `(health: starting)`; any other value, or no suffix, is `null`. No new verb, call or privilege.
2. **Gate:** `hello.programs` gains `"docker_health": true` from an agent that implements this section. The server sends `config.docker_health: true` only to a host that advertised it (an older agent refuses unknown config keys); an older server never sends it. `config_update` carries it the same way. Without `config.docker_health: true` the agent emits no docker samples.
3. **Docker health sample:** on each complete discovery pass, for each `kind:"container"` service in the set it sends (owned or not) whose item `health` is `healthy` or `unhealthy`, the agent emits `{"m":"health","t",...,"tags":{"service":<name>,"protocol":"docker"},"f":{"result":"success"|"unhealthy","http_status":null,"latency_ms":null}}`. `check_id` and `port` are absent for this form only. `starting` and `null` emit nothing. One sample per service per pass.
4. **Validation:** the health sample rules become: protocol `http|tcp` requires `check_id` and `port` (1–65535) and a non-null `latency_ms`, result in `success|status_mismatch|timeout|connection_refused|error`; protocol `docker` forbids `check_id` and `port`, requires `http_status` and `latency_ms` null, result in `success|unhealthy`.
5. Configured TCP/HTTP checks are unchanged and keep running on container services.
6. **ImageSpec `healthcheck`** *(rev 2, approved 2026-10-20)*: the agent advertises `hello.programs.healthcheck: true`. A task's ImageSpec may carry `healthcheck` (api-v1 v1.11 item 7); the helper's `container_deploy` passes it to the Engine create body as `Healthcheck {Test: ["CMD", argv…], Interval, Timeout, Retries, StartPeriod}`.
7. **Deploy wait** *(rev 2)*: for a service whose spec has `healthcheck`, after `container_deploy` the agent polls `container_list` every 2 s: `healthy` (and any configured TCP/HTTP checks passing) → `succeeded`; `starting` keeps waiting; `unhealthy`, or the wait limit (`max(timeout_sec, start_period_sec + interval_sec × (retries + 1))`) → `container_rollback` → `rolled_back` / `rollback_failed`. `image_rollback` waits on the restored container the same way. Without `healthcheck` the v1.5 rule is unchanged.

## v1.7 secret previews in task output (2026-10-20, approved spec `docs/spec-env-editing.md` rev 1; no wire change)

1. In image task output (log tails, error text) the agent replaces an env secret value from the task (raw, base64 and JSON-escaped forms, values of at least 6 bytes as before) with `preview(v)`: the first 4 runes followed by `…` when `v` has at least 16 runes, otherwise `••••`.
2. Secret-file pieces keep `[REDACTED]`. The keyword rule is unchanged.
3. The task frame is unchanged. An agent before v1.7 keeps writing `[REDACTED]`; the API accepts both.

## v1.6.1 health-aware unchanged, first-deploy failure (2026-10-20, `docs/spec-docker-health.md` rev 3 §10, approved 2026-10-21; no wire change)

1. **Unchanged** (amends rule 11 for service deploys): when the task's ImageSpec has `healthcheck`, `unchanged` also requires the current container's `container_list` health to be `healthy`; otherwise the agent deploys and waits per v1.6 item 7. Without `healthcheck`, rule 11 is unchanged.
2. **Nothing good to roll back to:** a service deploy whose wait fails, when the container that was current before the deploy was not `running` (`missing`, `exited`, `created`, `dead`), does not call `container_rollback`. The agent calls `container_stop` on the new container (it is kept) and reports outcome `failed`, with that container's status and the last ≤ 50 redacted log lines. When it was `running`, v1.6 item 7 applies (`rolled_back` / `rollback_failed`). A parked older container stays on `-prev` and is not started. *(Amended 2026-10-21 from the w28 spec review.)*
3. `image_rollback` and jobs are unchanged.

## v1.8 container log limit (2026-10-21, approved spec `docs/spec-container-logging.md` rev 1; additive, gated)

1. **Create body:** every `container_deploy` create (service or job) sets `HostConfig.LogConfig = {"Type":"json-file","Config":{"max-size":"<n>m","max-file":"<k>"}}`. `n` and `k` come from the task ImageSpec `logging` (`max_size_mb`, `max_files`) when present, else **10** and **3**. The driver is always `json-file`, whatever the daemon default.
2. **Validation in the helper:** `max_size_mb` 1–100 and `max_files` 1–10 (integers); out of range or unknown keys → refusal `forbidden_spec`, nothing created.
3. **Gate:** `hello.programs` gains `"logging": true` from an agent that implements this section. The ImageSpec `logging` key is sent only to such an agent (api-v1 v1.14 item 2).
4. `container_logs` and `image_logs` are unchanged; Docker's log API reads rotated `json-file` files.
5. Existing containers keep their creation-time `LogConfig`. `unchanged` (digest, `spec_hash`, `files_hash`) is unchanged, so a container is limited only after its next real deploy.

## v1.9 health failure policy (2026-10-23, approved spec `docs/spec-health-policy.md` rev 1; additive, gated)

1. **ImageSpec `health_policy`** (services only, optional; api-v1 v1.15): `{"on_deploy_failure":"rollback"|"stop"|"keep","on_unhealthy":"none"|"restart"|"rollback","unhealthy_after_sec":int 15–600,"max_actions_per_hour":int 1–10}`. Defaults for an absent field or key are `rollback`, `none`, `45` and `3`. The agent validates the shape again; unknown keys, bad enums or out-of-range values cause `task_reject` `bad_spec`. A `job` spec with the key causes `bad_spec`.
2. **Failed health wait in `image_deploy`** (replaces the fixed v1.6.1 rule only when `on_deploy_failure` is not `rollback`):
   - `rollback`: unchanged v1.6.1 behaviour. Roll back to `-prev` if the pre-deploy container was `running` (outcome `rolled_back` or `rollback_failed`); otherwise stop the new container and keep it (outcome `failed`).
   - `stop`: stop the new container and keep it, whatever the pre-deploy state. Do not restart `-prev`. Outcome `failed`.
   - `keep`: leave the new container running. Outcome `failed`.

   `image_rollback` is unchanged.
3. **Labels at create.** For a service with a non-default `on_unhealthy`, the helper sets `dev.deploybot.on_unhealthy`, `dev.deploybot.unhealthy_after_sec` and `dev.deploybot.max_actions_per_hour` (decimal strings), next to the existing owner labels. `container_list` returns these three labels for owned containers as `"policy":{"on_unhealthy","unhealthy_after_sec","max_actions_per_hour"}` (or `null` when absent). The helper validates label values like item 1 and ignores (treats as `null`) anything invalid.
4. **Supervisor.** Every 15 s the agent runs `container_list` and considers owned, running service containers with `policy.on_unhealthy` ≠ `none`. It acts on a container only when all of these hold:
   - its Docker health has been `unhealthy` on every poll for at least `unhealthy_after_sec`;
   - no image task for that `(project, entry)` is in flight, using the same per-entry lock as task handling;
   - fewer than `max_actions_per_hour` automatic actions on that container in the last 60 minutes;
   - for `rollback`, a `-prev` container exists and no automatic rollback has happened for that entry since its last `image_deploy` or `image_rollback` task.

   The action is helper `container_restart` (`restart`) or `container_rollback` (`rollback`). A container that is not `running` is never acted on. Counters live in memory: they reset on agent restart and, per entry, on every `image_deploy` or `image_rollback` task. When the limit or the once-per-deploy rule blocks an action, the agent reports `gave_up` once and then ignores that container until the next deploy or rollback task for its entry.
5. **`auto_action` frame** (agent → server), one per action or `gave_up`:
   ```json
   {"v":1,"type":"auto_action","id":"<uuid>","t":"<RFC3339>","project":"<slug>","entry":"<name>","container":"<name>","action":"restart"|"rollback"|"gave_up","reason":"unhealthy for <n>s","result":"ok"|"error","digest":"sha256:<64 hex>"|null}
   ```
   Server → agent: `{"v":1,"type":"auto_action_ack","id":"<uuid>"}`, sent after the server has stored it, and also for a replayed `id` (not stored twice). The agent keeps unacknowledged frames in memory (cap 100, oldest dropped) and resends them after a reconnect. `reason` is at most 200 bytes. `digest` is the image digest of the container running after the action.
6. **Gate:** `hello.programs` gains `"health_policy": true`. The server sends `health_policy` in a spec only to such an agent (api-v1 v1.15 item 2), and sends `auto_action_ack` only on a connection whose hello advertised it.
7. **Docker restart policy is unchanged.** Docker still restarts only on exit; the supervisor covers `unhealthy`.

### v1.9 rev 2 amendments (2026-10-23, Joe "yes" after the w31 reviews; these replace the matching rules above)
- **Item 1:**
  - `on_deploy_failure: "keep"` combined with `on_unhealthy` other than `none` → `bad_spec`. A container kept after a failed deploy is never supervised.
  - `on_unhealthy` other than `none` requires `spec.healthcheck` → `bad_spec` otherwise. This repeats the API rule as defense in depth, so an image-defined HEALTHCHECK alone never triggers actions.
- **Item 4:**
  - A `rollback` action with no `-prev` container reports `gave_up`.
  - A supervisor action (helper call plus its status read) has a **30 s** budget.
  - An image task for the same entry waits for the supervisor's lock within its own task timeout.
  - When the agent cannot compute its host label (unreadable machine-id), the supervisor does not run.
- **Item 5:**
  - `result: "error"` means the helper call failed. `digest` is then `null`.
  - The server stores such a frame with outcome `auto_failed` (api-v1 v1.15 rev 2).

### v1.9 rev 3 amendments (2026-10-23, Joe "yes" after the pi3-2 live check of dev build 0.2.5.1; these replace the matching rules above)
- **Finding:** an automatic restart or rollback of a container that ignores SIGTERM ran past the 25 s helper-call budget. The helper stop gives a container 30 s (`t=30`). The cancelled helper call left the container stopped, the agent reported `auto_failed`, and the supervisor then ignored the stopped container.
- **Item 4, budget:** a supervisor action has a **60 s** helper call plus a **5 s** status read on its own context, 65 s in total. This replaces the rev 2 30 s budget. An image task for the same entry still waits for the supervisor's lock within its own task timeout.
- **Item 4, completion (new):** once the helper has stopped a container for `container_restart` or `container_rollback`, it finishes the sequence on its own bounded context (at most 60 s) even when the caller's context is cancelled.
  - For a restart: start the container.
  - For a rollback: start `-prev`. If that fails, start the original container again.
  - An automatic action must never leave a container stopped that was running before it. The same applies to the task verbs.
- **Item 5:** when the helper call returns an error but the completion step left a container running, the frame is still `result: "error"`. The next poll judges the running container afresh.
- **`container_rollback` order** (amends v1.5 rule 10 for a running original). The rev 3 completion rule needs the original to stay restorable:
  - The original is renamed `<project>-<entry>-old`, and its file slot is moved aside.
  - `-prev` and its slot are renamed back and started.
  - Only then are the original and its slot removed.
  - On any failure, both containers and both slots are put back and the original is started.
  - A non-running original keeps the old order.
  - The end state is unchanged. A leftover `-old`, for example after a crash, is cleaned up by the next deploy or rollback of that entry. It is never supervised or counted as current.
- **Rev 3b (after the fix round 5 review, 2026-10-23).** This amendment replaces the cleanup sentence above.
  - **Cleaning up a leftover `-old`.** Every service or job deploy, every rollback (running or not) and every remove of an entry first removes an `-old` container that the agent owns for that entry, together with its file slot. Its labels must pass the owner, project, entry and host checks. This happens before the `max_containers` check, so the removed `-old` frees its slot. A container named `-old` whose labels don't match is never touched; it makes the verb fail with `conflict`.
  - **Recovery budget.** The restart's start step and the rollback's restore run on a fresh recovery context of 20 s. This context is not derived from the 60 s finish context, so a stop that used up the whole budget still restarts the original.
  - **Entry names.** An entry name must not end with `-old` (agent `ValidEntry`, same rule as `-prev`).
  - **Logging.** A failure to restore a file slot, or to remove `-old` after a successful rollback, is logged. The rollback still reports success.

## v1.14 (renumbered from v1.10, spec-env-generate rev 4) env hash (2026-10-28, approved spec `docs/spec-env-generate.md` rev 2 §3.7; additive, gated)

A value-only env change used to be reported `unchanged` (rule 11 compares digest, `spec_hash` and `files_hash`; `spec_hash` covers env key names only), so it never reached the container without `force`.

1. **Gate:** `hello.programs` gains `"env_hash": true`. The server sends `env_hash` only to such an agent (api-v1 v1.27 item 8, renumbered from v1.20).
2. **Task:** `image_deploy` and `image_rollback` may carry `"env_hash": "<64 lowercase hex>"`, an opaque value. The agent stores it as the container label `dev.deploybot.env_hash` on every container it creates for that task.
3. **Unchanged (amends rule 11 and v1.6.1 item 1):** when the task has `env_hash`, a service deploy is `unchanged` only if, in addition, the current container's `dev.deploybot.env_hash` label equals it. A container without the label counts as changed.
4. **Rollback (amends v1.5.2):** when the task has `env_hash`, the kept `-prev` container is swapped back only if its label equals the task's `env_hash`; otherwise the agent pulls by digest and creates a new container with the task's env and spec (v1.4/v1.5 path).
5. Without `env_hash` in the task, behaviour is exactly v1.9. Jobs, programs and all other verbs are unchanged.
6. **Helper wire (root runner):** `container_deploy` takes optional `env_hash` (same 64-hex check; malformed → `bad_spec` before any Engine call) and sets the label; `container_status` returns optional `env_hash` and `prev.env_hash` (amends v1.5.2 `prev` = `{digest, spec_hash, files_hash}`), omitted when the container has no valid label. Agent and helper ship in one bundle; a mixed pair mid-upgrade refuses the unknown field rather than skipping the label. `container_list` labels are unchanged.
7. **Agent auto-rollback (v1.9 health policy)** has no task `env_hash` and swaps `-prev` back as before: it restores the last known good container, including its env. The next deploy carries `env_hash` and recreates if the env differs.

## v1.11 secrets for program entries (2026-11-13, approved spec `docs/spec-program-secrets.md` rev 1)

1. **`hello.programs.secrets: true`** from this agent version.
2. **`program_start`** takes an optional `"secret_keys":[KEY]` (the key set stays closed otherwise). Every listed key must be in `env` (`bad_spec` otherwise). A key matching the secret-looking pattern (v1.3 check 6) is allowed only when listed. Every other env rule still applies to listed keys: key grammar, `PATH` and `HOME` reserved, value ≤ 4096 bytes with no NUL or newline, and the v1.3.1 changes-what-runs denylist. The agent (`bad_spec`) and the helper apply the same rules; the helper's re-check of a stored spec on `program_restart` (v1.5.1 item 3) accepts the stored `secret_keys`.
3. **Files:** unchanged. The env file `/var/lib/bot-agent-runner/programs/<unit>.env` (root 0600) holds the resolved values; the stored spec `<unit>.json` (root 0600, directory root 0700) keeps `secret_keys`.
4. **Redaction:** the agent and the helper redact every listed value (raw, base64 and JSON-escaped forms; values of 6 bytes or more) from `program_start`, `program_stop`, `program_restart`, `program_status` and `program_logs` output, as `preview(v)` (v1.7). The helper takes the values from the stored spec. Values never appear in agent or helper logs.
5. **Without `secret_keys`** the frame and every check are exactly v1.9.

## v1.12 the host's projects allowlist covers programs (2026-11-13, approved spec `docs/spec-projects-gate-programs.md` rev 1)

1. **Gate.** `program_start` and `program_restart` require the spec's `project` to match the slug grammar (v1.4), to be in the policy's `projects`, and `projects` to have no overlapping pair (v1.5.2). Otherwise the agent answers `task_reject` `policy_denied` and the helper `policy_denied`. This amends v1.5.2 "Program verbs are unaffected": overlap now turns programs off as well as images.
2. **Still allowed** for an owned unit whatever its project: `program_stop`, `program_status`, `program_logs`, and the helper `list`.
3. **Restart refused for the project** (not listed, or overlap) leaves the unit in place: the helper answers `policy_denied` and does not disable, stop or remove it. Every other failed re-check keeps the v1.5.1 removal.
4. **Boot and agent start:** units are not stopped. At start the agent logs once per owned unit whose project is not listed: `bot-agent: program unit <unit> is for project <slug>, which is not in projects`.
5. **`hello.programs.projects: [slug]`**: the policy's valid project list, sent whenever `hello.programs` is. A policy with more than 32 projects is invalid (`parse`). This replaces the v1.4 rule that project lists are not sent; image patterns, `dirs` and `exec_allow` are still never sent.
6. **`bot-agent policy check`** keeps its lines and adds `projects_overlap=true|false`.
7. **Installer:** `install.sh check|install|upgrade --runner` exits 1 and changes nothing when the policy has programs on (`enabled`, at least one `run_as`, `max_programs > 0`) and `projects` is empty, with `runner.toml allows programs but lists no projects; add projects = ["<slug>"] (from v0.2.8 programs run only for listed projects)`.

## v1.13 shared networks (2026-11-14, approved spec `docs/spec-cross-project-network.md` rev 2; additive, gated)

1. **Runner policy keys** (agent ≥ 0.2.9; an older agent rejects them as `unknown_key`, so the agent is upgraded first). Both keys are one-line string arrays.
   - **`shared_networks`:** items `<name>=<cidr>`.
     - The name matches `^[a-z0-9][a-z0-9-]{0,31}$`. At most 8 networks; names unique.
     - The CIDR is IPv4 in canonical form (host bits zero), inside RFC 1918 (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), with a prefix length from 16 to 28. No two subnets overlap.
   - **`shared_network_projects`:** items `<name>:<project slug>`.
     - The name must be defined in `shared_networks`, and the slug must be in `projects`.
     - No duplicates; at most 32 items.
   - Any violation makes the policy invalid (`parse`). `policy_hash` stays the file hash.
2. **`hello.programs.shared_networks: [{"name","subnet","projects":[slug]}]`.**
   - Sent whenever `hello.programs` is, by agents ≥ 0.2.9, from a valid policy only.
   - It is `[]` when the keys are unset. Order follows the file.
   - Agents < 0.2.9 never send the key.
3. **Image deploy spec `networks: [name]`.** Optional; 1–4 unique names with the same grammar.
   - The server sends it only when non-empty and only to an agent that sent `programs.shared_networks`.
   - The agent pre-check and the helper re-check both require every name to be defined in the policy and to list the task's `project`. Otherwise `task_reject` `policy_denied` (agent) or `policy_denied` (helper), with nothing created.
   - A bad name, a duplicate, an empty array or more than 4 is `bad_spec`.
4. **Shared network rule** (helper). For each name it ensures the network `deploybot-shared-<name>`:
   - **inspect:** decode `Driver`, `Labels` and `IPAM.Config[].Subnet`;
   - **if missing,** create it with:
     - `Driver:"bridge"`;
     - labels `dev.deploybot.owner=bot-agent`, `dev.deploybot.host=<host id>`, `dev.deploybot.kind=shared`, `dev.deploybot.network=<name>`;
     - `IPAM:{"Config":[{"Subnet":"<policy cidr>"}]}`;
   - **if present,** it must have that driver, those four labels and exactly one IPAM subnet equal to the policy's. Otherwise `conflict`, with the diagnostic `shared network deploybot-shared-<name> exists with a different subnet or labels; remove it by hand (docker network rm) after stopping its containers`;
   - **if Docker refuses the create because the address pool overlaps,** that is `conflict` with `subnet <cidr> overlaps an existing Docker network`;
   - the agent and the helper never remove or recreate a shared network (only the operator's `install.sh remove --remove-workloads` teardown removes labelled networks).
5. **Create body** (amends the v1.4 create-body rule):
   - `NetworkingConfig.EndpointsConfig` holds the project network with `Aliases:[<entry>]`, plus one endpoint per shared network with no `Aliases` and no fixed address.
   - `HostConfig.NetworkMode` stays the project network.
   - A spec without `networks` gives a body byte-identical to v1.12.
   - The `-prev` rollback container keeps the networks it was created with.
     After a member or network is revoked, a rollback or a start of a container created on the network re-attaches it; the operator removes such a container to end the attachment.
   - A project whose slug is `shared-<name>` cannot join shared network `<name>`: its project network would have the same name, so the helper answers `conflict` before any Docker call.
   - The helper ensures the shared networks and the project network before it stops or parks the current container, so a `conflict` on either leaves the running container untouched.
6. **`bot-agent policy check`** keeps its lines and adds, after `projects_overlap=`:
   - `shared_networks=<count>`;
   - one line `shared_network <name> <cidr> <slug,slug,…>` per network, in file order, with `-` when there are no members.

## v1.15 per-host edge (2026-11-17, approved spec `docs/spec-domains-edge-routes.md` rev 2; additive, gated)

*Status: approved 2026-11-17 ("Approve both"); implemented on dev 2026-11-17 (bot-agent), not released. Ships in bot-agent v0.2.11. Rollout order: API first, then agent, then the runner.toml key. Items 7–9 record the implementation details that refine items 1–6.*

1. **Policy key** `edge = true|false` (bool; default false). An older agent rejects the key as `unknown_key`, so a host gets it only after it runs v0.2.11. `bot-agent policy check` adds `edge=<true|false>` after the shared-network lines.
2. **Hello:** `programs.edge = {"enabled": bool, "image": "<repo@sha256:…>"}` — `enabled` mirrors the policy; `image` is the Caddy image digest pinned in this agent release (multi-arch). Omitted by older agents; the API treats a missing field as disabled.
3. **Names and labels (no project):** container `deploybot-edge`, volume `deploybot-edge-data` (Caddy `/data`), config directory `/var/lib/bot-agent-runner/edge/` (root 0700) holding `config.json` (root 0444) and the admin socket `admin.sock`. Labels `dev.deploybot.owner=bot-agent`, `dev.deploybot.host=<id>`, `dev.deploybot.kind=edge`, `dev.deploybot.config_hash=<sha256 of config.json>`. Network mode `host`. The container binds 80 and 443 itself; Caddy's admin API listens only on `unix//run/edge/admin.sock` (the config directory bind-mounted at `/run/edge`), never on TCP.
4. **Task kinds** (server → agent), sent only when the hello says `edge.enabled`:
   - `edge_apply` `{"config": <Caddy JSON object>, "config_hash": "<hex>"}` (≤ 512 KiB). The agent checks the hash, then the helper verb `edge.apply`: refuse `edge_disabled` when the policy is off; refuse `port_in_use` (with the holder's name when known) when 80 or 443 is held by anything but `deploybot-edge`; write `config.json` atomically; ensure the volume; create and start the container when missing or when its image differs from the pinned digest (pull by digest first), else POST the config to Caddy's `/load` through the admin socket. Idempotent: an equal `config_hash` with a running edge answers `unchanged`. Result `{"outcome":"applied"|"unchanged"|"started","config_hash","image"}`; a Caddy load error answers `invalid_config` with Caddy's first error line, keeping the previous config running.
   - `edge_stop` (verb `edge.stop`): stop and remove the container; keep the volume and the config directory. Result `{"outcome":"stopped"|"absent"}`.
   - `edge_status` (verb `edge.status`): `{"state":"running"|"stopped"|"absent","image","config_hash","started_at"}`.
5. **Discovery:** the edge's listeners are reported with owner kind `edge` (new) so the API never counts the edge as an occupant of 80/443.
6. Older agents refuse the new kinds as `forbidden`; the API never sends them without the hello flag.
7. **Config allowlist (security review 2026-11-17).** The agent and the helper refuse with `bad_spec` (detail names the offending path, never a value) any `edge_apply` config that is not exactly this shape. Keys are matched exactly and case-sensitively, and duplicate keys are refused at every level:
   - top level `admin` and `apps` only; `admin` is exactly `{"listen":"unix//run/edge/admin.sock"}`;
   - `apps` holds `http` and optionally `tls`; each server (name `^[a-z0-9-]{1,32}$`) is exactly `{listen:[":443"], routes}`; a top-level route is `{match, handle}`, a subroute route `{handle}` with optional `match`; no `terminal`, no `group`;
   - matchers `host` (DNS names), `path` (lowercase segments, `*` only as the whole last segment), `remote_ip.ranges` (CIDRs), and `header` only as exactly `{"Cf-Connecting-Ip":["*"]}` beside `remote_ip`;
   - handlers `subroute` (at most 4 deep), `encode` (zstd, gzip, `prefer`), `headers` only as `response:{deferred:true,set:{"Cache-Control":["private, no-store"]}}`, and `reverse_proxy` with `upstreams:[{dial:"127.0.0.1:<1024–65535>"}]` and request headers limited to setting `X-Forwarded-For` to `{http.request.header.CF-Connecting-IP}` and deleting `Cf-Connecting-Ip`, `True-Client-Ip`, `X-Real-Ip`, and the optional `stream_close_delay` only as `"1h"` (the API sets it on every proxy so WebSockets survive a config load; spec-domains-edge-routes rev 3 §11, amended in place 2026-11-17 before release);
   - `tls` `automation.policies[{subjects, issuers?}]` with issuer `{"module":"internal"}` or `{"module":"acme","email"}`;
   - no other placeholder anywhere. `config_hash` is the sha256 of the exact compact bytes of `config` as framed.
8. **Results and gating as implemented:** `edge_disabled`, `port_in_use` and `invalid_config` come back as `task_result` `failed` with `error_code` and payload `{"outcome":code,"detail":"<one sanitised line, ≤ 300 bytes>"}`; further codes `conflict` (a container or volume named like the edge that Deploybot does not own), `bad_spec`, `docker_unavailable`, `image_unavailable`, `io_error`, `helper_unavailable`. `edge_stop` and `edge_status` work whatever the policy key says. While the pinned image digest is the all-zero placeholder, hello reports `edge.enabled:false` and apply is refused `edge_disabled` before any Docker call (a release-gate test fails until the real digest is set). `port_in_use` also checks UDP 443; an unreadable `/proc/net/tcp` or `udp` is `docker_unavailable`.
9. **Container as implemented:** host network; `CapDrop ALL` + `CapAdd NET_BIND_SERVICE`; `no-new-privileges`; read-only root filesystem with tmpfs `/config` and `/tmp`; memory 512 MiB; restart `unless-stopped`; private cgroup namespace; pids limit; json-file logs 10m × 3. A failed start restores the previous config and container. An unlabelled local volume `deploybot-edge-data` is adopted (the certificate-migration path). The `config_hash` label is informational. Discovery reports the running edge as one row named `deploybot-edge` with owner `{"kind":"edge","project":"","entry":""}` and ports 80 and 443; the API recognises it by that name.

## v1.16 a restart loop fails the health wait; time kept for the failure path (2026-11-21, approved spec `docs/spec-deploy-stuck.md` rev 1; bot-agent v0.2.12)

1. **Health wait after a service deploy or rollback** (v1.4/v1.5 service deploy, v1.6 item 7): in every branch (configured health checks, Docker healthcheck, or neither), a new container with `restart_count > 0` or state `restarting` is not healthy, and the wait ends at once. Before, only the branch without checks counted restarts; the others waited until the task deadline. The failure then follows `on_deploy_failure` as before (v1.6.1, v1.9 item 2).
2. **The health wait ends 60 s before the task deadline** (or at the deadline when the budget is 60 s or less). Rollback, stop, log tail and status for the failure path run in that reserve, on a context that has not expired, so the rollback or stop the policy asks for can complete. The task result is sent before the deadline.
3. In every branch, a `container_status` state of `exited`, `dead` or `missing` also ends the wait at once as not healthy, so a service with restart `no` that crashes fails without waiting out the budget. The reserve also applies when the budget comes from the `healthcheck` floor (the budget is not raised, so the agent never outlasts the server's wait), and it counts from the task start, so a pull that runs past budget − 60 s ends the wait at once.
4. No frame, field or outcome value changes. Servers need no change; an older agent keeps the old wait.

## v1.17 Docker API version negotiation; the docker flag follows Docker (2026-11-21, approved spec `docs/spec-agent-docker-versions.md` rev 1; bot-agent v0.2.12)

1. **Engine API version:** the root helper no longer fixes `/v1.47`. Each helper run first calls `GET /_ping` and reads the daemon's `API-Version` header, then uses the lower of 1.47 and that version for every request. The minimum is 1.41 (Docker 20.10): below it every verb refuses `docker_unavailable`, and the helper logs both versions.
2. **Below API 1.44** (Docker 20.10 to 24):
   - network create sends `CheckDuplicate: true`;
   - a deploy or rollback whose spec has shared `networks` (v1.13) and would create a container is refused before any container, network or volume change (the image pull may already have run; a job's previous container is kept). The task result is `failed`, and the first of at most 50 `lines` says that shared networks need Docker 25 (API 1.44). A rollback to the kept container (no create) is not refused.
3. **`hello.programs.docker`** is no longer latched for the life of the agent. The agent re-evaluates it on every discovery pass. When the value changes and holds for two passes in a row, the agent closes the session and reconnects, so the next `hello` carries the new value. It waits while a task is running. A change to `false` reconnects at most once per 5 minutes; a change back to `true` reconnects as soon as it is confirmed. No frame or field changes; servers need no change.
4. **Helper diagnostics:** Docker failures are logged by the helper with method, path (without query), HTTP status, the Engine message (at most 300 bytes, redacted) and the negotiated version. Not logged as failures: a 404 on a lookup or delete, and a 409 on volume or network create. The ping has its own deadline. Codes, outcomes and result fields are unchanged. This supersedes the fixed `/v1.47` in the image section above.
5. **New helper verb `docker_check`** (between the installer and the helper; never sent by the server): it is not gated by the runner policy, and it answers `present`, `api`, `ok`, `networks_ok` and a fixed `reason`. `install.sh check` uses it, and fails when the policy needs Docker (images or edge) and the check is not ok.


## v1.18 host config files (2026-11-22, approved spec `docs/spec-host-config-files.md` rev 1a; additive, gated; implemented in bot-agent work-hostfiles-agent 2026-11-22, pi3 live check passed; not yet released)
1. **`runner.toml` keys** (one-line string arrays; any violation makes the policy invalid with reason `parse`): `host_files = ["<name>=<path>"]`, `host_file_modes = ["<name>:0600|0640|0644"]`, `host_file_secret = ["<name>"]`, `host_file_units = ["<name>:<unit>.service"]`, `host_file_run_as = ["<name>:<account>"]`, `host_file_checks = ["<name>:<abs exe> <args…>"]`, `host_file_health = [...]`, `host_file_idle = [...]`. Rules as spec §2.1: name `^[a-z][a-z0-9-]{0,31}$`, at most 8 files; path exactly `/etc/<dir>/<file>` with reserved dirs refused; reserved units refused; `run_as` uid and gid ≥ 1000; commands split on single spaces, no shell, at most 16 arguments, executable (after resolving symlinks) and every directory to it root-owned and not group- or world-writable, rechecked at every apply. An older agent refuses the keys (`unknown_key`). (The `run_as` rule is superseded by v1.18.1.)
2. **`policy check`** prints `host_files=<count>` and one `host_file <name> <path> <mode> <unit> <run_as> secret=<bool> health=<bool>` line per file after the `shared_network` lines.
3. **`hello.programs.host_files`:** `[{"name","path","secret":bool,"unit","health":bool}]` from the loaded policy; `[]` when none. No content, no command lines. The API must know this field before an agent sends it.
4. **Task kind `host_file_apply`:** spec `{"name", "content_b64", "settle_sec", "force"}`; timeout 420 s. Refused `forbidden` by an agent whose hello has no `host_files`. Result `{"outcome", "stage", "reason", "took_ms", "sha256"?}` with outcome `succeeded | unchanged | worker_busy | check_failed | rolled_back | rollback_failed | forbidden_spec`; `sha256` only for a non-secret file; never bytes or stderr beyond the reason token (`^bad_[a-z0-9_]{1,64}$`, `check_error`, `idle_error`, `not_connected`, `policy_invalid`, `not_running`, `health_error`). `settle_sec` 30–180 (default 60), at least the health command's `--wait` + 15. Order and safety rules: spec §2.3.
5. **Frame `host_files_status`** (agent → API, at hello, after each apply, every 10 minutes): `{"files":[{"name","exists","owner_ok","reason"?,"matches_applied":bool|null,"size"?,"sha256"?}]}`; `size` and `sha256` only for a non-secret file.
6. **Helper verbs** `host_file_apply` and `host_file_status` (agent → helper only); checks, idle and health run as `run_as` in transient `systemd-run` units with `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateNetwork`; the previous file is kept at `/var/lib/bot-agent-runner/host-files/<name>.prev`.
7. **Installer:** `install|upgrade --runner` writes `/etc/systemd/system/bot-agent-runner@.service.d/host-files.conf` with one `ReadWritePaths=-/etc/<dir>` per distinct host-file directory, or removes it when there are none.
8. **Amendments (spec rev 1a §10 A1–A6, approved 2026-11-22; supersede items 1, 4 and 6 where they differ):** the helper serialises applies per unit (`/run/bot-agent/hostfile-unit-<unit>.lock`), not per file. An unsafe or missing check, health or idle executable does not invalidate the policy: the apply is `forbidden_spec` stage `exec` and `policy check` prints `exec=unsafe` on that file's line; syntax errors stay `parse`. Task timeout **600 s** (was 420); the health window starts after `systemctl restart` returns, the restart bounded at 60 s. Result: `succeeded` and `unchanged` are task state `succeeded`; the other five outcomes are state `failed` with `error_code` = outcome and payload `{outcome, stage, reason, took_ms}`; helper refusals and transport failures are state `failed` with `error_code` `bad_spec | policy_denied | io_error | helper_unavailable` and no `outcome`; `sha256` only on `succeeded`/`unchanged` of a non-secret file. Stages `lookup, settle, exec, target, check, idle, restart, health, rollback`. Status frame items may carry `reason` (`missing, symlink, not_regular, owner, mode, nlink, dir, too_large, io_error`). `unchanged` records the applied hash. Default settle 60, raised to the health `--wait` + 15; an explicit value below that is `forbidden_spec` stage `settle`; a health `--wait` above 165 s is `parse`. Check, health and idle units also get `InaccessiblePaths=` (Docker, containerd and Podman sockets), `PrivateDevices`, `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectControlGroups`, `RestrictSUIDSGID`, `LockPersonality`, `LimitCORE=0`, `TimeoutStopSec=2`, `MemoryMax=256M`, `TasksMax=64`, `ProtectProc=invisible`, `ProcSubset=pid`. More reserved dirs and unit prefixes (spec A6); a unit starts with a letter or digit; a secret file is mode 0600 or 0640; path components start with a letter, digit or `_`.

## v1.18.1 host-file run_as accepts a service account (2026-11-22, spec `docs/spec-host-config-files.md` §11 A8; bot-agent v0.2.14)
1. **`host_file_run_as`** (supersedes the `run_as` rule in v1.18 item 1). Before: "`run_as` uid and gid ≥ 1000". After: `host_file_run_as` names an existing account other than `bot-agent` and `nobody`, with uid ≥ 100 and a primary gid ≥ 100 that is not `root`/`0` or `nogroup` (gid 65534), and that shares neither uid nor primary gid with `bot-agent`. Any other account still makes the policy invalid with `parse`. Programs' `run_as` keeps uid and gid ≥ 1000.
2. **Older agents:** an agent ≤ 0.2.13 refuses a system account here with `parse`, so a host owner upgrades before naming one. No wire change.

## v1.19 deploy failure reason (2026-11-23, spec `docs/spec-deploy-reason.md` rev 1 in bot-control-plane; additive; with api-v1 v1.47; implemented on bot-agent `dev` 2c84a27, merged fe8c1ef)
1. **`task_result.payload` of `image_deploy` and `image_rollback`** gains an optional `"reason": string`: one line (no control characters), 1–512 bytes, present only when `outcome` is neither `succeeded` nor `unchanged`. The other five keys are unchanged; `ValidateImageDeploy` accepts six keys only when the sixth is `reason`.
2. **What it says.** The agent builds it from fixed words and its own observations only, never from helper, Docker, registry or container text, so it holds no env value and needs no redaction:
   - a helper refusal code: `image_unavailable` → "the image could not be pulled: the tag is missing or the registry refused the pull"; `policy_denied`, `bad_spec`, `conflict`, `docker_unavailable`, `shared_networks_need_docker25`, `helper_unavailable` get their own fixed sentence; any other code "the host refused the deploy (<code>)";
   - a helper transport or decode failure: "the Docker helper on the host failed";
   - the health wait (v1.16): "the container kept restarting (N restarts[, last exit code C])", "the container exited with code C", "the container stopped", "the container is gone", "the Docker health check reported unhealthy", "the Docker health check was still starting when the wait ended", "health check GET :<port><path> answered <status>[ (expected <min>-<max>)]", "health check <proto> :<port>[<path>] timed out", "health check … : connection refused (nothing listens on that port)", "health check … failed", "the container did not stay running for <d>", "the container was not running", or "the container did not become healthy in time".
3. For `rolled_back` and `rollback_failed` the reason is why the new container failed; the server adds what happened next. A job's non-zero exit stays in `exit_code` with no reason.
4. **Servers:** an API before v1.47 ignores the key (the payload is read by field). API v1.47 stores it as the deploy's reason, after its own exact-value redaction, and falls back to a reason it works out from the outcome and status when the key is absent or not one line of at most 512 bytes. No other frame changes.

## v1.20 agent upgrade from the server (renumbered from v1.19 on 2026-11-23 because of the clash above) (2026-11-23, approved spec `docs/spec-agent-self-upgrade.md` rev 1; additive, gated; with api-v1 v1.48; implemented on bot-agent `dev` 79194ce, merged 79d7612; in bot-agent v0.2.15)
1. **`runner.toml` keys:**
   - `agent_upgrade = true|false` (bool; missing = false).
   - Optional `agent_upgrade_https_proxy = "http(s)://host:port"`: no userinfo or path, at most 256 bytes; anything else makes the policy invalid with `parse`. `policy check` prints `agent_upgrade_https_proxy=<set|unset>`.
   - An older agent refuses it as `unknown_key`.
   - `policy check` prints `agent_upgrade=<bool>` after the `host_file` lines.
2. **Hello:**
   - `programs.agent_upgrade` (bool). It is true only when the policy is valid and the key is true. It is absent where there is no `programs` block (no helper), which the API reads as false.
   - Top-level **`last_upgrade`**: `{"version","outcome","reason","at"}` or `null`.
     - `outcome` is one of `succeeded`, `rolled_back` or `failed`.
     - `reason` is a token: `download_failed`, `signature_invalid`, `bundle_invalid`, `version_mismatch`, `install_failed`, `not_reconnected`, or empty.
     - `at` is RFC 3339 UTC.
     - It comes from the helper's result file, read at start.
   - The API must know these fields before an agent sends them.
3. **Frame `agent_upgrade_result`** (agent → API): `{"result":{same as last_upgrade}}`.
   - It is sent whenever the helper's result differs from the last one this agent process sent or put in its hello.
   - The result file is written after the installer finishes, so the new or restored agent usually starts before it exists.
   - When the host is opted in, the agent polls the helper's `agent_upgrade_result` every 10 s, for up to 15 minutes: at start, and after it accepts an `agent_upgrade` task. It stops early once the upgrade unit is inactive and a new result has been sent.
4. **Task kind `agent_upgrade`**, spec `{"version":"X.Y.Z"}`, timeout 120 s.
   - The agent refuses it with `forbidden` when its hello had `agent_upgrade` false.
   - It forwards the task to the helper verb `agent_upgrade`.
   - **Success:** state `succeeded`, payload `{"state":"accepted"}`. The upgrade runs on after the task ends.
   - **Failure:** state `failed` with `error_code` one of `policy_denied`, `busy`, `not_newer`, `bad_spec` or `helper_unavailable`.
5. **Helper verb `agent_upgrade {"version"}`.**
   - Checks, in order:
     - it re-reads `runner.toml`; `agent_upgrade` must be true, else `policy_denied`;
     - `version` matches `^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$` and is newer than the installed version (the installer record's `version` line), else `bad_spec` or `not_newer`;
     - it takes the helper-wide lock without waiting, and no other helper lock may be held, else `busy`;
     - no `bot-agent-upgrade.service` may be active, activating or deactivating, else `busy`;
     - if the result file is under 10 minutes old, its outcome is not `succeeded` and its version equals the requested one, `busy` (a cool-down).
   - It then starts `systemd-run --unit=bot-agent-upgrade --collect --property=RuntimeMaxSec=900 --property=TimeoutStopSec=150 --property=KillMode=mixed /usr/local/libexec/bot-agent-runner upgrade-run <version>` and answers `accepted`.
   - While that unit is active, the mutating helper verbs (programs, images, edge, `host_file_apply`) answer `busy`.
   - The host checks only "newer than installed, and signed". "Latest" is enforced by the server.
   - Nothing from the request reaches the command except the checked version.
6. **`bot-agent-runner upgrade-run <version>`** (root, transient unit, not the helper's sandbox):
   1. **Download** from the fixed base `https://github.com/more-than-code/bot-agent/releases/download/v<version>/`: first `SHA256SUMS` and `SHA256SUMS.sig`, then only after step 2 passes `bot-agent-v<version>.tar.gz`.
      - HTTPS only; redirects only to `github.com`, `objects.githubusercontent.com` or `release-assets.githubusercontent.com` on port 443 with no userinfo, at most 5.
      - 64 MiB per file and 120 s per file.
      - A proxy is used only from the root-owned `runner.toml` key `agent_upgrade_https_proxy` (item 1), never from the agent's env file.
   2. **Verify** `SHA256SUMS.sig` (base64 Ed25519 over the exact `SHA256SUMS` bytes) with the public key compiled into the helper, else `signature_invalid`.
      - A release built with the placeholder key refuses every bundle.
   3. **Unpack** into `/var/lib/bot-agent-runner/upgrade/<version>/` (0700 root). Only regular files and directories are allowed, under one top directory `bot-agent-v<version>/`, with no `..`, absolute paths or links, and 256 MiB in total, including the decompressed stream. Otherwise `bundle_invalid`.
      - The bundled `SHA256SUMS` must equal the signed one byte for byte.
      - It must list `install.sh` and the four `units/*` files.
      - Every listed file must match, and no unlisted file may be present.
      - The bundled `bot-agent` for this architecture must print `<version>`, else `version_mismatch`.
   4. **Install:** run the bundled `install.sh upgrade`, with `--runner` when the runner is installed, with `BOT_AGENT_INSTALL_BUNDLE` set to that directory.
      - Exit 0 means `succeeded`.
      - The installer's own restore means `rolled_back` with `not_reconnected` or `install_failed`.
      - Anything else, including a stop by SIGTERM (passed on to the installer, which then gets 120 s to roll back), means `failed` with `install_failed`.
   5. **Result:** write `/var/lib/bot-agent-runner/upgrade/result.json` (0600) with the outcome. Old staged versions are removed except the current one.
7. **Helper verb `agent_upgrade_result {}`:** the result file's content, or `null`. Read only.
8. **Installer (bundled `install.sh`):** `verify_active_version` additionally waits up to 90 s for `/var/lib/bot-agent/last_hello` to have an mtime after the restart.
   - The agent writes that file on every acknowledged hello.
   - A missing or old marker fails the verify, so the existing rollback runs. The rollback's verify keeps the existing checks (active, `NRestarts` 0, version) and does **not** wait for the marker: an agent older than 0.2.15 never writes it, and with the server down no agent can say hello. A rollback whose verify passes is `rolled_back`; a failed rollback verify is `failed` `install_failed`.
   - `install.sh` documents the marker check. A plain SSH upgrade gets it as well.
9. **Bootstrap:** this needs bot-agent 0.2.15 or later, installed once by the host owner, with `agent_upgrade = true` in `runner.toml`.

## v1.21 containers that deploy Deploybot (2026-11-23, approved spec `docs/spec-deploybot-on-containers.md` rev 1; additive, gated by `runner.toml`; with api-v1 v1.50; implemented on bot-agent `dev` c22987e, merged 2513a9e; in bot-agent v0.2.15)
1. **`runner.toml` keys** (an older agent refuses both as `unknown_key`):
   - `host_network_projects = ["<slug>", …]` (default empty). Each slug must also be in `projects`, else the policy is invalid with `parse`. At most 8.
   - `host_env_files = ["<project>/<entry>=<path>", …]` (default empty), at most 32. `<project>` is a slug in `projects`; `<entry>` is a catalog entry name (the catalog name rules). `<path>` is absolute, directly under a directory of `/etc/` (`/etc/<dir>/<name>.env`), with no `..`. The file must be a regular file (not a symlink), owned by root:root, mode 0600, nlink 1, at most 64 KiB, and made of `KEY=VALUE` lines (key `^[A-Za-z_][A-Za-z0-9_]{0,127}$`; blank lines and `#` comments allowed; no duplicate keys). Any failure makes the policy invalid with `host_env_file` and the entry name; the value is never printed.
   - `policy check` prints `host_network_projects=<comma list or none>` and one line per env file: `host_env_file <project>/<entry> keys=<n> sha256=<first 12 hex of the file>`, never a value.
2. **Hello:** `programs.host_network` (bool): true only when the policy is valid and `host_network_projects` is non-empty. Absent without a `programs` block, read as false.
3. **ImageSpec `network`** (optional string; absent = today's behaviour; the only other value is `"host"`):
   - Allowed only when the task's project is in `host_network_projects`, else the task fails `policy_denied`.
   - With `"host"`: the container runs with Docker network mode `host`; `ports` are not published, and each `ports[].host` is only the loopback port the health check uses; `networks` (shared networks, v1.13) must be empty, else `bad_spec`. User, filesystem, memory limit, log limit and health policy are unchanged.
   - The spec hash covers `network`, so changing it is not `unchanged`.
4. **Host env files at deploy and run** (image deploy, rollback and job run of an entry that has a `host_env_files` line):
   - The helper re-reads and re-checks the file at that moment (item 1 rules), else the task fails `policy_denied` with reason `host_env_file`.
   - Its keys are added to the container's environment. A key that is also in the entry's own env (env set or spec env) fails the task `bad_spec` with reason `env_conflict` and the key name, never the value. A key on the API's env denylist is refused the same way.
   - The env hash (v1.14) includes the file's sha256, so a changed file is not `unchanged`; the file's values never appear in previews (v1.7), results, logs or `deploy_history`.
5. **Deploys finish on the host:**
   - An image deploy, its health wait and its automatic rollback (and a job run) continue when the API connection drops or the agent restarts its session; they run to their own timeout.
   - The final result (the same body as the task's result frame) is written to `/var/lib/bot-agent/results/<task_id>.json` (0600, owner `bot-agent`) before it is sent. At most 100 files and 7 days; the oldest go first.
   - After each hello, the agent sends every stored result in the normal task-result frame with its original `task_id`, plus `"late": true` when the task was started on an earlier connection. It deletes a file when the API answers with frame **`task_result_ack {"task_id"}`**; an API that never acks (older) leaves the files to the cap and age limit.
6. **Clarifications from implementation (2026-11-23, before release):**
   - The agent itself (unprivileged) checks only the `host_env_files` lines; the file checks of item 1 are done by the root helper (at every deploy, rollback and run) and by `policy check` (run as root). So a hello can report a valid policy while the helper refuses a bad file with `policy_denied` / `host_env_file`.
   - A malformed line (no `<project>/<entry>=<path>` shape) or more than 32 lines is `parse`; other line or file failures are `host_env_file` with the entry name. Two entries may name the same file; a repeated entry is refused.
   - A changed host env file is noticed only when the task carries `env_hash` (v1.14): the helper keeps the file's sha256 in the container label `dev.deploybot.host_env_sha256`, and `container_status` reports `env_hash` only while that label matches the current file. The kept-container rollback does not re-check the file, so an automatic rollback always works.
   - Helper refusals carry `{"error","detail","key"}`; the agent turns them into fixed v1.19 reason sentences (`env_conflict <KEY>: …`, `host_env_file: …`) that never contain a value. The helper also redacts the entry's host env values (6 bytes or longer) from that entry's container logs.
   - "Detached" covers `image_deploy` (a job run is one) and `image_rollback`. If the agent process stops, the work stops too; a stored result is sent after the restart with `"late": true`.
   - With `network: "host"`, `ports[].public` is accepted and has no effect (nothing is published).
   - Only `task_result_ack` deletes a stored result; the older `task_ack` does not.
7. **Review changes (2026-11-23, before release; supersede item 6 where they differ):**
   - **Image binding:** every `host_env_files` line needs a matching `host_env_images = ["<project>/<entry>=<image pattern>", …]` line (same pattern syntax as `images`; it must also be within `images`), else the policy is invalid with `host_env_file`. The helper refuses a deploy, rollback-by-pull or run whose image does not match with `policy_denied` / `host_env_image`. The env file's secrets reach only images the host owner named for that entry.
   - **The helper loads lines only:** every helper verb loads the policy without reading env files; the files are read only when merging (deploy, run), for status `env_hash`, and for log redaction, and by `policy check`. A bad file therefore refuses only deploys of its entry (`policy_denied` / `host_env_file`); rollback, stop, start, status, logs, list, programs, edge, host files and agent upgrade keep working.
   - **No overlap with host files:** a `host_env_files` path equal to a `host_files` path, or inside a `host_files` directory, makes the policy invalid with `host_env_file`. The env file's directory and `/etc` must be root-owned and not group/world-writable; the file is opened with `O_NOFOLLOW|O_NONBLOCK|O_NOCTTY`.
   - **Host mode hardening:** a host-network container gets `CapDrop: ["NET_RAW"]` and `SecurityOpt: ["no-new-privileges"]`. It still shares loopback and abstract unix sockets with the host; the host owner allows it per project knowing that.
   - **Manual rollback and start re-check:** a manual rollback, start or restart of a host-network container is refused `policy_denied` when its project is no longer in `host_network_projects`. The automatic rollback inside a deploy still runs for an allowed project.
   - **Log redaction fails closed:** logs of an entry with a host env file are redacted only when the file's sha256 equals the container's `dev.deploybot.host_env_sha256` label; otherwise (changed or unreadable file) the lines are withheld with one line saying so. Values under 6 bytes are not redacted (documented; do not put short secrets in a host env file).
   - **Stopping agent:** a detached task interrupted by the agent stopping stores and sends no result; the API's sweep settles it (api-v1 v1.50 item 8). A reject of an admitted detached task goes to the current connection.
   - **Hello:** `programs.host_network` is sent only when true; an API older than v1.50 never sees it. Upgrade the server first anyway.
   - **Health in host mode:** `ports[].host` probes reach whatever listens on that loopback port; use a Docker healthcheck in the image (the Deploybot images have one) so another process on the port cannot mask a broken deploy.
   - **At rest:** stored results may hold up to the deploy's log tail (already redacted) for up to 7 days, 0600 under the agent's state directory.

## v1.22 host policy from the console (2026-11-23, spec `docs/spec-console-host-policy.md` rev 2 §2.2 in bot-control-plane, as implemented; additive, gated by `runner.toml`; with api-v1 v1.51; implemented on bot-agent `dev` e2f7049, merged 20895c3; in bot-agent v0.2.16)
1. **`runner.toml` key `console_policy = true|false`** (missing = false). The host's root opts in once by hand; an older agent refuses the key as `unknown_key`. `bot-agent policy check` prints `console_policy=<bool>` as its last line.
   - Only three keys are console-editable: `edge`, `images` and `projects`. Never `enabled`, `run_as`, `dirs`, `exec_allow`, `allow_shell`, `max_programs`, `max_containers`, `allow_public_ports`, shared networks, `host_files`, `agent_upgrade`, its proxy, or `console_policy` itself.
2. **Hello:** `programs.console_policy` (bool) and `programs.image_patterns` (the runner.toml `images`, sent only when `console_policy` is true; at most 64). The existing `programs.policy_hash`, `edge` and `projects` carry the rest. There is no result frame: the server reads the outcome from the next hello's `policy_hash`.
3. **Task kind `host_policy_apply`**, spec `{"base_hash","edge","images","projects"}`, timeout 60 s. Strict: no other key, valid patterns and slugs, no repeats, at most 64 images and 32 projects, else `bad_spec`. Refused `forbidden` when the host is not opted in. It is forwarded to the helper verb of the same name; success is state `succeeded` with `{"state":"accepted","new_hash"}`.
4. **Helper verb `host_policy_apply`** (root), in order:
   - re-read runner.toml; `console_policy` must be true, else `policy_denied`;
   - its SHA-256 must equal `base_hash`, else `conflict`;
   - rewrite only the three keys, line by line, keeping comments and order (a missing key is appended); no change is `bad_spec`;
   - load the result with the same policy loader: it must be valid and keep `console_policy = true`, else `bad_spec`;
   - no other helper lock held, no agent upgrade running and the policy unit not active, else `busy`;
   - stage the new file under `/var/lib/bot-agent-runner/policy/`, start the transient unit `bot-agent-policy` (`bot-agent-runner policy-run <base_hash> <new_hash>`) and answer `accepted` with `new_hash`. The helper's sandbox cannot write `/etc`; the unit does.
5. **`bot-agent-runner policy-run`** (the transient unit, `RuntimeMaxSec` 300): wait 3 s so the agent can send the task result; check runner.toml still has `base_hash` and the staged file `new_hash`; keep `/etc/bot-agent/runner.toml.prev` (0600); write atomically; restart bot-agent and wait up to 90 s for `/var/lib/bot-agent/last_hello` newer than the restart. On failure it puts the previous file back and restarts again (`rolled_back`, `not_reconnected`). It writes `result.json` for the root and the journal.

## v1.23 bad_spec names the field (2026-11-23, DD DEF-20261123-agent-bad-spec-refusals-don-t-name-the-f-yars; additive, no gate; with api-v1 v1.55; spec `docs/spec-refused-deploy.md` in bot-control-plane; in bot-agent v0.2.16)

1. **`task_reject` gains an optional `"field": string`**, sent only with `error_code` `bad_spec` on an image task (`image_*`). It is the key that failed, never a value:
   - a key of the task spec: `project`, `entry`, `spec`, `env`, `registry_auth`, `spec_hash`, `digest`, `files`, `secret_keys`, `env_hash`, `lines`; or `service` when the task frame's `service` is not null;
   - `spec.<key>` for an ImageSpec rule (v1.4 and later): the name `ImageSpec.Validate` returns, for example `spec.secret_env`, `spec.ports`, `spec.health_policy.on_unhealthy`. A healthcheck key is `spec.healthcheck.<key>` (`spec.healthcheck.timeout_sec` is not the spec's own `spec.timeout_sec`).
   - A JSON type error names the same key as the rule would: `spec.healthcheck.timeout_sec`, not `spec.healthcheck`.
   - Shape `^[a-z][a-z0-9_]{0,31}(\.[a-z][a-z0-9_]{0,31}){0,2}$`, at most 64 bytes. The agent builds it only from key names it defines. A JSON type error names its key only when that key is one of the above; an unknown key, an env key name, a map key or a body that is not JSON gives no `field`.
   - The agent log line becomes `task_reject code=bad_spec field=<field>`.
2. **No gate.** Every API reads `task_reject` by field and ignores an unknown key, so an older API keeps today's answer. API v1.55 and later show the field (api-v1 v1.55 item 1).
3. A helper `bad_spec` (a `task_result`) is unchanged: v1.19 `reason` already explains it.

## v1.24 containers on kernels without cgroup namespaces (2026-11-24, approved spec `docs/spec-agent-0216-cgroupns.md` rev 1 in the umbrella; additive, gated by `runner.toml`; no API change; in bot-agent v0.2.16)

1. **`runner.toml` key `container_cgroupns = "private"|"host"`** (missing = `private`). Only the host's root sets it; the v1.22 console rewrite never changes it. Any other value, or a duplicate key, is `parse`. An older agent refuses the key as `unknown_key`.
   - The helper sends `HostConfig.CgroupnsMode` from it for every container `container_deploy` creates (service and job, including an `image_rollback` that pulls) and for the edge container. `container_rollback` (the swap to the kept container) creates nothing. It was always `"private"` before; with the key absent nothing changes.
   - `bot-agent policy check` prints `container_cgroupns=<value>` as its last line (after `console_policy=`).
2. **A container start the runtime refuses has its own helper code `container_start_failed`.** It covers Docker answering `POST /containers/{id}/start` with 500: a runtime refusal (for example `cgroup namespaces aren't enabled in the kernel`) or any other daemon error during start. It applies to every catalog verb that starts a container: `container_deploy`, `container_start`, `container_restart` and `container_rollback`. It was `docker_unavailable` before; a failed ping, a transport error and other Engine answers stay `docker_unavailable`.
   - On `container_deploy` the helper removes the container it just created (no force; it was never started), on its own short context so a cancelled caller still cleans up. It logs the runtime's message in the helper journal (`bot-agent-runner@*`), scrubbed as today. The message never leaves the host.
   - The agent words it in the v1.19 deploy-result `reason`: "the container runtime on the host could not start the container (see the bot-agent-runner log on the host)". A rollback whose restart of the previous container is refused also reports `container_start_failed`.
   - The edge verbs keep `docker_unavailable` for a start failure: they have their own codes.
3. **No wire change:** there is no new frame or hello field. An API that does not know the code shows it as the agent sent it; API v1.47 and later store the reason text.

## v1.25 Docker disk usage (2026-11-24, approved spec `docs/spec-disk-usage-breakdown.md` rev 1 §10 in the umbrella; additive; with api-v1 v1.59; in bot-agent v0.2.17)

1. **Task kind `docker_usage`**, spec exactly `{}` (any other spec, or a non-null `service`, is `task_reject` `bad_spec`), `timeout_sec` 45 from the API (unset is 45 s, at most 60 s). Nothing is stored. An agent before 0.2.17 rejects the kind `forbidden`, as for any kind it does not know; the API checks the hello `agent_version` first.
   - Success: state `succeeded`, payload `{"outcome":"ok","summary":{…}}` (item 3).
   - Failure: state `failed`, `error_code` and payload `{"outcome":"failed","reason":code}` with the same code: `docker_usage_disabled` (neither `images` nor `edge` is on; the agent checks its own policy first, so a host without the helper gets it too), `docker_unavailable` (Docker did not answer or answered badly; also any other helper refusal, a transport error or a reply that fails the agent's strict check), `timeout` (the helper's Engine deadline, or this task's own deadline; a session that ends first is `docker_unavailable`).
2. **Helper verb `docker_df`** (read-only, not mutating, so it runs during an agent upgrade). Request exactly `{}` (else `bad_spec`). The gate is `container_list`'s: `images` enabled OR `edge` enabled in `runner.toml`, re-read per call; otherwise `{"error":"docker_usage_disabled"}` before any Engine call.
   - The Engine call is `GET /system/df` at the negotiated version, with its own 30 s deadline and a 32 MiB body limit for this call only (every other call keeps 4 MiB). A deadline overrun is `{"error":"timeout"}`; unreachable, a non-200 answer, a body over the limit or one that does not parse is `{"error":"docker_unavailable"}`. The Engine's message goes only to the helper journal, scrubbed as today (v1.17 item 4).
   - Success is `{"summary":{…}}`. The agent decodes it strictly (no other key, lists ≤ 10, strings ≤ 200 characters, no negative number).
3. **Summary** (bytes as integers):
   ```json
   {"totals":{"images":{"count":N,"active":N,"size":B,"reclaimable":B},
              "containers":{…},"volumes":{…},"build_cache":{…}},
    "images":[{"ref":"ghcr.io/o/app:1.2","id":"0123456789ab","size":B,"containers":N}],
    "volumes":[{"name":"pgdata","size":B,"in_use":true}],
    "containers":[{"name":"deploybot-api","state":"running","size":B}]}
   ```
   - A size Docker reports as -1 counts as 0 in the totals and is `null` in a list; it sorts below every known size.
   - **images:** `ref` is the first RepoTag, or `<none>` when there is none or it is `<none>:<none>`; `id` the first 12 hex digits after `sha256:` (`""` if the id has another shape); `containers` is Docker's `Containers` with -1 as 0. `active` counts images with `containers` > 0. The totals follow `docker system df`, checked against Docker 29.9's raw answer on a dev host (2026-11-24): `size` is Docker's `LayersSize` (a shared layer counted once; the sum of the sizes when the daemon sends none), and `reclaimable` is the sum of `Size` - `SharedSize` over the images with `containers` 0 (the whole `Size` when `SharedSize` is -1 or larger than `Size`; an image whose `Size` is -1 adds nothing), at most `size`.
   - **containers:** `size` is `SizeRw` (missing is 0); `name` the first name without its leading `/`; `active` counts `running`; `reclaimable` sums the rest.
   - **volumes:** `size` is `UsageData.Size`; `in_use` is `UsageData.RefCount` > 0; `active` counts those in use; `reclaimable` sums the rest. A volume whose `UsageData` is null or missing has an unknown size (null in the list, 0 in the totals) and is not in use.
   - **build_cache:** `count` counts every record and `active` those `InUse`. As in `docker system df`, a record with `Shared` true (its data is also held by another record) is left out of `size` and `reclaimable`: `size` sums `Size` of the others, `reclaimable` those of them not in use. There is no build-cache list.
   - Each list holds the 10 largest, by size descending, then `name` or `ref` ascending (images then by `id`). Totals cover everything. Empty lists are `[]`.
   - Every string is capped at 200 characters (invalid UTF-8 replaced). Nothing else is read from the Engine answer: no labels, env, mounts, host paths, commands, or digests other than the short image id.
4. **No new `runner.toml` key and no hello field.**

## v1.26 host env files reach only the pinned run (2026-11-25, approved spec `docs/spec-host-env-run-pin.md` rev 1 in bot-agent; fixes DD DEF-20261123-host-env-file-secrets-readable-by-anyone-w54e; gated by `runner.toml`; no API change; in bot-agent v0.2.18)

1. **`runner.toml` keys** (root only; the v1.22 console rewrite never changes them; an older agent refuses them as `unknown_key`):
   - `host_env_command = ["<project>/<entry>=<argv>", …]` and `host_env_healthcheck = ["<project>/<entry>=<argv>", …]`, at most 32 items each, one per entry, each for an entry with a `host_env_files` line.
   - `<argv>` is split on single spaces: 1 to 17 arguments (a program and at most 16 arguments), each 1–256 bytes of printable ASCII without space, `"`, `'` or `\`. A healthcheck's first argument is an absolute path.
   - An item that does not split into `<project>/<entry>=<value>`, or more than 32 items of either key, is `parse`; a bad argv, a repeated entry or an entry without a `host_env_files` line is `host_env_file` with the entry name.
   - `policy check` ends each `host_env_file` line with ` command=<n|image> healthcheck=<n|image>` (the argument count, or `image` without a line).
2. **Helper check** (`container_deploy`: image deploy, rollback by pull and job run), for an entry with a `host_env_files` line, after the image check (v1.21 item 7) and before the file is read:
   - `spec.command` equals the `host_env_command` argv (empty without a line);
   - `spec.healthcheck` is absent without a `host_env_healthcheck` line, else present with `test` equal to it (timings are the entry's);
   - `spec.entrypoint`, `spec.user`, `spec.volumes`, `spec.files` and the task's `files`, `spec.env`, `spec.secret_env`, `spec.env_sets` and the task's `env` are empty.
   - Else `{"error":"policy_denied","detail":"host_env_run","key":"<field>"}`, field one of `command`, `entrypoint`, `healthcheck`, `user`, `volumes`, `files`, `env` (the first that differs, in that order). No container is created; no value is logged. A key that both the entry and the file set is therefore `host_env_run env`; v1.21 `env_conflict` remains for a file key on the env denylist.
   - Start, stop, restart, the kept-container rollback (`container_rollback`) and the automatic rollback inside a deploy are not re-checked (a rollback that pulls is a `container_deploy` and is): env is fixed at create, so a container from before v1.26 holds only the values from then (rotate them if anyone but root could deploy the entry).
3. **Agent:** the v1.19 deploy-result reason is `host_env_run <field>: this entry gets a host env file, so its command and healthcheck must match host_env_command and host_env_healthcheck in runner.toml, and it may not set entrypoint, user, volumes, files or its own env`; a field outside the list gives the sentence without it.
4. **Supersedes** v1.21 item 4 where an entry-env key overlaps the file (now `host_env_run env`) and the last sentence of item 7: the file's secrets reach only the named images, run with the pinned command and healthcheck.
5. **No wire change** beyond the new detail: no frame, hello field or task spec. An API stores the reason text as for v1.21.
