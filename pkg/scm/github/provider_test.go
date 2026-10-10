package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/mock/gomock"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/scm"
	"github.com/xyzjace/terraplane/pkg/scm/github/mock_github"
)

type ProviderSuite struct {
	suite.Suite
	ctrl   *gomock.Controller
	client *mock_github.MockClient
	prov   *provider
}

func TestProviderSuite(t *testing.T) {
	suite.Run(t, new(ProviderSuite))
}

func (s *ProviderSuite) SetupTest() {
	s.ctrl = gomock.NewController(s.T())
	s.client = mock_github.NewMockClient(s.ctrl)
	s.prov = NewProvider(log.Noop(), &config.Config{
		OrchestratorGithubWebhookSecret: testWebhookSecret,
	}, s.client).(*provider)
}

func (s *ProviderSuite) TestName() {
	require.Equal(s.T(), "github", s.prov.Name())
}

func (s *ProviderSuite) TestMissingWebhookSecret() {
	p := NewProvider(log.Noop(), &config.Config{}, s.client)
	req, err := http.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	require.NoError(s.T(), err)

	_, err = p.ParseWebhook(req)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "ORCHESTRATOR_GITHUB_WEBHOOK_SECRET")
}

func (s *ProviderSuite) TestMissingSignatureHeader() {
	req, err := http.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	require.NoError(s.T(), err)
	req.Header.Set("X-GitHub-Event", "ping")

	_, err = s.prov.ParseWebhook(req)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "X-Hub-Signature-256")
}

func (s *ProviderSuite) TestInvalidSignature() {
	body := []byte(`{}`)
	req, err := http.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	require.NoError(s.T(), err)
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	_, err = s.prov.ParseWebhook(req)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "signature verification failed")
}

func (s *ProviderSuite) TestMissingEventHeaderReturnsEmpty() {
	body := []byte(`{}`)
	req, err := http.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	require.NoError(s.T(), err)
	req.Header.Set("X-Hub-Signature-256", signBody(s.T(), body, testWebhookSecret))

	got, err := s.prov.ParseWebhook(req)
	require.NoError(s.T(), err)
	require.Nil(s.T(), got)
}

func (s *ProviderSuite) TestPingReturnsEmpty() {
	body := []byte(`{}`)
	got, err := s.prov.ParseWebhook(signedRequest(s.T(), "ping", body))
	require.NoError(s.T(), err)
	require.Nil(s.T(), got)
}

func (s *ProviderSuite) TestUnhandledEventReturnsEmpty() {
	body := []byte(`{}`)
	got, err := s.prov.ParseWebhook(signedRequest(s.T(), "push", body))
	require.NoError(s.T(), err)
	require.Nil(s.T(), got)
}

func (s *ProviderSuite) TestIssueCommentIgnoresNonCreatedAction() {
	body := issueCommentPayload(s.T(), "edited", true, "terraplane plan")
	got, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.NoError(s.T(), err)
	require.Nil(s.T(), got)
}

func (s *ProviderSuite) TestIssueCommentIgnoresNonPullRequest() {
	body := issueCommentPayload(s.T(), "created", false, "terraplane plan")
	got, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.NoError(s.T(), err)
	require.Nil(s.T(), got)
}

func (s *ProviderSuite) TestIssueCommentEmptyBody() {
	body := issueCommentPayload(s.T(), "created", true, "")
	_, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "empty comment body")
}

func (s *ProviderSuite) TestIssueCommentInvalidJSON() {
	body := []byte(`{not-json`)
	_, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to unmarshal")
}

func apiComment(login, body string) scm.IssueComment {
	return scm.IssueComment{
		ID:        1,
		Body:      body,
		UserLogin: login,
		IssueURL:  "https://api.github.com/repos/acme/infra/issues/42",
	}
}

func (s *ProviderSuite) TestIssueCommentGetCommitSHAFailure() {
	body := issueCommentPayload(s.T(), "created", true, "terraplane plan")
	s.client.EXPECT().GetIssueComment(gomock.Any(), "acme/infra", 1).Return(apiComment("jace", "terraplane plan"), nil)
	s.client.EXPECT().GetCommitSHA(gomock.Any(), "acme/infra", 42).Return("", errors.New("api down"))

	_, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to resolve pull request head commit")
}

