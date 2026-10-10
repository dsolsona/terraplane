package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/authz"
	"github.com/xyzjace/terraplane/pkg/command"
	"github.com/xyzjace/terraplane/pkg/feedback"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
	"github.com/xyzjace/terraplane/pkg/scm"
	"github.com/xyzjace/terraplane/pkg/storage/models"
	"github.com/xyzjace/terraplane/pkg/storage/repository"
	"github.com/xyzjace/terraplane/pkg/terraplaneconfig"
)

const applyLockWorkspace = "default"

type JobService interface {
	CreatePendingJobs(ctx context.Context, webhook *scm.Webhook) error
	ClaimPendingJob(ctx context.Context, agentID string) (*command.Command, error)
	ReleaseClaim(ctx context.Context, jobID string) error
	FailClaimedJob(ctx context.Context, jobID, errMsg string) error
	ReapExpiredClaims(ctx context.Context) error
	RefreshAgentClaims(ctx context.Context, agentID string) error
	AckJob(ctx context.Context, jobID, agentID string) error
	CommitJobResult(ctx context.Context, jobID, agentID, result, output, errMsg string) error
	CleanupExpiredJobs(ctx context.Context) error
	HandlePeriodicResult(ctx context.Context, jobID, agentID, output string) error
}

type jobService struct {
	logger             log.Logger
	jobRepository      repository.JobRepository
	lockRepository     repository.LockRepository
	scmProvider        scm.Provider
	scmPublisher       scm.Publisher
	orchestratorConfig *orchestratorconfig.ConfigFile
	authorizer         authz.Authorizer
	jobLease           time.Duration
	jobCleanupInterval time.Duration
}

func NewJobService(
	logger log.Logger,
	jobRepository repository.JobRepository,
	lockRepository repository.LockRepository,
	scmProvider scm.Provider,
	scmPublisher scm.Publisher,
	config *config.Config,
	orchestratorConfig *orchestratorconfig.ConfigFile,
	authorizer authz.Authorizer,
) JobService {
	return &jobService{
		logger:             logger,
		jobRepository:      jobRepository,
		lockRepository:     lockRepository,
		scmProvider:        scmProvider,
		scmPublisher:       scmPublisher,
		orchestratorConfig: orchestratorConfig,
		authorizer:         authorizer,
		jobLease:           config.OrchestratorJobLease,
		jobCleanupInterval: config.OrchestratorJobCleanupInterval,
	}
}

func (j *jobService) CreatePendingJobs(ctx context.Context, webhook *scm.Webhook) error {
	cmd := command.ParseWebhook(webhook)
	if cmd.Kind == command.KindUnknown {
		j.logger.Debug(
			"Ignoring pull request comment that is not a terraplane command",
			"repo", webhook.RepositorySlug,
			"pr", webhook.PRNumber,
			"user", webhook.TriggeringUser,
			"comment", webhook.FullCommand,
		)
		return nil
	}

	if cmd.Kind == command.KindUnlock {
		return nil
	}

	file, err := j.scmProvider.GetFile("terraplane.yaml", webhook.CommitSHA, webhook.RepositorySlug)
	if err != nil {
		return fmt.Errorf("failed to fetch terraplane.yaml for repository %s at commit %s: %w", webhook.RepositorySlug, webhook.CommitSHA, err)
	}

	config, err := terraplaneconfig.ParseConfigFile([]byte(file))
	if err != nil {
		return fmt.Errorf("failed to parse terraplane.yaml for repository %s at commit %s: %w", webhook.RepositorySlug, webhook.CommitSHA, err)
	}
	if !j.orchestratorConfig.Enabled() {
		if err := config.ValidateAgents(); err != nil {
			return fmt.Errorf("failed to parse terraplane.yaml for repository %s at commit %s: %w", webhook.RepositorySlug, webhook.CommitSHA, err)
		}
	}

	stackNames, environmentNames, action := j.resolveStacksAndEnvironments(&cmd)

	resolvedStacks, err := config.ResolveStacks(stackNames, environmentNames)
	if err != nil {
		return fmt.Errorf("failed to resolve stacks for repository %s pull request #%d: %w", webhook.RepositorySlug, webhook.PRNumber, err)
	}

	resolvedStacks, unbound := bindStackDirs(j.logger, j.orchestratorConfig, webhook.RepositorySlug, webhook.PRNumber, resolvedStacks)

	resolvedStacks, denied, err := authorizeStacks(
		ctx, j.logger, j.authorizer,
		webhook.TriggeringUser, webhook.RepositorySlug, webhook.PRNumber,
		resolvedStacks, authz.Action(action),
	)
	if err != nil {
		return err
	}
	denied = append(unbound, denied...)
	if len(denied) > 0 {
		publishDeniedComment(ctx, j.logger, j.scmPublisher, webhook.RepositorySlug, webhook.PRNumber, authz.Action(action), denied)
	}

	for _, stack := range resolvedStacks {
		agent := j.agentFor(webhook.RepositorySlug, stack)

		if action == string(models.JobActionApply) {
			ok, err := j.acquireApplyLock(ctx, webhook, stack)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
		}

		payload := map[string]interface{}{
			"trigger_user": webhook.TriggeringUser,
			"commit_sha":   webhook.CommitSHA,
			"stack_name":   stack.Name,
			"dir":          stack.Dir,
			"tool_version": stack.ToolVersion,
		}
		if action == string(models.JobActionPlan) {
			payload["plan_flags"] = cmd.Plan.PlanFlags
		}

		job, err := j.jobRepository.UpsertPendingJob(
			ctx, webhook.RepositorySlug,
			webhook.PRNumber,
			stack.Name,
			action,
			payload,
			agent,
		)
		if err != nil {
			return fmt.Errorf("failed to upsert pending job for repository %s pull request #%d: %w", webhook.RepositorySlug, webhook.PRNumber, err)
		}

		j.logger.Debug(
			"Upserted pending job",
			"repo", webhook.RepositorySlug,
			"pr", webhook.PRNumber,
			"stack", stack.Name,
			"action", action,
			"agent", agent,
			"job_id", job.ID,
		)
	}

	return nil
}

