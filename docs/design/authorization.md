# Multi-team authorization for the PR workflow

Status: proposal. Tracks the work in the PR checklist at the bottom.

## Problem

Terraplane today assumes a single team that trusts itself:

- Anyone who can comment on a PR can `plan`, `apply`, or `unlock` any stack. The webhook tells
  the orchestrator who commented, but nothing checks it.
- All agents share one bearer token and send their own `agent_id` in the request body. Any
  agent can claim any other agent's jobs.
- The orchestrator reads `terraplane.yaml` from the PR head commit, and that file decides which
  agent runs each stack. A PR can edit the file and route its stack to a privileged agent.

The agent model (one agent per cloud project, credentials never leave it, outbound only) is the
right shape for several teams managing their own infrastructure from their own repos. These
three gaps are what stop it being safe to hand to more than one team.

## Goals

- Per repo, optionally per stack: which GitHub users or teams may `plan`, `apply`, `unlock`.
- Optional gates on `apply`: PR approved, PR checks green.
- Agent identity derived from its token, not from the request body.
- Stack-to-agent routing owned by the orchestrator operator, not by the PR author.
- A denied command gets a PR comment saying why.

## Non-goals

- A UI, tracked runs, merge queues, drift detection. None of this changes how jobs run.
- Permissions stored in the governed repo. They live with the orchestrator on purpose.
- Changing the agent. Agents keep the same five endpoints and the same env vars.

## Compatibility rule

Every new behaviour is switched on by the presence of the orchestrator config file.
With `ORCHESTRATOR_CONFIG_PATH` unset, terraplane behaves exactly as it does today: shared
token, `agent_id` from the body, routing from `terraplane.yaml`, no authorization.
Each PR below must keep the existing test suite passing unchanged.

## Configuration

One YAML file on the orchestrator, path given by `ORCHESTRATOR_CONFIG_PATH`.

```yaml
agents:
  agent-prod:
    token_env: AGENT_TOKEN_PROD          # env var holding this agent's bearer token
  agent-platform:
    token_env: AGENT_TOKEN_PLATFORM
  agent-data:
    token_env: AGENT_TOKEN_DATA

repos:
  acme/infra:
    agent: agent-prod                     # default agent for every stack in this repo
    permissions:
      plan:   ["@acme/engineering"]       # GitHub team
      apply:  ["alice"]                   # GitHub user
      unlock: ["alice"]
    stacks:
      reporting:                          # per-stack overrides
        dir: stacks/reporting             # required: pins the name to a directory
        agent: agent-data
        permissions:
          plan:  ["alice", "bob"]
          apply: ["alice", "bob"]

  acme/platform:
    agent: agent-platform
    permissions:
      plan:   ["@acme/platform"]
      apply:  ["@acme/platform-leads"]
      unlock: ["@acme/platform-leads"]
    apply_requires:
      approved: true                      # an approving review and no outstanding change requests
      mergeable: true                     # GitHub mergeable_state clean/unstable/has_hooks (no conflicts, not draft, protection satisfied)
      undiverged: true                    # PR head contains the latest base branch commit
      checks_green: true                  # all status checks and check runs on the head commit passed
```

Semantics:

- `agents` lists every agent allowed to talk to the orchestrator. Tokens are read from the named
  env vars at startup. Two agents may not share a token.
- `repos.<slug>.agent` is the default agent for the repo. `stacks.<name>.agent` overrides it.
  Both must name an entry in `agents`. Repo slugs are case-insensitive.
- Every stack listed under `stacks` must give its `dir`, relative to the repository, and two
  stacks in a repo cannot share a dir. Permissions and agents are keyed by stack *name* while the
  PR's `terraplane.yaml` decides which directory a name maps to, so without this pin a PR could
  rename a privileged stack onto another directory. A PR stack whose name is listed must keep the
  pinned dir, and a PR stack whose dir is pinned to another name is denied. Unlisted names fall
  back to the repo defaults, so the repo default agent and permissions are the floor: make them
  the least privileged in the repo.
- `permissions` lists are GitHub logins or `@org/team` slugs. A stack-level `permissions` block
  replaces the repo-level one for that stack entirely. An action with an empty or missing list is
  denied for everyone. `unlock` defaults to the `apply` list when omitted.
- `apply_requires` applies to every `apply` in the repo. All gates default to false. The names
  follow Atlantis: `approved`, `mergeable`, `undiverged`, `checks_green`. With a config file an
  apply always requires the pull request to be open, regardless of gates.
