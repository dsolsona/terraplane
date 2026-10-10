package webserver_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/mock/gomock"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/command"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestrator/services"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
	"github.com/xyzjace/terraplane/pkg/scm"
	"github.com/xyzjace/terraplane/pkg/scm/mock_scm"
	"github.com/xyzjace/terraplane/pkg/webserver"
)

type stubJobs struct {
	err         error
	called      chan *scm.Webhook
	claimCmd    *command.Command
	claimErr    error
	claimed     []string
	refreshErr  error
	refreshed   []string
	ackErr      error
	acked       []string
	commitErr   error
	committed   []commitCall
	periodicErr error
	periodics   []periodicCall
}

type commitCall struct {
	jobID  string
	result string
	output string
	errMsg string
}

type periodicCall struct {
	jobID  string
	output string
}

func (s *stubJobs) CreatePendingJobs(_ context.Context, webhook *scm.Webhook) error {
	if s.called != nil {
		s.called <- webhook
	}
	return s.err
}

func (s *stubJobs) ClaimPendingJob(_ context.Context, agentID string) (*command.Command, error) {
	s.claimed = append(s.claimed, agentID)
	if agentID == "" {
		return nil, services.ErrEmptyAgentID
	}
	return s.claimCmd, s.claimErr
}
func (s *stubJobs) ReleaseClaim(context.Context, string) error { return nil }
func (s *stubJobs) FailClaimedJob(context.Context, string, string) error {
	return nil
}
func (s *stubJobs) ReapExpiredClaims(context.Context) error  { return nil }
func (s *stubJobs) CleanupExpiredJobs(context.Context) error { return nil }
func (s *stubJobs) RefreshAgentClaims(_ context.Context, agentID string) error {
	s.refreshed = append(s.refreshed, agentID)
	return s.refreshErr
}
func (s *stubJobs) AckJob(_ context.Context, jobID, agentID string) error {
	s.acked = append(s.acked, jobID)
	return s.ackErr
}
func (s *stubJobs) CommitJobResult(_ context.Context, jobID, agentID, result, output, errMsg string) error {
	s.committed = append(s.committed, commitCall{
		jobID: jobID, result: result, output: output, errMsg: errMsg,
	})
	return s.commitErr
}
func (s *stubJobs) HandlePeriodicResult(_ context.Context, jobID, agentID, output string) error {
	s.periodics = append(s.periodics, periodicCall{jobID: jobID, output: output})
	return s.periodicErr
}

type stubUnlock struct {
	err error
	ran []command.UnlockCommand
}

func (s *stubUnlock) RunUnlock(_ context.Context, unlock command.UnlockCommand) error {
	s.ran = append(s.ran, unlock)
	return s.err
}

type HandlerSuite struct {
	suite.Suite
	ctrl      *gomock.Controller
	scm       *mock_scm.MockProvider
	publisher *mock_scm.MockPublisher
	jobs      *stubJobs
	unlocks   *stubUnlock
	handler   http.Handler
}

func TestHandlerSuite(t *testing.T) {
	suite.Run(t, new(HandlerSuite))
}

func (s *HandlerSuite) SetupTest() {
	s.ctrl = gomock.NewController(s.T())
	s.scm = mock_scm.NewMockProvider(s.ctrl)
	s.publisher = mock_scm.NewMockPublisher(s.ctrl)
	s.jobs = &stubJobs{called: make(chan *scm.Webhook, 8)}
	s.unlocks = &stubUnlock{}
	s.handler = s.newHandler()
}

func (s *HandlerSuite) newHandler() http.Handler {
	return webserver.NewHandler(
		log.Noop(),
		s.scm,
		s.publisher,
		s.jobs,
		s.unlocks,
		&config.Config{SharedAuthToken: "secret"},
		&orchestratorconfig.ConfigFile{},
	)
}

func (s *HandlerSuite) TestHealthCheck() {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusOK, rec.Code)
	require.Equal(s.T(), "OK", rec.Body.String())
}

