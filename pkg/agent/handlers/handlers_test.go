package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"go.uber.org/mock/gomock"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/internal/process"
	"github.com/xyzjace/terraplane/pkg/agent/orchestrator/mock_orchestrator"
	"github.com/xyzjace/terraplane/pkg/agent/terraform/mock_terraform"
	"github.com/xyzjace/terraplane/pkg/agent/workspace/mock_workspace"
	"github.com/xyzjace/terraplane/pkg/command"
	"github.com/xyzjace/terraplane/pkg/log"
)

type HandlersSuite struct {
	suite.Suite
	ctrl *gomock.Controller
	ws   *mock_workspace.MockManager
	tf   *mock_terraform.MockManager
	oc   *mock_orchestrator.MockClient
}

func TestHandlersSuite(t *testing.T) {
	suite.Run(t, new(HandlersSuite))
}

func (s *HandlersSuite) SetupTest() {
	s.ctrl = gomock.NewController(s.T())
	s.ws = mock_workspace.NewMockManager(s.ctrl)
	s.tf = mock_terraform.NewMockManager(s.ctrl)
	s.oc = mock_orchestrator.NewMockClient(s.ctrl)
}

func (s *HandlersSuite) newHandlers() *Handlers {
	return New(log.Noop(), &config.Config{AgentID: "agent-test"}, s.ws, s.tf, s.oc)
}

func planCmd() *command.PlanCommand {
	cmd := &command.PlanCommand{
		Stacks:    []string{"stg"},
		PlanFlags: "-target=module.vpc",
	}
	cmd.JobID = "job-1"
	cmd.Repo = "acme/infra"
	cmd.CommitSHA = "abc123"
	cmd.PRNumber = 42
	cmd.Tool = "tofu"
	cmd.ToolVersion = "1.5.0"
	return cmd
}

func applyCmd() *command.ApplyCommand {
	cmd := &command.ApplyCommand{
		Stacks: []string{"stg"},
	}
	cmd.JobID = "job-1"
	cmd.Repo = "acme/infra"
	cmd.CommitSHA = "abc123"
	cmd.PRNumber = 42
	cmd.Tool = "tofu"
	cmd.ToolVersion = "1.5.0"
	return cmd
}

func unlockCmd() *command.UnlockCommand {
	cmd := &command.UnlockCommand{}
	cmd.JobID = "job-1"
	cmd.Repo = "acme/infra"
	cmd.PRNumber = 42
	return cmd
}

// --- Plan ---

func (s *HandlersSuite) TestHandlePlanSuccessKeepsWorkspace() {
	// Intention: successful plans leave the workspace in place for a later apply.
	h := s.newHandlers()

	s.ws.EXPECT().ProvisionWorkspace(gomock.Any(), "job-1", "acme/infra", "abc123", "stg").Return("/tmp/ws", nil)
	s.tf.EXPECT().RunPlan(gomock.Any(), "/tmp/ws", "stg", "tofu", "1.5.0", "-target=module.vpc", gomock.Any()).Return("Plan: 1 to add", nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), "job-1", "agent-test", true, "Plan: 1 to add", "").Return(nil)
	// No RemoveWorkspace on success.

	h.handlePlan(context.Background(), planCmd())
}

func (s *HandlersSuite) TestHandlePlanProvisionFailureSubmitsError() {
	h := s.newHandlers()

	s.ws.EXPECT().ProvisionWorkspace(gomock.Any(), "job-1", "acme/infra", "abc123", "stg").Return("", errors.New("clone failed"))
	s.oc.EXPECT().SubmitResult(gomock.Any(), "job-1", "agent-test", false, "", gomock.Any()).Return(nil)

	h.handlePlan(context.Background(), planCmd())
}

func (s *HandlersSuite) TestHandlePlanProvisionFailureSubmitErrorIsLoggedNotReturned() {
	h := s.newHandlers()
	s.ws.EXPECT().ProvisionWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("", errors.New("clone failed"))
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), false, gomock.Any(), gomock.Any()).Return(errors.New("submit failed"))

	h.handlePlan(context.Background(), planCmd())
}

func (s *HandlersSuite) TestHandlePlanTerraformFailureRemovesWorkspace() {
	h := s.newHandlers()

	s.ws.EXPECT().ProvisionWorkspace(gomock.Any(), "job-1", "acme/infra", "abc123", "stg").Return("/tmp/ws", nil)
	s.tf.EXPECT().RunPlan(gomock.Any(), "/tmp/ws", "stg", "tofu", "1.5.0", "-target=module.vpc", gomock.Any()).Return("partial", errors.New("tf failed"))
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), "job-1", "agent-test", false, "partial", gomock.Any()).Return(nil)

	h.handlePlan(context.Background(), planCmd())
}

