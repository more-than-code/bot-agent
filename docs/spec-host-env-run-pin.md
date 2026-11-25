# Spec: Host env files reach only the run the host owner pinned

**Status:** revision 1 **approved by Joe 2026-11-25 ("approved", H1–H6 defaults)**; implemented on this branch.
**Tier:** 3 (root helper, `runner.toml`, agent-protocol). Claims **agent-protocol v1.26** (v1.25 is the newest implemented; no open PR in either repo claims v1.26 on 2026-11-25; renumber on this branch if another PR merges it first).
**Repo:** bot-agent only. No API, console, MCP or database change (see §6).
**Fixes:** DD `DEF-20261123-host-env-file-secrets-readable-by-anyone-w54e` (P1, 2026-11-23 security review).
**Builds on:** agent-protocol v1.21 (`docs/spec-deploybot-on-containers.md` in bot-control-plane), items 4 and 7.

## 1. Problem

v1.21 lets root give a catalog entry environment variables from a root-only file (`host_env_files`), bound to an image repository (`host_env_images`). Deploybot uses it for its own server on its host: `deploybot/api`, `deploybot/migrate` and `deploybot/mcp` get `/etc/deploybot/api.env` and `mcp.env` (database password, OAuth and session secrets).

The helper checks only the image (`internal/runner/hostenv.go:15-44`, `hostEnvImageOK`). Everything else about how the container runs is still chosen by the server, so by anyone with the `deploy` right on the entry (a project member, an MCP agent acting for them, or a compromised control plane):