func (s *HandlerSuite) TestWebhookParseFailure() {
	s.scm.EXPECT().ParseWebhook(gomock.Any()).Return(nil, errors.New("bad signature"))

	req := httptest.NewRequest(http.MethodPost, "/scm/webhook", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
	require.Contains(s.T(), rec.Body.String(), "Failed to parse SCM webhook")
}

func (s *HandlerSuite) TestWebhookNoActionableEvents() {
	s.scm.EXPECT().ParseWebhook(gomock.Any()).Return(nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/scm/webhook", nil)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	require.Equal(s.T(), http.StatusOK, rec.Code)
	require.Contains(s.T(), rec.Body.String(), "No actionable event found")
}

func (s *HandlerSuite) TestWebhookIgnoresUnknownCommands() {
	s.scm.EXPECT().ParseWebhook(gomock.Any()).Return([]scm.Webhook{{
		RepositorySlug: "acme/infra",
		PRNumber:       1,
		FullCommand:    "not a terraplane command",
		TriggeringUser: "jace",
		CommitSHA:      "abc",
	}}, nil)

	req := httptest.NewRequest(http.MethodPost, "/scm/webhook", nil)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	require.Equal(s.T(), http.StatusOK, rec.Code)
	require.Contains(s.T(), rec.Body.String(), "Webhook parsed successfully")

	// Neither CreatePendingJobs nor AcknowledgeComment should be called for non-terraplane comments.
	select {
	case got := <-s.jobs.called:
		s.T().Fatalf("CreatePendingJobs should not have been called, but got: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func (s *HandlerSuite) TestWebhookEnqueuesPendingJobs() {
	s.scm.EXPECT().ParseWebhook(gomock.Any()).Return([]scm.Webhook{
		{RepositorySlug: "acme/infra", PRNumber: 1, FullCommand: "terraplane plan -s a", TriggeringUser: "jace", CommitSHA: "abc", CommentID: 11},
		{RepositorySlug: "acme/infra", PRNumber: 1, FullCommand: "terraplane apply -s a", TriggeringUser: "jace", CommitSHA: "abc", CommentID: 12},
		{RepositorySlug: "acme/infra", PRNumber: 1, FullCommand: "terraplane unlock -s a", TriggeringUser: "jace", CommitSHA: "abc", CommentID: 13},
	}, nil)
	s.publisher.EXPECT().AcknowledgeComment(gomock.Any(), "acme/infra", 1, 11).Return(nil)
	s.publisher.EXPECT().AcknowledgeComment(gomock.Any(), "acme/infra", 1, 12).Return(nil)
	s.publisher.EXPECT().AcknowledgeComment(gomock.Any(), "acme/infra", 1, 13).Return(nil)

	req := httptest.NewRequest(http.MethodPost, "/scm/webhook", nil)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusOK, rec.Code)

	for _, want := range []string{"terraplane plan -s a", "terraplane apply -s a"} {
		select {
		case got := <-s.jobs.called:
			require.Equal(s.T(), want, got.FullCommand)
		case <-time.After(2 * time.Second):
			s.T().Fatalf("timed out waiting for CreatePendingJobs (%s)", want)
		}
	}
	require.Len(s.T(), s.unlocks.ran, 1)
	require.Equal(s.T(), []string{"a"}, s.unlocks.ran[0].Stacks)
}

func (s *HandlerSuite) TestWebhookServiceErrorsAreLoggedNotReturned() {
	s.jobs.err = errors.New("upsert failed")

	s.scm.EXPECT().ParseWebhook(gomock.Any()).Return([]scm.Webhook{
		{RepositorySlug: "acme/infra", PRNumber: 1, FullCommand: "terraplane plan", TriggeringUser: "jace", CommitSHA: "abc", CommentID: 21},
	}, nil)
	s.publisher.EXPECT().AcknowledgeComment(gomock.Any(), "acme/infra", 1, 21).Return(nil)

	req := httptest.NewRequest(http.MethodPost, "/scm/webhook", nil)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusOK, rec.Code)

	select {
	case <-s.jobs.called:
	case <-time.After(2 * time.Second):
		s.T().Fatal("timed out waiting for CreatePendingJobs")
	}
}

func (s *HandlerSuite) TestWebhookAcknowledgeFailureStillEnqueues() {
	s.scm.EXPECT().ParseWebhook(gomock.Any()).Return([]scm.Webhook{{
		RepositorySlug: "acme/infra",
		PRNumber:       1,
		FullCommand:    "terraplane plan -s a",
		TriggeringUser: "jace",
		CommitSHA:      "abc",
		CommentID:      99,
	}}, nil)
	s.publisher.EXPECT().AcknowledgeComment(gomock.Any(), "acme/infra", 1, 99).Return(errors.New("react failed"))

	req := httptest.NewRequest(http.MethodPost, "/scm/webhook", nil)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusOK, rec.Code)

	select {
	case got := <-s.jobs.called:
		require.Equal(s.T(), "terraplane plan -s a", got.FullCommand)
	case <-time.After(2 * time.Second):
		s.T().Fatal("timed out waiting for CreatePendingJobs")
	}
}

func (s *HandlerSuite) TestBearerRequiredOnAgentRoutes() {
	paths := []string{
		"/agent/jobs/claim",
		"/agent/jobs/job-1/heartbeat",
		"/agent/jobs/job-1/ack",
		"/agent/jobs/job-1/result",
	}
	for _, path := range paths {
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		require.Equal(s.T(), http.StatusUnauthorized, rec.Code, path)

		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rec = httptest.NewRecorder()
		s.handler.ServeHTTP(rec, req)
		require.Equal(s.T(), http.StatusUnauthorized, rec.Code, path)
	}
}

func (s *HandlerSuite) TestAgentRoutesAcceptBearerToken() {
	for _, path := range []string{
		"/agent/jobs/job-1/heartbeat",
		"/agent/jobs/job-1/ack",
		"/agent/jobs/job-1/result",
	} {
		body := `{"agent_id":"agent-dev"}`
		if path == "/agent/jobs/job-1/result" {
			body = `{"agent_id":"agent-dev","result":"success"}`
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, req)
		require.Equal(s.T(), http.StatusNoContent, rec.Code, path)
		require.Empty(s.T(), rec.Body.String(), path)
	}
}

func (s *HandlerSuite) TestAgentClaimEmptyAgentID() {
	req := httptest.NewRequest(http.MethodPost, "/agent/jobs/claim", strings.NewReader(`{"agent_id":""}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusBadRequest, rec.Code)
	require.Contains(s.T(), rec.Body.String(), "agent_id is required")
}

func (s *HandlerSuite) TestAgentClaimNoJob() {
	req := httptest.NewRequest(http.MethodPost, "/agent/jobs/claim", strings.NewReader(`{"agent_id":"agent-dev"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusNoContent, rec.Code)
	require.Empty(s.T(), rec.Body.String())
}

func (s *HandlerSuite) TestAgentClaimNoJobHasNoJSONContentType() {
	req := httptest.NewRequest(http.MethodPost, "/agent/jobs/claim", strings.NewReader(`{"agent_id":"agent-dev"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Empty(s.T(), rec.Header().Get("Content-Type"))
}

func (s *HandlerSuite) TestAgentClaimReturnsJob() {
	cmd := command.Command{Kind: command.KindPlan}
	cmd.Plan.JobID = "job-1"
	s.jobs.claimCmd = &cmd

	req := httptest.NewRequest(http.MethodPost, "/agent/jobs/claim", strings.NewReader(`{"agent_id":"agent-dev"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusOK, rec.Code)
	require.Equal(s.T(), "application/json", rec.Header().Get("Content-Type"))
	require.Contains(s.T(), rec.Body.String(), `"Kind":"plan"`)
	require.Contains(s.T(), rec.Body.String(), "job-1")
}

func (s *HandlerSuite) TestAgentClaimInvalidJSON() {
	req := httptest.NewRequest(http.MethodPost, "/agent/jobs/claim", strings.NewReader(`{`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
}

func (s *HandlerSuite) TestAgentClaimServiceError() {
	s.jobs.claimErr = errors.New("db")
	req := httptest.NewRequest(http.MethodPost, "/agent/jobs/claim", strings.NewReader(`{"agent_id":"agent-dev"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
}

func (s *HandlerSuite) agentPOST(path, body string) *httptest.ResponseRecorder {
	s.T().Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func (s *HandlerSuite) TestAgentHeartbeatExtendsClaims() {
	rec := s.agentPOST("/agent/jobs/job-1/heartbeat", `{"agent_id":"agent-dev"}`)
	require.Equal(s.T(), http.StatusNoContent, rec.Code)
	require.Equal(s.T(), []string{"agent-dev"}, s.jobs.refreshed)
}

func (s *HandlerSuite) TestAgentHeartbeatInvalidJSON() {
	rec := s.agentPOST("/agent/jobs/job-1/heartbeat", `{`)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
	require.Empty(s.T(), s.jobs.refreshed)
}

func (s *HandlerSuite) TestAgentHeartbeatServiceError() {
	s.jobs.refreshErr = errors.New("db")
	rec := s.agentPOST("/agent/jobs/job-1/heartbeat", `{"agent_id":"agent-dev"}`)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
	require.Equal(s.T(), []string{"agent-dev"}, s.jobs.refreshed)
}

func (s *HandlerSuite) TestAgentAckMarksJobRunning() {
	rec := s.agentPOST("/agent/jobs/job-1/ack", `{"agent_id":"agent-dev"}`)
	require.Equal(s.T(), http.StatusNoContent, rec.Code)
	require.Equal(s.T(), []string{"job-1"}, s.jobs.acked)
}

func (s *HandlerSuite) TestAgentAckInvalidJSON() {
	rec := s.agentPOST("/agent/jobs/job-1/ack", `{`)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
	require.Empty(s.T(), s.jobs.acked)
}

func (s *HandlerSuite) TestAgentAckServiceError() {
	s.jobs.ackErr = errors.New("db")
	rec := s.agentPOST("/agent/jobs/job-1/ack", `{"agent_id":"agent-dev"}`)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
	require.Equal(s.T(), []string{"job-1"}, s.jobs.acked)
}

func (s *HandlerSuite) TestAgentResultCommitsJob() {
	rec := s.agentPOST("/agent/jobs/job-1/result", `{"agent_id":"agent-dev","result":"success","output":"ok"}`)
	require.Equal(s.T(), http.StatusNoContent, rec.Code)
	require.Empty(s.T(), rec.Body.String())
	require.Equal(s.T(), []commitCall{{
		jobID: "job-1", result: "success", output: "ok",
	}}, s.jobs.committed)
}

func (s *HandlerSuite) TestAgentResultInvalidJSON() {
	rec := s.agentPOST("/agent/jobs/job-1/result", `{`)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
	require.Empty(s.T(), s.jobs.committed)
}

func (s *HandlerSuite) TestAgentResultServiceError() {
	s.jobs.commitErr = errors.New("db")
	rec := s.agentPOST("/agent/jobs/job-1/result", `{"agent_id":"agent-dev","result":"failed","error":"boom"}`)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
	require.Equal(s.T(), []commitCall{{
		jobID: "job-1", result: "failed", errMsg: "boom",
	}}, s.jobs.committed)
}

func (s *HandlerSuite) TestAgentPeriodicResultSuccess() {
	rec := s.agentPOST("/agent/jobs/job-1/periodic_result", `{"agent_id":"agent-dev","output":"partial"}`)
	require.Equal(s.T(), http.StatusNoContent, rec.Code)
	require.Empty(s.T(), rec.Body.String())
	require.Equal(s.T(), []periodicCall{{
		jobID: "job-1", output: "partial",
	}}, s.jobs.periodics)
}

func (s *HandlerSuite) TestAgentPeriodicResultInvalidJSON() {
	rec := s.agentPOST("/agent/jobs/job-1/periodic_result", `{`)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
	require.Empty(s.T(), s.jobs.periodics)
}

func (s *HandlerSuite) TestAgentPeriodicResultServiceError() {
	s.jobs.periodicErr = errors.New("db")
	rec := s.agentPOST("/agent/jobs/job-1/periodic_result", `{"agent_id":"agent-dev","output":"partial"}`)
	require.Equal(s.T(), http.StatusInternalServerError, rec.Code)
	require.Equal(s.T(), []periodicCall{{
		jobID: "job-1", output: "partial",
	}}, s.jobs.periodics)
}

func TestServerStartShutdown(t *testing.T) {
	cfg := &config.Config{
		OrchestratorListenAddress: "127.0.0.1",
		OrchestratorListenPort:    0, // ephemeral
	}
	srv := webserver.NewServer(cfg, log.Noop(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- srv.Start(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-started:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Start to return on cancel")
	}

	require.NoError(t, srv.Shutdown(context.Background()))
}

func (s *HandlerSuite) TestWebhookUnlockServiceErrorIsLoggedNotReturned() {
	s.unlocks.err = errors.New("unlock failed")
	s.scm.EXPECT().ParseWebhook(gomock.Any()).Return([]scm.Webhook{{
		RepositorySlug: "acme/infra",
		PRNumber:       1,
		FullCommand:    "terraplane unlock -s a",
		TriggeringUser: "jace",
		CommitSHA:      "abc",
		CommentID:      13,
	}}, nil)
	s.publisher.EXPECT().AcknowledgeComment(gomock.Any(), "acme/infra", 1, 13).Return(nil)

	req := httptest.NewRequest(http.MethodPost, "/scm/webhook", nil)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(s.T(), http.StatusOK, rec.Code)
	require.Len(s.T(), s.unlocks.ran, 1)
}

func TestServerStartListenError(t *testing.T) {
	cfg := &config.Config{
		OrchestratorListenAddress: "127.0.0.1",
		OrchestratorListenPort:    -1, // invalid
	}
	srv := webserver.NewServer(cfg, log.Noop(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	err := srv.Start(context.Background())
	require.Error(t, err)
}

// --- per-agent tokens ---

const agentTokensYAML = `
agents:
  agent-prod:
    token_env: AGENT_TOKEN_PROD
  agent-data:
    token_env: AGENT_TOKEN_DATA
repos:
  acme/infra:
    agent: agent-prod
`

func (s *HandlerSuite) newHandlerWithAgentTokens() http.Handler {
	cfgFile, err := orchestratorconfig.Parse([]byte(agentTokensYAML), func(key string) string {
		return map[string]string{
			"AGENT_TOKEN_PROD": "prod-token",
			"AGENT_TOKEN_DATA": "data-token",
		}[key]
	})
	require.NoError(s.T(), err)
	return webserver.NewHandler(
		log.Noop(),
		s.scm,
		s.publisher,
		s.jobs,
		s.unlocks,
		&config.Config{SharedAuthToken: "secret"},
		cfgFile,
	)
}

func (s *HandlerSuite) tokenPOST(h http.Handler, token, path, body string) *httptest.ResponseRecorder {
	s.T().Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (s *HandlerSuite) TestAgentTokenIdentifiesAgent() {
	h := s.newHandlerWithAgentTokens()

	rec := s.tokenPOST(h, "prod-token", "/agent/jobs/claim", `{}`)
	require.Equal(s.T(), http.StatusNoContent, rec.Code)
	require.Equal(s.T(), []string{"agent-prod"}, s.jobs.claimed)

	rec = s.tokenPOST(h, "data-token", "/agent/jobs/job-1/heartbeat", `{}`)
	require.Equal(s.T(), http.StatusNoContent, rec.Code)
	require.Equal(s.T(), []string{"agent-data"}, s.jobs.refreshed)
}

func (s *HandlerSuite) TestAgentTokenAcceptsMatchingBodyAgentID() {
	h := s.newHandlerWithAgentTokens()

	rec := s.tokenPOST(h, "prod-token", "/agent/jobs/job-1/heartbeat", `{"agent_id":"agent-prod"}`)
	require.Equal(s.T(), http.StatusNoContent, rec.Code)
	require.Equal(s.T(), []string{"agent-prod"}, s.jobs.refreshed)
}

func (s *HandlerSuite) TestAgentTokenRejectsMismatchedBodyAgentID() {
	h := s.newHandlerWithAgentTokens()
	for _, path := range []string{
		"/agent/jobs/claim",
		"/agent/jobs/job-1/heartbeat",
		"/agent/jobs/job-1/ack",
		"/agent/jobs/job-1/result",
		"/agent/jobs/job-1/periodic_result",
	} {
		rec := s.tokenPOST(h, "prod-token", path, `{"agent_id":"agent-data"}`)
		require.Equal(s.T(), http.StatusForbidden, rec.Code, path)
		require.Contains(s.T(), rec.Body.String(), "agent_id does not match token", path)
	}
	require.Empty(s.T(), s.jobs.claimed)
	require.Empty(s.T(), s.jobs.refreshed)
	require.Empty(s.T(), s.jobs.acked)
	require.Empty(s.T(), s.jobs.committed)
	require.Empty(s.T(), s.jobs.periodics)
}

func (s *HandlerSuite) TestAgentTokenRejectsUnknownAndSharedTokens() {
	h := s.newHandlerWithAgentTokens()
	for _, token := range []string{"", "wrong", "secret"} {
		rec := s.tokenPOST(h, token, "/agent/jobs/job-1/heartbeat", `{}`)
		require.Equal(s.T(), http.StatusUnauthorized, rec.Code, token)
	}
	require.Empty(s.T(), s.jobs.refreshed)
}

func (s *HandlerSuite) TestAgentTokenReachesEveryAgentRoute() {
	h := s.newHandlerWithAgentTokens()

	require.Equal(s.T(), http.StatusNoContent, s.tokenPOST(h, "prod-token", "/agent/jobs/job-1/ack", `{}`).Code)
	require.Equal(s.T(), http.StatusNoContent, s.tokenPOST(h, "prod-token", "/agent/jobs/job-1/result", `{"result":"success"}`).Code)
	require.Equal(s.T(), http.StatusNoContent, s.tokenPOST(h, "prod-token", "/agent/jobs/job-1/periodic_result", `{"output":"x"}`).Code)
	require.Equal(s.T(), []string{"job-1"}, s.jobs.acked)
	require.Len(s.T(), s.jobs.committed, 1)
	require.Len(s.T(), s.jobs.periodics, 1)
}

func (s *HandlerSuite) TestWebhookRouteIgnoresAgentTokens() {
	h := s.newHandlerWithAgentTokens()
	s.scm.EXPECT().ParseWebhook(gomock.Any()).Return(nil, nil)

	rec := s.tokenPOST(h, "", "/scm/webhook", `{}`)
	require.Equal(s.T(), http.StatusOK, rec.Code)
}

func (s *HandlerSuite) TestWebhookIgnoresRepoNotInOrchestratorConfig() {
	h := s.newHandlerWithAgentTokens()
	s.scm.EXPECT().ParseWebhook(gomock.Any()).Return([]scm.Webhook{
		{RepositorySlug: "acme/unknown", PRNumber: 1, FullCommand: "terraplane plan", TriggeringUser: "jace", CommitSHA: "abc", CommentID: 31},
		{RepositorySlug: "acme/unknown", PRNumber: 1, FullCommand: "terraplane unlock -s a", TriggeringUser: "jace", CommitSHA: "abc", CommentID: 32},
		{RepositorySlug: "acme/infra", PRNumber: 2, FullCommand: "terraplane plan", TriggeringUser: "jace", CommitSHA: "abc", CommentID: 33},
	}, nil)
	// Only the listed repo is acknowledged.
	s.publisher.EXPECT().AcknowledgeComment(gomock.Any(), "acme/infra", 2, 33).Return(nil)

	rec := s.tokenPOST(h, "", "/scm/webhook", `{}`)
	require.Equal(s.T(), http.StatusOK, rec.Code)

	select {
	case got := <-s.jobs.called:
		require.Equal(s.T(), "acme/infra", got.RepositorySlug)
	case <-time.After(2 * time.Second):
		s.T().Fatal("timed out waiting for CreatePendingJobs")
	}
	select {
	case got := <-s.jobs.called:
		s.T().Fatalf("CreatePendingJobs should not have been called for %s", got.RepositorySlug)
	default:
	}
	require.Empty(s.T(), s.unlocks.ran)
}