func (s *HandlersSuite) TestHandlePlanTerraformFailureRemoveWorkspaceErrorStillSubmitsResult() {
	h := s.newHandlers()

	s.ws.EXPECT().ProvisionWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("/tmp/ws", nil)
	s.tf.EXPECT().RunPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("", errors.New("tf failed"))
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(errors.New("rm failed"))
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), false, gomock.Any(), gomock.Any()).Return(nil)

	h.handlePlan(context.Background(), planCmd())
}

func (s *HandlersSuite) TestHandlePlanTerraformFailureSubmitError() {
	h := s.newHandlers()
	s.ws.EXPECT().ProvisionWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("/tmp/ws", nil)
	s.tf.EXPECT().RunPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("", errors.New("tf failed"))
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), false, gomock.Any(), gomock.Any()).Return(errors.New("submit failed"))

	h.handlePlan(context.Background(), planCmd())
}

func (s *HandlersSuite) TestHandlePlanSuccessSubmitFailureRemovesWorkspace() {
	// Intention: failing to deliver a successful result still cleans up (err is set from writeErr).
	h := s.newHandlers()
	s.ws.EXPECT().ProvisionWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("/tmp/ws", nil)
	s.tf.EXPECT().RunPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("ok", nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), true, "ok", "").Return(errors.New("submit failed"))
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(nil)

	h.handlePlan(context.Background(), planCmd())
}

// --- Apply ---

func (s *HandlersSuite) TestHandleApplySuccessAlwaysRemovesWorkspace() {
	// Intention: apply always tears down the workspace, success or failure.
	h := s.newHandlers()

	s.ws.EXPECT().FetchWorkspace(gomock.Any(), "acme/infra", "abc123", "stg").Return("/tmp/ws", nil)
	s.tf.EXPECT().RunApply(gomock.Any(), "/tmp/ws", "stg", "tofu", "1.5.0", gomock.Any()).Return("Apply complete!", nil)
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), "job-1", "agent-test", true, "Apply complete!", "").Return(nil)

	h.handleApply(context.Background(), applyCmd())
}

func (s *HandlersSuite) TestHandleApplyFetchFailureSubmitsError() {
	h := s.newHandlers()
	s.ws.EXPECT().FetchWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("", errors.New("missing ws"))
	s.oc.EXPECT().SubmitResult(gomock.Any(), "job-1", "agent-test", false, "", gomock.Any()).Return(nil)

	h.handleApply(context.Background(), applyCmd())
}

func (s *HandlersSuite) TestHandleApplyFetchFailureSubmitErrorIsLoggedNotReturned() {
	h := s.newHandlers()
	s.ws.EXPECT().FetchWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("", errors.New("missing ws"))
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), false, gomock.Any(), gomock.Any()).Return(errors.New("submit failed"))

	h.handleApply(context.Background(), applyCmd())
}

func (s *HandlersSuite) TestHandleApplyTerraformFailureRemovesWorkspace() {
	h := s.newHandlers()
	s.ws.EXPECT().FetchWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("/tmp/ws", nil)
	s.tf.EXPECT().RunApply(gomock.Any(), "/tmp/ws", "stg", "tofu", "1.5.0", gomock.Any()).Return("out", errors.New("apply failed"))
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), "job-1", "agent-test", false, "out", gomock.Any()).Return(nil)

	h.handleApply(context.Background(), applyCmd())
}

func (s *HandlersSuite) TestHandleApplyTerraformFailureSubmitError() {
	h := s.newHandlers()
	s.ws.EXPECT().FetchWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("/tmp/ws", nil)
	s.tf.EXPECT().RunApply(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("", errors.New("apply failed"))
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), false, gomock.Any(), gomock.Any()).Return(errors.New("submit failed"))

	h.handleApply(context.Background(), applyCmd())
}

func (s *HandlersSuite) TestHandleApplyRemoveWorkspaceErrorIsBestEffort() {
	h := s.newHandlers()
	s.ws.EXPECT().FetchWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("/tmp/ws", nil)
	s.tf.EXPECT().RunApply(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("ok", nil)
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(errors.New("rm failed"))
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), true, "ok", "").Return(nil)

	h.handleApply(context.Background(), applyCmd())
}

func (s *HandlersSuite) TestHandleApplySuccessSubmitFailureStillRemovesWorkspace() {
	h := s.newHandlers()
	s.ws.EXPECT().FetchWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("/tmp/ws", nil)
	s.tf.EXPECT().RunApply(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("ok", nil)
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), true, "ok", "").Return(errors.New("submit failed"))

	h.handleApply(context.Background(), applyCmd())
}