// agentFor returns the agent that runs stack. The orchestrator config owns
// routing when present; otherwise the agent comes from terraplane.yaml.
func (j *jobService) agentFor(repo string, stack terraplaneconfig.ResolvedStack) string {
	if !j.orchestratorConfig.Enabled() {
		return stack.Agent
	}
	agent, _ := j.orchestratorConfig.AgentForStack(repo, stack.Name)
	return agent
}

func (j *jobService) acquireApplyLock(ctx context.Context, webhook *scm.Webhook, stack terraplaneconfig.ResolvedStack) (bool, error) {
	err := j.lockRepository.Create(ctx, &models.ProjectLock{
		Repo:      webhook.RepositorySlug,
		StackName: stack.Name,
		Workspace: applyLockWorkspace,
		Dir:       stack.Dir,
		CommitSHA: webhook.CommitSHA,
		LockedBy:  webhook.TriggeringUser,
		PRNumber:  int32(webhook.PRNumber),
	})
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, repository.ErrLockExists) {
		return false, fmt.Errorf("failed to create lock for stack %q in repository %s: %w", stack.Name, webhook.RepositorySlug, err)
	}

	existing, getErr := j.lockRepository.Get(ctx, webhook.RepositorySlug, stack.Name, applyLockWorkspace)
	if getErr != nil {
		return false, fmt.Errorf("failed to fetch lock for stack %q in repository %s: %w", stack.Name, webhook.RepositorySlug, getErr)
	}
	if existing != nil && existing.PRNumber == int32(webhook.PRNumber) {
		return true, nil
	}

	lockedPR := int32(0)
	lockedBy := ""
	if existing != nil {
		lockedPR = existing.PRNumber
		lockedBy = existing.LockedBy
	}
	j.logger.Warn(
		"Skipping stack because it is locked",
		"repo", webhook.RepositorySlug,
		"pr", webhook.PRNumber,
		"stack", stack.Name,
		"locked_pr", lockedPR,
		"locked_by", lockedBy,
	)
	return false, nil
}

func (j *jobService) ClaimPendingJob(ctx context.Context, agentID string) (*command.Command, error) {
	if agentID == "" {
		return nil, ErrEmptyAgentID
	}
	expires := time.Now().Add(j.jobLease)
	job, err := j.jobRepository.ClaimPendingJobForAgent(ctx, agentID, models.JobStatusClaimed, &expires)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, nil
	}

	cmd, err := j.commandFromJob(job)
	if err != nil {
		if failErr := j.jobRepository.FailClaimedJob(ctx, job.ID, err.Error()); failErr != nil {
			return nil, fmt.Errorf("%w (also failed to mark job failed: %v)", err, failErr)
		}
		return nil, err
	}
	return &cmd, nil
}

func (j *jobService) ReleaseClaim(ctx context.Context, jobID string) error {
	return j.jobRepository.ReleaseClaimedJob(ctx, jobID)
}

func (j *jobService) FailClaimedJob(ctx context.Context, jobID, errMsg string) error {
	return j.jobRepository.FailClaimedJob(ctx, jobID, errMsg)
}

