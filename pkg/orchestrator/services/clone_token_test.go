package services_test

import (
	"context"
	"errors"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/xyzjace/terraplane/pkg/orchestrator/services"
	"github.com/xyzjace/terraplane/pkg/scm"
	"github.com/xyzjace/terraplane/pkg/storage/models"
	"github.com/xyzjace/terraplane/pkg/storage/repository"
)

var issuedToken = scm.CloneToken{Token: "ghs_job1", ExpiresAt: time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)}

// issueToken puts a token for job-1 into the service so a later call can be
// expected to revoke it.
func (s *JobServiceSuite) issueToken(token string) {
	s.T().Helper()
	job := claimedJob(models.JobActionPlan, `{}`)
	job.Status = models.JobStatusClaimed
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.scm.EXPECT().CloneToken(gomock.Any(), "acme/infra").Return(scm.CloneToken{Token: token, ExpiresAt: issuedToken.ExpiresAt}, nil)

	got, err := s.svc.IssueCloneToken(context.Background(), "job-1", "agent-a")
	require.NoError(s.T(), err)
	require.Equal(s.T(), token, got.Token)
}

func (s *JobServiceSuite) TestIssueCloneTokenForClaimedJob() {
	s.issueToken("ghs_job1")
}

func (s *JobServiceSuite) TestIssueCloneTokenForRunningJob() {
	job := resultJob(models.JobActionApply)
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.scm.EXPECT().CloneToken(gomock.Any(), "acme/infra").Return(issuedToken, nil)

	got, err := s.svc.IssueCloneToken(context.Background(), "job-1", "agent-a")
	require.NoError(s.T(), err)
	require.Equal(s.T(), issuedToken, got)
}

func (s *JobServiceSuite) TestIssueCloneTokenJobNotFound() {
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(nil, repository.ErrJobNotFound)

	_, err := s.svc.IssueCloneToken(context.Background(), "job-1", "agent-a")
	require.ErrorIs(s.T(), err, services.ErrJobNotFound)
}

func (s *JobServiceSuite) TestIssueCloneTokenGetFailure() {
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(nil, errors.New("db down"))

	_, err := s.svc.IssueCloneToken(context.Background(), "job-1", "agent-a")
	require.Error(s.T(), err)
	require.NotErrorIs(s.T(), err, services.ErrJobNotFound)
	require.Contains(s.T(), err.Error(), "failed to fetch job job-1")
}

func (s *JobServiceSuite) TestIssueCloneTokenWrongAgent() {
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(resultJob(models.JobActionPlan), nil)

	_, err := s.svc.IssueCloneToken(context.Background(), "job-1", "other-agent")
	require.ErrorIs(s.T(), err, services.ErrJobWrongAgent)
}

func (s *JobServiceSuite) TestIssueCloneTokenRequiresClaimedOrRunning() {
	for _, status := range []models.JobStatus{models.JobStatusPending, models.JobStatusSucceeded, models.JobStatusFailed} {
		job := claimedJob(models.JobActionPlan, `{}`)
		job.Status = status
		s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)

		_, err := s.svc.IssueCloneToken(context.Background(), "job-1", "agent-a")
		require.ErrorIs(s.T(), err, services.ErrJobInvalidStatus, status)
	}
}

func (s *JobServiceSuite) TestIssueCloneTokenProviderFailure() {
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(resultJob(models.JobActionPlan), nil)
	s.scm.EXPECT().CloneToken(gomock.Any(), "acme/infra").Return(scm.CloneToken{}, errors.New("github down"))

	_, err := s.svc.IssueCloneToken(context.Background(), "job-1", "agent-a")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to issue clone token for job job-1 in repository acme/infra")
}

func (s *JobServiceSuite) TestIssueCloneTokenUnavailablePassesThrough() {
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(resultJob(models.JobActionPlan), nil)
	s.scm.EXPECT().CloneToken(gomock.Any(), "acme/infra").Return(scm.CloneToken{}, scm.ErrCloneTokenUnavailable)

	_, err := s.svc.IssueCloneToken(context.Background(), "job-1", "agent-a")
	require.ErrorIs(s.T(), err, scm.ErrCloneTokenUnavailable)
}

