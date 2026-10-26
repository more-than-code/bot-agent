# Install a host

This guide is for a project admin adding a Linux host to Deploybot. You download a release bundle, run `install.sh` from that directory, and type the join token at a hidden prompt.

Run every command in this guide on the host you are adding, in a terminal on that host or an `ssh -t <host>` session. Do not run them on your own computer.

The installer copies files only from the bundle. It does not download anything. No command here takes the token as an argument, in a file, or on a pipe. Do not print `/etc/bot-agent/env`. That file holds the host credential.

## What you need

- A Linux host with systemd, and root on that host. `uname -m` must print `x86_64` or `aarch64`. The installer calls these `amd64` and `arm64`. It is written for a Debian-family host.
- A terminal with a tty. From your own machine, `ssh -t <host>` gives you one. Enrol cannot show the hidden prompt without a tty.
- Docker on the host, if it will run catalog images. A host that runs programs only does not need Docker.
- A join token. In the console, open the project, open Join tokens, and create one. Create it just before you enrol. The token expires, and it can be used once.
- The server URL for enrol. Use `wss://<your-api-host>`. A dev server on loopback uses `ws://127.0.0.1:<port>` or `ws://localhost:<port>`, and those commands need `--insecure-dev`.
- For agent v0.2.5 and later, Deploybot API v0.2.7 or later. An older API marks the host's programs as disabled, and deploys are refused.

## Enrol with an AI agent

An AI coding agent (Claude Code, Codex CLI, or another) can do every step of this guide for you except one: the join token. The agent prepares the host; you create the token and type it yourself. The token never passes through the agent.

The agent must run on your own computer, with your SSH access to the host and your sudo on it. It works through `ssh <host> '<command>'`. You can start it with a prompt such as: "Add my server `<host>` to Deploybot project `<slug>` following https://deploybot.dev/llms.txt."

**Instructions for the agent.** Follow them in order. Stop where it says stop.

1. Check access: `ssh <host> 'uname -m; systemctl --version | head -1; sudo -n true && echo sudo-ok'`. `uname -m` must print `x86_64` or `aarch64`. If sudo needs a password, ask the user to run the sudo steps, or to give you a session where sudo works. Never ask for or handle their password.
2. Find the latest version:
   ```bash
   V=$(curl -fsSL https://api.github.com/repos/more-than-code/bot-agent/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
   ```
   The tarball holds both architectures; `install.sh` picks the right one.
3. Download and verify on the host, in a directory without spaces:
   ```bash
   ssh <host> "set -e; mkdir -p ~/bot-agent-install && cd ~/bot-agent-install && \
     curl -fsSLO https://github.com/more-than-code/bot-agent/releases/download/v$V/bot-agent-v$V.tar.gz && \
     curl -fsSLO https://github.com/more-than-code/bot-agent/releases/download/v$V/bot-agent-v$V.tar.gz.sha256 && \
     sha256sum -c bot-agent-v$V.tar.gz.sha256 && tar -xzf bot-agent-v$V.tar.gz && \
     cd bot-agent-v$V && sha256sum -c SHA256SUMS"
   ```
   Every checked line must end in `OK`. Stop if any line does not.
4. Check, dry run, install. Add `--runner` only if the host will run catalog containers or programs:
   ```bash
   ssh <host> "cd ~/bot-agent-install/bot-agent-v$V && sudo ./install.sh check --runner && \
     sudo ./install.sh install --runner --dry-run && sudo ./install.sh install --runner"
   ```
   The install must end with `summary: installed <version>`.
