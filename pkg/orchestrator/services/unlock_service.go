package services

import (
	"context"
	"fmt"

	"github.com/xyzjace/terraplane/pkg/authz"
	"github.com/xyzjace/terraplane/pkg/command"
	"github.com/xyzjace/terraplane/pkg/feedback"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/scm"
	"github.com/xyzjace/terraplane/pkg/storage/models"
	"github.com/xyzjace/terraplane/pkg/storage/repository"
	"github.com/xyzjace/terraplane/pkg/terraplaneconfig"
)

type UnlockService interface {
	RunUnlock(ctx context.Context, unlock command.UnlockCommand) error
}

type unlockService struct {
	logger       log.Logger
	scmProvider  scm.Provider
	scmPublisher scm.Publisher
	jobs         repository.JobRepository
	locks        repository.LockRepository
	authorizer   authz.Authorizer
}

func NewUnlockService(
	logger log.Logger,
	scmProvider scm.Provider,
	scmPublisher scm.Publisher,
	jobs repository.JobRepository,
	locks repository.LockRepository,
	authorizer authz.Authorizer,
) UnlockService {
	return &unlockService{
		logger:       logger,
		scmProvider:  scmProvider,
		scmPublisher: scmPublisher,
		jobs:         jobs,
		locks:        locks,
		authorizer:   authorizer,
	}
}

func (s *unlockService) RunUnlock(ctx context.Context, unlock command.UnlockCommand) error {
	s.logger.Info(
		"Starting terraplane unlock",
		"repo", unlock.Repo,
		"pr", unlock.PRNumber,
		"user", unlock.TriggerUser,
		"commit", unlock.CommitSHA,
		"stacks", unlock.Stacks,
		"environments", unlock.Environments,
	)

	if len(unlock.Stacks) == 0 && len(unlock.Environments) == 0 {
		err := fmt.Errorf("unlock requires at least one stack (-s) or environment (-e)")
		s.publishUnlockFailure(ctx, unlock, "", err)
		return err
	}

	file, err := s.scmProvider.GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo)
	if err != nil {
		err = fmt.Errorf("failed to fetch terraplane.yaml for repository %s at commit %s: %w", unlock.Repo, unlock.CommitSHA, err)
		s.publishUnlockFailure(ctx, unlock, "", err)
		return err
	}

	config, err := terraplaneconfig.ParseConfigFile([]byte(file))
	if err != nil {
		err = fmt.Errorf("failed to parse terraplane.yaml for repository %s at commit %s: %w", unlock.Repo, unlock.CommitSHA, err)
		s.publishUnlockFailure(ctx, unlock, "", err)
		return err
	}

	stacks, err := config.ResolveStacks(unlock.Stacks, unlock.Environments)
	if err != nil {
		err = fmt.Errorf("failed to resolve stacks for repository %s pull request #%d: %w", unlock.Repo, unlock.PRNumber, err)
		s.publishUnlockFailure(ctx, unlock, "", err)
		return err
	}

	stacks, denied, err := authorizeStacks(
		ctx, s.logger, s.authorizer,
		unlock.TriggerUser, unlock.Repo, unlock.PRNumber,
		stacks, authz.ActionUnlock,
	)
	if err != nil {
		s.publishUnlockFailure(ctx, unlock, "", err)
		return err
	}
	if len(denied) > 0 {
		publishDeniedComment(ctx, s.logger, s.scmPublisher, unlock.Repo, unlock.PRNumber, authz.ActionUnlock, denied)
	}
	if len(stacks) == 0 {
		return nil
	}

	stackNames := make([]string, len(stacks))
	for i, stack := range stacks {
		stackNames[i] = stack.Name
	}

	s.logger.Info(
		"Resolved terraplane stacks for unlock",
		"repo", unlock.Repo,
		"pr", unlock.PRNumber,
		"stack_count", len(stacks),
		"stacks", stackNames,
	)

	// Locks are keyed by repo + stack + workspace, not PR — release the resolved stacks in this repo.
	deletedLocks, err := s.locks.DeleteByRepoAndStacks(ctx, unlock.Repo, stackNames)
	if err != nil {
		err = fmt.Errorf(
			"failed to delete project locks for repository %s stacks %v: %w",
			unlock.Repo, stackNames, err,
		)
		for _, name := range stackNames {
			s.publishUnlockFailure(ctx, unlock, name, err)
		}
		return err
	}

	deletedJobs, err := s.jobs.DeleteByRepoPRAndStacks(ctx, unlock.Repo, unlock.PRNumber, stackNames)
	if err != nil {
		err = fmt.Errorf(
			"failed to delete jobs for repository %s pull request #%d stacks %v: %w",
			unlock.Repo, unlock.PRNumber, stackNames, err,
		)
		for _, name := range stackNames {
			s.publishUnlockFailure(ctx, unlock, name, err)
		}
		return err
	}

	for _, name := range stackNames {
		s.publishUnlockSuccess(ctx, unlock, name)
	}

	s.logger.Info(
		"Finished terraplane unlock",
		"repo", unlock.Repo,
		"pr", unlock.PRNumber,
		"resolved_stack_count", len(stacks),
		"deleted_lock_count", deletedLocks,
		"deleted_job_count", deletedJobs,
	)

	return nil
}

func (s *unlockService) publishUnlockSuccess(ctx context.Context, unlock command.UnlockCommand, stackName string) {
	comment := feedback.UnlockResultComment(stackName, models.JobStatusSucceeded, "")
	// TODO: We should handle retries here
	if _, err := s.scmPublisher.WriteComment(ctx, unlock.Repo, unlock.PRNumber, comment); err != nil {
		s.logger.Error(
			"Failed to write unlock result comment",
			"repo", unlock.Repo,
			"pr", unlock.PRNumber,
			"stack", stackName,
			"error", err,
		)
	}
}

func (s *unlockService) publishUnlockFailure(ctx context.Context, unlock command.UnlockCommand, stackName string, unlockErr error) {
	comment := feedback.UnlockResultComment(stackName, models.JobStatusFailed, unlockErr.Error())
	if _, err := s.scmPublisher.WriteComment(ctx, unlock.Repo, unlock.PRNumber, comment); err != nil {
		s.logger.Error(
			"Failed to write unlock failure comment",
			"repo", unlock.Repo,
			"pr", unlock.PRNumber,
			"stack", stackName,
			"error", err,
		)
	}
}
