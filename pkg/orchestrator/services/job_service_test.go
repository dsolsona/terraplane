package services_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/mock/gomock"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/authz"
	"github.com/xyzjace/terraplane/pkg/authz/mock_authz"
	"github.com/xyzjace/terraplane/pkg/command"
	"github.com/xyzjace/terraplane/pkg/feedback"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestrator/services"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
	"github.com/xyzjace/terraplane/pkg/scm"
	"github.com/xyzjace/terraplane/pkg/scm/mock_scm"
	"github.com/xyzjace/terraplane/pkg/storage/models"
	"github.com/xyzjace/terraplane/pkg/storage/repository"
	"github.com/xyzjace/terraplane/pkg/storage/repository/mock_repository"
)

type JobServiceSuite struct {
	suite.Suite
	ctrl      *gomock.Controller
	scm       *mock_scm.MockProvider
	publisher *mock_scm.MockPublisher
	jobs      *mock_repository.MockJobRepository
	locks     *mock_repository.MockLockRepository
	authz     *mock_authz.MockAuthorizer
	svc       services.JobService
}

func TestJobServiceSuite(t *testing.T) {
	suite.Run(t, new(JobServiceSuite))
}

func (s *JobServiceSuite) SetupTest() {
	s.ctrl = gomock.NewController(s.T())
	s.scm = mock_scm.NewMockProvider(s.ctrl)
	s.publisher = mock_scm.NewMockPublisher(s.ctrl)
	s.jobs = mock_repository.NewMockJobRepository(s.ctrl)
	s.locks = mock_repository.NewMockLockRepository(s.ctrl)
	s.authz = mock_authz.NewMockAuthorizer(s.ctrl)
	s.svc = services.NewJobService(log.Noop(), s.jobs, s.locks, s.scm, s.publisher, &config.Config{
		OrchestratorJobLease:           time.Minute,
		OrchestratorJobCleanupInterval: 30 * 24 * time.Hour,
	}, &orchestratorconfig.ConfigFile{}, authz.AllowAll())
}

func webhook(comment string) *scm.Webhook {
	return &scm.Webhook{
		RepositorySlug: "acme/infra",
		PRNumber:       42,
		FullCommand:    comment,
		TriggeringUser: "jace",
		CommitSHA:      "abc123",
	}
}

func (s *JobServiceSuite) expectUpsert(stack, dir, action, agent, jobID string) {
	payload := map[string]interface{}{
		"trigger_user": "jace",
		"commit_sha":   "abc123",
		"stack_name":   stack,
		"dir":          dir,
		"tool":         "",
		"tool_version": "",
	}
	if action == string(models.JobActionPlan) {
		payload["plan_flags"] = ""
	}
	s.jobs.EXPECT().UpsertPendingJob(
		gomock.Any(),
		"acme/infra",
		42,
		stack,
		action,
		payload,
		agent,
	).Return(&models.Job{ID: jobID}, nil)
}

func (s *JobServiceSuite) expectLockCreate(stack, dir string, err error) {
	s.locks.EXPECT().Create(gomock.Any(), gomock.AssignableToTypeOf(&models.ProjectLock{})).DoAndReturn(
		func(_ context.Context, lock *models.ProjectLock) error {
			require.Equal(s.T(), "acme/infra", lock.Repo)
			require.Equal(s.T(), stack, lock.StackName)
			require.Equal(s.T(), "default", lock.Workspace)
			require.Equal(s.T(), dir, lock.Dir)
			require.Equal(s.T(), "abc123", lock.CommitSHA)
			require.Equal(s.T(), "jace", lock.LockedBy)
			require.Equal(s.T(), int32(42), lock.PRNumber)
			return err
		},
	)
}

