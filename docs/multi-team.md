# Multi-team mode

By default terraplane trusts everyone who can comment on a pull request and
every agent that holds the shared token. That is fine for one team. When
several teams share an orchestrator, each with its own agent and cloud
credentials, set `ORCHESTRATOR_CONFIG_PATH` to a config file and terraplane
switches to multi-team mode:

- Each agent has its own token. The orchestrator derives the agent's identity
  from the token and an agent can only claim its own jobs.
- The orchestrator decides which agent runs each repository's stacks. The
  `agent` field in `terraplane.yaml` is ignored, so a pull request cannot
  route itself to a privileged agent.
- Per repository, and optionally per stack, a list of GitHub users and teams
  may `plan`, `apply`, and `unlock`. Everyone else gets a comment saying why.
- Optional gates on `apply`: review approval, mergeability, up to date with
  the base branch, green checks. Apply always requires an open pull request.
- Webhooks from repositories not listed in the file are ignored.

Without the file nothing changes. Agents never change: each one is deployed
with its own value in `SHARED_AUTH_TOKEN`.

## Config file

```yaml
agents:
  agent-prod:
    token_env: AGENT_TOKEN_PROD          # env var on the orchestrator holding this agent's token
  agent-platform:
    token_env: AGENT_TOKEN_PLATFORM

repos:
  acme/infra:
    agent: agent-prod                     # default agent for every stack in this repo
    permissions:
      plan:   ["@acme/engineering"]       # GitHub team
      apply:  ["alice"]                   # GitHub user
      unlock: ["alice"]                   # defaults to the apply list when omitted
    stacks:
      reporting:                          # per-stack overrides
        dir: stacks/reporting             # required: pins the stack name to a directory
        agent: agent-data
        permissions:                      # replaces the repo block for this stack
          plan:  ["alice", "bob"]
          apply: ["alice", "bob"]
    apply_requires:
      approved: true
      mergeable: true
      undiverged: true
      checks_green: true
```

### `agents`

Every agent allowed to talk to the orchestrator, and the name of the env var
that holds its bearer token. Tokens never appear in the file. Two agents may
not share a token. Deploy each agent with the same value in its
`SHARED_AUTH_TOKEN`.

### `repos`

Keyed by `owner/name`, case-insensitive. A webhook from a repository that is
not listed is ignored, so listing a repository is how you onboard it.

- `agent` is required: the default agent for the repository's stacks.
- `permissions` lists who may run each action. Entries are GitHub logins or
  `@org/team` slugs. An action with an empty or missing list is denied for
  everyone, so a repository without `permissions` is locked until someone
  decides who gets access. `unlock` defaults to the `apply` list.
- `stacks.<name>` overrides `agent` and `permissions` for one stack. A stack
  block's `permissions` replaces the repository block entirely rather than
  merging with it.
- `stacks.<name>.dir` is required for every listed stack. Permissions and
  agents are keyed by stack name, while the pull request's `terraplane.yaml`
  controls which directory a name maps to. Pinning the directory stops a pull
  request from renaming a privileged stack onto another directory, or reusing
  a pinned directory under a new name. Either is denied with a reason in the
  comment. Stack names not listed here get the repository defaults, so make
  the repository default agent and permissions the least privileged ones.

### `apply_requires`

Gates checked when someone comments `terraplane apply`, after the permission
check. The names follow Atlantis.

| Gate | Passes when |
|------|-------------|
| `approved` | At least one reviewer's latest review is an approval and no reviewer's latest review requests changes |
| `mergeable` | GitHub reports the pull request as mergeable: no conflicts, not a draft, branch protection rules satisfied. `unstable` (a non-required check failing) still passes, as in Atlantis |
| `undiverged` | The pull request head contains the latest commit on its base branch |
| `checks_green` | Every commit status and check run on the head has passed. A head with nothing reported passes |

Regardless of gates, apply requires the pull request to be open. A failed
gate is reported in the denial comment and nothing is queued. If GitHub
cannot be reached the apply is blocked rather than let through.

## What a denial looks like

```
### apply · ⛔ denied

> [!CAUTION]
> - bob is not allowed to apply stack reporting in acme/infra
> - apply requires an approved review on pull request #42
```

Stacks the user may act on still run; only the denied ones are skipped.

## GitHub credentials

The orchestrator authenticates to GitHub either with a personal access token
(`ORCHESTRATOR_GITHUB_ACCESS_TOKEN`) or as a GitHub App
(`ORCHESTRATOR_GITHUB_APP_ID` plus `ORCHESTRATOR_GITHUB_APP_PRIVATE_KEY` or
`ORCHESTRATOR_GITHUB_APP_PRIVATE_KEY_PATH`). An app is the better fit for a
shared orchestrator: no token tied to a person, permissions limited to what is
listed below, hour-long installation tokens minted on demand, and comments
posted under the app's own identity. Without
`ORCHESTRATOR_GITHUB_APP_INSTALLATION_ID` the orchestrator finds the
installation for each repository's organisation or user and caches it, so one
app installed in several organisations works.

An app needs these repository permissions: Contents read, Metadata read, Pull
requests read and write (comments and reactions on a pull request are pull
request writes, and GitHub answers 403 without it), Issues write, Checks read,
Commit statuses read; and the organisation permission Members read when `@org/team`
entries are used. Subscribe it to the `issue_comment` event and point its
webhook at `/scm/webhook` with `ORCHESTRATOR_GITHUB_WEBHOOK_SECRET`.

A personal access token additionally needs permission to read organisation
membership when `@org/team` entries are used: `read:org` on a classic token,
or organisation "Members: read" on a fine-grained token. Without it GitHub
answers "not found" for every membership lookup, which terraplane reads as
"not a member", so team entries would deny everyone. User entries do not need
it.

Membership answers are cached for `ORCHESTRATOR_TEAM_CACHE_TTL` (default
`5m`). A user removed from a team keeps access for up to that long.

## Things the config does not protect against

- **Plan permission is code execution on the agent.** `terraform init`
  downloads providers chosen by the pull request's own configuration, and data
  sources such as `external` run commands during plan. Anyone in a `plan`
  list can run code with that stack's agent credentials. The per-agent
  credentials are what limit the damage. Choose `plan` lists as carefully as
  `apply` lists.
- **The webhook secret is shared.** Terraplane re-reads every command comment
  from the GitHub API after verifying the signature, so a forged webhook can
  only trigger what a real comment says, by its real author. Still, only the
  operator should configure webhooks, ideally one organisation-level webhook.
- **Commands run against the pull request head at the time the comment is
  processed.** A pull request author can push between a reviewer's `apply`
  comment and its processing. The `undiverged` and `checks_green` gates narrow
  the window; branch protection with "dismiss stale approvals" closes it.

## Deploying with Helm

Set `orchestrator.config` in the chart values to the contents of the file.
The chart renders it to a ConfigMap, mounts it, and sets
`ORCHESTRATOR_CONFIG_PATH`. Put the agent tokens in the orchestrator's
Secret under the env var names the file references, and give each agent
release its own `SHARED_AUTH_TOKEN`. See
[charts/terraplane/examples/multi-team](../charts/terraplane/examples/multi-team/).
