# Install a host

This guide is for an organization owner or admin adding a Linux host to Deploybot. You download a release bundle, run `install.sh` from that directory, and type the join token at a hidden prompt.

Run every command in this guide on the host you are adding, in a terminal on that host or an `ssh -t <host>` session. Do not run them on your own computer.

The installer copies files only from the bundle. It does not download anything. No command here takes the token as an argument, in a file, or on a pipe. Do not print `/etc/bot-agent/env`. That file holds the host credential.

## What you need

- A Linux host with systemd, and root on that host. `uname -m` must print `x86_64` or `aarch64`. The installer calls these `amd64` and `arm64`. It is written for a Debian-family host.
- A terminal with a tty. From your own machine, `ssh -t <host>` gives you one. Enrol cannot show the hidden prompt without a tty.
- Docker on the host, if it will run catalog images or the edge: Docker Engine 20.10 or later (API 1.41). Shared networks need Docker 25 or later (API 1.44). The agent uses the newest API version both sides speak, up to 1.47. A host that runs programs only does not need Docker.
- An organization and a project in the console. The host joins your organization; each project then gets `metrics` or `deploy` access to it from an org owner or admin. Creating a project, creating a join token and granting host access are done in the console only; there is no MCP tool for them.
- A join token. In the console, open the organization, then Hosts → Add a host, and create a join token, optionally granting one project access. From a project's Hosts tab, Add a host makes a token that grants that project `deploy`. Only org owners and admins can create tokens. Create it just before you enrol. The token expires, and it can be used once.
- The server URL for enrol. Use `wss://<your-api-host>`. A dev server on loopback uses `ws://127.0.0.1:<port>` or `ws://localhost:<port>`, and those commands need `--insecure-dev`.
- For the policy flags (`--project`, `--image`, `--enable`) and the printed `next:` enrol command, bundle v0.2.7 or later.
- For agent v0.2.5 and later, Deploybot API v0.2.7 or later. An older API marks the host's programs as disabled, and deploys are refused.

## Enrol with an AI agent

An AI coding agent (Claude Code, Codex CLI, or another) can do every step of this guide for you except one: the join token. The agent prepares the host; you create the token and type it yourself. The token never passes through the agent.

### Give the agent access to the host

Reaching the host for setup is your job. Deploybot has no access to a host before it is enrolled, and the Deploybot MCP server only sees hosts that are already enrolled.

You can give your agent access in one of these ways:

1. The agent runs on your computer and uses your own SSH (`ssh -t`) and keys. This is the most common case.
2. The agent runs on the host itself, for example Codex CLI on the server. No SSH is needed, but sudo is.
3. The agent uses your cloud provider's remote tools, for example Alibaba Cloud Assistant or AWS SSM, with your cloud credentials.
4. The agent has no access. It prepares each command, and you paste it on the host.

Setup is a one-time step under your control. Once the host shows online, you can remove the agent's SSH or shell access. From then on everything goes through Deploybot: the agent on the host dials out, only catalog entries run, and each change has a confirm step.

In every option, you type the join token at the hidden enrol prompt. The agent never sees it.

You can start the agent with a prompt such as: "Add my server `<host>` to Deploybot project `<slug>` following https://deploybot.dev/llms.txt."

### Instructions for the agent

Follow them in order. Stop where it says stop. Run each command on the host. The examples use option 1, `ssh <host> '<command>'`. With option 2, run the command without the `ssh <host>` part. With options 3 and 4, run or hand over the same command on the host.

1. Check access: `ssh <host> 'uname -m; systemctl --version | head -1; sudo -n true && echo sudo-ok'`. `uname -m` must print `x86_64` or `aarch64`. If sudo needs a password, ask the user to run the sudo steps, or to give you a session where sudo works. Never ask for or handle their password.
2. Find the latest version:
   ```bash
   V=$(curl -fsSL https://api.github.com/repos/more-than-code/bot-agent/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
   ```
   The GitHub API allows 60 unauthenticated requests an hour per IP address, so this can fail on a shared network. If `V` is empty, read the tag from the `releases/latest` redirect instead:
   ```bash
   V=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/more-than-code/bot-agent/releases/latest | sed -n 's#.*/tag/v\([0-9.]*\)$#\1#p')
   ```
   Or ask the user for the version and set `V` by hand.
   Stop if `V` is empty or does not look like a version such as `0.2.7` (digits and dots only). The steps below need v0.2.7 or later. The tarball holds both architectures; `install.sh` picks the right one.
3. Download and verify on the host, in a plain path (letters, digits, `. _ / + -`):
   ```bash
   ssh <host> "set -e; mkdir -p ~/bot-agent-install && cd ~/bot-agent-install && \
     curl -fsSLO https://github.com/more-than-code/bot-agent/releases/download/v$V/bot-agent-v$V.tar.gz && \
     curl -fsSLO https://github.com/more-than-code/bot-agent/releases/download/v$V/bot-agent-v$V.tar.gz.sha256 && \
     sha256sum -c bot-agent-v$V.tar.gz.sha256 && tar -xzf bot-agent-v$V.tar.gz && \
     cd bot-agent-v$V && sha256sum -c SHA256SUMS"
   ```
   Every checked line must end in `OK`. Stop if any line does not.