func (j *jobService) ReapExpiredClaims(ctx context.Context) error {
	result, err := j.jobRepository.ReapExpiredClaims(ctx, time.Now())
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}

	for _, job := range result.RunningFailed {
		if job.Action != models.JobActionApply {
			continue
		}
		if err := j.releaseApplyLock(ctx, job, job.ID); err != nil {
			j.logger.Error(
				"Failed to release lock for reaped running apply job",
				"job_id", job.ID,
				"repo", job.Repo,
				"stack", job.StackName,
				"error", err,
			)
		}
	}

	n := result.ClaimedReturned + len(result.RunningFailed)
	if n > 0 {
		j.logger.Info(
			"Reaped expired job claims",
			"count", n,
			"claimed_returned", result.ClaimedReturned,
			"running_failed", len(result.RunningFailed),
		)
	}
	return nil
}

func (j *jobService) RefreshAgentClaims(ctx context.Context, agentID string) error {
	expires := time.Now().Add(j.jobLease)
	return j.jobRepository.RefreshAgentClaims(ctx, agentID, &expires)
}

func (j *jobService) AckJob(ctx context.Context, jobID, agentID string) error {
	job, err := j.jobRepository.Get(ctx, jobID)
	if err != nil {
		return fmt.Errorf("failed to fetch job %s: %w", jobID, err)
	}
	if err := validateJobAgent(job, agentID); err != nil {
		return err
	}
	if job.Status != models.JobStatusClaimed {
		return fmt.Errorf("%w: job %s is %s, expected claimed", ErrJobInvalidStatus, jobID, job.Status)
	}

	job.Status = models.JobStatusRunning
	if err := j.jobRepository.Update(ctx, job); err != nil {
		return fmt.Errorf("failed to update job %s status to running: %w", jobID, err)
	}
	return nil
}

func (j *jobService) CommitJobResult(ctx context.Context, jobID, agentID, result, output, errMsg string) error {
	job, err := j.jobRepository.Get(ctx, jobID)
	if err != nil {
		return fmt.Errorf("failed to fetch job %s: %w", jobID, err)
	}
	if err := validateJobAgent(job, agentID); err != nil {
		return err
	}
	if job.Status != models.JobStatusRunning {
		return fmt.Errorf("%w: job %s is %s, expected running", ErrJobInvalidStatus, jobID, job.Status)
	}

	success := result == "success"
	if success {
		job.Status = models.JobStatusSucceeded
	} else {
		job.Status = models.JobStatusFailed
	}
	job.Output = output
	job.ErrorMsg = errMsg

	if err := j.jobRepository.Update(ctx, job); err != nil {
		if job.Action == models.JobActionApply {
			if releaseErr := j.releaseApplyLock(ctx, job, jobID); releaseErr != nil {
				return fmt.Errorf(
					"failed to update job %s with result: %w (also failed to release lock: %v)",
					jobID, err, releaseErr,
				)
			}
		}
		return fmt.Errorf("failed to update job %s with result: %w", jobID, err)
	}

	if job.Action == models.JobActionApply {
		if err := j.releaseApplyLock(ctx, job, jobID); err != nil {
			return err
		}
	}

	comment := feedback.JobResultComment(job, job.Status, output, errMsg)
	j.publishResultComment(ctx, job, comment)
	return nil
}

func (j *jobService) CleanupExpiredJobs(ctx context.Context) error {
	if j.jobCleanupInterval <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-j.jobCleanupInterval)
	n, err := j.jobRepository.CleanupExpiredJobs(ctx, cutoff)
	if err != nil {
		return err
	}
	if n > 0 {
		j.logger.Debug("Cleaned up expired jobs", "count", n, "cutoff", cutoff)
	}
	return nil
}

func (j *jobService) HandlePeriodicResult(ctx context.Context, jobID, agentID, output string) error {
	job, err := j.jobRepository.Get(ctx, jobID)
	if err != nil {
		return fmt.Errorf("failed to fetch job %s: %w", jobID, err)
	}
	if err := validateJobAgent(job, agentID); err != nil {
		return err
	}
	if job.Status != models.JobStatusRunning {
		return fmt.Errorf("%w: job %s is %s, expected running", ErrJobInvalidStatus, jobID, job.Status)
	}

	comment := feedback.JobResultComment(job, job.Status, output, "")
	j.publishResultComment(ctx, job, comment)
	return nil
}

