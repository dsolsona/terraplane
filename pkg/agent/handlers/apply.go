package handlers

import (
	"context"

	"github.com/xyzjace/terraplane/internal/process"
	"github.com/xyzjace/terraplane/pkg/command"
)

func (h *Handlers) handleApply(ctx context.Context, cmd *command.ApplyCommand) {
	jobID := cmd.JobID
	h.logger.Info(
		"Running terraplane apply",
		"job_id", jobID,
		"repo", cmd.Repo,
		"pr", cmd.PRNumber,
		"commit", cmd.CommitSHA,
		"tool", cmd.Tool,
	)

	workspaceDir, err := h.workspaceManager.FetchWorkspace(ctx, cmd.Repo, cmd.CommitSHA, cmd.Stacks[0])
	if err != nil {
		h.logger.Error(
			"Failed to fetch workspace for terraplane apply",
			"repo", cmd.Repo,
			"pr", cmd.PRNumber,
			"error", err,
		)
		if writeErr := h.orchestratorClient.SubmitResult(ctx, jobID, h.agentID, false, "", err.Error()); writeErr != nil {
			h.logger.Error(
				"Failed to submit apply result to orchestrator",
				"job_id", jobID,
				"agent_id", h.agentID,
				"error", writeErr,
			)
		}
		return
	}

	defer func() {
		if removeErr := h.workspaceManager.RemoveWorkspace(ctx, workspaceDir); removeErr != nil {
			h.logger.Error(
				"Failed to remove workspace after apply",
				"repo", cmd.Repo,
				"pr", cmd.PRNumber,
				"error", removeErr,
			)
		}
	}()

	out := &process.Buffer{}
	stop := h.watchOutput(ctx, jobID, out)
	defer stop()

	output, err := h.terraformManager.RunApply(ctx, workspaceDir, cmd.Stacks[0], cmd.Tool, cmd.ToolVersion, out)
	if err != nil {
		h.logger.Error(
			"Failed to run terraform apply",
			"repo", cmd.Repo,
			"pr", cmd.PRNumber,
			"error", err,
		)
		if writeErr := h.orchestratorClient.SubmitResult(ctx, jobID, h.agentID, false, output, err.Error()); writeErr != nil {
			h.logger.Error(
				"Failed to submit apply result to orchestrator",
				"job_id", jobID,
				"agent_id", h.agentID,
				"error", writeErr,
			)
		}
		return
	}

	if writeErr := h.orchestratorClient.SubmitResult(ctx, jobID, h.agentID, true, output, ""); writeErr != nil {
		h.logger.Error(
			"Failed to submit apply result to orchestrator",
			"job_id", jobID,
			"repo", cmd.Repo,
			"error", writeErr,
		)
	}
}