- A webhook from a repo not listed under `repos` is logged and ignored. No comment is written,
  since the orchestrator has no relationship with that repo.
- When the config file is present, the `agent` field in `terraplane.yaml` is ignored. Stacks
  still come from `terraplane.yaml` (name, dir, tool_version). A stack in `terraplane.yaml` that
  has no agent in the orchestrator config falls back to the repo default agent.

Validation at startup fails fast on: unknown agent references, duplicate tokens, empty token env
vars, malformed team references (`@org/team` is the only accepted form), unknown keys.

## Behaviour

### Agent identity

With config present, `requireBearer` matches the presented token against the configured agent
tokens using `crypto/subtle.ConstantTimeCompare` and stores the matched agent name in the request
context. Handlers take the agent ID from the context. If the body still carries `agent_id` and it
differs from the context value, the request is rejected with 403. The `SHARED_AUTH_TOKEN` env var
is ignored for agent routes when config is present.

Agents are unchanged. Each one is deployed with its own value in `SHARED_AUTH_TOKEN`.

### Routing

`JobService.CreatePendingJobs` resolves stacks from `terraplane.yaml` as today, then asks the
config for the agent of each resolved stack instead of using `stack.Agent`. `UnlockService` is
unaffected (it never routes to an agent).

`terraplaneconfig.Validate` currently rejects a stack with no agent. That check moves behind a
flag so the orchestrator can skip it when config is present. The agent-side `terraform.Manager`
calls `ResolveStacks` for dir lookup and must keep working on a `terraplane.yaml` with no agent
fields at all.

### Authorization

A new `Authorizer` interface is injected into `JobService` and `UnlockService`:

```go
type Authorizer interface {
    // Allowed reports whether user may perform action on stack in repo.
    // Returns a reason suitable for a PR comment when denied.
    Allowed(ctx context.Context, user, repo, stack string, action Action) (bool, string, error)
}
```

The config-backed implementation checks the user against the stack's list for that action.
Entries that start with `@` are team slugs and are resolved through
`GET /orgs/{org}/teams/{team}/memberships/{username}` on the SCM provider, cached for a short TTL
(default 5 minutes, configurable). A membership is active only when the API returns 200 with
`state: active`.

Enforcement happens per resolved stack, after stacks are resolved and before any job is upserted
or lock is taken. Stacks the user may act on proceed; the rest are collected and reported in a
single denial comment on the PR. If every stack is denied, nothing is queued and the command is
still acknowledged with a reaction so the user knows it was seen.

The no-config implementation always returns allowed.

### Apply gates

Checked only for `apply`, after authorization passes. With a config file the pull request is
always fetched (`GET /repos/{slug}/pulls/{n}`) and must be open. Then, per configured gate:

- `approved`: fetch `GET /repos/{slug}/pulls/{n}/reviews`, keep the latest review per reviewer
  (`COMMENTED` does not change it, `DISMISSED` clears it), require at least one `APPROVED` and no
  `CHANGES_REQUESTED`.
- `mergeable`: the pull request must not be a draft (its `draft` field; GitHub reports a draft
  as `clean` when the base branch is unprotected) and its `mergeable_state` must be `clean`,
  `unstable`, or `has_hooks`, as in Atlantis. `dirty`, `draft`, `blocked`, and `behind` fail with
  a reason naming the cause; `unknown` (GitHub still computing after a push) fails with a "try
  again shortly" reason.
- `undiverged`: `GET /repos/{slug}/compare/{base}...{head}` must report `behind_by` of zero.
- `checks_green`: fetch `GET /repos/{slug}/commits/{sha}/status` and
  `GET /repos/{slug}/commits/{sha}/check-runs` for the PR head. Require the combined status to be
  `success` (or no statuses at all) and every check run to be `completed` with conclusion
  `success`, `neutral`, or `skipped`.

A failed gate produces a denial comment naming the gate. Gate failures are not errors; the
webhook still returns 200.

### Denial comment

Reuses `feedback` formatting so it looks like every other terraplane comment:

```
### `reporting` · apply · ⛔ denied

> [!CAUTION]
> @bob is not allowed to apply stack `reporting` in acme/infra.
```

One comment per command listing every denied stack and reason, not one comment per stack.

## Security notes

Things the design relies on that are not enforced by code, found in review of PR 4:

- **Plan permission is code execution on the agent.** `terraform init` fetches providers from the
  PR's own configuration and data sources such as `external` run commands during plan. Anyone in
  a `plan` list can run code with that stack's agent credentials. Per-agent credentials are what
  bound the damage; choose `plan` lists accordingly.
