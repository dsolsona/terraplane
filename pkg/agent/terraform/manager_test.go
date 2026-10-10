package terraform_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/mock/gomock"

	"github.com/xyzjace/terraplane/pkg/agent/terraform"
	"github.com/xyzjace/terraplane/pkg/agent/terraform/mock_terraform"
	"github.com/xyzjace/terraplane/pkg/log"
)

type ManagerSuite struct {
	suite.Suite
	ctrl *gomock.Controller
	vm   *mock_terraform.MockVersionManager
	run  *mock_terraform.MockRunner
	ws   string
}

func TestManagerSuite(t *testing.T) {
	suite.Run(t, new(ManagerSuite))
}

func (s *ManagerSuite) SetupTest() {
	s.ctrl = gomock.NewController(s.T())
	s.vm = mock_terraform.NewMockVersionManager(s.ctrl)
	s.run = mock_terraform.NewMockRunner(s.ctrl)
	s.ws = s.T().TempDir()
}

func (s *ManagerSuite) writeConfig(yaml string) {
	require.NoError(s.T(), os.WriteFile(filepath.Join(s.ws, "terraplane.yaml"), []byte(yaml), 0o644))
	require.NoError(s.T(), os.MkdirAll(filepath.Join(s.ws, "stacks/stg"), 0o755))
}

func (s *ManagerSuite) mgr(defaultVer string) terraform.Manager {
	return terraform.NewManagerWith(log.Noop(), defaultVer, s.vm, s.run)
}

func (s *ManagerSuite) TestRunPlanHappyPath() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: stacks/stg
        tool_version: 1.5.0
`)
	stackDir := filepath.Join(s.ws, "stacks/stg")
	s.vm.EXPECT().Ensure(gomock.Any(), "1.5.0").Return("/bin/terraform", nil)
	s.run.EXPECT().Init(gomock.Any(), "/bin/terraform", stackDir, gomock.Any()).Return(nil)
	s.run.EXPECT().Plan(gomock.Any(), "/bin/terraform", stackDir, "-target=x", gomock.Any()).Return("plan out", nil)

	out, err := s.mgr("1.0.0").RunPlan(context.Background(), s.ws, "stg", "", "-target=x", nil)
	require.NoError(s.T(), err)
	require.Equal(s.T(), "plan out", out)
}

func (s *ManagerSuite) TestRunPlanJobToolVersionOverridesConfig() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: stacks/stg
        tool_version: 1.5.0
`)
	stackDir := filepath.Join(s.ws, "stacks/stg")
	s.vm.EXPECT().Ensure(gomock.Any(), "1.8.0").Return("/bin/terraform", nil)
	s.run.EXPECT().Init(gomock.Any(), "/bin/terraform", stackDir, gomock.Any()).Return(nil)
	s.run.EXPECT().Plan(gomock.Any(), "/bin/terraform", stackDir, "", gomock.Any()).Return("ok", nil)

	_, err := s.mgr("1.0.0").RunPlan(context.Background(), s.ws, "stg", "1.8.0", "", nil)
	require.NoError(s.T(), err)
}

func (s *ManagerSuite) TestRunPlanUsesDefaultVersion() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: stacks/stg
`)
	stackDir := filepath.Join(s.ws, "stacks/stg")
	s.vm.EXPECT().Ensure(gomock.Any(), "1.9.0").Return("/bin/terraform", nil)
	s.run.EXPECT().Init(gomock.Any(), "/bin/terraform", stackDir, gomock.Any()).Return(nil)
	s.run.EXPECT().Plan(gomock.Any(), "/bin/terraform", stackDir, "", gomock.Any()).Return("ok", nil)

	_, err := s.mgr("1.9.0").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.NoError(s.T(), err)
}

func (s *ManagerSuite) TestRunPlanWorksWithoutAgentsInConfig() {
	// Intention: when the orchestrator owns routing, terraplane.yaml carries no agent fields.
	s.writeConfig(`
environments:
  - name: default
    stacks:
      - name: stg
        dir: stacks/stg
