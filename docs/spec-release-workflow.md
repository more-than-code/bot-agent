# Spec: Releases from a GitHub workflow (tag, build, sign, publish)

**Status:** revision 1 **approved by Joe 2026-11-25 ("approved"; D1 "New dedicated CI key" on the decision card, D2–D5 defaults)**, with the review amendments in §12.
**Tier:** 3 (how hosts trust agent upgrades: the release key the root helper checks; the release flow of both repos). Claims **agent-protocol v1.27** (v1.26 is the newest implemented; no open or merged PR in either repo claims v1.27 on 2026-11-25; renumber on the code branch if another PR merges it first).
**Repos:** bot-agent (this spec, the signing key change, the agent workflow) and bot-control-plane (the server workflow; picks up its parked PR #4). The code lands as one PR per repo after approval; each names the other.
**Asked by:** Joe, 2026-11-25: "Can we make this sign workflow on GitHub, like storing my private key in GitHub? Then the GitHub workflow can use the key to sign the image."

## 1. Problem

A release today needs Joe's machine twice:

1. **The tag.** A release thread prepares the version commit and the release PRs; Joe merges into `main`. Cloud sessions cannot push tags or create GitHub releases (the cloud GitHub proxy refuses tag pushes, the git refs API and release creation), so Joe or Main Dev tags `main` by hand. v0.2.49 (server) and v0.2.18 (agent) are merged to `main` and still untagged.
2. **The signature.** The helper upgrades an agent only to a bundle whose `SHA256SUMS` carries an Ed25519 signature (`SHA256SUMS.sig`) made with the private half of the one public key compiled into it (`internal/release.PublicKey`, Joe's offline key from 2026-11-23). That private key lives only on Joe's machine; `go run ./cmd/release-sign sign` makes the signature there. The bundle itself is built by hand too: there is no bundle script in bot-agent.

The server has no signature, but its tag, binaries and images (`docs/containers.md` in bot-control-plane) are also built and pushed from Joe's machine.

## 2. Goal

When Joe merges a release PR into `main`, a GitHub Actions workflow in that repo:

- checks that the merged commit is a release (its version is new and consistent),
- runs the release gates,
- builds the release reproducibly (twice, compared),
- for bot-agent, signs `SHA256SUMS` with a key held only by GitHub, after Joe approves the run with one tap,
- creates the tag `vX.Y.Z` on that commit and publishes the GitHub release with its assets.

Nothing in that path needs Joe's machine. Deploying the server and upgrading hosts stay where they are today (§8).

## 3. Decisions for Joe (defaults marked)

| # | Question | Default (recommended) | Alternative |
|---|---|---|---|
| D1 | Which key signs in CI? | **A new, dedicated CI release key.** Your offline key stays off GitHub as a recovery key. Needs one helper change (§5) and one transitional release you still sign locally. | Put your existing offline key into GitHub. No code change and works from the next release, but your only key then lives on GitHub, and a leak can only be cleaned up by a new key that hosts learn from a release signed with the leaked one. |
| D2 | Must you approve each signing run? | **Yes:** the `release` environment has you as required reviewer, so each agent release waits for one tap ("Review deployments" → Approve) in GitHub. | No reviewer: merging the release PR into `main` is the only approval. |
| D3 | Does the server release also wait for your tap? | **No:** it holds no secret beyond the run's own token, and the merge into `main` is your approval. | Yes, same `release` environment. |
| D4 | Server images | **Build and push both images to `ghcr.io` by the workflow, and put the pushed digests in the release notes.** Deploying them stays your step. | Keep building and pushing images on your machine; the workflow publishes only the binary and console bundle. |
| D5 | Parked bot-control-plane PR #4 (self-hosted production deploy job) | **Leave its deploy job out.** Reuse its bundle script and checks in the new workflow, then close #4. | Keep the deploy job (needs a self-hosted runner and a deploy hook on the server host). |

## 4. The workflow (both repos)

`.github/workflows/release.yml`, one per repo.

**Trigger.** `push` to `main`, plus `workflow_dispatch` (rerun for the current `main` head, for example after fixing a secret). Every step is idempotent:

- The version comes from the commit: bot-agent `internal/agent/protocol.AgentVersion`; bot-control-plane the newest `CHANGELOG.md` heading, which must equal `console/package.json` `version` (as PR #4's script checks).
- If tag `vX.Y.Z` already exists on this commit and its release is published, the run ends green with nothing to do. If the tag exists on another commit, the run fails. So a push to `main` that is not a release (a docs sync) does nothing, and a manual tag Joe made meanwhile is respected.
- The version must be newer than the newest `v*` tag; otherwise the run fails.

**Jobs**, each with `permissions` set per job (default `contents: read`):

1. **check** (GitHub-hosted `ubuntu-24.04`, as the non-root `runner` user): the release gates from AGENTS.md.
   - bot-agent: `gofmt -l .`, `go vet ./...`, `go test -count=1 ./...` (with the packaging and installer suite), `go test -race ./internal/agent/run/...`, `go test -tags release ./internal/release` (the compiled keys are not the placeholder).
   - bot-control-plane: `sudo bash scripts/cloud-setup.sh` (the same PostgreSQL 18 + TimescaleDB test database as cloud threads), then the full Go suite with the test database and **0 skipped** (the run fails if `go test -json` reports a skip), the db package twice more, and console `pnpm install --frozen-lockfile`, `check`, `test`, `build`.
2. **build**: builds the release twice in separate clean checkouts and `cmp`s every file. Uploads the result as a run artifact.
   - bot-agent: a new `scripts/release-bundle.sh vX.Y.Z` builds the four linux binaries (`CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w"`, amd64 and arm64), stages `bot-agent-vX.Y.Z/` exactly as `docs/install.md` lists it (`install.sh`, `units/`, binaries, `SHA256SUMS`), and packs a reproducible tarball (`tar --sort=name --mtime=@$SOURCE_DATE_EPOCH --owner=0 --group=0 --numeric-owner`, `gzip -n`; `SOURCE_DATE_EPOCH` is the commit time). Assets: `bot-agent-vX.Y.Z.tar.gz`, `SHA256SUMS` (byte-identical to the one inside), and later `SHA256SUMS.sig`.
   - bot-control-plane: PR #4's `scripts/release-bundle.sh` (server binary linux amd64 and arm64, console tarball, `RELEASE.md`, `SHA256SUMS`), and with D4 the two images built as `docs/containers.md` describes (Docker BuildKit, `rewrite-timestamp`, both platforms) twice with compared image ids.
3. **sign** (bot-agent only; `environment: release`): runs only on `main` (the environment refuses other refs). It does not trust the build job's output: it checks out the same commit, rebuilds the bundle itself, requires its `SHA256SUMS` to equal the build job's byte for byte, then signs:
   - the key is written from the secret `BOT_AGENT_RELEASE_KEY` to a 0600 file under `$RUNNER_TEMP` with the shell's tracing off, used once with `go run ./cmd/release-sign sign`, and deleted in an `always()` step;
   - the signature is checked with `release-sign verify` against **each** public key compiled into the helper until one passes, so a key that hosts would refuse never gets published;
   - the job uploads only `SHA256SUMS.sig`. No step prints the key, and the job has no other secret.
4. **publish** (`contents: write`; for D4 also `packages: write` in the server repo): creates the tag on the checked commit through the API with the run's `GITHUB_TOKEN`, creates the release as a draft, uploads the assets, writes the notes, then publishes it. A failure before publish leaves at most a draft and a tag, which a rerun reuses.
   - Notes: bot-agent takes them from a new `release-notes/vX.Y.Z.md` in the version commit (written by the release thread, the same text as today's release bodies); bot-control-plane takes the `CHANGELOG.md` section. Image digests are appended for D4.
   - A tag pushed with `GITHUB_TOKEN` starts no other workflow, which is why tagging is the last step of the same run.

**Hardening, all jobs.** Third-party actions pinned by full commit SHA (only `actions/checkout`, `actions/setup-go`, `actions/setup-node`, `actions/upload-artifact`, `actions/download-artifact`, and for D4 `docker/setup-buildx-action` and `docker/login-action`); `persist-credentials: false` on checkout; no `pull_request_target`; `concurrency: release` without cancel; Go from `go.mod`'s toolchain; nothing from a PR title or body reaches a shell.

## 5. Host trust (bot-agent code, agent-protocol v1.27)

Needed only for D1 = new CI key.

- `internal/release.PublicKey` becomes `PublicKeys`, a fixed list compiled into the helper: Joe's offline key (recovery) and the CI key. `Verify` accepts a signature that verifies under any listed key, refuses the placeholder and an empty list, and reports `signature_invalid` exactly as today otherwise. No new reason, no new frame, no `runner.toml` key: hosts learn keys only from a release they already trust.
- `release_key_test.go` (`-tags release`) checks every listed key is a real 32-byte key, none is the placeholder, and there are no duplicates.
- `release-sign` is unchanged.
- agent-protocol v1.27, item 6.2 (upgrade-run verify): "with **one of** the public keys compiled into the helper". `docs/design.md` and `docs/install.md` say the same and name both keys' purposes.

**Rotation and revocation.** A key is added or removed only by a release signed with a key hosts already trust. If the CI key leaks: delete the secret, then release (signed locally with the offline key) a version whose list drops it; hosts that upgrade stop trusting it. Until they do, a leaked CI key alone still cannot reach a host: the helper downloads only from `github.com/more-than-code/bot-agent/releases`, so an attacker also needs to publish a release in this repo, and an upgrade happens only when an org owner or admin asks for it.

**Transition.** Today's helpers (v0.2.18 and older) trust only the offline key.

1. Joe makes the CI key pair locally (`go run ./cmd/release-sign keygen -out <dir>`), stores the private half as the environment secret, and pastes the public half (not secret) in the thread.
2. The code PR adds it to `PublicKeys`. The next release (planned v0.2.19) is the **bridge**: built and tagged by the workflow, but its notes file starts with the line `signing: offline`, so the workflow skips the sign job and leaves the release as a draft. Joe downloads its `SHA256SUMS`, signs it locally as today, uploads `SHA256SUMS.sig` and publishes the draft. It ships helpers that trust both keys. Later notes files must not carry that line; the workflow refuses it once the newest published release was signed in CI.
3. Every enrolled host upgrades to the bridge. From the release after it, CI signs.
4. A host still older than the bridge refuses CI-signed releases (`signature_invalid`) because Deploybot offers only the latest version. Such a host is upgraded once by hand with the bridge bundle by the Ops session. The workflow cannot avoid this step; it is the cost of D1.

With D1 = existing key, §5 is not needed, there is no bridge, and the first release after the workflow lands is signed in CI.

## 6. Steps only Joe can do (GitHub settings and the key)

In each repo, Settings:

1. **Environment `release`** (bot-agent; also bot-control-plane if D3 = yes): Deployment branches and tags → "Selected branches and tags" → `main` only. Required reviewers → Joe (D2). Leave "Prevent self-review" **off**: the run is started by your own merge, so it would block you.
2. **Environment secret** `BOT_AGENT_RELEASE_KEY` in `release` (bot-agent): the one-line base64 private key from `release-sign keygen` (D1) or your existing key file's line. Never a repository secret, which any workflow on any branch can read.
3. **Ruleset for `main`**: block force pushes and deletion, require a pull request.
4. **Ruleset for tags `v*`**: block update and deletion, so a published release tag cannot move. Turn on **immutable releases** (repository Settings → General → Releases) so a published release's assets cannot be replaced.
5. **Actions → General**: workflow permissions "Read repository contents" (each job asks for more itself); "Allow GitHub Actions to create and approve pull requests" off.
6. For D4: let the repository's Actions write the two `ghcr.io` packages (package settings → Manage Actions access → `bot-control-plane`, role Write).
7. For D1 = new key: run `keygen` locally, add the secret, paste the public key; sign the bridge release locally once; keep the offline key where it is.

**A limit to know.** Cloud sessions act on GitHub as your account, so GitHub cannot tell your tap from a session's. The approval is only as strong as the rule that sessions never approve a deployment, never edit environments, rulesets or secrets, and never touch `.github/workflows/` outside a reviewed PR. The code PR adds that rule to both AGENTS.md files and the project instructions note.

## 7. Contract diff

| Surface | Before | After |
|---|---|---|
| agent-protocol 6.2 (upgrade-run verify) | "with the public key compiled into the helper" | "with one of the public keys compiled into the helper" (D1 only) |
| Release assets, bot-agent | built and uploaded by hand: `bot-agent-vX.Y.Z.tar.gz`, `SHA256SUMS`, `SHA256SUMS.sig` | the same three names and layout, built by the workflow. `docs/install.md` still lists a `.tar.gz.sha256` file that recent releases do not have; the code PR fixes the guide to the real assets |
| Release assets, server | tag only, no GitHub release | GitHub release with `deploybot-server-vX.Y.Z-linux-{amd64,arm64}`, `bot-console-vX.Y.Z.tar.gz`, `RELEASE.md`, `SHA256SUMS` (+ image digests, D4) |
| Tags | pushed by hand | created by the workflow on the release commit; a hand-made tag on the same commit is accepted |

No api-v1 change, no migration, no frame change, no `runner.toml` key.

## 8. Out of scope

- Deploying the server (the parked self-hosted deploy job, D5) and upgrading hosts: they stay with Joe, Main Dev and the Ops session.
- Signing the server or its images (hosts do not verify them; images are deployed by digest). Build provenance attestations can follow later.
- Changing how hosts download or what they check beyond the key list.

## 9. Alternatives considered

- **Sigstore keyless signing (cosign) or GitHub artifact attestations.** No long-lived key, but the root helper would have to verify certificate chains and a transparency log, a large new trust surface on every host. Rejected for now.
- **Signing on a self-hosted runner holding the key.** Keeps the key off GitHub, but needs a machine that stays up and has Joe's trust, which is what this change removes.
- **Tag-push trigger (PR #4).** Cloud sessions cannot push tags, so Joe would still tag by hand. Rejected in favour of tagging inside the run.

## 10. Rollback

The workflow adds nothing hosts depend on until the first CI-signed release. To roll back: disable the workflow and release by hand as today; signed by the offline key, which every helper keeps trusting under D1. A bad CI-signed release is fixed by a newer release, as today.

## 11. Acceptance criteria and tests

1. A push to `main` without a new version ends green with nothing created.
2. A release commit produces the tag on that commit, a published release with exactly the assets in §7, and (bot-agent) a `SHA256SUMS.sig` that `release-sign verify` accepts with the CI public key and a v0.2.19+ helper accepts in `internal/upgrade` tests.
3. The two builds differ → the run fails before signing; the sign job's own build differs from the build job's → it fails without signing.
4. A tag already on another commit, or a version not newer than the newest tag → the run fails and creates nothing.
5. A run from a branch other than `main` cannot reach `release` or its secret (checked once by Joe with a throwaway branch run, which must stop at "waiting"/refused).
6. Go tests (D1): `Verify` accepts either listed key, refuses an unlisted key, the placeholder, an empty list and a duplicate; existing `internal/upgrade` tests pass with a two-key list.
7. The log of a full run contains no line of the private key (the run masks secrets; the workflow also never echoes the file).

Verification commands: the bot-agent and bot-control-plane change gates from AGENTS.md; `actionlint` on the workflow files; the bridge release (§5) is the first live run of the workflow up to the draft, so the CI signing path first runs live on the release after it.

## 12. Amendments after review (2026-11-25)

The separate reviewer thread's findings on PR #15 (review of ac91ad1), folded in. These rules are normative and win over the earlier text where they are stricter. None of them changes a decision Joe made; A1 can, depending on its result, and then comes back to Joe.

- **A1 (H1) Check that a session cannot approve.** Before the bridge release, a cloud session tries, against a harmless target, each of: approving a pending deployment (`POST …/actions/runs/{run_id}/pending_deployments`), writing an environment, an environment secret and a ruleset. The result goes into this spec. If the cloud GitHub proxy lets any of them through, D2 does not protect against a session, and Joe chooses a second identity (a separate GitHub account as the only required reviewer) before the first CI-signed release.
- **A2 (H2) Show what the tap approves.** A job before **sign** writes the run's job summary, which the "Review deployments" page links: the diff stat since the previous `v*` tag of `.github/workflows/`, `scripts/release-bundle.sh`, `cmd/release-sign/` and `internal/release/`; the compiled `PublicKeys` (base64, public); and the `SHA256SUMS` to be signed.
- **A3 (M1) Tags and releases only from this workflow on `main`.** §6 step 4's ruleset also gets "Restrict creations" for `v*`. If GitHub does not allow the Actions token as a bypass actor, tags stay creatable by any branch workflow; then the §5 sentence "an attacker also needs to publish a release in this repo" is weaker than it reads (a leaked CI key plus a pushed branch with a workflow is enough to publish a signed release), and the defence left is that an upgrade happens only when an org owner or admin asks for it. The **publish** job refuses to run unless `github.workflow_ref` is `<repo>/.github/workflows/release.yml@refs/heads/main`.
- **A4 (M2) Sign job.** `if: github.ref == 'refs/heads/main'` besides the environment rule; only `actions/checkout`, `actions/setup-go` (`cache: false`) and `actions/download-artifact` (only `SHA256SUMS`); `GOFLAGS=-mod=readonly`, `GOTOOLCHAIN=local` after setup-go installs `go.mod`'s toolchain; the secret is in the `env:` of the one signing step, which runs after the rebuild and compare passed, never job-level.
- **A5 (M3) The list reaches the upgrade code.** `upgrade.Env.PublicKey` becomes `PublicKeys` (nil means `release.PublicKeys`); `internal/upgrade` tests cover a two-key helper accepting a bundle signed by the second key and refusing an unlisted key, an empty list and a list holding the placeholder.
- **A6 (L1) Revocation sweep.** After a release that drops a key, Ops lists the hosts whose agent is older than it (the host list shows each agent version) and upgrades them, by Deploybot or by hand; until then they still trust the dropped key.
- **A7 (L2) How "signed in CI" is detected.** The `signing: offline` line is refused when the newest published release's `SHA256SUMS.sig` verifies with a key in the list other than the offline key. The guard catches mistakes only: the line can only ever produce an unsigned draft.
- **A8 (L3) Why the sign job rebuilds.** The two builds in **build** prove determinism; the sign job's rebuild on a separate runner is the independent check that what gets signed is what the commit builds. It must not be "simplified" into signing the build job's artifact.
- **A9 (L4) What D3 means.** With D3 = no tap, a session that merges a release PR into `main` when Joe asked for a release also publishes the server release and (D4) pushes the images, with no further action by Joe. Deploying still needs Joe.

## 13. Amendment: the CI key lives in AWS KMS ("computerless", 2026-11-25)

**Status: proposal, not approved.** Joe (2026-11-25 18:35Z): where the key lives is still being worked out ("solutioning phase rather than decision"). The workflow on this branch implements this proposal so it can be judged concretely; it is not merged until Joe decides.

Joe's standing requirement (2026-11-25, "computerless"): releases, signing and key storage must work from his phone, with no step on his own computer. He leans towards a stable managed service ("I think the key is have a managed service which is stable, like AWS") over a GitHub environment secret or his own VMs. If accepted, this replaces where the D1 key lives, not D1 itself, and it replaces the `BOT_AGENT_RELEASE_KEY` secret everywhere above.

**Key type.** AWS KMS supports Ed25519: key spec `ECC_NIST_EDWARDS25519`, key usage `SIGN_VERIFY`. Signing with `ED25519_SHA_512` and `MessageType` `RAW` is plain Ed25519 (RFC 8032 PureEdDSA), so the 64-byte signature verifies with Go's `ed25519.Verify` and the helper code is unchanged. `RAW` messages are at most 4096 bytes; `SHA256SUMS` is about 1 KB, and the sign job refuses one over the limit. The private key cannot be exported from KMS.

**Sign job.** It has `id-token: write` and no secret. After the rebuild and compare pass, its signing step requests a GitHub OIDC token for audience `sts.amazonaws.com`, exchanges it with `sts:AssumeRoleWithWebIdentity` for 15-minute credentials, and calls `kms:Sign`. The role ARN, key id and region are environment *variables* of `release` (not secret). Then it checks the signature against the compiled keys as before. No third-party action is added; the runner's own `aws` CLI is used.

**Steps only Joe can do, all from a phone browser** (they replace §6 steps 2 and 7):

1. AWS console → KMS → Create key: Asymmetric, Sign and verify, `ECC_NIST_EDWARDS25519`, single region. Alias `bot-agent-release`. Key administrators: yourself only. Key users: none yet (the role below gets its rights from its own policy).
2. IAM → Identity providers → Add provider: OpenID Connect, URL `https://token.actions.githubusercontent.com`, audience `sts.amazonaws.com` (skip if it exists).
3. IAM → Roles → Create role "Web identity" for that provider, named `bot-agent-release-signer`. Trust policy condition: `token.actions.githubusercontent.com:aud` equals `sts.amazonaws.com` and `token.actions.githubusercontent.com:sub` equals `repo:more-than-code/bot-agent:environment:release` (exactly; no wildcard). Permissions: one inline policy allowing only `kms:Sign` on the key's ARN, with condition `kms:SigningAlgorithm` = `ED25519_SHA_512`. Maximum session 1 hour.
4. GitHub, bot-agent → Settings → Environments → `release` → Environment variables: `AWS_REGION`, `AWS_RELEASE_ROLE_ARN` (the role's ARN), `AWS_KMS_KEY_ID` (the key's ARN). No secrets.
5. KMS → the key → Public key tab → copy the PEM and paste it in the thread. It is public; the thread converts it to the 32-byte key for `PublicKeys`.

**What changes in the threat model.**
- The private key can no longer leak from GitHub, a log or a runner; the worst a compromised run can do is ask KMS for signatures while its 15-minute credentials last, and every `Sign` call is in CloudTrail's event history with the run id as session name.
- Revocation is immediate: disable the KMS key (one tap) and nothing can be signed with it; then drop it from `PublicKeys` in a release signed by the offline key (§5, A6).
- The `sub` condition ties signing to a job that ran in the `release` environment, which only `main` can reach and which waits for Joe's tap (D2). A workflow on another branch gets a different `sub` and is refused by AWS.
- A1 still applies: also check that a cloud session cannot write IAM or KMS in that AWS account. Sessions never get AWS credentials for it.

**Still needing a computer, and the phone-only way:**
- *The bridge release (§5)*, signed with Joe's offline key, which today is a file on his Mac. Phone-only alternative, for Joe to choose when the bridge is cut: add the offline key's line as a temporary `release` environment secret, let a one-off sign step use it, then delete the secret. Or sign the bridge once on the Mac while he still has it. Until he chooses, the workflow keeps the `signing: offline` draft path.
- *The offline recovery key itself*: move it into a password manager as a secure note and delete it from the Mac. Using it later (revoking the CI key) takes the same temporary-secret path.

`release-notes/`, the summary page, publishing and the server workflow are unchanged.
