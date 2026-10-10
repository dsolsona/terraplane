package handlers

import (
	"context"

	"github.com/xyzjace/terraplane/internal/process"
	"github.com/xyzjace/terraplane/pkg/command"
)

func (h *Handlers) handlePlan(ctx context.Context, cmd *command.PlanCommand) {
	jobID := cmd.JobID
	h.logger.Info(
		"Running terraplane plan",
		"job_id", jobID,
		"repo", cmd.Repo,
		"pr", cmd.PRNumber,
		"commit", cmd.CommitSHA,
		"tool", cmd.Tool,
		"plan_flags", cmd.PlanFlags,
	)

	workspaceDir, err := h.workspaceManager.ProvisionWorkspace(ctx, cmd.Repo, cmd.CommitSHA, cmd.Stacks[0])
	if err != nil {
		h.logger.Error(
			"Failed to provision workspace for terraplane plan",
			"repo", cmd.Repo,
			"pr", cmd.PRNumber,
			"error", err,
		)
		if writeErr := h.orchestratorClient.SubmitResult(ctx, jobID, h.agentID, false, "", err.Error()); writeErr != nil {
			h.logger.Error(
				"Failed to submit plan result to orchestrator",
				"job_id", jobID,
				"agent_id", h.agentID,
				"error", writeErr,
			)
		}
		return
	}

	defer func() {
		if err != nil {
			if removeErr := h.workspaceManager.RemoveWorkspace(ctx, workspaceDir); removeErr != nil {
				h.logger.Error(
					"Failed to remove workspace after plan failure",
					"repo", cmd.Repo,
					"pr", cmd.PRNumber,
					"error", removeErr,
				)
			}
		}
	}()

	out := &process.Buffer{}
	stop := h.watchOutput(ctx, jobID, out)
	defer stop()

	output, err := h.terraformManager.RunPlan(ctx, workspaceDir, cmd.Stacks[0], cmd.Tool, cmd.ToolVersion, cmd.PlanFlags, out)
	if err != nil {
		h.logger.Error(
			"Failed to run terraform plan",
			"repo", cmd.Repo,
			"pr", cmd.PRNumber,
			"error", err,
		)
		if writeErr := h.orchestratorClient.SubmitResult(ctx, jobID, h.agentID, false, output, err.Error()); writeErr != nil {
			h.logger.Error(
				"Failed to submit plan result to orchestrator",
				"job_id", jobID,
				"agent_id", h.agentID,
				"error", writeErr,
			)
		}
		return
	}

	if writeErr := h.orchestratorClient.SubmitResult(ctx, jobID, h.agentID, true, output, ""); writeErr != nil {
		err = writeErr
		h.logger.Error(
			"Failed to send plan result to orchestrator",
			"job_id", jobID,
			"repo", cmd.Repo,
			"error", writeErr,
		)
	}
}