func (s *ProviderSuite) TestIssueCommentHappyPath() {
	body := issueCommentPayload(s.T(), "created", true, "terraplane plan -s stg")
	s.client.EXPECT().GetIssueComment(gomock.Any(), "acme/infra", 1).Return(apiComment("jace", "terraplane plan -s stg"), nil)
	s.client.EXPECT().GetCommitSHA(gomock.Any(), "acme/infra", 42).Return("abc123", nil)

	got, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.NoError(s.T(), err)
	require.Len(s.T(), got, 1)
	require.Equal(s.T(), "acme/infra", got[0].RepositorySlug)
	require.Equal(s.T(), 42, got[0].PRNumber)
	require.Equal(s.T(), "terraplane plan -s stg", got[0].FullCommand)
	require.Equal(s.T(), "jace", got[0].TriggeringUser)
	require.Equal(s.T(), "abc123", got[0].CommitSHA)
}

func (s *ProviderSuite) TestGetFileDelegatesToClient() {
	s.client.EXPECT().GetFile(gomock.Any(), "acme/infra", "terraplane.yaml", "abc123").Return("stacks: []", nil)
	got, err := s.prov.GetFile("terraplane.yaml", "abc123", "acme/infra")
	require.NoError(s.T(), err)
	require.Equal(s.T(), "stacks: []", got)
}

func (s *ProviderSuite) TestVerifySignatureBodyReadError() {
	req, err := http.NewRequest(http.MethodPost, "/", errReader{})
	require.NoError(s.T(), err)
	req.Header.Set("X-Hub-Signature-256", "sha256=abc")

	_, err = s.prov.verifyWebhookSignature(req)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to read GitHub webhook request body during signature verification")
}

func (s *ProviderSuite) TestIssueCommentBodyReadError() {
	// Signature verification succeeds on empty body; issue_comment then fails re-reading a broken body.
	body := []byte(`{}`)
	req, err := http.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	require.NoError(s.T(), err)
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", signBody(s.T(), body, testWebhookSecret))
	// After verify restores the body, replace it with a failing reader before parseIssueCommentWebhook runs.
	// ParseWebhook calls verify then parseIssueCommentWebhook — inject by calling parse directly.
	req.Body = io.NopCloser(errReader{})
	_, err = s.prov.parseIssueCommentWebhook(req)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to read GitHub issue comment webhook request body")
}

func (s *ProviderSuite) TestTeamMemberDelegatesToClient() {
	s.client.EXPECT().TeamMember(gomock.Any(), "acme", "platform", "alice").Return(true, nil)
	member, err := s.prov.TeamMember(context.Background(), "acme", "platform", "alice")
	require.NoError(s.T(), err)
	require.True(s.T(), member)

	s.client.EXPECT().TeamMember(gomock.Any(), "acme", "platform", "bob").Return(false, errors.New("boom"))
	_, err = s.prov.TeamMember(context.Background(), "acme", "platform", "bob")
	require.Error(s.T(), err)
}

// --- command verification against the API ---

func (s *ProviderSuite) TestIssueCommentUsesAuthorAndBodyFromAPI() {
	// Intention: a forged webhook can only trigger what the real comment says, by the real author.
	body := issueCommentPayload(s.T(), "created", true, "terraplane apply -s prod")
	s.client.EXPECT().GetIssueComment(gomock.Any(), "acme/infra", 1).Return(apiComment("mallory", "terraplane plan -s stg"), nil)
	s.client.EXPECT().GetCommitSHA(gomock.Any(), "acme/infra", 42).Return("abc123", nil)

	got, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.NoError(s.T(), err)
	require.Len(s.T(), got, 1)
	require.Equal(s.T(), "mallory", got[0].TriggeringUser)
	require.Equal(s.T(), "terraplane plan -s stg", got[0].FullCommand)
	require.Equal(s.T(), 1, got[0].CommentID)
}

func (s *ProviderSuite) TestIssueCommentDeletedIsIgnored() {
	body := issueCommentPayload(s.T(), "created", true, "terraplane plan")
	s.client.EXPECT().GetIssueComment(gomock.Any(), "acme/infra", 1).Return(scm.IssueComment{}, fmt.Errorf("gone: %w", ErrNotFound))

	got, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.NoError(s.T(), err)
	require.Nil(s.T(), got)
}