5. Write the host policy (`--runner` only). Build `/etc/bot-agent/runner.toml` from the [Review the host policy](#review-the-host-policy) rules: `projects = ["<slug>"]`, and `images` limited to the registries the user named. **Stop and show the user the file before you write it.** `enabled = true` lets this host run those images as root through Docker, so only the user decides the image patterns. After they agree, write it as `root:root` mode `0644` (for example `sudo tee` then `sudo chmod 0644`), and run `sudo ./install.sh check --runner`. It must print `valid` and `summary: check ok`.
6. **Stop. Hand over to the user for the token.** Tell them:
   - In the console, open project `<slug>`, then Join tokens, and create a token.
   - In **their own terminal** (not through you), run:
     ```bash
     ssh -t <host> "cd ~/bot-agent-install/bot-agent-v<version> && sudo ./install.sh enroll --server 'wss://api.deploybot.dev' --runner"
     ```
     Then type the token at the `join token (not echoed):` prompt. Drop `--runner` if step 4 did not use it. For a self-hosted server, use its own `wss://` URL.
   - Never ask for the token, never read it from the screen, chat or a file, and never put it in a command, URL or file.
7. When the user says enrol finished, verify: `ssh <host> 'sudo systemctl is-active bot-agent bot-agent-runner.socket'` prints `active` (one line per unit). If you have the Deploybot MCP server, `list_hosts` for the project shows the host online. Otherwise ask the user to check the Hosts tab.

Never print or copy `/etc/bot-agent/env`; it holds the host credential. If a step fails, use [If something goes wrong](#if-something-goes-wrong) and tell the user what you saw.

## Download and verify

Open the latest release:

<https://github.com/more-than-code/bot-agent/releases/latest>

Every release is listed at <https://github.com/more-than-code/bot-agent/releases>.

Download both files for that version. Replace `X.Y.Z` below with the version in the release name.

- `bot-agent-vX.Y.Z.tar.gz`
- `bot-agent-vX.Y.Z.tar.gz.sha256`

The tarball contains one directory, `bot-agent-vX.Y.Z/`. Inside it are `bot-agent-vX.Y.Z-linux-amd64`, `bot-agent-vX.Y.Z-linux-arm64`, `bot-agent-runner-vX.Y.Z-linux-amd64`, `bot-agent-runner-vX.Y.Z-linux-arm64`, `install.sh`, `units/`, and `SHA256SUMS`.

Copy both files to the host if you downloaded them elsewhere. Check them on the host, in a directory whose path has no spaces. Check the tarball first, then the files inside it.

```bash
sha256sum -c bot-agent-vX.Y.Z.tar.gz.sha256
tar -xzf bot-agent-vX.Y.Z.tar.gz
cd bot-agent-vX.Y.Z
sha256sum -c SHA256SUMS
```

Each checked line ends with `OK`. Run the rest of this guide from `bot-agent-vX.Y.Z/`.

## Check (changes nothing)

```bash
sudo ./install.sh check --runner
```

`check` confirms the architecture, systemd, and the bundle hashes. It prints `arch: amd64` or `arch: arm64`, then `systemd: present`, and ends with `summary: check ok`. A host with no agent yet prints `installed: none`. A hash mismatch prints `sha256 mismatch:` and the relative path. `check` writes nothing.

The script must run as root. Otherwise it prints `must run as root` and exits 1.

With `--runner`, before `runner.toml` exists, `check` tests a disabled template. That template has no image patterns, so the line is `docker: not checked`. After you set `images`, a later check prints `docker: present` or `docker: absent`.

Leave `--runner` off when this host will not run catalog workloads.

## Install

Run the dry run first. It prints each write and does not apply it. Then install.

```bash
sudo ./install.sh install --runner --dry-run
sudo ./install.sh install --runner
```

The dry run ends with `summary: dry-run install <version>`. The install ends with `units: disabled` and `summary: installed <version>`.

Install creates the system user and group `bot-agent` (no home, no login shell), installs the agent and `bot-agent.service`, and leaves that unit disabled until enrol. With `--runner` it also installs the helper, the socket, the template unit, the tmpfiles snippet, and `runner.toml`. It creates `/etc/bot-agent` as `root:bot-agent` mode `1770`. The new `runner.toml` is `root:root` mode `0644` and has `enabled = false`. An existing `runner.toml` or `/etc/bot-agent/env` is kept.

For a dev server, add `--insecure-dev` to both commands. That writes a drop-in that sets `BOT_AGENT_INSECURE_DEV=1`. Use it only with a loopback `ws://` URL.

Leave `--runner` off when this host will not run catalog workloads.

## Review the host policy

Edit `/etc/bot-agent/runner.toml` as root. Keep it `root:root` mode `0644`. The `bot-agent` user cannot change it. The agent reads the file only at startup.

The installed file is a short disabled template. Replace it with the policy you want. Put each setting on one line. An array that spans more than one line is invalid, and an invalid file leaves programs and images off.

- `images` lists the images this host may run, for example `ghcr.io/example/*`. A `*` is one whole path component. With `--runner` installed, the helper runs as root and can open `/var/run/docker.sock`. An empty list does not remove that socket. An image deploy runs only when images are enabled: the policy is valid, `enabled = true`, there is at least one image pattern, there is at least one project, and no project slug overlaps another. Review the patterns before you set `enabled = true`.
- `projects` lists the project slugs allowed to deploy here. Use the slug. A slug is lowercase letters, digits, and dashes, such as `your-project`. The project name in the console can be different. One slug must not be another slug plus `-` and more text, as with `acme` and `acme-web`. That pair leaves images off.
- `max_containers` is how many containers this host may run. If you omit it, the limit is 20. A value you set must be from 1 to 200.
- A port that is not public always binds `127.0.0.1`. `allow_public_ports = false` refuses a port marked public. `allow_public_ports = true` binds those ports on `0.0.0.0`.
- Programs use `run_as`, `dirs`, `exec_allow`, `allow_shell`, and `max_programs`. `run_as` drops root, an account whose uid is below 1000, an account whose primary gid is below 1000, and an account whose primary group is `root` or `0`. Set `max_programs`. If you omit it, the limit is 0 and no program can start. Leave `allow_shell = false` unless this host should run a shell.
- Set `enabled = true` when the lists are reviewed. Nothing runs while it is `false`.

Replace `example` and `your-project` before you rely on this file:

```toml
enabled = true
allow_shell = false
images = ["ghcr.io/example/*"]
projects = ["your-project"]
max_containers = 20
allow_public_ports = false
```

Check the file again:

```bash
sudo ./install.sh check --runner
```

A good file prints `valid`, then the effective flags, then `summary: check ok`. `check` does not test the owner or the mode of `runner.toml`, so a file that fails those tests can still print `valid` and `summary: check ok`.

A later `install`, when `runner.toml` already exists, prints `runner.toml mode must be 644` or `runner.toml must be root:root`. At startup an invalid policy logs `bot-agent: runner policy invalid: …` and leaves programs and images off. If the file's user is not root, or the file is group-writable or world-writable, the quoted reason is `not_root`, `group_writable`, or `world_writable`.

On a new host, stop here and enrol. The service does not start until `/etc/bot-agent/env` exists, and enrol creates that file. The agent reads the policy when enrol starts it.

If the agent is already running, restart it after you save the file:

```bash
sudo systemctl restart bot-agent
```

## Enrol

Stay on a tty (`ssh -t <host>`). Dry run first. The dry run does not ask for the token, and it does not start or restart the agent.

```bash
sudo ./install.sh enroll --server 'wss://<your-api-host>' --runner --dry-run
sudo ./install.sh enroll --server 'wss://<your-api-host>' --runner
```

The dry run ends with `summary: dry-run enroll`. The real command prompts:

```text
join token (not echoed):
```

Type the join token and press Enter. The token is not echoed. It is not an argument of the command.

A server URL with an empty path, or a path of only `/`, is stored with `/agent/v1/ws` appended. Any other path is kept as given. Pass the bare `wss://<your-api-host>` URL.

Success prints `verify: active, NRestarts 0, version <version>`, then `summary: enrolled`. With `--runner` it also prints `verify: runner socket active`. Enrol restarts the agent when it is already active. Otherwise it enables and starts `bot-agent.service`. With `--runner`, or when the helper is already installed, it enables `bot-agent-runner.socket`.

For a dev server, add `--insecure-dev` and use loopback:

```bash
sudo ./install.sh enroll --server 'ws://127.0.0.1:<port>' --runner --insecure-dev
```

`ws://localhost:<port>` is the other loopback form. The agent accepts `ws://` only for `127.0.0.1` or `localhost`, and only with `--insecure-dev`. Any other URL must be `wss://`.

Leave `--runner` off when you did not install the helper.

## Verify

In the console, the host shows as online, with this agent version and architecture.

On the host:

```bash
sudo systemctl is-active bot-agent
sudo systemctl is-active bot-agent-runner.socket
sudo stat -c '%a %U:%G' /etc/bot-agent
sudo journalctl -u bot-agent --since -5min
```

Both `is-active` commands print `active` when you installed the helper. Without the helper, check `bot-agent` only. `stat` prints `1770 root:bot-agent`. The journal should not show errors.

## If something goes wrong

| Symptom | Cause | Fix |
|---|---|---|
| `enroll --insecure-dev` exits 2 and the usage line for `enroll` does not list `--insecure-dev` | The bundle is older than v0.2.3. That flag is refused for `enroll` on those bundles. | Download a current release and use that bundle. |
| The journal contains `expected handshake response status code 101 but got 404` | The server URL does not end in `/agent/v1/ws`. | On v0.2.3 or later, run enrol again with the bare `wss://<your-api-host>` URL. On an older bundle, open `/etc/bot-agent/env` as root and change only `BOT_AGENT_SERVER_URL` so its path is `/agent/v1/ws`, then `sudo systemctl restart bot-agent`. Do not copy the file into chat or into a command. |
| The journal contains `bot-agent: could not persist enrollment`. The host works until the next restart. | `/etc/bot-agent` is not writable by group `bot-agent`, often because the mode is `0750`. The agent could not save its credential. | `sudo chown root:bot-agent /etc/bot-agent`, then `sudo chmod 1770 /etc/bot-agent`, then `sudo systemctl restart bot-agent`. Enrol again with a new token. |
| `bot-agent: env file already has a credential`, then `enroll failed` | The host is already enrolled, or the env file still holds a credential. | `sudo systemctl stop bot-agent`, move `/etc/bot-agent/env` aside (do not delete it), then enrol with a new token. |
| `enroll failed: credential not persisted` | The installer timed out waiting for the credential to replace the join token (up to 30 seconds). | Check that `/etc/bot-agent` is mode `1770` and owner `root:bot-agent`. Then read the journal for the server URL and the handshake. Mint a new token only after the URL and the directory are right. |
| `enroll failed: agent did not stay up. Run systemctl stop bot-agent, move /etc/bot-agent/env aside, then enroll again` | The agent did not stay active with `NRestarts` 0 after the credential was saved. | Run the two steps in that message, then enrol again with a new token. Keep the moved file. Do not delete it. |
| The new host's name ends in `-2`, or a higher number | That hostname is already used in the project. A revoked host still holds its name. | The suffix is expected. Revoking the old host does not free the name. |
| A deploy is refused and the console shows programs disabled | The API is older than v0.2.7 and the agent is v0.2.5 or later. The old API stores this agent's programs report as disabled. | Upgrade the API to v0.2.7 or later, or roll this host back with `sudo ./install.sh rollback`. |
| Catalog containers seem to vanish from Deploybot after you enrol again | They did not. Ownership follows `/etc/machine-id`, not the host id. | Nothing to do. They stay owned. |

## Undo

Rollback restores the previous install. It is one level, from the `.prev` files. A second rollback prints `summary: nothing to roll back`.

```bash
sudo ./install.sh rollback
```

A successful rollback prints `summary: rolled back` and the restored version.

Remove stops the agent and the helper, and deletes their units and binaries. It keeps `/etc/bot-agent`. It leaves deployed units and containers in place. It prints `revoke the host in the console` and `summary: removed`.

```bash
sudo ./install.sh remove
```

`--purge` also deletes `/etc/bot-agent`, `/var/lib/bot-agent`, `/var/lib/bot-agent-runner`, and the `bot-agent` user. The group is left in place. Volumes are removed only when you pass both `--remove-workloads` and `--purge`.

```bash
sudo ./install.sh remove --purge
```

`--remove-workloads` stops and deletes the units and containers this agent installed, and it removes Docker networks labelled `dev.deploybot.owner=bot-agent`. Leave that flag off unless you mean to remove those workloads.

On a production host, do not run `docker compose down -v`, `docker volume rm`, or `docker system prune`.

## Upgrade

Download the new release and check both checksums, as in Download and verify. Run the commands in the new `bot-agent-vX.Y.Z/` directory.

```bash
sudo ./install.sh check --runner
sudo ./install.sh upgrade --runner --dry-run
sudo ./install.sh upgrade --runner
```

`check` changes nothing and ends with `summary: check ok`. The dry run changes nothing and ends with `summary: dry-run upgrade <old> -> <new>`. The upgrade ends with `summary: upgraded <old> -> <new>`.

Leave `--runner` off when this host has no helper. The same version prints `summary: already at <version>` and changes nothing, unless you pass `--force`. If the upgrade fails after it has written the backup, it restores the `.prev` set. `rollback` restores that set later.