| Spec field | How it leaks the file |
|---|---|
| `command`, `entrypoint` (`create.go:179-184`) | `sh -c 'env \| base64'`, then read `entry_logs` or the deploy log tail. Redaction (`hostEnvLogs`) matches exact values only, so any encoding passes. With `network: "host"` the values can also be sent out directly. Needs only a shell or interpreter in some image of the repository. |
| `healthcheck.test` | Docker runs it inside the container with the same env; it can write to `/proc/1/fd/1` (the container log) or the network. Same effect as `command`. |
| `volumes` | A volume is per project, so another entry of the project (any image in `images`) can fill it; mounted over a path of the trusted image (for example the binary's directory), it replaces the code that runs with the secrets. |
| `files` | Same idea with a 0444 file at any target: a config file the app trusts, or `/etc/ld.so.preload` pointing at a library from a volume. |
| `env`, `env_sets`, `secret_env` | Keys the file does not set but the program reads: `HTTPS_PROXY` plus `SSL_CERT_FILE` (from `files`) lets a proxy read the program's outbound TLS traffic, which carries the secrets; app keys can point a mail or webhook target elsewhere. The v1.21 denylist (`PATH`, `LD_*`, `NODE_OPTIONS`, …) does not cover these and a denylist cannot. |
| `user` | Not a leak by itself; refused for the same reason as the rest: the trusted run is the image's own. |

So the promise in `docs/install.md` ("the file's secrets reach only the images you name") and v1.21 item 7 does not hold: the file reaches anyone who can deploy the entry.

Out of the problem: `ports`, `network`, shared `networks`, `memory_mb`, `restart`, `kind`, timings, logging and health policy decide where the trusted program listens and how it is supervised, not what code runs or what it reads. They stay the server's.

## 2. Decisions (Joe to confirm; defaults marked)

| # | Question | Default | Alternative |
|---|---|---|---|
| H1 | Fix in code or in the docs | **Code:** the helper refuses a run of a host-env entry whose shape is not the one root pinned (§3). | Docs only: say that anyone who can deploy the entry can read the file. Leaves Deploybot's own secrets with every deployer of the `deploybot` project. |
| H2 | How root pins the command | **Two new `runner.toml` keys, `host_env_command` and `host_env_healthcheck`,** one line per entry, exact argv. No line means the image's own (`command` / `healthcheck` must be absent). | Require the image defaults only (no keys). Does not fit today: one image serves `api`, `mcp` and `migrate`, picked by `command`, and the image's built-in healthcheck cannot know which port. |
| H3 | Entry's own env on a host-env entry | **Refused:** `env`, `env_sets` and `secret_env` must be empty; all of its env comes from the root file. Today's Deploybot entries set none. | An allowlist key per entry (`host_env_extra_keys`). More config for no current user; can be added later. |
| H4 | Mounts on a host-env entry | **Refused:** `volumes` and `files` must be empty. Today's entries set none. | Root-pinned mount lists. Same reasoning as H3. |
| H5 | Containers created before the fix | **No re-check on start, restart or rollback.** Env is fixed when a container is created, so an old container holds only the values from before the fix. Upgrade step (§5): if anyone other than root could deploy the entry, rotate the file's secrets, then deploy. | A helper label with the pin's hash, checked on manual start/restart/rollback. It would also refuse the agent's own health-policy restart of Deploybot's running API until the first deploy after upgrade, and it adds nothing once secrets are rotated. |
| H6 | Tag or digest pin | **Not in this change.** Patterns name repositories only (`images` syntax has no tag). The trust anchor stays "whoever can push to the repository". With the command pinned, an older tag runs only that tag's real `api`/`mcp` code. | Add a digest or tag pin per entry. Every release would then need a root edit on the host, which the console upgrade path avoids. Can be a later spec. |

## 3. Design (agent-protocol v1.26)

### 3.1 `runner.toml` keys (root only)

```toml
host_env_command     = ["deploybot/api=api", "deploybot/migrate=api --migrate", "deploybot/mcp=mcp"]
host_env_healthcheck = ["deploybot/api=/usr/local/bin/deploybot-server health http://127.0.0.1:8190/health", "deploybot/mcp=/usr/local/bin/deploybot-server health http://127.0.0.1:8191/health"]
```

- Item shape `"<project>/<entry>=<argv>"`. The `<project>/<entry>` must have a `host_env_files` line; one item per entry; at most 32 items per key.
- `<argv>` is split on single spaces, the same as the `host_file_*` commands: a program and at most 16 arguments (1 to 17 items), no empty argument, no shell and no quotes. Each argument 1–256 bytes of printable ASCII without space, `"`, `'` or `\`. The first argument of `host_env_healthcheck` is an absolute path (Docker `CMD` form runs it without a shell).
- An item that does not split into `<project>/<entry>=<value>`, or more than 32 items, makes the policy invalid with `parse` (as v1.21 item 6). A bad argv, a duplicate, or a `<project>/<entry>` without a `host_env_files` line makes it invalid with `host_env_file` and that entry name. An older agent refuses both keys as `unknown_key`.
- The v1.22 console rewrite never changes them (they are not `edge`, `images` or `projects`).
- `policy check` adds two fields to the end of each `host_env_file` line: `command=<n args|image>` and `healthcheck=<n args|image>`. Still no value of the file.

### 3.2 Helper check at deploy and run

In `hostEnvMerge`, after the image check and before the file is read, for an entry that has a `host_env_files` line (image deploy, rollback by pull and job run, so every path that creates a container with the file):

| Field | Must be |
|---|---|
| `spec.command` | equal to the `host_env_command` argv; empty when the entry has no line |
| `spec.entrypoint` | empty |
| `spec.healthcheck` | absent when there is no `host_env_healthcheck` line; otherwise present with `test` equal to that argv (its timings are free) |
| `spec.user` | empty |
| `spec.volumes`, `spec.files` (and the request's `files`) | empty |
| `spec.env`, `spec.secret_env`, `spec.env_sets` and the request's merged `env` | empty |

The first field that differs refuses the task: `policy_denied`, detail **`host_env_run`**, key = the field name (`command`, `entrypoint`, `healthcheck`, `user`, `volumes`, `files`, `env`). The refusal is logged like today's (`refuse <verb> policy_denied host_env_run <field>`), never with a value. No container is created and the file is not read. (The pull is an earlier helper verb with no env; it is unchanged.)

Because the entry's own env is refused first, a key that both the entry and the file set is now `host_env_run env`, not v1.21's `env_conflict <KEY>`; `env_conflict` remains for a file key on the env denylist.

`buildCreate` is unchanged: with these fields fixed it already sends only the pinned `Cmd` and `Healthcheck.Test`, no `Entrypoint`, no `User`, no mounts and only the file's env.

### 3.3 Agent

`internal/agent/run/reason.go` gains one fixed v1.19 reason for the new detail, with the field from a fixed list (never a value):
`host_env_run <field>: this entry gets a host env file, so its command and healthcheck must match host_env_command and host_env_healthcheck in runner.toml, and it may not set entrypoint, user, volumes, files or its own env`.
A field outside the list gives the sentence without the field. No hello, frame or task change.

### 3.4 Unchanged

- v1.21 image binding, file checks, env conflict rule, env hash, log redaction (still a second line of defence) and host-network rules.
- Start, stop, restart, status, logs, the kept-container rollback and the automatic rollback inside a deploy (H5). A rollback that pulls is a `container_deploy` and is checked.
- Entries without a `host_env_files` line: no change at all.

## 4. Contract diff

| Interface | Before (v1.21) | After (v1.26) |
|---|---|---|
| `runner.toml` | `host_env_files`, `host_env_images` | plus `host_env_command`, `host_env_healthcheck` (§3.1); older agents: `unknown_key` |
| `policy check` `host_env_file` line | `… keys=<n> sha256=<12 hex> image=<pattern>` | plus ` command=<n|image> healthcheck=<n|image>` at the end |
| Helper `container_deploy` refusals | `host_env_file`, `host_env_image`, `env_conflict <KEY>` | plus `policy_denied` / `host_env_run` / `<field>` |
| Deploy result `reason` (v1.19) | sentences above | plus the `host_env_run` sentence (§3.3) |
| Hello, frames, task specs, API | — | unchanged |

**Behaviour change for existing users:** on a host that already has `host_env_files`, a deploy of an entry with a `command` or `healthcheck` is refused until root adds the matching lines. Only Deploybot's own host uses the feature today; §5 orders the steps so nothing running stops.

## 5. Rollout and rollback

Upgrade (Deploybot's own host; the root steps are Joe's or the Ops session's):
1. Release bot-agent with v1.26 and upgrade the host's agent (`upgrade_agent` or `install.sh upgrade --runner`). Running containers keep running; deploys of `deploybot/*` are refused (`host_env_run command`) until step 2.
2. Root adds the two lines (§3.1 example, matching today's catalog entries `api`, `mcp`, `migrate`), runs `sudo ./install.sh check --runner`, and restarts `bot-agent`.
3. If any account other than Joe has had `deploy` on the `deploybot` project since v1.21, rotate the secrets in the env files first (H5).
4. Deploy `migrate`, `api`, `mcp` as usual.

Rollback: remove the two lines, then downgrade the agent to 0.2.17 (an older agent refuses the keys as `unknown_key`, which turns images off). The catalog entries need no change either way.

Docs in the same PR as the code: `docs/install.md` "Host networking and host env files" (the two keys, what is refused, the corrected promise), `docs/design.md`, `docs/contracts/agent-protocol-v1.md` gets the v1.26 section; `docs/contract-status.md` in bot-control-plane gets one line at release (Main Dev).

## 6. Why no server change

The server needs no new field: the refusal arrives as a v1.19 reason, which the API already stores and the console and MCP already show. A console hint ("this entry's command is pinned by the host") would need the host to report its pins in hello; that is a possible later api-v1 change, not needed for the fix.

## 7. Acceptance criteria

1. With `host_env_files` for `p/e` and no other new line, a deploy of `p/e` with any non-empty `command`, any `entrypoint`, a `healthcheck`, a `user`, a volume, a file, a spec env key, a secret env ref, an env set, or a merged `env` key is refused `policy_denied` / `host_env_run` / `<field>`; no container is created.
2. With `host_env_command = ["p/e=api"]`, `command: ["api"]` deploys; `["api","x"]`, `["sh"]` and empty are refused `command`.
3. With `host_env_healthcheck`, the same for `healthcheck.test`; changed timings with the same `test` deploy.
4. A job run and an `image_rollback` that pulls are checked the same way; start, restart, stop, logs and the kept-container rollback are not.
5. An entry without a `host_env_files` line deploys exactly as before (existing tests unchanged).
6. Policy load: a `host_env_command`/`host_env_healthcheck` item without a `host_env_files` line, a duplicate, an empty argv or one of more than 17 items, a quote or backslash, or a relative healthcheck path makes the policy invalid with `host_env_file <project>/<entry>`.
7. `policy check` prints the two new fields; the console rewrite (v1.22) leaves both keys untouched.
8. The deploy result reason is the §3.3 sentence, and no file value appears in any log, result or refusal.

## 8. Tests

- `internal/agent/policy`: parse and validate the two keys (criteria 6, 7), including the console rewrite round trip.
- `internal/runner/hostenv_test.go`: table test over every refused field and the allowed pinned shapes (1–3), asserting no pull and no create on the fake Docker; job run and pull rollback (4); entry without a line (5).
- `internal/runner`: a `buildCreate` body test for the pinned shape (only `Cmd`, `Healthcheck.Test` and file env).
- `internal/agent/run/reason_test.go`: the sentence per field and for an unknown field (8).
- Gates: `gofmt -l .`, `go build ./...`, `go vet ./...`, `go test -count=1 ./...` as a non-root user, `go test -race ./internal/agent/run/... ./internal/runner/...`, and the linux amd64 and arm64 builds (helper change).
