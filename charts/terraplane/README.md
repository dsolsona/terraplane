# Terraplane Helm Chart

PR-driven Terraform automation with remote agents.

Agents usually run in a **different** network/cluster than the orchestrator (private infra vs public webhooks). Use two Helm releases of this chart, or disable the half you do not want.

Copy-pasteable values live under **[examples/](examples/)**:

| Example | Scenario |
|---------|----------|
| [orchestrator](examples/orchestrator/) | Orchestrator only (ingress + webhooks) |
| [agent](examples/agent/) | Agents only (remote orchestrator URL) |
| [combined](examples/combined/) | Orchestrator + agent in one release (lab / single cluster) |
| [multi-team](examples/multi-team/) | Orchestrator with per-agent tokens, per-repo permissions, and apply gates |

## Install

### Orchestrator release

```bash
kubectl create namespace terraplane

kubectl -n terraplane create secret generic terraplane-orchestrator \
  --from-literal=DATABASE_URL='postgres://user:pass@host:5432/terraplane?sslmode=require' \
  --from-literal=DATABASE_DRIVER=postgres \
  --from-literal=SHARED_AUTH_TOKEN='change-me' \
  --from-literal=ORCHESTRATOR_GITHUB_ACCESS_TOKEN='ghp_...' \
  --from-literal=ORCHESTRATOR_GITHUB_WEBHOOK_SECRET='...'

# Or authenticate as a GitHub App instead of a token:
#   --from-literal=ORCHESTRATOR_GITHUB_APP_ID='12345' \
#   --from-file=ORCHESTRATOR_GITHUB_APP_PRIVATE_KEY=./terraplane.private-key.pem \

helm install terraplane-orch oci://ghcr.io/terraplane-org/charts/terraplane \
  --version 0.3.0 \
  -n terraplane \
  -f examples/orchestrator/values.yaml
```

### Agents release (separate cluster/namespace)

```bash
kubectl create namespace terraplane-agents

kubectl -n terraplane-agents create secret generic terraplane-agent \
  --from-literal=SHARED_AUTH_TOKEN='change-me'

kubectl -n terraplane-agents create secret generic agent-dev-ssh \
  --from-file=ssh-private-key=./id_ed25519_dev

kubectl -n terraplane-agents create secret generic agent-prod-ssh \
  --from-file=ssh-private-key=./id_ed25519_prod

helm install terraplane-agents oci://ghcr.io/terraplane-org/charts/terraplane \
  --version 0.3.0 \
  -n terraplane-agents \
  -f examples/agent/values.yaml
```

### Combined release (same cluster)

See [examples/combined](examples/combined/). Use release name `terraplane` so agents can reach `http://terraplane-orchestrator:8080`.

Cloud secret managers (AWS Secrets Manager, GCP Secret Manager, Vault, etc.) are **out of band**: sync into Kubernetes Secrets with External Secrets Operator / CSI / your own tooling, then reference those Secret names from values.

To attach arbitrary volumes (for example a Secrets Store CSI mount that drives `secretObjects` sync) or a workload-identity ServiceAccount, use `serviceAccountName`, `extraVolumes`, and `extraVolumeMounts`. The chart does not create ServiceAccounts or cloud-specific secret resources.

```yaml
agentDefaults:
  orchestratorURL: https://orchestrator.example.com
  serviceAccountName: terraplane-agent
  envFrom:
    - secretRef:
        name: terraplane-agent
  sshKey:
    secretName: terraplane-agent-ssh
    secretKey: ssh-private-key
    mountPath: /etc/terraplane/git_ssh_key
  # Platform-provided volume so secret sync / sidecar CSI can run on this pod
  extraVolumes:
    - name: secrets-store
      csi:
        driver: secrets-store.csi.k8s.io
        readOnly: true
        volumeAttributes:
          secretProviderClass: terraplane-agent-secrets
  extraVolumeMounts:
    - name: secrets-store
      mountPath: /mnt/secrets-store
      readOnly: true
agents:
  - name: stg-apse2
```

Generic non-CSI example:

```yaml
agentDefaults:
  extraVolumes:
    - name: config
      configMap:
        name: agent-extra-config
  extraVolumeMounts:
    - name: config
      mountPath: /etc/agent-config
      readOnly: true
```

`agentDefaults.extraVolumes` / `extraVolumeMounts` are concatenated with the matching per-agent lists (same merge rule as `envFrom`). Per-agent `serviceAccountName` wins when non-empty.

## Multi-team mode

Several teams can share one orchestrator, each with its own agent and permissions. Set `orchestrator.config` to the orchestrator config (agents, repos, permissions, apply gates). The chart renders it to a ConfigMap, mounts it at `/etc/terraplane/config/orchestrator.yaml`, and sets `ORCHESTRATOR_CONFIG_PATH`. A change to the config rolls the orchestrator pods.

```yaml
orchestrator:
  envFrom:
    - secretRef:
        name: terraplane-orchestrator   # holds AGENT_TOKEN_PROD, AGENT_TOKEN_PLATFORM, ...
  config:
    agents:
      agent-prod:
        token_env: AGENT_TOKEN_PROD
    repos:
      acme/infra:
        agent: agent-prod
        permissions:
          plan: ["@acme/engineering"]
          apply: ["alice"]
        apply_requires:
          approved: true
          mergeable: true
```

