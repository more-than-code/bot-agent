# bot-agent

Design v2 (2026-10-13): `docs/design.md` adds the `cmd/bot-agent` daemon, which replaces Telegraf after an overlap period. Cross-repo design: umbrella `docs/design-bot-agent-deploy.md`. Phase 1 of the daemon (`cmd/bot-agent`, `internal/agent/`, `packaging/`) is implemented and not installed on any host; build with `go build ./cmd/bot-agent` and test with `go test ./...`. The renderer below stays current until the overlap ends.

Telegraf snippets and Services SQL for Deploybot. Umbrella: `/home/joe/workspace/deploybot`. Spec: `docs/spec-v1.md` (Services).

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