4. Choose the install command. The policy flags and the `next:` line need bundle v0.2.7 or later.
   - **Host runs catalog images only.** Ask the user for the project slug and the image patterns, for example `docker.io/library/*` or `ghcr.io/<owner>/*`. Follow the [Review the host policy](#review-the-host-policy) rules: one `--project`, and `--image` only for the registries the user named. Repeat `--image '<pattern>'` for each pattern, and keep each one in single quotes. Pass the slug and patterns exactly as the user gave them; they must match the rules in that section (lowercase letters, digits and `. _ / * : -`), and the installer refuses anything else. The installer writes `/etc/bot-agent/runner.toml` itself; do not write that file.
     ```bash
     sudo ~/bot-agent-install/bot-agent-v$V/install.sh install --runner --project <slug> --image '<pattern>' --enable
     ```
   - **Host runs programs** (with or without images). Use `install --runner` with no policy flags. After step 5 the policy needs a hand-written file, as in [Review the host policy](#review-the-host-policy):
     1. Ask the user for the project slug, the accounts (`run_as`), the directories, the commands (`exec_allow`) and `max_programs`, and, if the host also runs images, the image patterns. From v0.2.8 programs run only for projects listed in `projects`, so the slug is needed for programs too.
     2. Draft the whole file from the programs example in that section, adding the image lines when the host runs both. Never add `allow_shell = true`, a `run_as` account, a directory or a command the user did not name.
     3. Show the user the exact text. Write it only after the user approves that exact text, unchanged: put it in a file on the host, then `sudo install -m 0644 -o root -g root <file> /etc/bot-agent/runner.toml`. If the user does not approve, or you cannot run sudo on the host, give the user the text and that command to run themselves.
     4. Either way, run `sudo ~/bot-agent-install/bot-agent-v$V/install.sh check --runner`. It must print `valid`, `programs=true`, a `projects=` count of at least 1 and `projects_overlap=false`. Show the user the output.
   - **Host runs no catalog workloads at all.** Leave out `--runner` and the policy flags.

   **Stop and show the user the command before you run it.** `--enable` lets this host run those images as root through Docker, so only the user decides the image patterns.
5. Check, dry run, install, with the flags the user agreed to. For an images-only host:
   ```bash
   ssh <host> "sudo ~/bot-agent-install/bot-agent-v$V/install.sh check --runner --project <slug> --image '<pattern>' --enable"
   ssh <host> "sudo ~/bot-agent-install/bot-agent-v$V/install.sh install --runner --project <slug> --image '<pattern>' --enable --dry-run"
   ssh <host> "sudo ~/bot-agent-install/bot-agent-v$V/install.sh install --runner --project <slug> --image '<pattern>' --enable"
   ```
   For the other hosts, drop the policy flags (and `--runner` when step 4 left it out).
   - Stop if `check` does not end with `summary: check ok`. For an images-only host it must also print `valid` and `images=true`. For a programs host it prints `valid` and `images=false` (the disabled template). Without `--runner` it prints `policy: not checked`.
   - `check` and `install` also exit 1 with `runner.toml exists and differs; remove it or edit it by hand` when the host already has a different `runner.toml`. Nothing is changed. Ask the user to move that file aside, or, if they want to keep it, run the three commands again without the policy flags. Do not move or edit the file yourself.
   - If `install.sh` exits 2 and its usage text does not list `--project`, the bundle is older than v0.2.7. Stop and tell the user. Do not hand-write `runner.toml`.
   - The install must end with a line that starts with `next:`, then `summary: installed <version>`.
6. **Stop. Hand over to the user for the token.** Give the user the command from the `next:` line, for example `sudo /home/<user>/bot-agent-install/bot-agent-v<version>/install.sh enroll --server '<server URL>' --runner`. Leave `<server URL>` as it is, unless the user's instructions from the Deploybot console give the server URL; then put in exactly that value. Never choose or guess the server URL. Tell the user:
   - In the console, open the organization (or the project), then Hosts → Add a host, and create a join token. The join token card on the Hosts tab shows the server URL. Replace `<server URL>` with that URL, and check that the `--server` host matches the card before you type the token. For a loopback dev server (`ws://127.0.0.1:<port>`), also add `--insecure-dev`.
   - Run the command in a terminal the agent cannot read or type into:
     - Option 1: your own terminal, with `ssh -t`:
       ```bash
       ssh -t <host> "sudo /home/<user>/bot-agent-install/bot-agent-v<version>/install.sh enroll --server '<server URL>' --runner"
       ```
     - Option 2: a separate login session on the host (a second SSH session, or the host's own terminal or serial console), not the agent's shell tool.
     - Option 3: an interactive console or session terminal from the cloud provider that the agent cannot read. Never a run-command API.
     - Option 4: paste it on the host yourself.
   - Type the token at the `join token (not echoed):` prompt.
   - Keep the bundle folder until enrol finishes. The command runs from it.
   - If the agent offers to run the enrol command for you, refuse.

   You, the agent, never ask for the token, never read it from the screen, chat or a file, and never put it in a command, URL or file.
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

Copy both files to the host if you downloaded them elsewhere. Check them on the host, in a plain path (letters, digits, `. _ / + -`). Check the tarball first, then the files inside it.

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

`check` confirms the architecture, systemd, and the bundle hashes. It prints `arch: amd64` or `arch: arm64`, then `systemd: present`, and ends with `summary: check ok`. A host with no agent yet prints `installed: none`. On a host that already has an agent, `check` prints one line per agent file: `stock: <path>` when the file matches what the installer wrote, `modified: <path>` when it was changed after that, `not installed by the installer: <path>` when the installer did not write it, and `absent: <path>` when it is missing. A hash mismatch prints `sha256 mismatch:` and the relative path. `check` writes nothing.

The script must run as root. Otherwise it prints `must run as root` and exits 1.

With `--runner`, `check` also asks the bundle's helper about Docker (`docker_check`, from bundle v0.2.12). It prints `docker: API <version> (ok)`, adds `; shared networks need Docker 25 (API 1.44)` for Docker 20.10 to 24, prints `docker: API unknown, assumed 1.41 (ok); shared networks need Docker 25 (API 1.44)` when Docker gave no `API-Version` header, or prints `docker: API <version> (too old: needs Docker 20.10, API 1.41)`, `docker: not answering` or `docker: absent`. The helper's ping has its own 5 s deadline, and `check` runs it under `timeout 15` when the `timeout` command exists. When the policy has `images` or the edge on and Docker is not ok, `check` ends with `summary: check failed: <reason>` and exits 1. When the policy needs no Docker (for example the disabled template that `check` tests before `runner.toml` exists), the line is only a report and `check` still passes. A bundle without the helper prints `docker: present` or `docker: absent`.

Leave `--runner` off when this host will not run catalog workloads.

## Install

Run the dry run first. It prints each write and does not apply it. Then install.

```bash
sudo ./install.sh install --runner --dry-run
sudo ./install.sh install --runner
```

The dry run ends with `summary: dry-run install <version>`. The install ends with `units: disabled`, a `next:` line, and `summary: installed <version>`. The `next:` line is the enrol command with the bundle's absolute path, for example `next: sudo /home/you/bot-agent-vX.Y.Z/install.sh enroll --server '<server URL>' --runner`. Replace `<server URL>` and run it from any directory.

Install creates the system user and group `bot-agent` (no home, no login shell), installs the agent and `bot-agent.service`, and leaves that unit disabled until enrol. With `--runner` it also installs the helper, the socket, the template unit, the tmpfiles snippet, and `runner.toml`. It creates `/etc/bot-agent` as `root:bot-agent` mode `1770`. The new `runner.toml` is `root:root` mode `0644` and has `enabled = false`. An existing `runner.toml` or `/etc/bot-agent/env` is kept.

For a dev server, add `--insecure-dev` to both commands. That writes a drop-in that sets `BOT_AGENT_INSECURE_DEV=1`. Use it only with a loopback `ws://` URL.

Leave `--runner` off when this host will not run catalog workloads.

## Review the host policy

Edit `/etc/bot-agent/runner.toml` as root. Keep it `root:root` mode `0644`. The `bot-agent` user cannot change it. The agent reads the file only at startup.

The installed file is a short disabled template. Replace it with the policy you want.

For a host that runs images only, `install` can write the policy for you. Pass `--project <slug>` once, `--image '<pattern>'` for each pattern (1 to 20), and `--enable` to set `enabled = true`. They need `--runner` and cannot be used with `--runner-config`. `check --runner` takes the same flags and changes nothing:

```bash
sudo ./install.sh check --runner --project your-project --image 'ghcr.io/example/*' --enable
sudo ./install.sh install --runner --project your-project --image 'ghcr.io/example/*' --enable
```

The written file has `enabled`, `projects`, and `images`, and is `root:root` mode `0644`. With `--enable`, the check must print `images=true` or nothing is installed. If `runner.toml` already exists, the same file is kept, and a different file stops the install with `runner.toml exists and differs; remove it or edit it by hand`. A value outside the slug or pattern rules exits 2. Programs, `max_containers`, and `allow_public_ports` still need a hand-written file. Put each setting on one line. An array that spans more than one line is invalid, and an invalid file leaves programs and images off.

- `images` lists the images this host may run, for example `ghcr.io/example/*`. A `*` is one whole path component. With `--runner` installed, the helper runs as root and can open `/var/run/docker.sock`. An empty list does not remove that socket. An image deploy runs only when images are enabled: the policy is valid, `enabled = true`, there is at least one image pattern, there is at least one project, and no project slug overlaps another. Review the patterns before you set `enabled = true`.
- `projects` lists the project slugs allowed to deploy **programs and images** here (programs from v0.2.8). A program or image for a project that is not listed is refused, even when the organization has given that project `deploy` access to this host. On a host that several projects use, list every project that should deploy here. Use the slug. A slug is lowercase letters, digits, and dashes, such as `your-project`. The project name in the console can be different. List at most 32 slugs; a longer list makes the file invalid. One slug must not be another slug plus `-` and more text, as with `acme` and `acme-web`. That pair leaves both programs and images off, and `check` prints `projects_overlap=true`.
- Taking a project out of `projects` does not stop its running programs. Start and Restart are refused for it; Stop, status and logs still work. When the agent starts, it logs one line for each such unit: `bot-agent: program unit <unit> is for project <slug>, which is not in projects`.
- `max_containers` is how many containers this host may run. If you omit it, the limit is 20. A value you set must be from 1 to 200.
- A port that is not public always binds `127.0.0.1`. `allow_public_ports = false` refuses a port marked public. `allow_public_ports = true` binds those ports on `0.0.0.0`.
- Programs use `run_as`, `dirs`, `exec_allow`, `allow_shell`, and `max_programs`. `dirs` are absolute directories; a program's working directory must be inside one of them after symlinks are resolved. `exec_allow` lists absolute paths that must match exactly; it has no wildcards. A command given as a bare name runs `<dir>/node_modules/.bin/<name>` when that file is inside the working directory, and otherwise the `exec_allow` entry with that file name. `run_as` drops root, an account whose uid is below 1000, an account whose primary gid is below 1000, and an account whose primary group is `root` or `0`. Set `max_programs`. If you omit it, the limit is 0 and no program can start. Leave `allow_shell = false` unless this host should run a shell.
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

For a host that runs programs only, replace `your-project`, `deploy`, `/srv/apps` and the commands with your own:

```toml
enabled = true
allow_shell = false
projects = ["your-project"]
run_as = ["deploy"]
dirs = ["/srv/apps"]
exec_allow = ["/usr/bin/node", "/usr/bin/npm"]
max_programs = 5
```

From v0.2.8, `check`, `install` and `upgrade` with `--runner` refuse a policy that has programs on (`enabled = true`, at least one `run_as` account and `max_programs` above 0) but no `projects`. They exit 1, change nothing, and print `runner.toml allows programs but lists no projects; add projects = ["<slug>"] (from v0.2.8 programs run only for listed projects)`.

A host that runs both programs and images has one `runner.toml` with both sets of lines: the program lines above plus `images`, `max_containers` and `allow_public_ports`, and a single `enabled`, `allow_shell` and `projects`.

### Shared networks

From v0.2.9 of the agent, root can open a shared Docker network to several projects on this host, so their containers reach each other. A container on a shared network is reachable there by its container name, `<project>-<entry>`, such as `defect-drainer-serve`. Nothing else on either project's own network is reachable through it. An entry joins in the console or over MCP by listing the network name in its image spec (`networks`).

Two keys define the networks. Each is one line:

```toml
projects = ["daikou", "defect-drainer"]
shared_networks = ["dd=10.200.10.0/24"]
shared_network_projects = ["dd:daikou", "dd:defect-drainer"]
```

- `shared_networks` lists `<name>=<subnet>`. The name is lowercase letters, digits and dashes, starts with a letter or digit, and has at most 32 characters. List at most 8 networks, each name once. The Docker network is `deploybot-shared-<name>`.
- The subnet is an IPv4 CIDR in a private range (`10.0.0.0/8`, `172.16.0.0/12` or `192.168.0.0/16`), with a prefix from `/16` to `/28`, written with the host bits zero (`10.200.10.0/24`, not `10.200.10.1/24`). Two networks may not overlap.
- `shared_network_projects` lists `<name>:<slug>`, one item per member. The network must be in `shared_networks` and the slug must be in `projects`. List each pair once, at most 32 in all. A network with no members is valid, but no entry can join it.
- A project whose slug is `shared-<name>` cannot join network `<name>`: its own project network is also called `deploybot-shared-<name>`, so the deploy fails `conflict`. Pick another network name.
- Any mistake makes the whole file invalid, which turns programs and images off. Check the file before you restart the agent, always naming it:

  ```bash
  sudo bot-agent policy check /etc/bot-agent/runner.toml
  # or, from the bundle:
  sudo ./install.sh check --runner
  ```

  After `projects_overlap=` it prints `shared_networks=<count>` and one line per network, such as `shared_network dd 10.200.10.0/24 daikou,defect-drainer`, with `-` when it has no members. A mistake in either key prints only `invalid: parse`, with no hint of which item is wrong, so check each item against the rules above.

**Order of upgrades.** Upgrade the server first (Deploybot v0.2.32, api-v1 v1.25), then the agent (v0.2.9), then add the keys and restart the agent: `sudo systemctl restart bot-agent`. An agent older than v0.2.9 rejects the keys as `unknown_key`, which turns programs and images off. The console and MCP show the network, and offer it to entries, after the agent has reconnected with the new policy. To go back to an older agent, remove the two keys first.

**Choose the subnet.** It must not overlap the host's own network (its VPC or LAN), its routes, or Docker's address pools. Before you add the key, check with `ip route` and `docker network inspect $(docker network ls -q) --format '{{.Name}} {{range .IPAM.Config}}{{.Subnet}} {{end}}'`. A range such as `10.200.x.0/24` is usually free. If Docker refuses the create because the pool overlaps, the deploy fails `conflict` and the helper's journal (`journalctl -u 'bot-agent-runner@*'`) shows `subnet <cidr> overlaps an existing Docker network`.

The helper creates the network on the first deploy that needs it, with the subnet from this file and labels that mark it as Deploybot's. It does this before the current container is stopped, parked or replaced, so a refused deploy leaves the running container as it was. It keeps the subnet fixed, so a proxy can trust it. If a network of that name already exists with another subnet, driver or labels, the deploy fails `conflict` and the journal shows `shared network deploybot-shared-<name> exists with a different subnet or labels; remove it by hand (docker network rm) after stopping its containers`. This is also what happens after you change a subnet in this file.

**Removing a member or a network.** The agent and the helper never remove a shared network. The one exception is `install.sh remove --remove-workloads`, which removes every Docker network Deploybot labelled (see [Undo](#undo)).

- To remove a member, take its pair out of `shared_network_projects` and restart the agent. New deploys of that project with the network are refused `policy_denied`. Its running containers stay attached until they are redeployed without `networks` or removed.
- To remove a network, take its lines out of **both** keys in the same edit: a member line for a network that is no longer in `shared_networks` makes the whole file invalid. First redeploy every entry that lists it without the network (or stop and remove those containers), then edit the file, check it, restart the agent, and remove the network by hand as root:

  ```bash
  docker network inspect deploybot-shared-dd --format '{{range .Containers}}{{.Name}} {{end}}'
  docker network rm deploybot-shared-dd
  ```

  The first command must print nothing before you run the second.
- After a revoke, a container created on the network still has it. A rollback to the `-prev` container, or a start of such a container, attaches it to the network again. Remove the container to end that. `policy check` reads only the file and cannot see Docker, so use `docker network inspect` as above to find containers still attached.

### Edge (public addresses)

From v0.2.11 of the agent, root can let Deploybot run a reverse proxy on this host. With it on, the org's admins can turn the host's edge on in the console, and projects can add routes (`hostname → entry port`). The edge gets and renews HTTPS certificates by itself. One line turns it on:

```toml
edge = true
```

- The edge is a Caddy container, `deploybot-edge`, from an image pinned by digest in the agent release (Caddy 2.11.9 since bot-agent 0.2.11). It runs in the host network and binds ports 80 and 443 itself. Its certificates are in the Docker volume `deploybot-edge-data`, and its generated config is in `/var/lib/bot-agent-runner/edge/` (root only). Caddy's admin API listens only on a unix socket in that directory, never on a TCP port.
- **Ports 80 and 443** must be free on the host and reachable from the internet, or certificates cannot be issued. A host behind NAT, such as a home Raspberry Pi, cannot use the edge yet. Turning the edge on is refused while anything else holds 80 or 443 (`port_in_use`, naming the holder when it is known), so stop an existing proxy first.
- The key needs Docker. `install.sh check --runner` prints `edge: on (needs Docker, and ports 80 and 443 free and reachable from the internet)` and checks Docker when the key is on.
- `edge` is `true` or `false` (the default). Anything else makes the whole file invalid, which turns programs and images off. `bot-agent policy check` prints `edge=true` or `edge=false` after the shared-network lines; the host file lines follow it.
- Removing the key stops new configs from being applied (`edge_disabled`), but does not stop a running edge. Turn the edge off in the console first; that stops and removes the container and keeps the volume and the config directory.
- Routes reach an image entry through its published port on `127.0.0.1`, and a program entry on its port. A route to a container port that is not published is refused.
- The helper does not trust the config it is sent. It accepts only the exact shape Deploybot generates: one server listening on `:443`; routes with host, path and client-IP matchers (plus the Cloudflare `Cf-Connecting-Ip` header check next to a client-IP match); the `subroute`, `encode` (zstd, gzip), `reverse_proxy` and the `Cache-Control: private, no-store` handlers; upstreams on `127.0.0.1` ports 1024–65535 only; the Cloudflare `X-Forwarded-For` rewrite and deletion of the client-IP headers; and TLS policies whose issuer is ACME (with an email) or Caddy's internal CA. Keys must match exactly, repeated keys are refused, and no other `{...}` placeholder is allowed. Anything else fails `bad_spec`, and the journal names the offending part of the config.
- The container runs with every capability dropped except binding ports below 1024, `no-new-privileges`, a read-only root filesystem (`/config` and `/tmp` are tmpfs) and 512 MiB of memory with no extra swap.
- The container's `dev.deploybot.config_hash` label is the hash at creation, for information only. Later configs are loaded without recreating the container. The applied config is `config.json`.
- While a release carries the placeholder image digest, the edge stays off: hello reports it disabled and the helper refuses `edge_disabled`. (Release builds always carry a real digest.)
- If a new edge container cannot start, the helper removes it, puts the previous `config.json` back and recreates the previous container from its own image.
- `edge_stop` and `edge_status` work without the key, so an edge can always be turned off.

**Order of upgrades.** Upgrade the server first (api-v1 v1.35), then the agent (v0.2.11), then add the key and restart the agent: `sudo systemctl restart bot-agent`. An agent older than v0.2.11 rejects the key as `unknown_key`, which turns programs and images off. To go back to an older agent, remove the key first.

**Moving an existing Caddy.** To keep the certificates of a Caddy that already serves the host's names, create the volume and copy its data in before the edge's first start, while the old proxy is stopped. The helper adopts a local volume named `deploybot-edge-data` that has no Deploybot labels:

```bash
docker volume create deploybot-edge-data
docker run --rm -v <old-caddy-data>:/from:ro -v deploybot-edge-data:/to alpine cp -a /from/. /to/
```

### Host config files

From bot-agent 0.2.13 (agent-protocol v1.18), root can let the org's owners and admins edit a root-owned config file of a unit Deploybot does not run, such as Defect Drainer's worker policy, from the console. The unit's own command checks each new file before it replaces the old one. The unit is then restarted and confirmed healthy, and the previous file comes back on its own if anything fails. You choose exactly which files, and what checks them, in `runner.toml`:

```toml
host_files        = ["dd-policy=/etc/dd-worker/policy.toml", "dd-secrets=/etc/dd-worker/secrets.env"]
host_file_modes   = ["dd-policy:0644", "dd-secrets:0600"]
host_file_secret  = ["dd-secrets"]
host_file_units   = ["dd-policy:dd-worker.service", "dd-secrets:dd-worker.service"]
host_file_run_as  = ["dd-policy:dd-worker", "dd-secrets:dd-worker"]
host_file_checks  = ["dd-policy:/usr/local/bin/dd-worker policy check --file -", "dd-secrets:/usr/local/bin/dd-worker secrets check --file -"]
host_file_health  = ["dd-policy:/usr/local/bin/dd-worker health --wait 30s", "dd-secrets:/usr/local/bin/dd-worker health --wait 30s"]
host_file_idle    = ["dd-policy:/usr/local/bin/dd-worker health --idle", "dd-secrets:/usr/local/bin/dd-worker health --idle"]
```

- Each key is one line. `host_files` names each file and its path; the other keys are `"<name>:<value>"`, and `host_file_secret` lists names. Modes, units, `run_as` and checks are required for every file; health, idle and secret are optional. At most 8 files.
- **Path:** exactly `/etc/<dir>/<file>`. Both parts start with a letter, digit or `_` (no dotfiles). These directories are refused: `bot-agent`, `systemd`, `ssh`, `sudoers.d`, `pam.d`, `security`, `cron.d` and every `cron.*`, `polkit-1`, `apt`, `docker`, `caddy`, `postgresql`, `tmpfiles.d`, `sysusers.d`, `sysctl.d`, `modprobe.d`, `profile.d`, `ld.so.conf.d`, `logrotate.d`, `update-motd.d`, `udev`, `init.d`, `apparmor.d`, `dpkg`, `rsyslog.d` and `default`.
- **The file must already exist,** owned by root (group root, or for `0640` the `run_as` account's group), with exactly the listed mode and one link. The directory and `/etc` must be root-owned and not group- or world-writable. Deploybot never creates a host file and never fixes a wrong owner or mode; it refuses the apply.
- **Mode:** `0600`, `0640` or `0644`. A `host_file_secret` file must be `0600` or `0640`.
- **Unit:** a `.service` name that starts with a letter or digit and is not Deploybot's own (`deploybot-*`, `bot-agent*`) or a system service (`ssh*`, `systemd-*`, `dbus*`, `docker*`, `containerd*`, `caddy*`, `postgresql*`, `networking*`, `NetworkManager*`, `tailscaled*`, `cron*`, `rsyslog*`, `polkit*`, `user@*`, `getty*`).
- **run_as:** an existing service or regular account: uid and primary gid 100 or above, not root, not in group root, not `nobody` or group `nogroup`, and not the `bot-agent` account or an account sharing its uid or primary group. For a `0640` file, give the account its own primary group, since every member of that group can read the file. A system account such as one made by `useradd --system` is fine here; programs' `run_as` still needs 1000 or above. Agents 0.2.13 and older refuse a system account here (`invalid: parse`), so upgrade the agent first. The check, idle and health commands run as that account in a short-lived systemd unit (`deploybot-hostfile-check-<name>-…`). That unit has no network, no devices, no core dumps, `/home` hidden, a private `/tmp` and a read-only filesystem; it cannot see the Docker, containerd or Podman sockets; and it has 256 MiB of memory and 64 tasks. The check reads the new file on stdin.
- **Commands:** an absolute executable followed by at most 16 arguments, split on single spaces; no shell and no quotes. A health `--wait` may be at most 165 s.
- **Executables must exist and stay root-owned.** The executable may be a symlink (for example to a versioned binary), but the link, its target and every directory on the way to both must be root-owned and not group- or world-writable. This is checked at the start of every apply and again right before each command. When it fails, `policy check` ends that file's line with `exec=unsafe` and applies of that file are refused (`forbidden_spec`, stage `exec`) until it is fixed. The rest of the policy (programs, images, the edge, other host files) keeps working.
- **Check:** exit 0 means valid; exit 2 with one `bad_<reason>` line on stderr is shown to the admin as that reason; anything else is `check_error`. **Health:** exit 0 once the restarted unit is healthy, within the settle window. The window starts when the restart returns; it is 60 s by default, 30 to 180 s, and at least the health command's `--wait` plus 15 s. Without a health command, the unit must stay active, without restarting, for the whole window. **Idle:** exit 4 while the unit is busy; the console then offers "Apply anyway", which skips it.
- Applies of files that share a unit run one at a time.
- A `host_file_secret` file never shows its bytes or its hash in Deploybot; only whether the host still has the applied version.
- The helper keeps the previous file in `/var/lib/bot-agent-runner/host-files/<name>.prev` (root only). To restore it by hand: `sudo install -m <mode> -o root -g root /var/lib/bot-agent-runner/host-files/<name>.prev <path>`, then restart the unit.
- A syntax mistake in any of these lines makes the whole file invalid (`invalid: parse`), which turns programs and images off too. Run `bot-agent policy check` (or `install.sh check --runner`) before you save the file. It prints `host_files=<n>` and one `host_file <name> <path> <mode> <unit> <run_as> secret=<bool> health=<bool>` line per file, with ` exec=unsafe` at the end when an executable fails the check above.

**Helper write access.** The helper unit can write only a few directories. After you add, change or remove `host_files`, run:

```bash
sudo ./install.sh upgrade --runner
```

It writes `/etc/systemd/system/bot-agent-runner@.service.d/host-files.conf`, with one `ReadWritePaths=-/etc/<dir>` line per directory (or removes it when there are no host files), and reloads systemd, even when the agent version is unchanged. If a run fails part way, or the file was edited by hand, run the same command again: it rewrites the drop-in. `install.sh check --runner` reports whether the drop-in is in sync. A symlinked `bot-agent-runner@.service.d` directory is refused.

**Order of upgrades.** Upgrade the server first (api-v1 v1.44), then the agent with `install.sh upgrade --runner`. Then add the keys, run `install.sh upgrade --runner` again to write the drop-in, and restart the agent: `sudo systemctl restart bot-agent`. An older agent rejects the keys as `unknown_key`, which turns programs and images off. To go back to an older agent, remove the keys first and run `install.sh upgrade --runner` to remove the drop-in.

### Host networking and host env files

From bot-agent 0.2.15 (agent-protocol v1.21, made for Deploybot running its own server in containers on its host), root can let chosen projects run a container on the host's network, and give a catalog entry environment variables from a root-only file on the host, bound to the images that may receive them. All are off until you add the keys:

```toml
host_network_projects = ["deploybot"]
host_env_files        = ["deploybot/api=/etc/deploybot/api.env", "deploybot/migrate=/etc/deploybot/api.env", "deploybot/mcp=/etc/deploybot/mcp.env"]
host_env_images       = ["deploybot/api=ghcr.io/more-than-code/deploybot-server", "deploybot/migrate=ghcr.io/more-than-code/deploybot-server", "deploybot/mcp=ghcr.io/more-than-code/deploybot-server"]
```

- **`host_network_projects`:** at most 8 slugs, each also in `projects`, no duplicates. An entry of a listed project may set ImageSpec `network: "host"`: the container shares the host's network (it binds `127.0.0.1` like a systemd unit and reaches Postgres on localhost) but keeps its own filesystem, user, memory limit and log limit, and runs without `NET_RAW` and with `no-new-privileges`. Its `ports` are not published; each `ports[].host` is only the loopback port the health check uses. Shared `networks` are refused with it. Any other project asking for host networking is refused (`policy_denied`), and so is a manual start, restart or rollback of a host-network container after its project leaves the list.
- **What host networking exposes:** the container shares the host's loopback (every service listening on `127.0.0.1`) and its abstract unix sockets. List only projects you trust like a host service.
- **Health in host mode:** a `ports[].host` probe reaches whatever listens on that loopback port, so another process on the port could make a broken deploy look healthy. Give the image a Docker healthcheck (the Deploybot images have one).
- **`host_env_files`:** at most 32 items `"<project>/<entry>=<path>"`. The project is in `projects`; the entry is a catalog entry name; one line per entry (two entries may share a file). The path is exactly `/etc/<dir>/<name>.env`, and must not be a `host_files` path or in a `host_files` directory. `/etc` and the file's directory must be root-owned and not group- or world-writable. The file must be a regular file (not a symlink), owned by `root:root`, mode `0600`, one link, at most 64 KiB, made of `KEY=VALUE` lines (key `^[A-Za-z_][A-Za-z0-9_]{0,127}$`, the value is the rest of the line as is, no quotes; blank lines and `#` comments allowed; no key twice; no carriage returns).
- **`host_env_images`:** every `host_env_files` entry needs one `"<project>/<entry>=<image pattern>"` line, with the same pattern syntax as `images` and within `images`. The helper refuses a deploy of any other image for that entry (reason `host_env_image`), so the file's secrets reach only the images you name.
- The helper reads the file at every deploy, rollback-by-pull and job run of that entry and adds its keys to the container's environment. A key that the entry's own env (env set or spec env) also sets, or one no container env may set (`PATH`, `HOME`, `NODE_OPTIONS`, `LD_*` and the other runtime keys), fails the deploy with reason `env_conflict <KEY>`; a file that fails the checks above fails it with reason `host_env_file`. A bad file stops only that entry's deploys: stop, start, rollback, status, logs, programs, the edge, host files and agent upgrade keep working. Changing the file makes the next deploy of that entry a real deploy (not `unchanged`).
- **Logs:** the values never reach the agent, the server, previews or deploy history. The helper redacts them from that entry's container logs while the file is the one the container was created with; after the file changes or when it cannot be read, the logs show one line saying they are withheld until the next deploy. Values shorter than 6 bytes cannot be redacted, so do not put short secrets in a host env file.
- `policy check` prints `host_network_projects=<slugs or none>` and one `host_env_file <project>/<entry> keys=<n> sha256=<first 12 hex> image=<pattern>` line per file, never a value. A bad file or line makes `policy check` print `invalid: host_env_file <project>/<entry>`, so run `sudo ./install.sh check --runner` after every change to the file. Run it as root: neither the agent nor the helper's other verbs read the files.
- An older agent rejects these keys as `unknown_key`. Upgrade the server first, then the agent, then add the keys and restart the agent.
- **Deploys finish on the host.** From the same version, an image deploy, its health wait and its automatic rollback (and a job run) keep running when the connection to the server drops. The result is kept in `/var/lib/bot-agent/results/` (`0700`, owned by `bot-agent`; at most 100 files and 7 days) and sent again after the agent reconnects, until the server confirms it. A stored result can hold the deploy's last log lines (already redacted), at rest for up to 7 days. If the agent itself stops in the middle, no result is kept; the server settles that deploy. The agent unit creates the directory.

### Program secrets

A program entry can use env sets and secret env, such as `GH_TOKEN`, from v0.2.8 of the agent. Before you give a program a secret, know these limits:

- **Other processes can read the values.** A process running as the same `run_as` account, or as root, can read a program's environment through `/proc/<pid>/environ`, the same as for a container. Give a program that holds a secret its own `run_as` account, and list only that program's directory for it.
- **Values are stored on the host.** The helper writes them to `/var/lib/bot-agent-runner/programs/<unit>.env` and keeps the spec in `<unit>.json`, both `root` mode `0600` in a `root` mode `0700` directory, so they survive a reboot. Stopping the program removes both files.
- **A changed value needs a Run.** Changing a secret value in the console takes effect at the next **Run**. **Restart** reuses the spec stored on the host, with the old value. After a Run, the helper keeps up to 64 earlier values in the stored spec so older log lines stay redacted; values retired before that are covered only by the keyword regex.
- Program logs show a listed value as a short preview. Values shorter than 6 bytes are not matched exactly and are covered only by the keyword regex, and a value printed in some other encoding can still appear.
- **Before `install.sh rollback`** to an agent older than v0.2.8, Stop the programs that use secrets, or remove their env sets in the console. An older helper does not know the stored `secret_keys`, so its re-check refuses a Restart of such a program and removes the unit. With the agent rolled back, the server refuses a Run of an entry with env sets on this host.

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
| `install.sh` exits 2, and its usage text does not list `--project` | The bundle is older than v0.2.7. It does not have the policy flags or the `next:` line. | Download v0.2.7 or later and use that bundle. An AI agent must stop and tell you; it must not hand-write `runner.toml`. |
| `bundle path has characters that are unsafe to paste into a shell; move the bundle to a plain path` | The bundle directory path has a character other than letters, digits, `. _ / + -`. | Move the bundle folder to a plain path, such as `~/bot-agent-install/`, and run the command again. |
| The journal contains `expected handshake response status code 101 but got 404` | The server URL does not end in `/agent/v1/ws`. | On v0.2.3 or later, run enrol again with the bare `wss://<your-api-host>` URL. On an older bundle, open `/etc/bot-agent/env` as root and change only `BOT_AGENT_SERVER_URL` so its path is `/agent/v1/ws`, then `sudo systemctl restart bot-agent`. Do not copy the file into chat or into a command. |
| The journal contains `bot-agent: could not persist enrollment`. The host works until the next restart. | `/etc/bot-agent` is not writable by group `bot-agent`, often because the mode is `0750`. The agent could not save its credential. | `sudo chown root:bot-agent /etc/bot-agent`, then `sudo chmod 1770 /etc/bot-agent`, then `sudo systemctl restart bot-agent`. Enrol again with a new token. |
| `bot-agent: env file already has a credential`, then `enroll failed` | The host is already enrolled, or the env file still holds a credential. | `sudo systemctl stop bot-agent`, move `/etc/bot-agent/env` aside (do not delete it), then enrol with a new token. |
| `enroll failed: credential not persisted` | The installer timed out waiting for the credential to replace the join token (up to 30 seconds). | Check that `/etc/bot-agent` is mode `1770` and owner `root:bot-agent`. Then read the journal for the server URL and the handshake. Mint a new token only after the URL and the directory are right. |
| `enroll failed: agent did not stay up. Run systemctl stop bot-agent, move /etc/bot-agent/env aside, then enroll again` | The agent did not stay active with `NRestarts` 0 after the credential was saved. | Run the two steps in that message, then enrol again with a new token. Keep the moved file. Do not delete it. |
| The new host's name ends in `-2`, or a higher number | That hostname is already used in the project. A revoked host still holds its name. | The suffix is expected. Revoking the old host does not free the name. |
| A deploy is refused and the console shows programs disabled | The API is older than v0.2.7 and the agent is v0.2.5 or later. The old API stores this agent's programs report as disabled. | Upgrade the API to v0.2.7 or later, or roll this host back with `sudo ./install.sh rollback`. |
| `runner.toml allows programs but lists no projects; add projects = ["<slug>"] (from v0.2.8 programs run only for listed projects)` | From v0.2.8 programs run only for listed projects. The policy has programs on and no `projects`. Nothing was changed. | Add `projects = ["<slug>"]` to `/etc/bot-agent/runner.toml` as root, then run the command again. |
| A program Run or Restart is refused with "the host refused this under its policy (runner.toml)" | The program's project is not in `projects`, or two slugs overlap (`check` prints `projects_overlap=true`). | Add the slug to `projects`, or fix the overlap, then `sudo systemctl restart bot-agent`. The running unit was left in place; Stop still works. |
| The journal shows `bot-agent: program unit … is for project …, which is not in projects` | A unit installed earlier belongs to a project that is not listed. It keeps running. | Add the slug to `projects` and restart `bot-agent`, or Stop that program. |
| A deploy with a shared network is refused `policy_denied` | The network is not in `shared_networks`, or `shared_network_projects` has no `<name>:<slug>` pair for the entry's project, or the agent was not restarted after the edit. | Add the pair (the slug must also be in `projects`), run `sudo bot-agent policy check /etc/bot-agent/runner.toml`, then `sudo systemctl restart bot-agent`. |
| A deploy with a shared network is refused `conflict` | The helper's journal (`journalctl -u 'bot-agent-runner@*'`) shows one of two lines. `subnet <cidr> overlaps an existing Docker network`: the subnet overlaps another Docker network. `shared network deploybot-shared-<name> exists with a different subnet or labels; remove it by hand (docker network rm) after stopping its containers`: a network of that name exists that Deploybot did not make with this subnet, often after the subnet was changed. Neither line: the project slug is `shared-<name>` for a network it joins, or another container holds the entry's name. | For an overlap, choose a free subnet (see Shared networks). For the second line, stop the containers on that network, remove it with `docker network rm`, and deploy again. For a `shared-<name>` slug, use another network name. |
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

From v0.2.8, programs run only for projects listed in `runner.toml`. On a host with the helper, where the policy has programs on but no `projects` line, `check`, the dry run and the upgrade exit 1 with `runner.toml allows programs but lists no projects; add projects = ["<slug>"] (from v0.2.8 programs run only for listed projects)` and change nothing. Add `projects = ["<slug>"]` with every project whose programs run on this host, as in [Review the host policy](#review-the-host-policy), run `check --runner` again, then upgrade.

Leave `--runner` off when this host has no helper. The same version prints `summary: already at <version>` and changes nothing, unless you pass `--force`. If the upgrade fails after it has written the backup, it restores the `.prev` set. `rollback` restores that set later.

From v0.2.15, on an enrolled host, the upgrade also waits up to 90 seconds after the restart for the agent to reconnect: the agent touches `last_hello` in its state directory (`/var/lib/bot-agent`, or `BOT_AGENT_STATE_DIR` from `/etc/bot-agent/env`) each time the server accepts its hello, and the installer prints `verify: hello acknowledged`. If the file is not newer than the restart, it prints `verify: no hello after restart`, and the upgrade restores the `.prev` set. Run an upgrade while the server is reachable. A rollback (the automatic one, or `install.sh rollback`) does not wait for the marker: it checks only that the restored agent is active with `NRestarts` 0 and the expected version, so it works with the server down and for agents older than 0.2.15. If that check fails, the installer ends with `rollback verify failed`.

## Upgrades from Deploybot

From bot-agent 0.2.15 (agent-protocol v1.20), the org's owners and admins can upgrade this host's agent from the console (or an AI agent through MCP, with their confirmation), without SSH. It is off until root turns it on, with one line in `/etc/bot-agent/runner.toml`:

```toml
agent_upgrade = true
```

Then check the file and restart the agent so its hello reports the change:

```bash
sudo bot-agent policy check /etc/bot-agent/runner.toml   # prints agent_upgrade=true
sudo systemctl restart bot-agent
```

The installer never writes this line. An agent older than 0.2.15 rejects it as `unknown_key`, which turns programs and images off, so install 0.2.15 or later first (the usual SSH upgrade above; it is the last one this host needs). A host without the helper (`--runner`) cannot be upgraded this way.

**What happens.** Deploybot names only a version. The host checks only that it is newer than the installed one and that its release is signed; that it is the latest release is up to Deploybot. The helper checks this line again, then starts a transient root unit, `bot-agent-upgrade`, that:

1. downloads `SHA256SUMS` and `SHA256SUMS.sig` from `https://github.com/more-than-code/bot-agent/releases/download/v<version>/` (the address is built into the helper; the server cannot change it) and checks the signature with the release key built into the helper; only then does it download `bot-agent-v<version>.tar.gz`;
2. checks that `SHA256SUMS` lists `install.sh` and the four `units/*` files, that every file matches, that the bundle holds no other file, and that the bundled agent prints the requested version;
3. unpacks into `/var/lib/bot-agent-runner/upgrade/<version>/` (root only) and runs that bundle's `install.sh upgrade` (with `--runner`), the same upgrade you would run by hand;
4. writes the outcome to `/var/lib/bot-agent-runner/upgrade/result.json`, which the agent reports to the console.

A bundle with a bad signature, a wrong, missing or extra file, or a wrong version is refused before anything outside `/var/lib/bot-agent-runner/upgrade/` is touched. An upgrade does not start while an image deploy, an edge apply or a host-file apply is running (`busy`), and only one runs at a time. While it runs, deploys, program and container actions, edge changes and host-file applies answer `busy`. After a failed or rolled-back upgrade, the same version can be started again after 10 minutes (`busy` until then). If the unit is stopped (for example at its 15-minute limit), `install.sh` gets SIGTERM and up to 120 seconds to put the previous files back; the outcome is then `failed`.

**Rollback.** The host rolls back by itself, without the server: if the new agent is not active, restarts, reports another version or does not reconnect within 90 seconds, `install.sh` restores the previous files and restarts the old agent, checking that it is active with the old version (it does not wait for it to reconnect, so this works with the server down). The console then shows `rolled_back` with the reason (`not_reconnected` or `install_failed`). If the restored agent fails that check, the outcome is `failed` (`install_failed`) and the host needs a look over SSH. The upgrade unit's output is in `journalctl -u bot-agent-upgrade`. Break-glass over SSH is the same as always, with any release bundle, for example the one the upgrade staged (its directory is root only, so run it through `sudo sh`):

```bash
sudo sh /var/lib/bot-agent-runner/upgrade/<version>/bot-agent-v<version>/install.sh rollback
```

To turn the feature off, remove the line (or set it to `false`) and `sudo systemctl restart bot-agent`. Deploybot can then never start an upgrade on this host.

**Network.** The host needs outbound HTTPS to `github.com`, `objects.githubusercontent.com` and `release-assets.githubusercontent.com` on port 443; at most 5 redirects, 64 MiB and 120 seconds per file. Behind a proxy, add it to `runner.toml` (only `http://` or `https://`, a host and a port, no user name, password or path; anything else makes the whole file invalid):

```toml
agent_upgrade_https_proxy = "http://proxy.lan:3128"
```

`policy check` prints `agent_upgrade_https_proxy=set` (never the value). The upgrade uses only this setting; `HTTPS_PROXY` in `/etc/bot-agent/env` or the environment is ignored.

**State directory.** If you moved the agent's state with `BOT_AGENT_STATE_DIR`, set it in `/etc/bot-agent/env`, not with a systemd `Environment=` line. The installer looks for the agent's `last_hello` where that file says; with the setting only in the unit, every upgrade rolls back.

**New policy keys.** A release that needs a new `runner.toml` key says so in its release notes. An upgrade never edits `runner.toml`; add the key yourself after the upgrade.
