# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `ORCHESTRATOR_CONFIG_PATH` points at an optional orchestrator config file (`agents` and `repos`). When set, each agent is identified by its own bearer token (`agents.<name>.token_env`) instead of `SHARED_AUTH_TOKEN`, and an agent request whose `agent_id` does not match its token is rejected with 403. Without the file nothing changes.

## [0.4.0] - 2026-09-17

### Added

- Agents periodically snapshot terraform plan/apply output and send it to the orchestrator (`POST /agent/jobs/{id}/periodic_result`), which creates or updates a single PR progress comment for the job (including the final result)
- `AGENT_PERIODIC_COMMENT_INTERVAL` (default `15s`) controls how often agents submit those snapshots
- Reaper periodically deletes terminal jobs older than `ORCHESTRATOR_JOB_CLEANUP_INTERVAL` (default `720h`), polled every `ORCHESTRATOR_JOB_CLEANUP_POLL_INTERVAL` (default `1h`)

### Changed

- Plan/apply PR comments accept a job status (not just success/failure); in-progress comments show `running` with expanded live output, final comments stay collapsed

## [0.3.1] - 2026-08-26

### Added

- Stack `tool_version` in `terraplane.yaml`
- `SCM_PROVIDER` ENV var to configure the chosen SCM provider at runtime

### Changed

- Comment parsing now prevents invalid positional arguments
- Comment long flags are `--stack` / `--env` (short `-s` / `-e` unchanged); single-dash `-stack` / `-env` are ignored
- Terraform plan flags in comments must come after `--` (for example `terraplane plan -s app -- -target=module.x`)
- `terraplane unlock` requires at least one `-s` / `--stack` or `-e` / `--env` (plain `terraplane unlock` is ignored); the orchestrator drops Terraplane apply locks and jobs for the resolved stacks and does not send work to agents

## [0.3.0] - 2026-08-19

### Changed

- Agents now communicate with the orchestrator exclusively over HTTP — WebSocket dispatch has been removed
- Agent poll loop: agents claim jobs, send heartbeats, and submit results via HTTP endpoints (`POST /agent/jobs/claim`, `/heartbeat`, `/ack`, `/result`)
- Orchestrator dispatcher refactored into a pure reaper — it no longer claims or dispatches jobs, only reaps expired claims on a timer
- `AGENT_ORCHESTRATOR_URL` replaces `AGENT_ORCHESTRATOR_WS_URL`; value is an HTTP(S) base URL (e.g. `http://orchestrator:8080`)

### Removed

- WebSocket endpoint (`GET /ws`) and all associated agent session infrastructure (`pkg/agentsession`, `pkg/wsproto`)
- Protobuf envelope definitions (`proto/`, `pkg/terraplane/v1/`) and `protoc-gen` Makefile target
- `ORCHESTRATOR_AGENT_PING_INTERVAL`, `ORCHESTRATOR_AGENT_PONG_TIMEOUT`, `ORCHESTRATOR_AGENT_MISSED_HEARTBEATS` config fields


## [0.2.0] - 2026-08-18

### Changed
- Orchestrators are now able to be "HA". They periodically poll the DB for enqueued jobs for connected agents and dispatch them. As long as an agent is connected to an orchestrator it will receive commands.


## [0.1.4] - 2026-08-10

### Changed
- `terraplane.yaml` is now environment-scoped: stacks live under `environments`, agents can be set per environment (optional stack override), and stack names must be globally unique
- Comment commands accept `-e` / `-env` to plan, apply, or unlock all stacks in an environment (combine with `-s` for intersection)
- PR result comments: terraplane-branded layout with stack header, dir/commit meta, add/change/destroy table, and collapsed output (less Atlantis-like)
- Valid comments will receive a reaction from Terraplane to acknowledge receipt
- Docker CI/release builds use Buildx GitHub Actions cache; CI no longer uses `load: true`
- Dockerfile fetches Atlas before copying sources so code changes do not re-download it

## [0.1.3] - 2026-08-08

### Added
- GitHub Actions release workflow: semver tags publish GHCR images, Helm OCI charts, and binaries
- MIT license
- Release cutting docs (`docs/release.md`)

### Changed
- CI no longer pushes images/charts on every `main` commit; releases are tag-driven
- Default container/Helm registry moved to `ghcr.io/terraplane-org`