`)
	stackDir := filepath.Join(s.ws, "stacks/stg")
	s.vm.EXPECT().Ensure(gomock.Any(), "1.9.0").Return("/bin/terraform", nil)
	s.run.EXPECT().Init(gomock.Any(), "/bin/terraform", stackDir, gomock.Any()).Return(nil)
	s.run.EXPECT().Plan(gomock.Any(), "/bin/terraform", stackDir, "", gomock.Any()).Return("ok", nil)

	_, err := s.mgr("1.9.0").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.NoError(s.T(), err)
}

func (s *ManagerSuite) TestRunPlanMissingStack() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: other
        dir: stacks/other
`)
	_, err := s.mgr("1.0.0").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), `stack "stg" not found`)
}

func (s *ManagerSuite) TestRunPlanMissingConfig() {
	_, err := s.mgr("1.0.0").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to read terraplane config")
}

func (s *ManagerSuite) TestRunPlanNoVersionConfigured() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: stacks/stg
`)
	_, err := s.mgr("").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "AGENT_DEFAULT_TERRAFORM_VERSION")
}

func (s *ManagerSuite) TestRunPlanEnsureFailure() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: stacks/stg
        tool_version: 1.5.0
`)
	s.vm.EXPECT().Ensure(gomock.Any(), "1.5.0").Return("", errors.New("download failed"))

	_, err := s.mgr("1.0.0").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "download failed")
}

func (s *ManagerSuite) TestRunPlanInitFailure() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: stacks/stg
        tool_version: 1.5.0
`)
	stackDir := filepath.Join(s.ws, "stacks/stg")
	s.vm.EXPECT().Ensure(gomock.Any(), "1.5.0").Return("/bin/terraform", nil)
	s.run.EXPECT().Init(gomock.Any(), "/bin/terraform", stackDir, gomock.Any()).Return(errors.New("init failed"))

	_, err := s.mgr("1.0.0").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "init failed")
}

func (s *ManagerSuite) TestRunApplyHappyPath() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: stacks/stg
        tool_version: 1.5.0
`)
	stackDir := filepath.Join(s.ws, "stacks/stg")
	s.vm.EXPECT().Ensure(gomock.Any(), "1.5.0").Return("/bin/terraform", nil)
	s.run.EXPECT().Apply(gomock.Any(), "/bin/terraform", stackDir, gomock.Any()).Return("apply out", nil)

	out, err := s.mgr("1.0.0").RunApply(context.Background(), s.ws, "stg", "", nil)
	require.NoError(s.T(), err)
	require.Equal(s.T(), "apply out", out)
}

func (s *ManagerSuite) TestRunApplyEnsureFailure() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: stacks/stg
        tool_version: 1.5.0
`)
	s.vm.EXPECT().Ensure(gomock.Any(), "1.5.0").Return("", errors.New("no binary"))

	_, err := s.mgr("1.0.0").RunApply(context.Background(), s.ws, "stg", "", nil)
	require.Error(s.T(), err)
}

func (s *ManagerSuite) TestRunPlanRejectsDirEscape() {
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: ../../outside
        tool_version: 1.5.0
`)
	_, err := s.mgr("1.0.0").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "escapes workspace")
}

func (s *ManagerSuite) TestRunPlanRejectsAbsoluteDir() {
	s.writeConfig(fmt.Sprintf(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: %s
        tool_version: 1.5.0
`, s.T().TempDir()))
	_, err := s.mgr("1.0.0").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "must be relative")
}

func (s *ManagerSuite) TestRunPlanRejectsSymlinkEscape() {
	outside := s.T().TempDir()
	link := filepath.Join(s.ws, "escape")
	require.NoError(s.T(), os.Symlink(outside, link))
	s.writeConfig(`
environments:
  - name: default
    agent: a
    stacks:
      - name: stg
        dir: escape
        tool_version: 1.5.0
`)
	_, err := s.mgr("1.0.0").RunPlan(context.Background(), s.ws, "stg", "", "", nil)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "escapes workspace")
}