func (s *JobServiceSuite) TestIssueCloneTokenAgainRevokesPrevious() {
	s.issueToken("ghs_first")
	s.scm.EXPECT().RevokeCloneToken(gomock.Any(), "ghs_first").Return(nil)
	s.issueToken("ghs_second")
}

func (s *JobServiceSuite) expectCommitFlow(job *models.Job) {
	s.T().Helper()
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(job, nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), job.Repo, int(job.PRNumber), gomock.Any()).Return(7, nil)
	s.jobs.EXPECT().UpdatePayload(gomock.Any(), job, gomock.Any()).Return(nil)
}

func (s *JobServiceSuite) TestCommitJobResultRevokesCloneToken() {
	for _, result := range []string{"success", "failed"} {
		s.SetupTest()
		s.issueToken("ghs_job1")
		s.expectCommitFlow(resultJob(models.JobActionPlan))
		s.scm.EXPECT().RevokeCloneToken(gomock.Any(), "ghs_job1").Return(nil)

		require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", result, "out", ""), result)
		s.ctrl.Finish()
	}
}

func (s *JobServiceSuite) TestCommitJobResultRevokeFailureIsBestEffort() {
	s.issueToken("ghs_job1")
	s.expectCommitFlow(resultJob(models.JobActionPlan))
	s.scm.EXPECT().RevokeCloneToken(gomock.Any(), "ghs_job1").Return(errors.New("github down"))

	require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "out", ""))
}

func (s *JobServiceSuite) TestCommitJobResultRevokesOnlyOnce() {
	s.issueToken("ghs_job1")
	s.expectCommitFlow(resultJob(models.JobActionPlan))
	s.scm.EXPECT().RevokeCloneToken(gomock.Any(), "ghs_job1").Return(nil)
	require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "out", ""))

	// The token is gone from the service: a second completion makes no
	// revoke call (gomock fails the test on an unexpected one).
	s.expectCommitFlow(resultJob(models.JobActionPlan))
	require.NoError(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "out", ""))
}

func (s *JobServiceSuite) TestCommitJobResultUpdateFailureKeepsCloneToken() {
	s.issueToken("ghs_job1")
	s.jobs.EXPECT().Get(gomock.Any(), "job-1").Return(resultJob(models.JobActionPlan), nil)
	s.jobs.EXPECT().Update(gomock.Any(), gomock.Any()).Return(errors.New("db down"))

	require.Error(s.T(), s.svc.CommitJobResult(context.Background(), "job-1", "agent-a", "success", "out", ""))
}

func (s *JobServiceSuite) TestFailClaimedJobRevokesCloneToken() {
	s.issueToken("ghs_job1")
	s.jobs.EXPECT().FailClaimedJob(gomock.Any(), "job-1", "nope").Return(nil)
	s.scm.EXPECT().RevokeCloneToken(gomock.Any(), "ghs_job1").Return(nil)

	require.NoError(s.T(), s.svc.FailClaimedJob(context.Background(), "job-1", "nope"))
}

func (s *JobServiceSuite) TestFailClaimedJobFailureKeepsCloneToken() {
	s.issueToken("ghs_job1")
	s.jobs.EXPECT().FailClaimedJob(gomock.Any(), "job-1", "nope").Return(errors.New("db down"))

	require.Error(s.T(), s.svc.FailClaimedJob(context.Background(), "job-1", "nope"))
}

func (s *JobServiceSuite) TestReapExpiredClaimsRevokesRunningFailedCloneTokens() {
	s.issueToken("ghs_job1")
	s.jobs.EXPECT().ReapExpiredClaims(gomock.Any(), gomock.Any()).Return(&repository.ReapExpiredClaimsResult{
		RunningFailed: []*models.Job{resultJob(models.JobActionPlan)},
	}, nil)
	s.scm.EXPECT().RevokeCloneToken(gomock.Any(), "ghs_job1").Return(nil)

	require.NoError(s.T(), s.svc.ReapExpiredClaims(context.Background()))
}
