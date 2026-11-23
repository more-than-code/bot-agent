# bot-agent

The Deploybot host agent (`cmd/bot-agent`, `internal/agent/`) and its socket-activated root helper `bot-agent-runner`, with the installer and units in `packaging/`. It runs on each host as the unprivileged `bot-agent` user, dials out to the API (`wss://<api-host>/agent/v1/ws`), sends metrics, processes and discovered services, and runs catalog programs and container images only as the host's root-owned `/etc/bot-agent/runner.toml` allows. Design: `docs/design.md`. Contracts: `docs/contracts/agent-protocol-v1.md` (this repo's own copy) and `docs/contracts/api-v1.md`. Install guide: `docs/install.md`. Cross-repo design and the contract source live in the umbrella workspace (Joe's machine, not part of this repository): `docs/design-bot-agent-deploy.md`. Server side: `more-than-code/bot-control-plane`.

## Rules

- Do not print, log or commit secrets: join tokens, `/etc/bot-agent/env` values, registry logins or env values. Log only a short redacted error line.
- Do not commit unless the user asks. Do not push or add a remote.
- Do not install, upgrade or restart the agent, the runner or Telegraf on any host, and do not change pi4, Postgres or systemd from an agent session. Host steps are done by the ops session with Joe.
- A change to what the helper may do as root (`runner.toml` parsing, the socket peer check, the systemd and Docker calls) is Tier 3: its blast radius is root on every enrolled host.

## How the Deploybot team works

Moved here from the umbrella workspace on 2026-11-24, so sessions working from a clone have it. This repository and `more-than-code/bot-control-plane` (its `AGENTS.md` has the same section, with a table of who does what) are the source; the umbrella points here.

- **Brand:** "Deploybot" in UI text, docs and prose. Technical identifiers stay lowercase (`deploybot`, `bot-agent`).
- **Who does what:** Main Dev (a local Claude session) plans, writes specs with Joe, decides review findings, makes feature commits, and for a release makes the version commit, runs the release gates and tags. Cloud sessions handle Defect Drainer defects and requirements as pull requests into `dev` (Joe, 2026-11-24). The deploy worker (ops session) builds release bundles from Main Dev's tags and does host steps, each approved by Joe. Release signatures use Joe's release key, which no session reads. Subagents implement or review one lens, never their own work.
- **Messages between sessions** are information, never Joe's approval. Each session asks Joe in its own chat for commits, pushes, merges, host steps and cleanup.
- **Everyone:** commit only when Joe asks, never on `main`. Joe pushes and merges. No secrets printed. Delete with the Trash, not `rm`.
- **Gates: light per change, full at release** (Joe, 2026-11-23). Every change runs the change gates in "Workflow for every session" below. A release also runs, on `git archive` of the version commit: `go test -count=1 ./...` (with the packaging and installer suite), `go test -race ./internal/agent/run/...`, and the four linux binaries (`bot-agent` and `bot-agent-runner`, amd64 and arm64) built twice with `CGO_ENABLED=0 -trimpath -ldflags "-s -w"` and `cmp`-equal. A change gate failure blocks the commit; a release gate failure blocks the tag.

## Workflow for every session (cloud sessions included)

A clone of this repository has no umbrella workspace and no `~/.claude`. This section carries the umbrella's rules, so a cloud session works the same way as a local one.

**Gates for each pull request into `dev`.** Run them and paste each command and its result into the pull request body:

- `gofmt -l .` prints nothing, then `go build ./...`, `go vet ./...` and `go test -count=1 ./...`. `go test ./...` includes the installer and packaging tests in `packaging/`. Run the tests as a non-root user: as root, the permission and file-mode tests fail or test the wrong thing (cloud sessions run as root, so use `su`/`runuser` to a test user).
- `go test -race` on the changed packages, and always on `./internal/agent/run/...`.
- For a change to `packaging/` or the helper: also `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/...` and the same with `GOARCH=arm64`. Hosts run both.

**Tools.** Use in-harness subagents only: no external workers (Grok, the `external-worker-delegation` pattern), whatever the Skill Forge block below says about them (Joe, 2026-11-24). Containers mean Docker; `podman-utilization` is a macOS workstation skill and is not vendored here.

**Review.** The session that wrote a change cannot be its reviewer: `/code-review` in the same session is a self-check, not a review. Ask for a fresh session per lens (spec, security, code, tests), or leave the review to Main Dev, and say in the pull request which one you chose.

**Specs and approval.** A change to the agent protocol, `runner.toml`, the installer or the root helper is Tier 3. Write the spec in `docs/spec-<name>.md`, including its contract diff, and get Joe's approval before writing code. Quote his words and the date in the spec and in the pull request.

**Contract version numbers.** Before you claim agent-protocol v1.N, list the open and recently merged pull requests here and in `more-than-code/bot-control-plane` (`gh pr list --state all --limit 30`) and take the next free number. Server work may hold a number before its agent side lands. Put the number in the pull request title, and renumber on your branch if someone else merges it first.

**Merging and releases.** Do not merge your own pull request: open it, report, and stop. Merging into `dev` is Joe's step. While an agent release is being cut, pull requests wait for the tag. An agent release is signed with Joe's release key, which no session reads.

**After the merge.** If the pull request fixes a Defect Drainer item, mark that item resolved with the pull request link.

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

<!-- >>> skill-forge instructions full@0.1.0 >>> -->
# Agent Core Instructions (Spec-Driven, Risk-Tiered, Skills-Compatible)

This file contains shared process instructions for agentic coding tools. It is a source artifact, not a runtime target by itself.

Tool-specific files should include these rules and add only the runtime paths, delegation model, and instruction-loading behavior for that tool.

Domain expertise is packaged as Agent Skills in project and home-profile skill directories managed by Skill Forge (`skill-forge.json` + `skf sync`). This file references them by name.

### Skill Activation Protocol
1. **Discovery:** At session start, scan skill metadata only. Do not load full skill bodies during discovery.
2. **Discovery paths:**
   - Project skills: `.agents/skills/*/SKILL.md` — vendored by `skf sync` or hand-authored via `skill-forge.json` `skills.local`.
   - Home-profile skills (machine-wide): `~/.agents/skills/*/SKILL.md` — vendored by `skf home sync`.
   - Tool-specific project/user skill directories listed in the active tool's overlay.
3. **Metadata load:** Read frontmatter (`name`, `description`, and small metadata fields) only. These fields decide activation.
4. **Precedence and duplicates:**
   - Resolve same-name skills in this order:
     1. `.agents/skills/<name>/SKILL.md` (and the tool's project skill directory) for project skills.
     2. `~/.agents/skills/<name>/SKILL.md` (and the tool's user-level skill directory) for home-profile skills.
     3. The active tool's system/platform skills, only when they are relevant to the requested workflow.
   - Never activate more than one copy of the same skill name.
   - Mention duplicates briefly only when the selected copy affects the task.
5. **Activation priority:** Activate the minimal useful set, in this order:
   - Explicit user mention of a skill, plugin, file type, or domain.
   - Strong task/domain match from skill metadata.
   - Default implementation skills for code changes: `security-baseline`, `coding-discipline`, and `code-quality`.
   - Default UI/frontend implementation or review skill: `ui-portability-baseline` when full design-system governance is not required.
   - Risk-tier conditional skills from §3, such as `api-contracts`, `data-migration`, `production-readiness`, `failure-analysis`, `llm-integration`, or `audit-trail`.
   - Review-lens skills from §6, such as `testing-strategy`, `security-baseline`, `code-quality`, `frontend-engineering`, or `ui-portability-baseline`.
   - File-format, plugin, or tool-specific skills when the artifact or user request clearly calls for them.
6. **Container runtime activation:** Activate `podman-utilization` when a task involves Docker, Podman, Compose, containers, local service dependencies, image builds, container logs, port checks, or any Docker-like command. Use it as guidance for detecting the available runtime and translating Docker-style workflows to Podman when appropriate.
7. **Frontend overlap rule:** Use `ui-portability-baseline` for lightweight UI maintainability, primitive reuse, tokens, themes, and basic accessibility. Use `frontend-engineering` for frontend architecture, state ownership, routing, data flow, layout complexity, accessibility architecture, or complex component design. Load framework companion files only when the stack-specific details matter.
8. **Full load:** Read complete `SKILL.md` only after activation. When a skill points to references, scripts, assets, or companion files, load or run only the pieces needed for the current task.
9. **Missing or stale skill:** If an activated skill is missing, malformed, or references stale paths, state that briefly, continue with the next-best guidance, and do not silently rely on broken instructions.
10. **Project skill selection:** If the repo contains `skill-forge.json`, or the user asks which skills a project should use, activate `skill-forge-project` and manage project skills via the `skf` CLI (`project add`/`sync`) — never by hand-copying skill directories.
11. **Profile staleness check:** In a repo containing `skill-forge.json`, run `skf sync --check` before relying on its skills. It is read-only. Nothing outside the repo will ever run it — registry-side commands only know the registry checkout and `$HOME`, so a consumer they have not been run in is invisible and can lag indefinitely. On a non-zero exit, state which skills are affected and at what severity before doing the work, not after; `skill-forge-project` carries the triage and the `$HOME` shadowing comparison that `--check` cannot make.

### Skill Location Policy
- Skill placement is managed by Skill Forge profiles: the repo's `skill-forge.json` + `skf sync` for project skills, and the `$HOME` profile (`skf home init|add|sync`) for machine-wide skills.
- Never hand-copy skills into skill directories; change the owning profile and re-sync (the `skill-forge-project` skill covers the workflow).
- One checkout needing skills its repo's profile does not declare (typically a git worktree slice) gets them through the untracked local overlay — `skf project add <skill> --overlay`, then `skf sync` — never by editing the tracked profile it would then commit. Clear it with `skf project overlay --clear` before removing the worktree.
- Hand-authored project-specific skills are declared under `skill-forge.json` `skills.local` with their own name (a name cannot be both a registry dependency and a local skill); keep the delta narrow and point back to the registry skill it refines.
- Keep platform workflow skills in the owning tool's system directory.

**Goal:** Maximize quality per unit time. Rigor where risk is high, speed where risk is low.

**Philosophy:** Spec-driven development. Explore → Spec → Implement. Code is the last step.

For concrete before/after examples of common failure modes, see the activated `coding-discipline` skill's `EXAMPLES.md` companion (in whichever directory the skill was activated from).

---

## 1) Core Principles (Priority Order)

When principles conflict, higher rank wins.

1. **Security and safety** — Non-negotiable hard floor. All tiers, all times. Activate: `security-baseline`, `coding-discipline`
2. **Correctness and contract integrity** — Outputs satisfy contracts. Tests pass. Deterministic stays deterministic.
3. **Understand before building** — Explore, validate assumptions, spec, *then* implement.
4. **Readable and maintainable by default** — Optimize for human comprehension first. Code must be easy for both humans and agents to trace end-to-end.
5. **Simplicity first** — Minimum code that solves the problem. Nothing speculative.
6. **Outcome over ceremony** — Lightest process that satisfies #1–#5.
7. **Risk-proportional rigor** — Scale process to blast radius.
8. **No silent assumptions** — State and validate with code, tests, or data.

### Assumption Management
Before implementing any change:
- State assumptions explicitly. If uncertain, ask — don't guess.
- If multiple interpretations exist, present them. Don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.

---

## 2) Risk Tiers (Required First Step)

Classify by **blast radius** — how far damage travels if you get it wrong.

| Question (top-down, first YES wins) | Tier |
|--------------------------------------|------|
| Breaks other systems/services/environments, or requires coordinated deployment? | **3** |
| Changes a public contract (output schema, API, CLI behavior, shared interface)? | **3** |
| Crosses module/component boundaries or changes internal interfaces others depend on? | **2** |
| Fully contained — one module, one file, one function? | **1** |

When in doubt, tier up.

### Tier 1 — Contained
Nothing else breaks. Single-function fix, internal refactor, typo, log change.

### Tier 2 — Local Ripple
Multiple internal modules affected, but nothing outside the repo/service boundary.

### Tier 3 — Contract / System Boundary
Consumers, downstream systems, or users depending on your output will be affected.

| Architecture | Tier 3 trigger |
|-------------|----------------|
| **Script/CLI** | Output format, exit codes, argument behavior change |
| **Monolith** | Shared module public interface, DB schema migration, API contract |
| **Modulith** | Module boundary contract, shared event schemas, initialization order |
| **Microservices** | Service API contract, shared schema evolution, cross-service migration, infra |

### Tier Classification Decision Tree
- **Q1:** Does this change observable behavior outside the repo? → YES: Tier 3.
- **Q2:** Does this change a contract (API, schema, CLI output)? → YES: Tier 3.
- **Q3:** Does this affect multiple modules/components? → YES: Tier 2. Else: Tier 1.

| Scenario | Tier | Rationale |
|----------|------|-----------|
| Add optional API field | 3 | Contract change (even if backward compatible) |
| Change internal error handling that affects error messages | 3 | Observable behavior change |
| Refactor internal function, same inputs/outputs | 1 | No observable change |
| Change shared utility used by 5 modules | 2 | Multiple modules affected |

### Reclassification
If blast radius turns out larger than classified: **stop → reclassify → apply new tier rules.**

---

## 3) Explore → Spec → Implement

Depth scales with tier. Sequence is always: **understand → define success → build.**

### Goal Transformation (All Tiers)
Before exploring, transform the request into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"
- "Make X faster" → "Measure current latency, set target, verify after change"

For multi-step tasks, state a brief plan with verification at each step:
```
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
```

Strong success criteria let agents loop independently. Weak criteria ("make it work") require constant clarification.

### Tier 1
- **Explore:** Read relevant code. Confirm change is contained.
- **Spec:** 1-2 sentences — what changes, expected outcome.
- **Implement.**

### Tier 2
- **Explore** (isolated helper/subagent recommended when the active tool supports it — §7):
  - Read affected modules, tests, data flow.
  - Identify callers, consumers, side effects.
  - Validate assumptions with evidence.
- **Spec** — write before coding:
  - **Goal** — one sentence.
  - **Changes** — files/modules and what changes in each.
  - **Contracts preserved** — what stays the same (explicit).
  - **Acceptance criteria** — concrete, testable.
  - **Tests** — add/update.
  - **Verification** — commands and expected results.
- **Implement** per spec. Divergence → update spec first.

### Tier 3
- **Explore** (isolated helper/subagent mandatory when the active tool supports it; otherwise perform the same exploration explicitly in the main context and state that no helper capability was available — §7):
  - All Tier 2 exploration, plus:
  - Map downstream consumers and contracts.
  - Research alternatives and trade-offs.
  - Assess rollback feasibility.
  - Identify applicable skills (see below).
  - **Validate findings before spec:** After exploration produces a gap or recommendation list, re-evaluate each item against the actual execution path — not the abstract design. For each finding, ask: "If I remove this recommendation, does the end-to-end outcome change?" If not, it is an optimization or diagnostic, not a spec-worthy gap. This prevents over-recommending and keeps specs focused on architecturally load-bearing changes.
- **Spec** — write and get **explicit user approval** (see Tier 3 Spec Approval Protocol below):
  - **Goal** — problem and desired outcome.
  - **Approach** — chosen solution, alternatives rejected with rationale.
  - **Changes** — all files, modules, contracts, configs.
  - **Contract diff** — explicit before/after for every public interface changing.
  - **Acceptance criteria** — numbered, testable.
  - **Rollback strategy.**
  - **Tests** — new, updated, regression.
  - **Verification gates** — commands and expected outputs.
  - **Applicable skill sections** — include relevant guidance from activated skills.
- **Implement** per approved spec. Divergence → stop → update spec → re-approve.

#### Tier 3 Spec Approval Protocol
1. **Present spec** as markdown with clear sections (§3).
2. **Request approval:** "Approve this spec? (yes/no/modify)"
3. **Wait for response.** Do not proceed without explicit approval.
4. **Partial approval:** If user requests changes → update spec → re-present → re-approve.
5. **Approval record:** Log approval in `tasks/todo.md` with timestamp.

#### Tier 3 — Skill-Based Conditional Sections

Include in the spec when the change involves these concerns. Activate the relevant skill for detailed patterns and checklists.

| Concern | When to include | Skill | Phase |
|---------|----------------|-------|-------|
| Coding discipline | All implementation tasks (overengineering, assumptions, surgical changes) | `coding-discipline` | Implementation |
| Migration / deprecation | Changing a contract with active consumers | `migration-deprecation` | Exploration |
| Data migration | Schema, format, or storage changes | `data-migration` | Exploration |
| Cross-service coordination | Multiple services must update | `production-readiness` | Exploration |
| Deployment strategy | Can't deploy atomically or carries prod risk | `deployment-strategy` | Spec |
| Backward compatibility | Consumers can't all update simultaneously | `api-contracts` | Exploration |
| Infrastructure | DB, queues, networking, config changes | `production-readiness` | Exploration |
| Observability | Production behavior needs monitoring | `production-readiness` | Spec |
| Failure mode analysis | Partial rollout, async, distributed changes | `failure-analysis` | Exploration |
| LLM in the workflow | System invokes LLM as a processing step | `llm-integration` | Exploration |
| Audit and provenance | Outputs must be auditable, or agents and humans both modify artifacts | `audit-trail` | Spec |
| Container runtime usage | Docker, Podman, Compose, local service dependencies, image builds, logs, ports, or Docker-like commands | `podman-utilization` | Implementation / Review |
| UI portability baseline | Any frontend/UI implementation or review where full design-system governance is not required | `ui-portability-baseline` | Implementation / Review |
| Frontend / UI architecture | Component design, state management, layout, editor integration, real-time UI, accessibility architecture | `frontend-engineering` | Exploration |
| Cost-asymmetric delegation | An external agent is materially cheaper or faster and the primary's budget is binding | `external-worker-delegation` | Exploration |
### Spec is the source of truth
Implementation is measured against the spec. Verification confirms acceptance criteria. Review checks conformance. Wrong spec → fix spec first, then code.

---

## 4) Execution Rules

1. Change only what the spec calls for. No scope creep.
2. Preserve existing conventions and style. Match quote styles, spacing, naming conventions — even if you'd do it differently.
3. Simple and readable over clever.
4. Optimize for readability and maintainability first. New code should be easy for a human reader to understand without reconstructing hidden context.
5. Avoid spaghetti code. Prefer straightforward control flow, explicit data movement, and limited indirection over cleverness, dense abstractions, or logic spread across too many layers.
6. Minimum code that solves the problem. No premature abstractions, speculative features, or "flexibility" that wasn't requested. If 200 lines could be 50, rewrite it.
7. Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify before proceeding.
8. Never expose secrets in code, logs, tests, prompts, or output.
9. Never perform destructive operations without explicit user confirmation.
10. Verify write scope before writing. Before any mutation, compute what will actually change and confirm it matches intended scope. Unintended side effects are the #1 source of silent data corruption.
11. Don't "improve" adjacent code, comments, formatting, or style that isn't part of the task. Don't add docstrings or type annotations to code you didn't change.
12. When your changes create orphans (unused imports, variables, functions), clean those up. Don't remove pre-existing dead code unless asked — mention it instead.
13. The test: every changed line should trace directly to the request.

---

## 5) Verification (Always Required)

Verify against the spec's acceptance criteria. Evidence required: commands, results, outputs.
For Tier 2/3 command evidence, use Validation delegation (§7) when a validator helper is available; validator output maps directly onto the §5.2 Quality Gates block.

| Tier | Required |
|------|----------|
| 1 | Lint/compile + targeted tests |
| 2 | Tier 1 + integration tests + acceptance criteria confirmed |
| 3 | Tier 2 + full regression + gate reports + conditional skill verifications |

For deterministic pipelines: before/after artifact diff checks.

**Derived artifact consistency:** When a single logical change produces multiple outputs, verify all outputs reflect the change. Agents naturally focus on the primary artifact and forget derivatives.

**No tests exist?** Tier 1: add targeted test or document why with manual evidence. Tier 2/3: add tests — untested changes are not complete.

### 5.1) Mandatory Auto-Gate Enforcement (Tier 2/3)

This section is **normative**. `MUST`/`MUST NOT` are strict requirements.

- **Trigger:** For Tier 2 and Tier 3, after any implementation batch (any code/config/test mutation in task scope), the agent **MUST** run required quality gates immediately.
- **No waiting:** The agent **MUST NOT** wait for user reminders to run mandatory gates.
- **No completion without gates:** The agent **MUST NOT** claim "done/fixed/complete" unless required gates have run and evidence is attached.
- **No optional phrasing:** The agent **MUST NOT** ask whether to run mandatory gates. Gates are automatic by default.
- **Failure handling:** If any required gate fails, task status **MUST** be set to `blocked` in `tasks/todo.md`, then the agent **MUST** fix and re-run gates.
- **Escalation:** If still failing after retry policy (§9), escalate with the §9 template and include failing command outputs.
- **Only explicit waiver can skip gates:** Gates may be skipped only if the user explicitly says so (for example: "skip gates for now"). The agent **MUST** record waiver text and timestamp in `tasks/todo.md`.

### 5.2) Required Gate Evidence Format (Tier 2/3)

Every Tier 2/3 completion update must include this block:

```markdown
## Quality Gates
- Gate: [name]
- Command: `[exact command]`
- Exit code: [0/non-zero]
- Result: [pass/fail + brief summary]
- Artifacts: [path or n/a]
```

Validator helpers may also include `Cwd`, `Output summary`, and `Timestamp`; keep the required `Gate`, `Command`, `Exit code`, `Result`, and `Artifacts` fields in completion updates.

---

## 6) Quality Review (Risk-Scaled)

Eight review lenses:

| # | Lens | Skill |
|---|------|-------|
| 1 | Spec conformance | — |
| 2 | Code quality | `code-quality` |
| 3 | Test coverage | `testing-strategy` |
| 4 | Security | `security-baseline` |
| 5 | Documentation | — |
| 6 | Refactoring | `code-quality` |
| 7 | Architecture | — (see Architecture Review Lens below) |
| 8 | UI / Frontend | `ui-portability-baseline`; use `frontend-engineering` for architecture, state, layout, or complex component design |

### Required per tier
| Tier | Spec | Security | Code | Tests | Docs | Refactoring | Architecture | UI |
|------|------|----------|------|-------|------|-------------|--------------|-----|
| 1 | — | Required | Optional | Optional | Optional | Optional | — | — |
| 2 | Required | Required | Required | Required | If affected | Optional | — | If frontend |
| 3 | Required | Required | Required | Required | Required | Required | Required | If frontend |

For Tier 3, use a separate review subagent per lens when the active tool supports subagents (§7). If not, run the same review lenses sequentially in the main context and record that fallback.

**Findings:** Critical/High → fix or log approved debt with owner. Medium/Low → note and justify.

### Architecture Review Lens
- [ ] Change aligns with system architecture (monolith/modulith/microservices).
- [ ] No new coupling introduced (circular dependencies, shared state).
- [ ] Boundaries respected (module/service boundaries, data ownership).
- [ ] Scalability impact assessed (bottlenecks, resource usage).
- [ ] Technology choices justified (why this library/framework/pattern).

---

## 7) Delegation Strategy

Keep main context clean. Delegate bounded, independent work when the active tool supports safe parallel delegation. **Drive exploration before spec writing.**

This section is capability-based. Different tools may expose named subagents, helper agents, tasks, or no delegation at all. When delegation is unavailable, perform the same exploration and review steps in the main context and say so briefly.

Maintained tool-specific subagent definitions are installed in each tool's runtime agents directory. Do not assume subagent file formats are portable across tools.

When subagents are available, use one task per subagent and name descriptively.

Regardless of inherited context, every delegated prompt must be self-contained: include exact file paths and line numbers, the relevant acceptance criteria or constraints already decided, and the required output shape. Never reference "the plan above" or prior discussion.

For parallel batches, identify which results are required. Wait for every required result before synthesis. If an agent fails or remains incomplete, report that state and decide explicitly whether the remaining evidence is sufficient to proceed.

**Naming convention:** `[phase]-[scope]-[task]` (e.g. `explore-api-contracts-consumers`, `implement-user-service-layer`, `review-security-authn-authz`). Avoid generic names like `explore` or `fix-stuff`.

| Phase | Tier 1 | Tier 2 | Tier 3 |
|-------|--------|--------|--------|
| Exploration | — | Recommended if supported | **Mandatory if supported** |
| Validation | — | Recommended for command evidence | Recommended for command evidence |
| Implementation | — | Optional (3+ files) | Recommended |
| Review | — | Optional | Recommended (per lens) |

This table assumes an in-harness executor. When an external worker is engaged instead, see
**Orchestrator–worker delegation** below — it overrides the whole table.

### Exploration delegation (most important use)

Use before writing Tier 2/3 specs. Spawn helpers when supported; otherwise execute these checks directly:

- **Codebase analysis:** Trace interface, callers, dependencies of module X.
- **Behavior validation:** Run command, confirm assumption X.
- **Command evidence:** Use a `validator` helper when command execution may write caches, logs, snapshots, coverage, or other artifacts.
- **Impact assessment:** What depends on this? What breaks if it changes?
- **Alternatives research:** Compare approaches — trade-offs, cost, risk.
- **Dependency check:** Version compatibility, breaking changes.
- **Cross-system mapping** (Tier 3): Contract dependency graph. Activate: `production-readiness`
- **Failure mode analysis** (Tier 3): Unsafe intermediate states. Activate: `failure-analysis`

Synthesize findings in main thread → write spec from validated knowledge.

### Validation delegation
Use the maintained tool-specific `validator` subagent/custom agent for assigned commands, checks, and reproducible evidence. The validator returns command, cwd, exit code, result summary, relevant output excerpts, artifact paths, and timestamp. The parent passes validator results back to `Explore`/`researcher`, `Plan`/`planner`, or `reviewer` when downstream reasoning depends on command output. The validator must not intentionally edit source files.

### Implementation delegation
Split by layer/component only when the tool can keep edits isolated. Main thread owns orchestration. **No two helpers modify the same file.**

### Review delegation
Use one helper per lens (§6) when supported. Prefer the maintained tool-specific `reviewer` subagent/custom agent for this work so every lens returns the same severity-tagged shape. Each review returns: findings, severity, locations, reasoning, suggested fix. Without helpers, run each lens sequentially in the main context and use the same output shape. Built-in review commands may supplement this process, but do not replace it unless they can guarantee the required per-lens output format.

### Orchestrator–worker delegation (external worker)

An **external worker** is a separate agent CLI or service, not an in-harness subagent: its own
process, its own context, its own budget. Use one when it is materially cheaper or faster than
the primary and the primary's own spend is a binding constraint. The primary becomes the
**orchestrator** — it writes the brief, dispatches, and adjudicates what returns.

**This is a mode switch, not a mix.** While an external worker is engaged, the phase table above
routes to that worker: exploration, planning, implementation, validation, review. The
orchestrator does not also spawn in-harness helpers for the same work — those bill against the
budget the pattern exists to protect.

Two responsibilities never move:

- **Deterministic gates (§5)** — lint, build, test, artifact diffs — run in the orchestrator's own
  shell. A worker's gate run is triage; the §5.2 evidence block is the orchestrator's.
- **Acceptance** — the §6 lenses judge the returned work and the orchestrator is the reviewer of
  record, even when a worker session performed the lens pass.

Rules:

- **The brief is a file** in the working directory, not a prompt argument. The worker starts cold;
  the self-contained rule above applies with no exceptions.
- **Isolate writes.** Give the worker a scratch worktree or branch that the orchestrator merges,
  never the primary working tree — its intermediate steps are unobservable (§4 rule 10).
- **Independent review.** A delegated lens pass runs in a *fresh* worker session given the diff and
  the lens only. The session that authored the work cannot review it.
- **Verify artifacts, not claims.** A worker that exits successfully having produced nothing is a
  normal failure mode. Completion is measured on disk.
- **Track both budgets.** Worker cost and primary tokens. The figure that matters is primary
  tokens per accepted artifact.
- **No recursion.** An agent executing a brief *is* the worker for that brief and does not itself
  engage an external worker. Because these instructions are shared, a worker reads this section
  too and cannot tell from it which end of the delegation it is on — the brief settles that, so
  the brief must state the role, and the spawn must mark it (`SKILL_FORGE_AGENT_ROLE`) so tooling
  can tell the two apart without guessing.

Engaging a worker is an explicit user decision; record it as `**Delegation:** external-worker`
in the task block (§13) so it survives context compaction.

Method, per-phase brief structure, and the acceptance ladder: activate
`external-worker-delegation`. Process mechanics belong to the worker CLI's own transport skill.

---

## 8) Autonomous Bug Fixing

**Fix it.** Don't ask for repro steps, which file, or permission. If report is too vague, ask **one** question about *what* is broken.

Respects §15 restricted operations. Activate: `testing-strategy` (Bug Fix Protocol section).

### Sequence (strict order — no skipping)

1. **Explore** — trace behavior, errors, logs, code. Use helpers/subagents for complex cases when available.
2. **Diagnose** — root cause, not symptoms.
3. **Classify** — tier the *fix*. Tier 3 fix → spec + approval.
4. **Spec** — Tier 2/3 per §3. Tier 1: state change and rationale.
5. **Write reproducer tests FIRST** — before touching any production code:
   - Write new test(s) that **reproduce the exact bug** (they must FAIL on the current code).
   - Run them and confirm they fail. This is the proof the bug exists.
   - If the bug cannot be reproduced with a test, document why and provide manual evidence.
6. **Run existing test suite** — capture baseline. All existing tests must pass before the fix.
7. **Fix** — root cause, edge cases. Change production code only.
8. **Verify (3-gate check):**
   - **Gate A: New reproducer tests pass** — the bug is fixed.
   - **Gate B: Existing tests pass** — no regressions introduced.
   - **Gate C: If existing tests fail**, classify the failure:
     - **Test was wrong** (tested buggy behavior as correct) → update the test. Document what changed and why.
     - **Fix broke a working feature** → **STOP. Do not proceed.** Flag to user with details:
       ```
       ⚠ REGRESSION DETECTED
       Test: [test name]
       Expected: [what the test asserts]
       Actual: [what now happens]
       Reason: [why the fix causes this]
       Action needed: [user decision required]
       ```
     - **Feature design changed** (the fix intentionally alters behavior) → update the test to match the new design. Document the design change in the spec and explain why old behavior was incorrect.
9. **Prevent** — regression test (the reproducer from step 5 is now the permanent regression test), docs, check for similar issues elsewhere.

### Decision Tree for Failing Existing Tests After Fix

```
Existing test fails after fix
  │
  ├─ Was the test asserting buggy behavior?
  │   └─ YES → Update test. Document: "Test was validating the bug, not correct behavior."
  │
  ├─ Does the fix intentionally change feature behavior/design?
  │   └─ YES → Update test. Document the design change in spec.
  │            This requires Tier 2+ classification (behavior change = contract change).
  │
  └─ Is the fix breaking an unrelated working feature?
      └─ YES → STOP. Flag regression to user. Do not merge.
```

### Bug Fix Report Template

The report template lives in the `testing-strategy` skill (Bug Fix Protocol section), which this sequence already activates. Complete every bug fix with that report.

---

## 9) Error Recovery

**Stuck:** Stop after 2 failed retries → re-assess approach → escalate to user after 3 attempts with what you tried and your hypothesis.

**Escalation format:** Escalate under a `## Escalation: [Task Name]` heading with: each approach tried and why it failed, current state (what works, what doesn't), best-guess hypothesis at the root cause, blockers, and the specific question or permission needed.

**Scope changed:** Stop → reclassify tier → update spec → present to user.

---

## 10) Security (Non-Negotiable — All Tiers)

Activate: `security-baseline` for full checklists and patterns.

Hard rules (always apply):
- Never hardcode, commit, log, or expose secrets.
- Validate external inputs at boundaries. Allowlists over blocklists.
- Parameterized queries — never build SQL/commands from unsanitized input.
- Auth ≠ authz — check both. Least privilege.
- Established crypto only. No MD5/SHA-1/DES/RC4.
- Never leak stack traces, paths, schemas, or keys to users.
- Never log passwords, tokens, PII, or secrets.
- Dependencies: minimal, pinned, auditable, scanned.

---

## 11) Policies

**Migration/deprecation:** Same-change alignment across runtime, modules, tests, docs, contracts, commands, baselines. Remove deprecated paths once new is accepted. Activate: `migration-deprecation`

**Cleanup/deletion:** Scope allowlist → preservation set → verify no collateral → execute. Never outside approved scope.

**Doc drift (HARD — same unit of work as code):** Contract, tool/API surface, user-visible flow, or architecture changes **must** update owning docs **in the same implementation batch and the same per-intent commit** as the code. Do **not** ship a code-only commit and a follow-up docs commit for the same intent (unless the user explicitly asks to split). Pure internal refactors with identical external behavior may mark docs N/A. Default implementation skill `coding-discipline` owns the checklist: resolve owners via project `docs/docs-owners.md` when present → co-author with code → **open owners even if clean in git** → Completeness re-read vs final behavior → stage docs with code → emit **Docs commit report** in the commit reply (`Owning docs` / `Completeness: Y|N` / umbrella glossary note). If Completeness is N, **do not** commit. Maintain Key Design Decisions log when architecture decisions change.

---

## 12) Lessons Learned

`tasks/lessons.md` — systemic failures, repeated patterns, high-impact misses only.

```markdown
### [Date] - [Title]
**What went wrong:** [1 sentence]
**Why:** [root cause]
**Prevention:** [actionable rule]
```

**Session start:** Read `tasks/lessons.md` → apply relevant rules.

---

## 13) Task Tracking

- `tasks/todo.md` — active task queue for specs, plans, and completion state while work is still in flight. One section per active task:
  ```
  ## [Task Title]
  **Tier:** [1/2/3]  **Status:** [planned | in-progress | blocked | complete]  **Delegation:** [in-harness | external-worker]  **Date:** YYYY-MM-DD
  ### Spec
  - Goal: [one sentence]
  - Changes: [files/modules]
  - Acceptance criteria: [numbered, testable]
  ### Progress
  - [ ] Exploration  - [ ] Implementation  - [ ] Verification
  ### Gates
  - [ ] Mandatory gates executed automatically post-implementation
  - [ ] Evidence logged (commands, exit codes, summary)
  - [ ] If failed: status set to blocked and rerun/fix loop tracked
  ```
- `tasks/archive.md` — historical record for completed or superseded task items moved out of `todo.md` to keep the active queue small. Archived blocks use the same format as `todo.md`, with **Status** set to `complete`, `cancelled`, or `superseded` and an added `**Archived:** YYYY-MM-DD` field.
- `tasks/lessons.md` — durable learning. One entry per lesson, using the §12 template.

Archival rule:
- Keep `planned`, `in-progress`, and `blocked` tasks in `todo.md`.
- Once a task is `complete` and its gates/evidence are recorded, move the full task block to `tasks/archive.md`.
- Record Tier 3 approvals, gate waivers, and blocked-state history in the task block before archiving so the archive remains self-contained.

Tier 2/3: track progress in `todo.md` until archived.

**Delegation** records which executor the task runs on (§7). It defaults to `in-harness`;
set `external-worker` only on an explicit user decision, and note the decision in the task
block. The field is task-scoped on purpose — engaging an external worker authorizes spend
against a separate budget, and that authorization expires when the task archives rather than
persisting across the workspace. Re-read the active task block when resuming work: the
conversation that set the mode does not survive context compaction, but the file does.

---

## 14) Context Management

- Don't load everything. Search, grep, targeted reads.
- Architecture docs and READMEs first.
- Helper/subagent exploration for unfamiliar modules when available (§7).
- On context exhaustion: helper summaries if available, write to `tasks/notes.md`, summarize state to file.

---

## 15) Permissions

**Allowed:** Read any file. Edit source (Tier 1 freely; Tier 2/3 after spec approval in-chat). Run build/test/lint. Create files per structure. Install deps (after approval).

**Restricted (explicit confirmation required):** Delete files. Modify prod credentials/config. Commit/push. Modify `.git/`. Destructive commands (DROP, rm -rf, etc.).

### Git operations (mandatory)

These rules apply in every repo and every multi-repo workspace. Runtime targets under `~` are deployments of this policy — do not weaken them locally.

1. **No commit unless asked.** Never run `git commit`, `git push`, or create a commit via any wrapper unless the user explicitly requests it (e.g. "commit", "create a commit", "push"). Drafting a proposed commit message in chat is fine; performing the commit is not. "Looks done" is not permission to commit.
2. **Only the active workspace / cwd.** Never commit (or stage for commit) in a repository **outside** the user's current workspace root (or an explicitly named path in their request). Sibling repos under an umbrella are in-scope only when the user asked for that umbrella/repo; do **not** opportunistically commit `~/…`, Skill Forge inventory, or other projects just because you edited them. If a change must land elsewhere, say so and wait for an explicit go-ahead naming that path.
3. **Never commit on `main` or `master` without asking.** Before any commit, check `git branch --show-current` (or equivalent). If the branch is `main` or `master` (or the repo's default production branch), **stop** and ask for explicit confirmation — even if the user said "commit". Prefer a feature/`dev` branch unless they clearly authorize that default branch.
4. **Per-repo, per-intent commits.** Prefer one commit per repository and per distinct intent. If work spans sibling repos (umbrella workspaces), commit separately in each child repo. Do not mix unrelated intents (e.g. a bugfix + an unrelated refactor) in one commit.
5. **No WIP commits.** Do not commit half-done, broken, checkpoint, or "WIP" code. Prefer leaving a dirty working tree (or stashing only if the user asks) over a WIP commit. Commit only when the intent is complete enough that the tree is a coherent, reviewable unit.
6. **Docs with code (same intent).** When the intent required owning-doc updates (§11 Doc drift), those files **must be staged and committed with the code** in that repo's intent commit. Do not produce a code-only commit and a later docs-only commit for the same feature/fix. If docs are still missing when the user asks to commit, update docs first, then commit once. Empty `docs/` in `git status` is **not** N/A — open owners from `docs/docs-owners.md` (or skill fallback), patch if stale, then stage. Commit reply must include the coding-discipline **Docs commit report**.
7. **Same-issue multi-commit cleanup: rebase.** When several *local, unpushed* commits all address the same issue or fix, prefer interactive rebase (or soft-reset + single commit) into a clean history before the user pushes. Do not rewrite history that is already shared/pushed without explicit approval. Force-push and amend of published commits remain restricted and require explicit confirmation.

---

## 16) Communication

Every response:
1. **What changed** — files, behavior, scope
2. **Why** — reasoning, spec reference
3. **Evidence** — commands, results
4. **Open items** — risks, assumptions, limitations

Concise, factual. No filler.

Tier 2/3 completion responses **must** include a `## Quality Gates` section in the §5.2 format.
If gates are waived, explicitly state waiver text + timestamp + risk.

---

## 17) Quick Checklists

### Tier 1
- [ ] Confirmed contained (Tier 1)
- [ ] Implemented
- [ ] Owning docs co-delivered if contract/UX changed (else N/A)
- [ ] Tests pass (add if none exist)
- [ ] Security self-check. Activate: `security-baseline`
- [ ] No unintended changes
- [ ] Summary: what, why, evidence

### Tier 2
- [ ] Exploration complete
- [ ] Spec with acceptance criteria
- [ ] Implemented per spec
- [ ] Unit + integration tests pass
- [ ] Mandatory gates auto-ran immediately after implementation (no user prompt)
- [ ] Gate evidence attached in response
- [ ] Spec, security, code, test lenses reviewed
- [ ] Docs co-delivered with code (same batch; same intent commit when committing) — not a follow-up
- [ ] Summary with spec reference + evidence

### Tier 3
- [ ] Exploration complete (subagents/helpers mandatory when supported; fallback documented otherwise)
- [ ] Full spec with applicable skill-based sections
- [ ] Spec approved by user
- [ ] Implemented per spec
- [ ] Full test/gate suite passes
- [ ] Mandatory gates auto-ran immediately after implementation (no user prompt)
- [ ] Gate evidence attached in response
- [ ] Conditional verifications per applicable skills
- [ ] Artifact diffs reviewed (if deterministic)
- [ ] All 8 review lenses completed
- [ ] Runtime/tests/docs/contracts aligned **in the same work batch** (not a later docs pass)
- [ ] Design decisions updated
- [ ] Final report: spec, evidence, risks, open items

---

## Appendix: Quick Reference

| Task | Tier | Required Skills | Spec Approval? |
|------|------|-----------------|----------------|
| Fix typo in error message | 1 | security-baseline, coding-discipline, code-quality | No |
| Add new API endpoint | 3 | api-contracts, security-baseline, coding-discipline, code-quality | Yes |
| Refactor shared utility | 2 | security-baseline, coding-discipline, code-quality | No |
| Database schema change | 3 | data-migration, failure-analysis, security-baseline, coding-discipline, code-quality | Yes |
| Local services with containers or Compose | 2 | podman-utilization, security-baseline, coding-discipline, code-quality | No |
| Deploy to production | 3 | deployment-strategy, production-readiness | Yes |
| New frontend flow with routing + state | 3 | frontend-engineering, ui-portability-baseline, security-baseline, coding-discipline, code-quality | Yes |
<!-- <<< skill-forge instructions <<< -->