Each agent release then gets its own `SHARED_AUTH_TOKEN` matching the orchestrator-side env var, for example via a per-agent Secret in `agents[].envFrom`. `SHARED_AUTH_TOKEN` is not needed on the orchestrator in this mode. The GitHub token needs `read:org` for `@org/team` entries. Full reference, including what the config does not protect against: [docs/multi-team.md](../../docs/multi-team.md).

Image tags follow release semver (`ghcr.io/terraplane-org/terraplane:<version>`). Chart `appVersion` matches the release; leave `image.tag` empty to use it. See [docs/release.md](../../docs/release.md).

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| namespaceOverride | string | `"terraplane"` | Override the release namespace. Set to `""` to use `helm -n` / Release.Namespace only. |
| image.repository | string | `"ghcr.io/terraplane-org/terraplane"` | Container image repository |
| image.tag | string | `""` | Image tag (defaults to Chart.AppVersion when empty) |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| imagePullSecrets | list | `[]` | Optional image pull secrets |
| commonLabels | object | `{}` | Extra labels applied to all resources |
| orchestrator.enabled | bool | `true` | Deploy the orchestrator. Set false for an agents-only release. |
| orchestrator.replicaCount | int | `1` | Number of orchestrator replicas. Each replica dispatches to agents connected to it. |
| orchestrator.migrate.enabled | bool | `true` | Run `terraplane db migrate` as an initContainer before the orchestrator starts |
| orchestrator.serviceAccountName | string | `""` | Existing ServiceAccount name (chart does not create it). Omit to use the namespace default. |
| orchestrator.env | object | `{}` | Extra environment variables for the orchestrator (non-secret config) |
| orchestrator.envFrom | list | `[]` | Load env from existing ConfigMaps/Secrets (chart does not create secrets) |
| orchestrator.extraVolumes | list | `[]` | Extra PodSpec volumes (pass-through; chart does not interpret volume types) |
| orchestrator.extraVolumeMounts | list | `[]` | Extra volumeMounts on the orchestrator container (pass-through) |
| orchestrator.config | object | `{}` | Orchestrator config for multi-team mode (agents, repos, permissions, apply gates). Rendered to a ConfigMap and mounted; sets `ORCHESTRATOR_CONFIG_PATH`. Empty keeps single-team behaviour. |
| orchestrator.resources | object | `{}` | Pod resources |
| orchestrator.service.type | string | `"ClusterIP"` | Kubernetes Service type |
| orchestrator.service.port | int | `8080` | Service / container port |
| orchestrator.ingress.enabled | bool | `false` | Enable Ingress for the orchestrator (webhook + health) |
| orchestrator.ingress.className | string | `""` | Ingress class name |
| orchestrator.ingress.annotations | object | `{}` | Ingress annotations |
| orchestrator.ingress.hosts | list | `[]` | Ingress hosts |
| orchestrator.ingress.tls | list | `[]` | Ingress TLS |
| agentDefaults.env | object | `{}` | Shared env for all agents (merged under per-agent env) |
| agentDefaults.envFrom | list | `[]` | Shared envFrom for all agents (concatenated with per-agent envFrom) |
| agentDefaults.orchestratorURL | string | `""` | HTTP(S) base URL agents use to reach the orchestrator (required when agents is non-empty). Example: `https://terraplane.example.com` |
| agentDefaults.serviceAccountName | string | `""` | Existing ServiceAccount for agent pods (chart does not create it). Per-agent non-empty override wins. |
| agentDefaults.extraVolumes | list | `[]` | Extra PodSpec volumes for all agents (concatenated with `agents[].extraVolumes`; pass-through) |
| agentDefaults.extraVolumeMounts | list | `[]` | Extra volumeMounts for all agents (concatenated with `agents[].extraVolumeMounts`; after work/ssh) |
| agentDefaults.workDir | string | `"/var/terraplane/work"` | Agent working directory (workspace + terraform bins) |
| agentDefaults.resources | object | `{}` | Default pod resources for agents |
| agentDefaults.scmAuth | string | `"ssh"` | How agents clone: `ssh` with the deploy key in `sshKey`, or `app` with per-job tokens minted by the orchestrator (orchestrator must authenticate as a GitHub App; no `sshKey` needed). Per-agent `scmAuth` overrides. |
| agentDefaults.sshKey.secretName | string | `""` | Existing Secret name containing the deploy key (required for private repos when `scmAuth` is `ssh`) |
| agentDefaults.sshKey.secretKey | string | `"ssh-private-key"` | Key within the Secret |
| agentDefaults.sshKey.mountPath | string | `"/etc/terraplane/git_ssh_key"` | Absolute path where the key is mounted in the container |
| agentDefaults.persistence.enabled | bool | `true` | Create a PVC per agent for plan/apply workspace reuse. false uses emptyDir |
| agentDefaults.persistence.size | string | `"20Gi"` | PVC size |
| agentDefaults.persistence.storageClassName | string | `""` | StorageClass name (empty = cluster default) |
| agentDefaults.persistence.accessMode | string | `"ReadWriteOnce"` | PVC access mode |
| agents | list | `[]` | Agent definitions. Each entry becomes a Deployment (replicas=1) with AGENT_ID = name. Optional per-agent: `serviceAccountName`, `extraVolumes`, `extraVolumeMounts`, `env`, `envFrom`, `scmAuth`, `sshKey`, `persistence`, `resources`, `workDir`. |