func (s *JobServiceSuite) TestIgnoresUnknownCommands() {
	err := s.svc.CreatePendingJobs(context.Background(), webhook("not a terraplane command"))
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestFetchConfigFailure() {
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return("", errors.New("404"))

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to fetch terraplane.yaml")
}

func (s *JobServiceSuite) TestParseConfigFailure() {
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return("stacks: [", nil)

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to parse terraplane.yaml")
}

func (s *JobServiceSuite) TestResolveStacksFailure() {
	wh := webhook("terraplane plan -s missing")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to resolve stacks")
}

func (s *JobServiceSuite) TestPlanUpsertsAllStacks() {
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectUpsert("a", "stacks/a", "plan", "agent-a", "job-a")
	s.expectUpsert("b", "stacks/b", "plan", "agent-b", "job-b")

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestPlanNamedStack() {
	wh := webhook("terraplane plan -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectUpsert("a", "stacks/a", "plan", "agent-a", "job-a")

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestPlanEnvironmentFlag() {
	wh := webhook("terraplane plan -e staging")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoEnvYAML, nil)
	s.expectUpsert("a", "stacks/a", "plan", "agent-a", "job-a")
	s.expectUpsert("b", "stacks/b", "plan", "agent-b", "job-b")

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestPlanPersistsPlanFlags() {
	wh := webhook("terraplane plan -s a -- -target=module.vpc")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.jobs.EXPECT().UpsertPendingJob(
		gomock.Any(),
		"acme/infra",
		42,
		"a",
		"plan",
		map[string]interface{}{
			"trigger_user": "jace",
			"commit_sha":   "abc123",
			"stack_name":   "a",
			"dir":          "stacks/a",
			"tool":         "",
			"tool_version": "",
			"plan_flags":   "-target=module.vpc",
		},
		"agent-a",
	).Return(&models.Job{ID: "job-a"}, nil)

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestPlanPersistsToolVersion() {
	wh := webhook("terraplane plan -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(`
environments:
  - name: default
    stacks:
      - name: a
        agent: agent-a
        dir: stacks/a
        tool_version: "1.9.0"
`, nil)
	s.jobs.EXPECT().UpsertPendingJob(
		gomock.Any(),
		"acme/infra",
		42,
		"a",
		"plan",
		map[string]interface{}{
			"trigger_user": "jace",
			"commit_sha":   "abc123",
			"stack_name":   "a",
			"dir":          "stacks/a",
			"tool":         "",
			"tool_version": "1.9.0",
			"plan_flags":   "",
		},
		"agent-a",
	).Return(&models.Job{ID: "job-a"}, nil)

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyCreatesLockThenUpserts() {
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectLockCreate("a", "stacks/a", nil)
	s.expectUpsert("a", "stacks/a", "apply", "agent-a", "job-a")

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplySupersedesWhenLockedBySamePR() {
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectLockCreate("a", "stacks/a", repository.ErrLockExists)
	s.locks.EXPECT().Get(gomock.Any(), "acme/infra", "a", "default").Return(&models.ProjectLock{
		PRNumber: 42,
		LockedBy: "jace",
	}, nil)
	s.expectUpsert("a", "stacks/a", "apply", "agent-a", "job-a")

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplySkipsWhenLockedByOtherPR() {
	wh := webhook("terraplane apply")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectLockCreate("a", "stacks/a", repository.ErrLockExists)
	s.locks.EXPECT().Get(gomock.Any(), "acme/infra", "a", "default").Return(&models.ProjectLock{
		PRNumber: 7,
		LockedBy: "other",
	}, nil)
	s.expectLockCreate("b", "stacks/b", nil)
	s.expectUpsert("b", "stacks/b", "apply", "agent-b", "job-b")

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplySkipsWhenLockExistsAndGetReturnsNil() {
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectLockCreate("a", "stacks/a", repository.ErrLockExists)
	s.locks.EXPECT().Get(gomock.Any(), "acme/infra", "a", "default").Return(nil, nil)

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyLockCreateFailure() {
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectLockCreate("a", "stacks/a", errors.New("db"))

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to create lock")
}

func (s *JobServiceSuite) TestApplyLockGetFailure() {
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectLockCreate("a", "stacks/a", repository.ErrLockExists)
	s.locks.EXPECT().Get(gomock.Any(), "acme/infra", "a", "default").Return(nil, errors.New("db"))

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to fetch lock")
}

func (s *JobServiceSuite) TestUnlockDoesNotEnqueueJobs() {
	wh := webhook("terraplane unlock -s a")
	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestUpsertFailureAbortsRemainingStacks() {
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.jobs.EXPECT().UpsertPendingJob(
		gomock.Any(), "acme/infra", 42, "a", "plan", gomock.Any(), "agent-a",
	).Return(nil, errors.New("db down"))

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to upsert pending job")
}

func claimedJob(action models.JobAction, payload string) *models.Job {
	return &models.Job{
		ID:        "job-1",
		Repo:      "acme/infra",
		PRNumber:  42,
		StackName: "a",
		Dir:       "stacks/a",
		CommitSHA: "abc123",
		AgentID:   "agent-a",
		Action:    action,
		Payload:   payload,
	}
}

func (s *JobServiceSuite) expectClaim(job *models.Job, err error) {
	s.jobs.EXPECT().ClaimPendingJobForAgent(
		gomock.Any(),
		"agent-a",
		models.JobStatusClaimed,
		gomock.Any(),
	).Return(job, err)
}

func (s *JobServiceSuite) requireLeaseAboutOneMinute(lease *time.Time, from time.Time) {
	s.T().Helper()
	require.NotNil(s.T(), lease)
	require.WithinDuration(s.T(), from.Add(time.Minute), *lease, 2*time.Second)
}

func (s *JobServiceSuite) TestClaimPendingJobEmptyAgent() {
	cmd, err := s.svc.ClaimPendingJob(context.Background(), "")
	require.ErrorIs(s.T(), err, services.ErrEmptyAgentID)
	require.Nil(s.T(), cmd)
}

func (s *JobServiceSuite) TestClaimPendingJobNone() {
	s.expectClaim(nil, nil)

	cmd, err := s.svc.ClaimPendingJob(context.Background(), "agent-a")
	require.NoError(s.T(), err)
	require.Nil(s.T(), cmd)
}

func (s *JobServiceSuite) TestClaimPendingJobRepositoryError() {
	s.expectClaim(nil, errors.New("db down"))

	cmd, err := s.svc.ClaimPendingJob(context.Background(), "agent-a")
	require.Error(s.T(), err)
	require.Nil(s.T(), cmd)
}

func (s *JobServiceSuite) TestClaimPendingJobPlan() {
	s.expectClaim(claimedJob(models.JobActionPlan, `{"trigger_user":"jace","plan_flags":"-target=x","tool":"tofu","tool_version":"1.9.0"}`), nil)

	cmd, err := s.svc.ClaimPendingJob(context.Background(), "agent-a")
	require.NoError(s.T(), err)
	require.Equal(s.T(), command.KindPlan, cmd.Kind)
	require.Equal(s.T(), "acme/infra", cmd.Plan.Repo)
	require.Equal(s.T(), 42, cmd.Plan.PRNumber)
	require.Equal(s.T(), "abc123", cmd.Plan.CommitSHA)
	require.Equal(s.T(), "jace", cmd.Plan.TriggerUser)
	require.Equal(s.T(), "agent-a", cmd.Plan.Agent)
	require.Equal(s.T(), "job-1", cmd.Plan.JobID)
	require.Equal(s.T(), "stacks/a", cmd.Plan.Dir)
	require.Equal(s.T(), "tofu", cmd.Plan.Tool)
	require.Equal(s.T(), "1.9.0", cmd.Plan.ToolVersion)
	require.Equal(s.T(), []string{"a"}, cmd.Plan.Stacks)
	require.Equal(s.T(), "-target=x", cmd.Plan.PlanFlags)
}

func (s *JobServiceSuite) TestClaimPendingJobApply() {
	s.expectClaim(claimedJob(models.JobActionApply, `{"trigger_user":"jace"}`), nil)

	cmd, err := s.svc.ClaimPendingJob(context.Background(), "agent-a")
	require.NoError(s.T(), err)
	require.Equal(s.T(), command.KindApply, cmd.Kind)
	require.Equal(s.T(), "agent-a", cmd.Apply.Agent)
	require.Equal(s.T(), "stacks/a", cmd.Apply.Dir)
	require.Equal(s.T(), []string{"a"}, cmd.Apply.Stacks)
}

func (s *JobServiceSuite) TestClaimPendingJobEmptyPayload() {
	s.expectClaim(claimedJob(models.JobActionPlan, ""), nil)

	cmd, err := s.svc.ClaimPendingJob(context.Background(), "agent-a")
	require.NoError(s.T(), err)
	require.Equal(s.T(), "", cmd.Plan.TriggerUser)
	require.Equal(s.T(), "", cmd.Plan.PlanFlags)
}

func (s *JobServiceSuite) TestClaimPendingJobPayloadNonStringFields() {
	s.expectClaim(claimedJob(models.JobActionPlan, `{"trigger_user":1,"plan_flags":null}`), nil)

	cmd, err := s.svc.ClaimPendingJob(context.Background(), "agent-a")
	require.NoError(s.T(), err)
	require.Equal(s.T(), "", cmd.Plan.TriggerUser)
	require.Equal(s.T(), "", cmd.Plan.PlanFlags)
}

func (s *JobServiceSuite) TestClaimPendingJobInvalidPayloadMarksFailed() {
	s.expectClaim(claimedJob(models.JobActionPlan, "{"), nil)
	s.jobs.EXPECT().FailClaimedJob(gomock.Any(), "job-1", gomock.Any()).Return(nil)

	cmd, err := s.svc.ClaimPendingJob(context.Background(), "agent-a")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "invalid payload")
	require.Nil(s.T(), cmd)
}

func (s *JobServiceSuite) TestClaimPendingJobInvalidPayloadFailErrorIsWrapped() {
	s.expectClaim(claimedJob(models.JobActionPlan, "{"), nil)
	s.jobs.EXPECT().FailClaimedJob(gomock.Any(), "job-1", gomock.Any()).Return(errors.New("db"))

	cmd, err := s.svc.ClaimPendingJob(context.Background(), "agent-a")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "invalid payload")
	require.Contains(s.T(), err.Error(), "also failed to mark job failed")
	require.Nil(s.T(), cmd)
}

func (s *JobServiceSuite) TestClaimPendingJobUnknownActionMarksFailed() {
	s.expectClaim(claimedJob(models.JobAction("nope"), `{}`), nil)
	s.jobs.EXPECT().FailClaimedJob(gomock.Any(), "job-1", gomock.Any()).Return(nil)

	cmd, err := s.svc.ClaimPendingJob(context.Background(), "agent-a")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "unknown job action")
	require.Nil(s.T(), cmd)
}

func (s *JobServiceSuite) TestReleaseClaim() {
	s.jobs.EXPECT().ReleaseClaimedJob(gomock.Any(), "job-1").Return(nil)
	require.NoError(s.T(), s.svc.ReleaseClaim(context.Background(), "job-1"))
}

func (s *JobServiceSuite) TestReleaseClaimError() {
	s.jobs.EXPECT().ReleaseClaimedJob(gomock.Any(), "job-1").Return(errors.New("db"))
	err := s.svc.ReleaseClaim(context.Background(), "job-1")
	require.Error(s.T(), err)
}

func (s *JobServiceSuite) TestFailClaimedJob() {
	s.jobs.EXPECT().FailClaimedJob(gomock.Any(), "job-1", "nope").Return(nil)
	require.NoError(s.T(), s.svc.FailClaimedJob(context.Background(), "job-1", "nope"))
}

func (s *JobServiceSuite) TestReapExpiredClaimsNone() {
	s.jobs.EXPECT().ReapExpiredClaims(gomock.Any(), gomock.Any()).Return(&repository.ReapExpiredClaimsResult{}, nil)
	require.NoError(s.T(), s.svc.ReapExpiredClaims(context.Background()))
}

func (s *JobServiceSuite) TestReapExpiredClaimsSome() {
	s.jobs.EXPECT().ReapExpiredClaims(gomock.Any(), gomock.Any()).Return(&repository.ReapExpiredClaimsResult{
		ClaimedReturned: 2,
	}, nil)
	require.NoError(s.T(), s.svc.ReapExpiredClaims(context.Background()))
}

func (s *JobServiceSuite) TestReapExpiredClaimsRunningApplyReleasesLock() {
	job := resultJob(models.JobActionApply)
	s.jobs.EXPECT().ReapExpiredClaims(gomock.Any(), gomock.Any()).Return(&repository.ReapExpiredClaimsResult{
		RunningFailed: []*models.Job{job},
	}, nil)
	s.locks.EXPECT().Delete(gomock.Any(), job.Repo, job.StackName, "default").Return(nil)

	require.NoError(s.T(), s.svc.ReapExpiredClaims(context.Background()))
}

func (s *JobServiceSuite) TestReapExpiredClaimsRunningApplyLockReleaseFailureIsBestEffort() {
	job := resultJob(models.JobActionApply)
	s.jobs.EXPECT().ReapExpiredClaims(gomock.Any(), gomock.Any()).Return(&repository.ReapExpiredClaimsResult{
		RunningFailed: []*models.Job{job},
	}, nil)
	s.locks.EXPECT().Delete(gomock.Any(), job.Repo, job.StackName, "default").Return(errors.New("unlock failed"))

	require.NoError(s.T(), s.svc.ReapExpiredClaims(context.Background()))
}

func (s *JobServiceSuite) TestReapExpiredClaimsRunningPlanDoesNotReleaseLock() {
	job := resultJob(models.JobActionPlan)
	s.jobs.EXPECT().ReapExpiredClaims(gomock.Any(), gomock.Any()).Return(&repository.ReapExpiredClaimsResult{
		RunningFailed: []*models.Job{job},
	}, nil)

	require.NoError(s.T(), s.svc.ReapExpiredClaims(context.Background()))
}

func (s *JobServiceSuite) TestReapExpiredClaimsNilResult() {
	s.jobs.EXPECT().ReapExpiredClaims(gomock.Any(), gomock.Any()).Return(nil, nil)
	require.NoError(s.T(), s.svc.ReapExpiredClaims(context.Background()))
}

func (s *JobServiceSuite) TestReapExpiredClaimsError() {
	s.jobs.EXPECT().ReapExpiredClaims(gomock.Any(), gomock.Any()).Return(nil, errors.New("db"))
	err := s.svc.ReapExpiredClaims(context.Background())
	require.Error(s.T(), err)
}

func (s *JobServiceSuite) TestCleanupExpiredJobsNone() {
	s.jobs.EXPECT().CleanupExpiredJobs(gomock.Any(), gomock.Any()).Return(0, nil)
	require.NoError(s.T(), s.svc.CleanupExpiredJobs(context.Background()))
}

func (s *JobServiceSuite) TestCleanupExpiredJobsSome() {
	before := time.Now()
	s.jobs.EXPECT().CleanupExpiredJobs(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, cutoff time.Time) (int, error) {
			s.Require().True(cutoff.Before(before))
			return 3, nil
		},
	)
	require.NoError(s.T(), s.svc.CleanupExpiredJobs(context.Background()))
}

func (s *JobServiceSuite) TestCleanupExpiredJobsError() {
	s.jobs.EXPECT().CleanupExpiredJobs(gomock.Any(), gomock.Any()).Return(0, errors.New("db"))
	err := s.svc.CleanupExpiredJobs(context.Background())
	require.Error(s.T(), err)
}

func (s *JobServiceSuite) TestCleanupExpiredJobsDisabledWhenIntervalZero() {
	svc := services.NewJobService(log.Noop(), s.jobs, s.locks, s.scm, s.publisher, &config.Config{
		OrchestratorJobCleanupInterval: 0,
	}, &orchestratorconfig.ConfigFile{}, authz.AllowAll())
	require.NoError(s.T(), svc.CleanupExpiredJobs(context.Background()))
}

func (s *JobServiceSuite) TestRefreshAgentClaims() {
	before := time.Now()
	s.jobs.EXPECT().RefreshAgentClaims(gomock.Any(), "agent-a", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, lease *time.Time) error {
			s.requireLeaseAboutOneMinute(lease, before)
			return nil
		},
	)
	require.NoError(s.T(), s.svc.RefreshAgentClaims(context.Background(), "agent-a"))
}

func (s *JobServiceSuite) TestRefreshAgentClaimsError() {
	s.jobs.EXPECT().RefreshAgentClaims(gomock.Any(), "agent-a", gomock.Any()).Return(errors.New("db"))
	err := s.svc.RefreshAgentClaims(context.Background(), "agent-a")
	require.Error(s.T(), err)
}

func (s *JobServiceSuite) TestAckJob() {
	job := claimedJob(models.JobActionPlan, `{}`)
	job.Status = models.JobStatusClaimed
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.AssignableToTypeOf(&models.Job{})).DoAndReturn(
		func(_ context.Context, updated *models.Job) error {
			require.Equal(s.T(), models.JobStatusRunning, updated.Status)
			return nil
		},
	)
	require.NoError(s.T(), s.svc.AckJob(context.Background(), "job-1", "agent-a"))
}

func (s *JobServiceSuite) TestAckJobWrongAgent() {
	job := claimedJob(models.JobActionPlan, `{}`)
	job.Status = models.JobStatusClaimed
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)

	err := s.svc.AckJob(context.Background(), "job-1", "other-agent")
	require.ErrorIs(s.T(), err, services.ErrJobWrongAgent)
}

func (s *JobServiceSuite) TestAckJobWrongStatus() {
	job := claimedJob(models.JobActionPlan, `{}`)
	job.Status = models.JobStatusRunning
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)

	err := s.svc.AckJob(context.Background(), "job-1", "agent-a")
	require.ErrorIs(s.T(), err, services.ErrJobInvalidStatus)
}

func (s *JobServiceSuite) TestAckJobGetFailure() {
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(nil, errors.New("db"))
	err := s.svc.AckJob(context.Background(), "job-1", "agent-a")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to fetch job")
}

func (s *JobServiceSuite) TestAckJobUpdateFailure() {
	job := claimedJob(models.JobActionPlan, `{}`)
	job.Status = models.JobStatusClaimed
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(errors.New("db"))
	err := s.svc.AckJob(context.Background(), "job-1", "agent-a")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to update job")
}

func (s *JobServiceSuite) TestCommitJobResultWrongAgent() {
	job := resultJob(models.JobActionPlan)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)

	err := s.svc.CommitJobResult(context.Background(), "job-1", "other-agent", "success", "", "")
	require.ErrorIs(s.T(), err, services.ErrJobWrongAgent)
}

func (s *JobServiceSuite) TestCommitJobResultWrongStatus() {
	job := resultJob(models.JobActionPlan)
	job.Status = models.JobStatusClaimed
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)

	err := s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "", "")
	require.ErrorIs(s.T(), err, services.ErrJobInvalidStatus)
}

func resultJob(action models.JobAction) *models.Job {
	job := claimedJob(action, `{}`)
	job.Status = models.JobStatusRunning
	return job
}

func (s *JobServiceSuite) TestCommitJobResultPlanSuccess() {
	job := resultJob(models.JobActionPlan)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.AssignableToTypeOf(&models.Job{})).DoAndReturn(
		func(_ context.Context, updated *models.Job) error {
			require.Equal(s.T(), models.JobStatusSucceeded, updated.Status)
			require.Equal(s.T(), "plan out", updated.Output)
			require.Empty(s.T(), updated.ErrorMsg)
			return nil
		},
	)
	s.publisher.EXPECT().WriteComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any()).Return(7, nil)
	s.jobs.EXPECT().UpdatePayload(gomock.Any(), job, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ *models.Job, payload map[string]interface{}) error {
			require.Equal(s.T(), "7", payload["comment_id"])
			return nil
		},
	)

	require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "plan out", ""))
}

func (s *JobServiceSuite) TestCommitJobResultUpdatesExistingComment() {
	job := resultJob(models.JobActionPlan)
	job.Payload = `{"comment_id":"42"}`
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(nil)
	s.publisher.EXPECT().UpdateComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any(), 42).Return(nil)

	require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "plan out", ""))
}

func (s *JobServiceSuite) TestCommitJobResultPlanFailure() {
	job := resultJob(models.JobActionPlan)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.AssignableToTypeOf(&models.Job{})).DoAndReturn(
		func(_ context.Context, updated *models.Job) error {
			require.Equal(s.T(), models.JobStatusFailed, updated.Status)
			require.Equal(s.T(), "boom", updated.ErrorMsg)
			return nil
		},
	)
	s.publisher.EXPECT().WriteComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any()).Return(8, nil)
	s.jobs.EXPECT().UpdatePayload(gomock.Any(), job, gomock.Any()).Return(nil)

	require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "failed", "", "boom"))
}

func (s *JobServiceSuite) TestCommitJobResultCommentFailureIsBestEffort() {
	job := resultJob(models.JobActionPlan)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any()).Return(0, errors.New("github down"))

	require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "ok", ""))
}

func (s *JobServiceSuite) TestCommitJobResultApplySuccessReleasesLock() {
	job := resultJob(models.JobActionApply)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(nil)
	s.locks.EXPECT().Delete(gomock.Any(), job.Repo, job.StackName, "default").Return(nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any()).Return(9, nil)
	s.jobs.EXPECT().UpdatePayload(gomock.Any(), job, gomock.Any()).Return(nil)

	require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "apply out", ""))
}

func (s *JobServiceSuite) TestCommitJobResultApplyFailureStillReleasesLock() {
	job := resultJob(models.JobActionApply)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(nil)
	s.locks.EXPECT().Delete(gomock.Any(), job.Repo, job.StackName, "default").Return(nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any()).Return(10, nil)
	s.jobs.EXPECT().UpdatePayload(gomock.Any(), job, gomock.Any()).Return(nil)

	require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "failed", "", "apply boom"))
}

func (s *JobServiceSuite) TestCommitJobResultApplyUpdateFailureStillAttemptsLockRelease() {
	job := resultJob(models.JobActionApply)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(errors.New("db"))
	s.locks.EXPECT().Delete(gomock.Any(), job.Repo, job.StackName, "default").Return(nil)

	err := s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "", "")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to update job")
	require.NotContains(s.T(), err.Error(), "also failed to release lock")
}

func (s *JobServiceSuite) TestCommitJobResultApplyUpdateAndLockReleaseFailure() {
	job := resultJob(models.JobActionApply)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(errors.New("db"))
	s.locks.EXPECT().Delete(gomock.Any(), job.Repo, job.StackName, "default").Return(errors.New("unlock failed"))

	err := s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "", "")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "also failed to release lock")
}

func (s *JobServiceSuite) TestCommitJobResultApplyLockReleaseFailureAfterUpdate() {
	job := resultJob(models.JobActionApply)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(nil)
	s.locks.EXPECT().Delete(gomock.Any(), job.Repo, job.StackName, "default").Return(errors.New("unlock failed"))

	err := s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "", "")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to release lock")
}

func (s *JobServiceSuite) TestCommitJobResultGetFailure() {
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(nil, errors.New("db"))
	err := s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "", "")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to fetch job")
}

func (s *JobServiceSuite) TestHandlePeriodicResultCreatesCommentAndStoresID() {
	job := resultJob(models.JobActionPlan)
	job.Payload = `{"trigger_user":"alice"}`
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any()).Return(99, nil)
	s.jobs.EXPECT().UpdatePayload(gomock.Any(), job, gomock.Any()).DoAndReturn(
		func(_ context.Context, updated *models.Job, payload map[string]interface{}) error {
			require.Equal(s.T(), "99", payload["comment_id"])
			require.Equal(s.T(), "alice", payload["trigger_user"])
			return nil
		},
	)

	require.NoError(s.T(), s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-a", "partial out"))
}

func (s *JobServiceSuite) TestHandlePeriodicResultUpdatesExistingComment() {
	job := resultJob(models.JobActionPlan)
	job.Payload = `{"comment_id":"42"}`
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.publisher.EXPECT().UpdateComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any(), 42).Return(nil)

	require.NoError(s.T(), s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-a", "more out"))
}

func (s *JobServiceSuite) TestHandlePeriodicResultWriteFailureDoesNotStoreID() {
	job := resultJob(models.JobActionPlan)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any()).Return(0, errors.New("github down"))

	require.NoError(s.T(), s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-a", "out"))
}

func (s *JobServiceSuite) TestHandlePeriodicResultInvalidCommentID() {
	job := resultJob(models.JobActionPlan)
	job.Payload = `{"comment_id":"nope"}`
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)

	// Invalid stored IDs are logged; the job result path stays best-effort.
	require.NoError(s.T(), s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-a", "out"))
}

func (s *JobServiceSuite) TestHandlePeriodicResultGetFailure() {
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(nil, errors.New("db"))
	err := s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-a", "out")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to fetch job")
}

func (s *JobServiceSuite) TestHandlePeriodicResultInvalidPayload() {
	job := resultJob(models.JobActionPlan)
	job.Payload = `{`
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)

	require.NoError(s.T(), s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-a", "out"))
}

func (s *JobServiceSuite) TestHandlePeriodicResultUpdateCommentFailureIsBestEffort() {
	job := resultJob(models.JobActionPlan)
	job.Payload = `{"comment_id":"42"}`
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.publisher.EXPECT().UpdateComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any(), 42).Return(errors.New("github down"))

	require.NoError(s.T(), s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-a", "out"))
}

func (s *JobServiceSuite) TestHandlePeriodicResultUpdatePayloadFailureIsBestEffort() {
	job := resultJob(models.JobActionPlan)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any()).Return(99, nil)
	s.jobs.EXPECT().UpdatePayload(gomock.Any(), job, gomock.Any()).Return(errors.New("db"))

	require.NoError(s.T(), s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-a", "out"))
}

// --- orchestrator-owned routing ---

const routedYAML = `
environments:
  - name: default
    stacks:
      - name: a
        dir: stacks/a
      - name: b
        agent: agent-from-yaml
        dir: stacks/b
`

func (s *JobServiceSuite) routedService() services.JobService {
	cfgFile, err := orchestratorconfig.Parse([]byte(`
agents:
  agent-prod:
    token_env: T1
  agent-data:
    token_env: T2
repos:
  acme/infra:
    agent: agent-prod
    stacks:
      b:
        dir: stacks/b
        agent: agent-data
`), func(key string) string { return key })
	require.NoError(s.T(), err)
	return services.NewJobService(log.Noop(), s.jobs, s.locks, s.scm, s.publisher, &config.Config{
		OrchestratorJobLease: time.Minute,
	}, cfgFile, authz.AllowAll())
}

func (s *JobServiceSuite) TestRoutingUsesOrchestratorConfigAndIgnoresYAMLAgents() {
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(routedYAML, nil)
	s.expectUpsert("a", "stacks/a", "plan", "agent-prod", "job-a")
	s.expectUpsert("b", "stacks/b", "plan", "agent-data", "job-b")

	err := s.routedService().CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestLegacyRoutingRequiresYAMLAgents() {
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(routedYAML, nil)

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to parse terraplane.yaml")
	require.Contains(s.T(), err.Error(), `stack "a" in environment "default" has no agent`)
}

// --- authorization ---

func (s *JobServiceSuite) authorizedService() services.JobService {
	return services.NewJobService(log.Noop(), s.jobs, s.locks, s.scm, s.publisher, &config.Config{
		OrchestratorJobLease: time.Minute,
	}, &orchestratorconfig.ConfigFile{}, s.authz)
}

func (s *JobServiceSuite) TestAuthorizationAllowsEveryStack() {
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "a", authz.ActionPlan).Return(true, "", nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "b", authz.ActionPlan).Return(true, "", nil)
	s.expectUpsert("a", "stacks/a", "plan", "agent-a", "job-a")
	s.expectUpsert("b", "stacks/b", "plan", "agent-b", "job-b")

	err := s.authorizedService().CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestAuthorizationQueuesAllowedStacksAndCommentsDenied() {
	wh := webhook("terraplane apply")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "a", authz.ActionApply).Return(true, "", nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "b", authz.ActionApply).Return(false, "jace is not allowed to apply stack b in acme/infra", nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), "acme/infra", 42, feedback.DeniedComment("apply", []string{
		"jace is not allowed to apply stack b in acme/infra",
	})).Return(0, nil)
	s.expectLockCreate("a", "stacks/a", nil)
	s.expectUpsert("a", "stacks/a", "apply", "agent-a", "job-a")
	// No lock and no upsert for b.

	err := s.authorizedService().CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestAuthorizationDeniesEverythingQueuesNothing() {
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", gomock.Any(), authz.ActionPlan).Return(false, "no", nil).Times(2)
	s.publisher.EXPECT().WriteComment(gomock.Any(), "acme/infra", 42, gomock.Any()).Return(0, nil)

	err := s.authorizedService().CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestAuthorizationDeniedCommentIsBestEffort() {
	wh := webhook("terraplane plan -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "a", authz.ActionPlan).Return(false, "no", nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), "acme/infra", 42, gomock.Any()).Return(0, errors.New("github down"))

	err := s.authorizedService().CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestAuthorizationLookupErrorFailsClosed() {
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "a", authz.ActionPlan).Return(false, "", errors.New("github down"))
	// No upsert, no comment: the whole command fails.

	err := s.authorizedService().CreatePendingJobs(context.Background(), wh)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), `failed to authorize plan of stack "a" in repository acme/infra for jace`)
	require.Contains(s.T(), err.Error(), "github down")
}

// --- dir binding ---

func (s *JobServiceSuite) TestRoutingDeniesListedStackMovedToAnotherDir() {
	// Intention: a PR cannot keep a privileged stack name while pointing it at a different directory.
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(`
environments:
  - name: default
    stacks:
      - name: a
        dir: stacks/a
      - name: b
        dir: stacks/elsewhere
`, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), "acme/infra", 42, feedback.DeniedComment("plan", []string{
		"stack b is pinned to dir stacks/b in the orchestrator config but the pull request maps it to stacks/elsewhere",
	})).Return(0, nil)
	s.expectUpsert("a", "stacks/a", "plan", "agent-prod", "job-a")

	err := s.routedService().CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestRoutingDeniesNewStackNameOnPinnedDir() {
	// Intention: a PR cannot reach a pinned directory under a fresh name that falls back to repo defaults.
	wh := webhook("terraplane plan")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(`
environments:
  - name: default
    stacks:
      - name: b2
        dir: ./stacks/b/
`, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), "acme/infra", 42, feedback.DeniedComment("plan", []string{
		"dir ./stacks/b/ belongs to stack b in the orchestrator config and cannot be used by stack b2",
	})).Return(0, nil)

	err := s.routedService().CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestRoutingAcceptsPinnedStackWithEquivalentDirSpelling() {
	wh := webhook("terraplane plan -s b")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(`
environments:
  - name: default
    stacks:
      - name: b
        dir: ./stacks/b/
`, nil)
	s.expectUpsert("b", "./stacks/b/", "plan", "agent-data", "job-b")

	err := s.routedService().CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestHandlePeriodicResultRejectsWrongAgent() {
	job := resultJob(models.JobActionPlan)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)

	err := s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-b", "out")
	require.ErrorIs(s.T(), err, services.ErrJobWrongAgent)
}

func (s *JobServiceSuite) TestHandlePeriodicResultRejectsJobNotRunning() {
	job := resultJob(models.JobActionPlan)
	job.Status = models.JobStatusSucceeded
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)

	err := s.svc.HandlePeriodicResult(context.Background(), "job-1", "agent-a", "out")
	require.ErrorIs(s.T(), err, services.ErrJobInvalidStatus)
}

// --- apply gates ---

func (s *JobServiceSuite) gatedService(gates string) services.JobService {
	cfgFile, err := orchestratorconfig.Parse([]byte(`
agents:
  agent-prod:
    token_env: T1
repos:
  acme/infra:
    agent: agent-prod
    apply_requires:
`+gates), func(key string) string { return key })
	require.NoError(s.T(), err)
	return services.NewJobService(log.Noop(), s.jobs, s.locks, s.scm, s.publisher, &config.Config{
		OrchestratorJobLease: time.Minute,
	}, cfgFile, authz.AllowAll())
}

func openPR(state string) scm.PullRequest {
	return scm.PullRequest{Open: true, MergeableState: state, HeadSHA: "abc123", BaseRef: "main"}
}

func (s *JobServiceSuite) expectPR(pr scm.PullRequest) {
	s.scm.EXPECT().GetPullRequest(gomock.Any(), "acme/infra", 42).Return(pr, nil)
}

func (s *JobServiceSuite) expectDenied(reasons ...string) {
	s.publisher.EXPECT().WriteComment(gomock.Any(), "acme/infra", 42, feedback.DeniedComment("apply", reasons)).Return(0, nil)
}

func (s *JobServiceSuite) TestApplyGatesPassQueuesJobs() {
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectPR(openPR("clean"))
	s.scm.EXPECT().PullRequestApproved(gomock.Any(), "acme/infra", 42).Return(true, nil)
	s.scm.EXPECT().CommitsBehind(gomock.Any(), "acme/infra", "main", "abc123").Return(0, nil)
	s.scm.EXPECT().ChecksGreen(gomock.Any(), "acme/infra", "abc123").Return(true, nil)
	s.expectLockCreate("a", "stacks/a", nil)
	s.expectUpsert("a", "stacks/a", "apply", "agent-prod", "job-a")

	err := s.gatedService("      approved: true\n      mergeable: true\n      undiverged: true\n      checks_green: true\n").CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyOnClosedPullRequestIsBlockedEvenWithoutGates() {
	wh := webhook("terraplane apply")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	pr := openPR("clean")
	pr.Open = false
	s.expectPR(pr)
	s.expectDenied("apply requires pull request #42 to be open")
	// No locks, no upserts.

	err := s.gatedService("      {}\n").CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyGateApprovalFailsBlocksEveryStack() {
	wh := webhook("terraplane apply")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectPR(openPR("clean"))
	s.scm.EXPECT().PullRequestApproved(gomock.Any(), "acme/infra", 42).Return(false, nil)
	s.expectDenied("apply requires an approved review on pull request #42")

	err := s.gatedService("      approved: true\n").CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyGateMergeableStates() {
	cases := map[string]string{
		"clean":     "",
		"unstable":  "",
		"has_hooks": "",
		"dirty":     "apply requires pull request #42 to have no merge conflicts",
		"draft":     "apply requires pull request #42 not to be a draft",
		"blocked":   "apply requires pull request #42 to satisfy its branch protection rules",
		"behind":    "apply requires pull request #42 to be up to date with its base branch",
		"unknown":   "GitHub has not finished computing whether pull request #42 is mergeable; try again shortly",
		"":          "GitHub has not finished computing whether pull request #42 is mergeable; try again shortly",
		"weird":     "apply requires pull request #42 to be mergeable (GitHub reports weird)",
	}
	for state, reason := range cases {
		s.SetupTest()
		wh := webhook("terraplane apply -s a")
		s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
		s.expectPR(openPR(state))
		if reason == "" {
			s.expectLockCreate("a", "stacks/a", nil)
			s.expectUpsert("a", "stacks/a", "apply", "agent-prod", "job-a")
		} else {
			s.expectDenied(reason)
		}

		err := s.gatedService("      mergeable: true\n").CreatePendingJobs(context.Background(), wh)
		require.NoError(s.T(), err, state)
		s.ctrl.Finish()
	}
}

func (s *JobServiceSuite) TestApplyGateUndivergedBlocksBranchBehindBase() {
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectPR(openPR("clean"))
	s.scm.EXPECT().CommitsBehind(gomock.Any(), "acme/infra", "main", "abc123").Return(3, nil)
	s.expectDenied("apply requires pull request #42 to contain the latest main commit (it is 3 behind)")

	err := s.gatedService("      undiverged: true\n").CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyGateChecksFailBlocksEveryStack() {
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectPR(openPR("clean"))
	s.scm.EXPECT().ChecksGreen(gomock.Any(), "acme/infra", "abc123").Return(false, nil)
	s.expectDenied("apply requires all checks on commit abc123 to pass")

	err := s.gatedService("      checks_green: true\n").CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyGatesReportEveryFailedGate() {
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectPR(openPR("dirty"))
	s.scm.EXPECT().PullRequestApproved(gomock.Any(), "acme/infra", 42).Return(false, nil)
	s.scm.EXPECT().CommitsBehind(gomock.Any(), "acme/infra", "main", "abc123").Return(1, nil)
	s.scm.EXPECT().ChecksGreen(gomock.Any(), "acme/infra", "abc123").Return(false, nil)
	s.expectDenied(
		"apply requires an approved review on pull request #42",
		"apply requires pull request #42 to have no merge conflicts",
		"apply requires pull request #42 to contain the latest main commit (it is 1 behind)",
		"apply requires all checks on commit abc123 to pass",
	)

	err := s.gatedService("      approved: true\n      mergeable: true\n      undiverged: true\n      checks_green: true\n").CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyGatesNotConfiguredOnlyCheckOpen() {
	// Only GetPullRequest is expected: no review, compare, or checks lookups.
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectPR(openPR("dirty"))
	s.expectLockCreate("a", "stacks/a", nil)
	s.expectUpsert("a", "stacks/a", "apply", "agent-prod", "job-a")

	err := s.gatedService("      {}\n").CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyGatesDoNotApplyToPlan() {
	wh := webhook("terraplane plan -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectUpsert("a", "stacks/a", "plan", "agent-prod", "job-a")

	err := s.gatedService("      approved: true\n      mergeable: true\n").CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyGatesSkippedWithoutOrchestratorConfig() {
	// Legacy mode: no pull request fetch at all.
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.expectLockCreate("a", "stacks/a", nil)
	s.expectUpsert("a", "stacks/a", "apply", "agent-a", "job-a")

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}

func (s *JobServiceSuite) TestApplyGateLookupErrorsFailClosed() {
	cases := map[string]func(){
		"failed to fetch pull request": func() {
			s.scm.EXPECT().GetPullRequest(gomock.Any(), "acme/infra", 42).Return(scm.PullRequest{}, errors.New("github down"))
		},
		"failed to check review approval": func() {
			s.expectPR(openPR("clean"))
			s.scm.EXPECT().PullRequestApproved(gomock.Any(), "acme/infra", 42).Return(false, errors.New("github down"))
		},
		"failed to compare pull request": func() {
			s.expectPR(openPR("clean"))
			s.scm.EXPECT().PullRequestApproved(gomock.Any(), "acme/infra", 42).Return(true, nil)
			s.scm.EXPECT().CommitsBehind(gomock.Any(), "acme/infra", "main", "abc123").Return(0, errors.New("github down"))
		},
		"failed to check commit status": func() {
			s.expectPR(openPR("clean"))
			s.scm.EXPECT().PullRequestApproved(gomock.Any(), "acme/infra", 42).Return(true, nil)
			s.scm.EXPECT().CommitsBehind(gomock.Any(), "acme/infra", "main", "abc123").Return(0, nil)
			s.scm.EXPECT().ChecksGreen(gomock.Any(), "acme/infra", "abc123").Return(false, errors.New("github down"))
		},
	}
	for want, expect := range cases {
		s.SetupTest()
		wh := webhook("terraplane apply -s a")
		s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
		expect()

		err := s.gatedService("      approved: true\n      undiverged: true\n      checks_green: true\n").CreatePendingJobs(context.Background(), wh)
		require.Error(s.T(), err, want)
		require.Contains(s.T(), err.Error(), want)
		s.ctrl.Finish()
	}
}

func (s *JobServiceSuite) TestApplyGatesRunAfterAuthorization() {
	// Intention: an unauthorized user must not trigger any pull request lookups.
	wh := webhook("terraplane apply -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(twoStackYAML, nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "a", authz.ActionApply).Return(false, "no", nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), "acme/infra", 42, gomock.Any()).Return(0, nil)

	cfgFile, err := orchestratorconfig.Parse([]byte(`
agents:
  agent-prod:
    token_env: T1
repos:
  acme/infra:
    agent: agent-prod
    apply_requires:
      approved: true
`), func(key string) string { return key })
	require.NoError(s.T(), err)
	svc := services.NewJobService(log.Noop(), s.jobs, s.locks, s.scm, s.publisher, &config.Config{}, cfgFile, s.authz)
	require.NoError(s.T(), svc.CreatePendingJobs(context.Background(), wh))
}

func (s *JobServiceSuite) TestPlanPersistsTool() {
	wh := webhook("terraplane plan -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", wh.CommitSHA, wh.RepositorySlug).Return(`
environments:
  - name: default
    agent: agent-a
    tool: tofu
    stacks:
      - name: a
        dir: stacks/a
`, nil)
	s.jobs.EXPECT().UpsertPendingJob(
		gomock.Any(), "acme/infra", 42, "a", "plan",
		map[string]interface{}{
			"trigger_user": "jace",
			"commit_sha":   "abc123",
			"stack_name":   "a",
			"dir":          "stacks/a",
			"tool":         "tofu",
			"tool_version": "",
			"plan_flags":   "",
		},
		"agent-a",
	).Return(&models.Job{ID: "job-a"}, nil)

	err := s.svc.CreatePendingJobs(context.Background(), wh)
	require.NoError(s.T(), err)
}