func (s *ProviderSuite) TestIssueCommentFetchFailureIsAnError() {
	body := issueCommentPayload(s.T(), "created", true, "terraplane plan")
	s.client.EXPECT().GetIssueComment(gomock.Any(), "acme/infra", 1).Return(scm.IssueComment{}, errors.New("api down"))

	_, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to fetch comment #1")
}

func (s *ProviderSuite) TestIssueCommentFromAnotherRepoIsIgnored() {
	body := issueCommentPayload(s.T(), "created", true, "terraplane plan")
	comment := apiComment("jace", "terraplane plan")
	comment.IssueURL = "https://api.github.com/repos/acme/other/issues/42"
	s.client.EXPECT().GetIssueComment(gomock.Any(), "acme/infra", 1).Return(comment, nil)

	got, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.NoError(s.T(), err)
	require.Nil(s.T(), got)
}

func (s *ProviderSuite) TestIssueCommentFromAnotherIssueIsIgnored() {
	body := issueCommentPayload(s.T(), "created", true, "terraplane plan")
	comment := apiComment("jace", "terraplane plan")
	comment.IssueURL = "https://api.github.com/repos/acme/infra/issues/7"
	s.client.EXPECT().GetIssueComment(gomock.Any(), "acme/infra", 1).Return(comment, nil)

	got, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.NoError(s.T(), err)
	require.Nil(s.T(), got)
}

func (s *ProviderSuite) TestIssueCommentBadIssueURLIsAnError() {
	body := issueCommentPayload(s.T(), "created", true, "terraplane plan")
	comment := apiComment("jace", "terraplane plan")
	comment.IssueURL = "https://api.github.com/nope"
	s.client.EXPECT().GetIssueComment(gomock.Any(), "acme/infra", 1).Return(comment, nil)

	_, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to verify comment #1")
}

func (s *ProviderSuite) TestIssueCommentChatterSkipsAPI() {
	// No client expectations: ordinary comments must not cost API calls.
	for _, text := range []string{"looks good to me", "please terraplane plan", "terraplaner plan", "   "} {
		body := issueCommentPayload(s.T(), "created", true, text)
		got, err := s.prov.ParseWebhook(signedRequest(s.T(), "issue_comment", body))
		require.NoError(s.T(), err, text)
		require.Nil(s.T(), got, text)
	}
}

func TestLooksLikeCommand(t *testing.T) {
	require.True(t, looksLikeCommand("terraplane plan"))
	require.True(t, looksLikeCommand("  Terraplane apply -s a\nmore text"))
	require.False(t, looksLikeCommand("please terraplane plan"))
	require.False(t, looksLikeCommand(""))
	// Leading blank lines are trimmed, matching how the command parser reads the first line.
	require.True(t, looksLikeCommand("\n\nterraplane plan"))
}

func TestParseIssueURL(t *testing.T) {
	repo, number, err := parseIssueURL("https://api.github.com/repos/acme/infra/issues/42")
	require.NoError(t, err)
	require.Equal(t, "acme/infra", repo)
	require.Equal(t, 42, number)

	repo, number, err = parseIssueURL("https://ghe.example.com/api/v3/repos/acme/infra/issues/7")
	require.NoError(t, err)
	require.Equal(t, "acme/infra", repo)
	require.Equal(t, 7, number)

	for _, raw := range []string{
		"https://api.github.com/repos/acme/infra/pulls/42",
		"https://api.github.com/repos/acme/infra/issues/42/comments",
		"https://api.github.com/repos/acme/infra/issues/x",
		"https://api.github.com/nope",
		"://bad",
	} {
		_, _, err := parseIssueURL(raw)
		require.Error(t, err, raw)
	}
}

func (s *ProviderSuite) TestApplyGatesDelegateToClient() {
	s.client.EXPECT().PullRequestApproved(gomock.Any(), "acme/infra", 42).Return(true, nil)
	approved, err := s.prov.PullRequestApproved(context.Background(), "acme/infra", 42)
	require.NoError(s.T(), err)
	require.True(s.T(), approved)

	s.client.EXPECT().ChecksGreen(gomock.Any(), "acme/infra", "abc123").Return(false, errors.New("boom"))
	_, err = s.prov.ChecksGreen(context.Background(), "acme/infra", "abc123")
	require.Error(s.T(), err)
}
