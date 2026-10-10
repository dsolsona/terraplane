package services_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/mock/gomock"

	"github.com/xyzjace/terraplane/pkg/authz"
	"github.com/xyzjace/terraplane/pkg/authz/mock_authz"
	"github.com/xyzjace/terraplane/pkg/command"
	"github.com/xyzjace/terraplane/pkg/feedback"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestrator/services"
	"github.com/xyzjace/terraplane/pkg/scm/mock_scm"
	"github.com/xyzjace/terraplane/pkg/storage/models"
	"github.com/xyzjace/terraplane/pkg/storage/repository/mock_repository"
)

type UnlockServiceSuite struct {
	suite.Suite
	ctrl      *gomock.Controller
	scm       *mock_scm.MockProvider
	publisher *mock_scm.MockPublisher
	jobs      *mock_repository.MockJobRepository
	locks     *mock_repository.MockLockRepository
	authz     *mock_authz.MockAuthorizer
	svc       services.UnlockService
}

func TestUnlockServiceSuite(t *testing.T) {
	suite.Run(t, new(UnlockServiceSuite))
}

func (s *UnlockServiceSuite) SetupTest() {
	s.ctrl = gomock.NewController(s.T())
	s.scm = mock_scm.NewMockProvider(s.ctrl)
	s.publisher = mock_scm.NewMockPublisher(s.ctrl)
	s.jobs = mock_repository.NewMockJobRepository(s.ctrl)
	s.locks = mock_repository.NewMockLockRepository(s.ctrl)
	s.authz = mock_authz.NewMockAuthorizer(s.ctrl)
	s.svc = services.NewUnlockService(log.Noop(), s.scm, s.publisher, s.jobs, s.locks, authz.AllowAll())
}

func (s *UnlockServiceSuite) TestRequiresSelector() {
	unlock := command.UnlockCommand{}
	unlock.Repo = "acme/infra"
	unlock.PRNumber = 42
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).Return(0, nil)

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "unlock requires at least one stack (-s) or environment (-e)")
}

func (s *UnlockServiceSuite) TestUnlockByEnvironment() {
	unlock := unlockCmd("terraplane unlock -e staging")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return(twoEnvYAML, nil)
	s.locks.EXPECT().DeleteByRepoAndStacks(gomock.Any(), unlock.Repo, []string{"a", "b"}).Return(2, nil)
	s.jobs.EXPECT().DeleteByRepoPRAndStacks(gomock.Any(), unlock.Repo, unlock.PRNumber, []string{"a", "b"}).Return(2, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).Return(0, nil).Times(2)

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.NoError(s.T(), err)
}

func (s *UnlockServiceSuite) TestFetchConfigFailurePublishesFailureComment() {
	unlock := unlockCmd("terraplane unlock -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return("", errors.New("404"))
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, _ int, body string) (int, error) {
			require.Contains(s.T(), body, "unlock ·")
			require.Contains(s.T(), body, "failed")
			require.Contains(s.T(), body, "failed to fetch terraplane.yaml")
			return 0, nil
		},
	)

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to fetch terraplane.yaml")
}

func (s *UnlockServiceSuite) TestFetchConfigFailureCommentIsBestEffort() {
	unlock := unlockCmd("terraplane unlock -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return("", errors.New("404"))
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).Return(0, errors.New("github down"))

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to fetch terraplane.yaml")
	require.NotContains(s.T(), err.Error(), "github down")
}

func (s *UnlockServiceSuite) TestParseConfigFailure() {
	unlock := unlockCmd("terraplane unlock -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return("stacks: [", nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).Return(0, nil)

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to parse terraplane.yaml")
}

func (s *UnlockServiceSuite) TestResolveStacksFailure() {
	unlock := unlockCmd("terraplane unlock -s missing")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return(twoStackYAML, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).Return(0, nil)

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to resolve stacks")
}

func (s *UnlockServiceSuite) TestLockDeleteFailurePublishesPerStack() {
	unlock := unlockCmd("terraplane unlock -s a -s b")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return(twoStackYAML, nil)
	s.locks.EXPECT().DeleteByRepoAndStacks(gomock.Any(), unlock.Repo, []string{"a", "b"}).Return(0, errors.New("db"))
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, _ int, body string) (int, error) {
			require.Contains(s.T(), body, "unlock ·")
			require.Contains(s.T(), body, "failed")
			return 0, nil
		},
	).Times(2)

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to delete project locks")
}