// --- Unlock ---

func (s *HandlersSuite) TestDispatchUnlockIsIgnored() {
	h := s.newHandlers()
	done := make(chan struct{})
	h.Dispatch(context.Background(), &command.Command{Kind: command.KindUnlock, Unlock: *unlockCmd()}, done)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		s.T().Fatal("timed out waiting for dispatch goroutine")
	}
}

func (s *HandlersSuite) TestDispatchUnknownKindIsIgnored() {
	h := s.newHandlers()
	done := make(chan struct{})
	// No mock expectations — nothing should be called.
	h.Dispatch(context.Background(), &command.Command{Kind: command.KindUnknown}, done)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		s.T().Fatal("timed out waiting for dispatch goroutine")
	}
}

func (s *HandlersSuite) TestDispatchPlanRunsHandler() {
	done := make(chan struct{})
	h := s.newHandlers()

	s.ws.EXPECT().ProvisionWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("/tmp/ws", nil)
	s.tf.EXPECT().RunPlan(gomock.Any(), "/tmp/ws", "stg", "tofu", "1.5.0", gomock.Any(), gomock.Any()).Return("ok", nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), true, "ok", "").Return(nil)

	h.Dispatch(context.Background(), &command.Command{Kind: command.KindPlan, Plan: *planCmd()}, done)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s.T().Fatal("timed out waiting for plan handler")
	}
}

func (s *HandlersSuite) TestDispatchApplyRunsHandler() {
	done := make(chan struct{})
	h := s.newHandlers()

	s.ws.EXPECT().FetchWorkspace(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("/tmp/ws", nil)
	s.tf.EXPECT().RunApply(gomock.Any(), "/tmp/ws", "stg", "tofu", "1.5.0", gomock.Any()).Return("ok", nil)
	s.ws.EXPECT().RemoveWorkspace(gomock.Any(), "/tmp/ws").Return(nil)
	s.oc.EXPECT().SubmitResult(gomock.Any(), gomock.Any(), gomock.Any(), true, "ok", "").Return(nil)

	h.Dispatch(context.Background(), &command.Command{Kind: command.KindApply, Apply: *applyCmd()}, done)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s.T().Fatal("timed out waiting for apply handler")
	}
}

func (s *HandlersSuite) TestWatchOutputSubmitsChangedSnapshotsOnly() {
	h := s.newHandlers()
	h.periodicInterval = 15 * time.Millisecond
	out := &process.Buffer{}

	first := make(chan struct{})
	second := make(chan struct{})
	s.oc.EXPECT().SubmitPeriodicResult(gomock.Any(), "job-1", "agent-test", "hello").DoAndReturn(
		func(context.Context, string, string, string) error {
			close(first)
			return nil
		},
	)
	s.oc.EXPECT().SubmitPeriodicResult(gomock.Any(), "job-1", "agent-test", "hello\nworld").DoAndReturn(
		func(context.Context, string, string, string) error {
			close(second)
			return nil
		},
	)

	stop := h.watchOutput(context.Background(), "job-1", out)
	defer stop()

	_, err := out.Write([]byte("hello"))
	s.Require().NoError(err)
	select {
	case <-first:
	case <-time.After(time.Second):
		s.T().Fatal("timed out waiting for first periodic result")
	}

	_, err = out.Write([]byte("\nworld"))
	s.Require().NoError(err)
	select {
	case <-second:
	case <-time.After(time.Second):
		s.T().Fatal("timed out waiting for second periodic result")
	}
}

func (s *HandlersSuite) TestWatchOutputSkipsEmptyAndRetriesAfterError() {
	h := s.newHandlers()
	h.periodicInterval = 15 * time.Millisecond
	out := &process.Buffer{}

	stop := h.watchOutput(context.Background(), "job-1", out)
	defer stop()
	time.Sleep(40 * time.Millisecond)

	retried := make(chan struct{})
	s.oc.EXPECT().SubmitPeriodicResult(gomock.Any(), "job-1", "agent-test", "hello").Return(errors.New("boom"))
	s.oc.EXPECT().SubmitPeriodicResult(gomock.Any(), "job-1", "agent-test", "hello").DoAndReturn(
		func(context.Context, string, string, string) error {
			close(retried)
			return nil
		},
	)

	_, err := out.Write([]byte("hello"))
	s.Require().NoError(err)
	select {
	case <-retried:
	case <-time.After(time.Second):
		s.T().Fatal("timed out waiting for retry after submit error")
	}
}
