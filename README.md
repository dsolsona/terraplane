

# Terraplane

PR-driven Terraform automation with remote agents.

[Website](https://terraplane.io) · [Docs](https://terraplane.io/docs) · [Quickstart](https://terraplane.io/docs/quickstart) · [Releases](https://github.com/terraplane-org/terraplane/releases)

---

# Early alpha

Terraplane works — you can run it, wire up agents, and drive plans/applies from PRs. It is still early. APIs and config can change, edges are rough, and there is no large community or long production track record behind it yet. Treat it as alpha software you can use, not as something with years of hardening.

---

## What it is

Terraform belongs in the pull request, and the runner belongs inside your network. Terraplane keeps an Atlantis-style comment workflow (`plan` / `apply` / `unlock`) but splits into:

- an **orchestrator** — webhooks, jobs, locks, PR feedback
- **agents** — poll the orchestrator over HTTP and run Terraform where credentials and network access already live

Stacks live under environments in `terraplane.yaml` and route work to the right agent (`agent` on the environment, optional per-stack override). Each stack runs with Terraform or OpenTofu (`tool: tofu`, on the stack or environment) at a pinned `tool_version`. Details: [Why Terraplane](https://terraplane.io/docs/why), [How it works](https://terraplane.io/docs/how-it-works).

## Several teams, one orchestrator

Set `ORCHESTRATOR_CONFIG_PATH` to a config file and terraplane gains per-agent tokens, orchestrator-owned routing of stacks to agents, per-repo and per-stack permissions for `plan` / `apply` / `unlock` by GitHub user or team, and Atlantis-style apply gates (`approved`, `mergeable`, `undiverged`, `checks_green`). Without the file nothing changes. See [docs/multi-team.md](docs/multi-team.md).

## Get started

**[Quickstart](https://terraplane.io/docs/quickstart)** is the path from zero to a PR plan. Everything else lives in the [docs](https://terraplane.io/docs) — configuration, deployment, local Make targets.

Helm:

```bash
helm install terraplane oci://ghcr.io/terraplane-org/charts/terraplane \
  --version 0.3.0 \
  -n terraplane --create-namespace \
  -f my-values.yaml
```

Chart values, split-install notes, and examples: `[charts/terraplane](charts/terraplane)` · `[examples](charts/terraplane/examples)`.

## Acknowledgements

Terraplane would not exist without [Atlantis](https://www.runatlantis.io/). The PR-comment UX is intentional; the remote-agent split is where Terraplane diverges.

## License

[MIT](LICENSE). Keep the copyright and permission notice; otherwise use it however you want.