func (s *UnlockServiceSuite) TestJobDeleteFailurePublishesPerStack() {
	unlock := unlockCmd("terraplane unlock -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return(twoStackYAML, nil)
	s.locks.EXPECT().DeleteByRepoAndStacks(gomock.Any(), unlock.Repo, []string{"a"}).Return(1, nil)
	s.jobs.EXPECT().DeleteByRepoPRAndStacks(gomock.Any(), unlock.Repo, unlock.PRNumber, []string{"a"}).Return(0, errors.New("db"))
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).Return(0, nil)

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to delete jobs")
}

func (s *UnlockServiceSuite) TestSuccessPublishesPerStackComments() {
	unlock := unlockCmd("terraplane unlock -s a -s b")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return(twoStackYAML, nil)
	s.locks.EXPECT().DeleteByRepoAndStacks(gomock.Any(), unlock.Repo, []string{"a", "b"}).Return(2, nil)
	s.jobs.EXPECT().DeleteByRepoPRAndStacks(gomock.Any(), unlock.Repo, unlock.PRNumber, []string{"a", "b"}).Return(2, nil)

	expectedA := feedback.UnlockResultComment("a", models.JobStatusSucceeded, "")
	expectedB := feedback.UnlockResultComment("b", models.JobStatusSucceeded, "")
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, expectedA).Return(0, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, expectedB).Return(0, nil)

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.NoError(s.T(), err)
}

func (s *UnlockServiceSuite) TestSuccessCommentFailureIsBestEffort() {
	unlock := unlockCmd("terraplane unlock -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return(twoStackYAML, nil)
	s.locks.EXPECT().DeleteByRepoAndStacks(gomock.Any(), unlock.Repo, []string{"a"}).Return(1, nil)
	s.jobs.EXPECT().DeleteByRepoPRAndStacks(gomock.Any(), unlock.Repo, unlock.PRNumber, []string{"a"}).Return(1, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).Return(0, errors.New("github down"))

	err := s.svc.RunUnlock(context.Background(), unlock)
	require.NoError(s.T(), err)
}

// --- authorization ---

func (s *UnlockServiceSuite) authorizedService() services.UnlockService {
	return services.NewUnlockService(log.Noop(), s.scm, s.publisher, s.jobs, s.locks, s.authz)
}

func (s *UnlockServiceSuite) TestUnlockSkipsDeniedStacks() {
	unlock := unlockCmd("terraplane unlock -s a -s b")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return(twoStackYAML, nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "a", authz.ActionUnlock).Return(true, "", nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "b", authz.ActionUnlock).Return(false, "jace is not allowed to unlock stack b in acme/infra", nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, feedback.DeniedComment("unlock", []string{
		"jace is not allowed to unlock stack b in acme/infra",
	})).Return(0, nil)
	s.locks.EXPECT().DeleteByRepoAndStacks(gomock.Any(), unlock.Repo, []string{"a"}).Return(1, nil)
	s.jobs.EXPECT().DeleteByRepoPRAndStacks(gomock.Any(), unlock.Repo, unlock.PRNumber, []string{"a"}).Return(1, nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, feedback.UnlockResultComment("a", models.JobStatusSucceeded, "")).Return(0, nil)

	err := s.authorizedService().RunUnlock(context.Background(), unlock)
	require.NoError(s.T(), err)
}

func (s *UnlockServiceSuite) TestUnlockDeniedForEveryStackDoesNothing() {
	unlock := unlockCmd("terraplane unlock -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return(twoStackYAML, nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "a", authz.ActionUnlock).Return(false, "no", nil)
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).Return(0, nil)
	// No lock or job deletes.

	err := s.authorizedService().RunUnlock(context.Background(), unlock)
	require.NoError(s.T(), err)
}

func (s *UnlockServiceSuite) TestUnlockAuthorizationErrorPublishesFailure() {
	unlock := unlockCmd("terraplane unlock -s a")
	s.scm.EXPECT().GetFile("terraplane.yaml", unlock.CommitSHA, unlock.Repo).Return(twoStackYAML, nil)
	s.authz.EXPECT().Allowed(gomock.Any(), "jace", "acme/infra", "a", authz.ActionUnlock).Return(false, "", errors.New("github down"))
	s.publisher.EXPECT().WriteComment(gomock.Any(), unlock.Repo, unlock.PRNumber, gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, _ int, body string) (int, error) {
			require.Contains(s.T(), body, "unlock ·")
			require.Contains(s.T(), body, "failed to authorize unlock")
			return 0, nil
		},
	)

	err := s.authorizedService().RunUnlock(context.Background(), unlock)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "github down")
}