// publishResultComment updates the existing PR comment when payload has comment_id,
// otherwise creates one and stores the ID for later updates. Failures are logged.
func (j *jobService) publishResultComment(ctx context.Context, job *models.Job, body string) {
	payload, err := unmarshalJobPayload(job.Payload)
	if err != nil {
		j.logger.Error(
			"Failed to unmarshal job payload for comment",
			"job_id", job.ID,
			"error", err,
		)
		return
	}

	if commentID := payloadString(payload, "comment_id"); commentID != "" {
		id, err := strconv.Atoi(commentID)
		if err != nil {
			j.logger.Error(
				"Failed to parse stored comment ID",
				"job_id", job.ID,
				"comment_id", commentID,
				"error", err,
			)
			return
		}
		if err := j.scmPublisher.UpdateComment(ctx, job.Repo, int(job.PRNumber), body, id); err != nil {
			j.logger.Error(
				"Failed to update job result comment",
				"job_id", job.ID,
				"repo", job.Repo,
				"pr", job.PRNumber,
				"stack", job.StackName,
				"error", err,
			)
		}
		return
	}

	id, err := j.scmPublisher.WriteComment(ctx, job.Repo, int(job.PRNumber), body)
	if err != nil {
		j.logger.Error(
			"Failed to write job result comment",
			"job_id", job.ID,
			"repo", job.Repo,
			"pr", job.PRNumber,
			"stack", job.StackName,
			"error", err,
		)
		return
	}

	payload["comment_id"] = strconv.Itoa(id)
	if err := j.jobRepository.UpdatePayload(ctx, job, payload); err != nil {
		j.logger.Error(
			"Failed to update job payload with comment ID",
			"job_id", job.ID,
			"error", err,
		)
	}
}

func (j *jobService) releaseApplyLock(ctx context.Context, job *models.Job, jobID string) error {
	if err := j.lockRepository.Delete(ctx, job.Repo, job.StackName, applyLockWorkspace); err != nil {
		return fmt.Errorf("failed to release lock for job %s stack %q: %w", jobID, job.StackName, err)
	}
	j.logger.Debug("Released lock for job", "job_id", jobID, "repo", job.Repo, "stack", job.StackName)
	return nil
}

func (j *jobService) commandFromJob(job *models.Job) (command.Command, error) {
	payload, err := unmarshalJobPayload(job.Payload)
	if err != nil {
		return command.Command{}, fmt.Errorf("invalid payload for job %s: %w", job.ID, err)
	}

	stacks := []string{job.StackName}
	triggerUser := payloadString(payload, "trigger_user")
	toolVersion := payloadString(payload, "tool_version")

	switch job.Action {
	case models.JobActionPlan:
		plan := command.PlanCommand{
			Stacks:    stacks,
			PlanFlags: payloadString(payload, "plan_flags"),
		}
		plan.Repo = job.Repo
		plan.PRNumber = int(job.PRNumber)
		plan.CommitSHA = job.CommitSHA
		plan.TriggerUser = triggerUser
		plan.Agent = job.AgentID
		plan.JobID = job.ID
		plan.Dir = job.Dir
		plan.ToolVersion = toolVersion
		return command.Command{Kind: command.KindPlan, Plan: plan}, nil
	case models.JobActionApply:
		apply := command.ApplyCommand{Stacks: stacks}
		apply.Repo = job.Repo
		apply.PRNumber = int(job.PRNumber)
		apply.CommitSHA = job.CommitSHA
		apply.TriggerUser = triggerUser
		apply.Agent = job.AgentID
		apply.JobID = job.ID
		apply.Dir = job.Dir
		apply.ToolVersion = toolVersion
		return command.Command{Kind: command.KindApply, Apply: apply}, nil
	default:
		return command.Command{}, fmt.Errorf("unknown job action: %s", job.Action)
	}
}

func unmarshalJobPayload(raw string) (map[string]any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func payloadString(payload map[string]any, key string) string {
	v, ok := payload[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func (j *jobService) resolveStacksAndEnvironments(cmd *command.Command) ([]string, []string, string) {
	// TODO: This is awful. We should find a more elegant way to do this
	stacks := []string{}
	environments := []string{}
	action := ""
	if cmd.Kind == command.KindPlan {
		stacks = cmd.Plan.Stacks
		environments = cmd.Plan.Environments
		action = string(models.JobActionPlan)
	}
	if cmd.Kind == command.KindApply {
		stacks = cmd.Apply.Stacks
		environments = cmd.Apply.Environments
		action = string(models.JobActionApply)
	}
	return stacks, environments, action
}

func validateJobAgent(job *models.Job, agentID string) error {
	if agentID == "" || job.AgentID != agentID {
		return fmt.Errorf("%w: job %s belongs to agent %q, not %q", ErrJobWrongAgent, job.ID, job.AgentID, agentID)
	}
	return nil
}
