---
name: bot-agent-root-helper
description: Threat-model checklist for any change to what the bot-agent root helper (cmd/bot-agent-runner, internal/runner, internal/upgrade, internal/release) or runner.toml parsing (internal/agent/policy, internal/agent/hostpolicy) does or accepts. Use before writing the spec.
---

# bot-agent root helper change

The helper runs as root on every enrolled host. A change to what it may do, or to how `runner.toml` is parsed, is Tier 3: write `docs/spec-<name>.md` and wait for Joe's approval before code.

## Trust boundary

- The helper trusts neither the agent nor the server. The agent is unprivileged and may be compromised, and the server (API) may be compromised. Only the root-owned `/etc/bot-agent/runner.toml` and what is compiled into the binary are trusted.
- `serve-conn` requires euid 0 and a socket peer uid of `bot-agent` (`cmd/bot-agent-runner/peercred_linux.go`). Do not widen that.
- Every request is checked against the policy before acting: kind allowlist, `projects`, `images` patterns, closed spec keys and bounds. A refusal is `policy_denied` or `bad_spec`, with a short reason and no secret.
- Upgrades: the server names only a version. The download address, the redirect hosts, the Ed25519 check against `internal/release.PublicKeys` and the command are fixed on the host (`docs/design.md`, "Agent upgrade from the server"). Changing the key list is Tier 3.

## Checklist for the spec and the review

1. What new input reaches the helper, and who controls each field (the agent, the server, or `runner.toml`)?
2. Is every field validated with an allowlist (closed key set, regexp, bounds) before use? Does any field reach a command line, a unit file, a path or a Docker argument? If it does, show how it is quoted or why it cannot carry an option or a `..`.
3. Paths: no symlink following outside the allowed roots, files created atomically with explicit modes, and nothing written under `/etc/bot-agent` except what the design names.
4. systemd and Docker: which unit or container names can the request choose? Can it reach a unit or container the policy did not create (the ownership check)?
5. Defaults stay off: a new capability is opt-in in `runner.toml`, and the installer never turns it on.
6. Secrets: env values, registry logins and file contents are never logged or returned. Log only a short redacted line.
7. Failure: a partial step leaves the host in a known state, and a retry is safe.
8. Agent-protocol: a new verb or field is a new agent-protocol v1.N. List open and merged PRs in both repos for the next number, and pair it with the bot-control-plane PR.

## Tests and gates

- Unit tests for every refusal path, not only the happy path.
- Run tests as a non-root user (`runuser -u <test user> -- go test ...`). As root, the permission and mode tests fail or test the wrong thing.
- `go test -race` on the changed packages and always on `./internal/agent/run/...`.
- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/...`, and the same with `GOARCH=arm64`.
- Never install, upgrade or restart the agent or helper on a host from a session. Hosts are upgraded through `upgrade_agent` after a signed release.