- **The webhook secret is a shared trust root.** Anyone who knows
  `ORCHESTRATOR_GITHUB_WEBHOOK_SECRET` can forge a signed webhook naming any repo, PR, user, and
  command. Only the operator should configure webhooks, ideally one org-level webhook. PR 5 below
  removes most of this risk by re-reading the comment from the GitHub API before acting on it.
- **Authorization is evaluated at comment time against the PR head fetched then.** A PR author
  can push between a reviewer's `apply` comment and the webhook arriving. Inherent to the
  comment-driven model; the `checks_green` gate narrows the window.
- **Team membership needs `read:org`.** Without it GitHub answers 404, which reads as "not a
  member", so team entries deny silently. Document, and consider a startup probe.
- **Cache TTL.** A user removed from a team keeps access for up to `ORCHESTRATOR_TEAM_CACHE_TTL`.

## Code changes by PR

Each PR branches from upstream `main`, is merged into `integration` for internal use, and is
opened upstream when ready. Each must: keep `make unit-test` green including the 100% gate on
pure packages, pass `make lint`, regenerate mocks with `make generate`, add a `CHANGELOG.md`
entry under Unreleased.

### PR 1: Config file loader

- `pkg/orchestratorconfig/config_file.go`: types, `Load(path) (*ConfigFile, error)`,
  `Validate()`, lookup helpers (`AgentForStack`, `PermissionsFor`, `ApplyRequires`, `AgentByToken`).
- `config/config.go`: `ORCHESTRATOR_CONFIG_PATH` (default empty) and
  `ORCHESTRATOR_TEAM_CACHE_TTL` (default `5m`).
- Wire provider for `*orchestratorconfig.ConfigFile` returning an empty, disabled value when the
  path is unset.
- Tests: parsing, every validation rule, lookup precedence. Package goes into
  `COVERAGE_FULL_PKGS`. No behaviour change anywhere else.

### PR 2: Per-agent tokens

- `pkg/webserver/handler.go`: `requireBearer` consults the config when enabled, stores agent
  name in context via a small unexported key, rejects body/context mismatch with 403.
- Handlers read agent ID from a helper `agentIDFrom(r)` that prefers context, falls back to body
  when config is disabled.
- `internal/auth`: constant-time comparison for both the shared and the per-agent path.
- Tests: token maps to agent, unknown token 401, mismatch 403, legacy path unchanged.

### PR 3: Orchestrator-owned routing

- `pkg/terraplaneconfig`: split the "every stack has an agent" rule out of `Validate` into
  `ValidateAgents`, called only from the legacy path.
- `pkg/orchestrator/services/job_service.go`: resolve agent through the config when enabled;
  log and ignore webhooks from unlisted repos.
- `pkg/webserver/handler.go`: drop unlisted repos before reaching the services so unlock follows
  the same rule.
- Tests: agent from repo default, stack override, unlisted repo ignored, legacy path unchanged,
  agent-side manager still resolves a `terraplane.yaml` without agents.

### PR 4: Authorization

- `pkg/authz`: `Authorizer` interface, `Action` type, config-backed implementation, allow-all
  implementation, mockgen.
- `pkg/scm`: add `TeamMember(ctx, org, team, user string) (bool, error)` to `Provider`; GitHub
  implementation plus TTL cache in the authz package, not in the client.
- `pkg/feedback`: `DeniedComment(...)`.
- `pkg/orchestrator/services`: enforce in `CreatePendingJobs` and `RunUnlock`; publish denial
  comment.
- Tests: user allowed, team allowed, team denied, partial denial queues the allowed stacks only,
  team lookup error fails closed, cache hit avoids second API call.

### PR 5: Verify commands against the GitHub API

- `pkg/scm/github`: after signature verification, fetch
  `GET /repos/{slug}/issues/comments/{id}` and `GET /repos/{slug}/pulls/{n}` with the
  orchestrator token; take the login, body, repo slug, and PR head from the API responses rather
  than the webhook body, and confirm the issue is a pull request. A comment that no longer exists
  or whose body differs is ignored.
- Effect: a forged or replayed webhook can at most trigger what the real comment says, authored
  by the real commenter. Legacy mode gets the same check since it costs one API call and changes
  no behaviour for genuine deliveries.
- As built: the API body is simply used as the truth rather than compared with the webhook body,
  so a quick typo edit does not drop the command. Comments whose first word is not `terraplane`
  are filtered before any API call, which also removes the head-SHA lookup every PR comment used
  to cost.
