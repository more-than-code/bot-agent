@AGENTS.md

<!-- >>> skill-forge instructions full@0.1.0 >>> -->
# Claude Code CLAUDE.md Overlay

This overlay extends the shared core instructions with Claude Code-specific behavior.

## Claude Code Skill Paths

Claude Code surfaces skills natively from two directories, both written by Skill Forge sync (never by hand):

- Project skills: `.claude/skills/*/SKILL.md` — written by `skf sync` from the repo's `skill-forge.json`.
- User-level skills: `~/.claude/skills/*/SKILL.md` — written by `skf home sync` from the `$HOME` profile.

These carry the same content as the core protocol's `.agents/skills`/`~/.agents/skills` paths (a Claude-Code-only project may have just `.claude/skills/`); apply the same project-over-home precedence and never activate two copies of one name.

For concrete before/after examples of common failure modes, also check the activated `coding-discipline` skill's `EXAMPLES.md` companion.

## Claude Code Delegation

Claude Code may expose subagents or helper workflows. When delegation is unavailable, perform the same exploration and review steps in the main context and say so briefly.

Maintained Claude Code subagent definitions are installed at `~/.claude/agents/`.

- For exploration (§7), use the built-in `Explore` agent. For planning, use the built-in `Plan` agent. Both are harness-enforced read-only. Include the required output shape (findings with file references, or spec-shaped plans with acceptance criteria and verification gates) in the task prompt.
- Use the maintained `validator`, `reviewer`, and `bulk-worker` subagents for their §7 roles.
- Use `.claude/rules/` or project-local Claude configuration for narrower file/path-specific guidance instead of expanding this file.
- Claude-specific auto memory may record learnings separately. Do not treat auto memory as a substitute for explicit safety, verification, or permission rules in the composed file.

## Claude Code Agent Reference

Maintained Claude Code subagents pin a Claude model-family alias and effort level to preserve each role's quality, latency, and cost class. Aliases resolve to a current model available through the user's configured provider; Claude Code's documented environment, invocation, and organization-policy precedence can still override or substitute the definition.

| Agent | Source | Model | Effort | Best for |
|---|---|---|---|---|
| `Explore` | built-in | inherited | inherited | Code exploration, API tracing, reading tests, in-scope synthesis |
| `Plan` | built-in | inherited | inherited | Architecture decisions, multi-file tradeoffs, design with real stakes (pass a `model` override such as `opus` for high-stakes design) |
| `general-purpose` | built-in | inherited | inherited | Catch-all for multi-step delegated tasks when no maintained role or built-in above fits |
| `bulk-worker` | maintained | `haiku` | `medium` | Formatting, renaming, repetitive transforms, file enumeration |
| `validator` | maintained | `sonnet` | `medium` | Command execution, checks, and reusable evidence capture |
| `reviewer` | maintained | `sonnet` | `high` | One review lens at a time with severity-tagged findings |

Other built-in utility agents (for example `claude-code-guide`, `statusline-setup`) vary by Claude Code version and surface; use them for their stated purpose, not for §7 delegation roles.

## Claude Code Built-In Commands

- `/verify` supplements §5 verification by exercising the changed flow end-to-end. Use it as additional evidence; it does not replace the required gate commands and evidence format.
- `/code-review` and `/simplify` supplement the §6 review lenses. Per §7, they do not replace lens reviews unless they produce the required severity-tagged per-lens output.

## Claude Code Permission Model

Claude Code subagent files do not pin Codex-style `sandbox_mode`. Enforcement is layered:

- Built-in `Explore` and `Plan` are harness-enforced read-only.
- All maintained roles declare a `tools` allowlist in frontmatter. `reviewer` is read-only. `validator` excludes the `Edit` and `Write` tools. `bulk-worker` receives file tools and `Bash`, but no browser, MCP, or delegation tools.
- `Bash` can still write, so the validator's source-file restriction and the bulk-worker's assigned-file restriction remain behavioral contracts reinforced by active session permissions.

## Claude Code Target Notes

- Runtime target: use the composed content as `CLAUDE.md` for Claude Code user/global or project memory.
- Managed global target: `~/.claude/CLAUDE.md`.
- Project-specific overrides should live in the repository `CLAUDE.md` file.
- Keep instructions concise. Claude Code loads `CLAUDE.md` as persistent context, not as an enforced policy engine.
- Keep this overlay focused on Claude Code-specific behavior. Shared process belongs in the core instructions.
<!-- <<< skill-forge instructions <<< -->