- Tests: API author and body win, deleted comment ignored, wrong issue ignored, API error fails
  closed, chatter makes no API calls.

### PR 6: Apply gates

- `pkg/scm`: add `PullRequestApproved(ctx, repo, pr)` and `ChecksGreen(ctx, repo, sha)` to
  `Provider`; GitHub implementations with httptest coverage.
- `pkg/orchestrator/services/job_service.go`: evaluate gates after authorization for `apply`.
- Tests: each gate pass and fail, gate not configured skips the API call, API error fails closed.

### PR 7: Packaging and docs

- Helm chart: `orchestrator.configFile` value rendered to a ConfigMap and mounted; agent token
  env vars documented in the examples.
- `charts/terraplane/README.md`, `README.md`: config reference and the compatibility rule.
- `.env-example`: `ORCHESTRATOR_CONFIG_PATH`.

## Open questions

- GitHub token scope: team membership lookups need `read:org` on a classic PAT or the
  organisation Members read permission on a fine-grained token. Document, or detect at startup
  with a probe call?
- Should `plan` flags (for example `-target`) be restrictable per stack? Easy to add as a
  `permissions.plan_flags` boolean later; left out of this round.
- Should an agent's allowed repos also be enforced on the agent side? Not needed while the
  orchestrator owns routing, but it would defend against a compromised orchestrator.

## Clone tokens (PR 8)

Agents clone with a deploy key today, one key per agent, registered by hand on every
repository and readable by anyone on the agent host. With the orchestrator authenticating as a
GitHub App, the key can go: the orchestrator mints a token per job and the agent uses it once.

Flow, agent in `AGENT_SCM_AUTH=app` mode:

1. The agent claims a job as before. The claim response is unchanged; no token travels with
   the job, so nothing secret lands in the `jobs` table or in agent logs of the command.
2. Right before cloning, the agent calls `POST /agent/jobs/{id}/clone_token` with its agent
   token. The orchestrator checks the job exists, belongs to that agent (per-agent tokens from
   PR 2), and is `claimed` or `running`, then asks GitHub for an installation token with
   `repositories: [<name>]` and `permissions: {contents: read, metadata: read}`. The token is
   returned once and remembered in memory, keyed by job, for revocation.
3. The agent passes the token to git as an `Authorization: Basic` header through
   `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0`/`GIT_CONFIG_VALUE_0`, with `GIT_TERMINAL_PROMPT=0`.
   The header never appears on argv, in `.git/config`, or on disk, and the remote URL is the
   plain HTTPS URL.
4. When the job's result arrives, when the reaper fails or returns it, or when a second token
   is issued for the same job, the orchestrator revokes the previous token
   (`DELETE /installation/token`). Revocation is best-effort and logged; GitHub expires the
   token after an hour in any case.

Boundaries:

- A personal access token cannot be narrowed, so an orchestrator in PAT mode answers 501 and
  the agent reports that it must use SSH. The PAT itself is never handed to an agent.
- A stolen agent token yields clone tokens only for jobs routed to that agent, the same
  repositories it could already clone with its deploy key, and only while a job is in flight.
- Private modules in sibling repositories over `git::https` are out of scope for the token by
  design; registries, same-repository modules, or SSH mode cover those cases.
- The endpoint is additive: SSH agents never call it, so mixed agent versions keep working.

## Checklist

- [x] PR 1 config file loader
- [x] PR 2 per-agent tokens
- [x] PR 3 orchestrator-owned routing
- [x] PR 4 authorization (hardened after security review)
- [x] PR 5 verify commands against the GitHub API
- [x] PR 6 apply gates (`approved`, `mergeable`, `undiverged`, `checks_green`, open PR)
- [x] PR 7 packaging and docs (`docs/multi-team.md` is the user-facing reference)
- [x] PR 8 per-job clone tokens (`AGENT_SCM_AUTH=app`), after GitHub App authentication landed

## Follow-ups not in this plan

- Hold the stack lock from apply until the PR merges or closes (Atlantis behaviour), needs a
  `pull_request` closed webhook.
- `automerge` after a successful apply so `main` catches up immediately.
- Scheduled drift plans per stack against `main`.
- Startup probe for `read:org` so team-based permissions cannot deny silently.
- Pagination for reviews and check runs beyond 100.
- Revoke clone tokens for claims the reaper returns to `pending`: the repository reports only a
  count today, so such a token stays valid until GitHub expires it or the job is re-issued one.
- Upstreaming: PRs 1 to 7 are individually upstreamable in order; each branch starts from the
  previous PR's commit.
