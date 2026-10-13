package workspace_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/internal/process"
	"github.com/xyzjace/terraplane/pkg/agent/workspace"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/scm"
)

type stubRepoAccess struct{}

func (stubRepoAccess) Name() string                { return "github" }
func (stubRepoAccess) SSHHost() string             { return "github.com" }
func (stubRepoAccess) CloneURL(slug string) string { return "git@github.com:" + slug + ".git" }
func (stubRepoAccess) HTTPSCloneURL(slug string) string {
	return "https://github.com/" + slug + ".git"
}

var _ scm.RepositoryAccess = stubRepoAccess{}

type fakeRunner struct {
	calls []process.Command
	fn    func(cmd process.Command) (process.Result, error)
}

func (f *fakeRunner) Run(_ context.Context, cmd process.Command) (process.Result, error) {
	f.calls = append(f.calls, cmd)
	if f.fn != nil {
		return f.fn(cmd)
	}
	if cmd.Name == "ssh-keyscan" {
		return process.Result{ExitCode: 0, Stdout: "github.com ssh-rsa AAAA\n"}, nil
	}
	return process.Result{ExitCode: 0}, nil
}

func successfulCloneRunner(t *testing.T) *fakeRunner {
	t.Helper()
	return &fakeRunner{fn: func(cmd process.Command) (process.Result, error) {
		if cmd.Name == "ssh-keyscan" {
			return process.Result{ExitCode: 0, Stdout: "github.com ssh-rsa AAAA\n"}, nil
		}
		if cmd.Name == "git" && len(cmd.Args) > 0 && cmd.Args[0] == "checkout" {
			require.NoError(t, os.WriteFile(filepath.Join(cmd.Dir, "terraplane.yaml"), []byte("stacks: []\n"), 0o644))
			require.NoError(t, os.MkdirAll(filepath.Join(cmd.Dir, ".git"), 0o755))
		}
		return process.Result{ExitCode: 0}, nil
	}}
}

type stubTokens struct {
	token string
	err   error
	calls [][2]string
}

func (s *stubTokens) CloneToken(_ context.Context, jobID string, agentID string) (string, error) {
	s.calls = append(s.calls, [2]string{jobID, agentID})
	return s.token, s.err
}

var _ workspace.CloneTokenSource = (*stubTokens)(nil)

// recordingLogger captures everything logged so tests can assert a secret
// never reached the log.
type recordingLogger struct {
	log.Logger
	lines []string
}

func (r *recordingLogger) record(msg string, args []any) {
	r.lines = append(r.lines, fmt.Sprint(append([]any{msg}, args...)...))
}
func (r *recordingLogger) Debug(msg string, args ...any) { r.record(msg, args) }
func (r *recordingLogger) Info(msg string, args ...any)  { r.record(msg, args) }
func (r *recordingLogger) Warn(msg string, args ...any)  { r.record(msg, args) }
func (r *recordingLogger) Error(msg string, args ...any) { r.record(msg, args) }

func newManager(t *testing.T, cfg *config.Config, tokens workspace.CloneTokenSource, runner process.Runner) workspace.Manager {
	t.Helper()
	m, err := workspace.NewManagerWith(cfg, log.Noop(), stubRepoAccess{}, tokens, runner)
	require.NoError(t, err)
	return m
}

type WorkspaceSuite struct {
	suite.Suite
	workDir string
	sshKey  string
}

func TestWorkspaceSuite(t *testing.T) {
	suite.Run(t, new(WorkspaceSuite))
}

func (s *WorkspaceSuite) SetupTest() {
	s.workDir = s.T().TempDir()
	s.sshKey = filepath.Join(s.T().TempDir(), "id_ed25519")
	require.NoError(s.T(), os.WriteFile(s.sshKey, []byte("fake-key"), 0o600))
}

func (s *WorkspaceSuite) cfg() *config.Config {
	return &config.Config{
		AgentSCMSSHKeyPath: s.sshKey,
		AgentWorkDir:       s.workDir,
	}
}

func (s *WorkspaceSuite) TestProvisionMissingSSHKeyConfig() {
	m := newManager(s.T(), &config.Config{AgentWorkDir: s.workDir}, nil, &fakeRunner{})
	_, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "AGENT_SCM_SSH_KEY_PATH")
}

func (s *WorkspaceSuite) TestProvisionMissingSSHKeyFile() {
	m := newManager(s.T(), &config.Config{
		AgentSCMSSHKeyPath: filepath.Join(s.T().TempDir(), "missing"),
		AgentWorkDir:       s.workDir,
	}, nil, &fakeRunner{})
	_, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "SSH key not found")
}

func (s *WorkspaceSuite) TestProvisionMissingWorkDir() {
	m := newManager(s.T(), &config.Config{AgentSCMSSHKeyPath: s.sshKey}, nil, &fakeRunner{})
	_, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "AGENT_WORK_DIR")
}

func (s *WorkspaceSuite) TestProvisionClonesAndReturnsPath() {
	runner := successfulCloneRunner(s.T())
	m := newManager(s.T(), s.cfg(), nil, runner)

	path, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.NoError(s.T(), err)
	require.DirExists(s.T(), path)
	require.FileExists(s.T(), filepath.Join(path, "terraplane.yaml"))
	require.DirExists(s.T(), filepath.Join(path, ".git"))

	var gitCmds []string
	for _, c := range runner.calls {
		if c.Name == "git" {
			gitCmds = append(gitCmds, strings.Join(c.Args, " "))
			require.NotEmpty(s.T(), c.Env)
			require.True(s.T(), strings.HasPrefix(c.Env[0], "GIT_SSH_COMMAND="))
			require.Contains(s.T(), c.Env[0], s.sshKey)
		}
	}
	require.Equal(s.T(), []string{
		"init",
		"remote add origin git@github.com:acme/infra.git",
		"fetch --depth 1 origin abc123",
		"checkout abc123",
	}, gitCmds)
}

func (s *WorkspaceSuite) TestProvisionReusesReadyWorkspace() {
	runner := successfulCloneRunner(s.T())
	m := newManager(s.T(), s.cfg(), nil, runner)

	path, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.NoError(s.T(), err)
	callsAfterFirst := len(runner.calls)

	path2, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.NoError(s.T(), err)
	require.Equal(s.T(), path, path2)
	require.Equal(s.T(), callsAfterFirst, len(runner.calls), "ready workspace must not re-clone")
}

func (s *WorkspaceSuite) TestProvisionReplacesIncompleteWorkspace() {
	dirName := "terraplane-workspace-acme-infra-abc123-stg"
	incomplete := filepath.Join(s.workDir, dirName)
	require.NoError(s.T(), os.MkdirAll(incomplete, 0o755))
	// Missing .git / terraplane.yaml → not ready.

	runner := successfulCloneRunner(s.T())
	m := newManager(s.T(), s.cfg(), nil, runner)
	path, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.NoError(s.T(), err)
	require.Equal(s.T(), incomplete, path)
	require.FileExists(s.T(), filepath.Join(path, "terraplane.yaml"))
}

func (s *WorkspaceSuite) TestProvisionPrunesStaleRevisions() {
	stale := filepath.Join(s.workDir, "terraplane-workspace-acme-infra-oldrev-stg")
	require.NoError(s.T(), os.MkdirAll(stale, 0o755))
	require.NoError(s.T(), os.WriteFile(filepath.Join(stale, "terraplane.yaml"), []byte("stacks: []\n"), 0o644))
	require.NoError(s.T(), os.MkdirAll(filepath.Join(stale, ".git"), 0o755))

	otherStack := filepath.Join(s.workDir, "terraplane-workspace-acme-infra-abc123-prod")
	require.NoError(s.T(), os.MkdirAll(otherStack, 0o755))

	m := newManager(s.T(), s.cfg(), nil, successfulCloneRunner(s.T()))
	_, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.NoError(s.T(), err)

	require.NoDirExists(s.T(), stale)
	require.DirExists(s.T(), otherStack, "workspaces for other stacks must be kept")
}

func (s *WorkspaceSuite) TestProvisionCleansUpOnCloneFailure() {
	runner := &fakeRunner{fn: func(cmd process.Command) (process.Result, error) {
		if cmd.Name == "ssh-keyscan" {
			return process.Result{ExitCode: 0, Stdout: "github.com ssh-rsa AAAA\n"}, nil
		}
		if cmd.Name == "git" && len(cmd.Args) > 0 && cmd.Args[0] == "fetch" {
			return process.Result{ExitCode: 1, Stderr: "fetch failed"}, nil
		}
		return process.Result{ExitCode: 0}, nil
	}}
	m := newManager(s.T(), s.cfg(), nil, runner)

	_, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to clone")

	entries, err := os.ReadDir(s.workDir)
	require.NoError(s.T(), err)
	require.Empty(s.T(), entries, "failed provision must remove the incomplete workspace")
}

func (s *WorkspaceSuite) TestFetchWorkspace() {
	runner := successfulCloneRunner(s.T())
	m := newManager(s.T(), s.cfg(), nil, runner)
	path, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.NoError(s.T(), err)

	got, err := m.FetchWorkspace(context.Background(), "acme/infra", "abc123", "stg")
	require.NoError(s.T(), err)
	require.Equal(s.T(), path, got)
}

func (s *WorkspaceSuite) TestFetchWorkspaceNotReady() {
	dirName := "terraplane-workspace-acme-infra-abc123-stg"
	require.NoError(s.T(), os.MkdirAll(filepath.Join(s.workDir, dirName), 0o755))

	m := newManager(s.T(), s.cfg(), nil, &fakeRunner{})
	_, err := m.FetchWorkspace(context.Background(), "acme/infra", "abc123", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "not ready")
}

func (s *WorkspaceSuite) TestFetchWorkspaceMissingWorkDir() {
	m := newManager(s.T(), &config.Config{}, nil, &fakeRunner{})
	_, err := m.FetchWorkspace(context.Background(), "acme/infra", "abc", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "AGENT_WORK_DIR")
}

func (s *WorkspaceSuite) TestRemoveWorkspace() {
	m := newManager(s.T(), s.cfg(), nil, &fakeRunner{})
	require.NoError(s.T(), m.RemoveWorkspace(context.Background(), ""))

	dir := filepath.Join(s.workDir, "to-remove")
	require.NoError(s.T(), os.MkdirAll(dir, 0o755))
	require.NoError(s.T(), m.RemoveWorkspace(context.Background(), dir))
	require.NoDirExists(s.T(), dir)
}

func (s *WorkspaceSuite) TestSSHKeyscanFailure() {
	runner := &fakeRunner{fn: func(cmd process.Command) (process.Result, error) {
		if cmd.Name == "ssh-keyscan" {
			return process.Result{ExitCode: 1, Stderr: "no keys"}, nil
		}
		return process.Result{ExitCode: 0}, nil
	}}
	m := newManager(s.T(), s.cfg(), nil, runner)
	_, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "ssh-keyscan")
}

func (s *WorkspaceSuite) appCfg() *config.Config {
	return &config.Config{
		AgentID:      "agent-dev",
		AgentSCMAuth: workspace.CloneAuthApp,
		AgentWorkDir: s.workDir,
	}
}

func (s *WorkspaceSuite) TestNewManagerRejectsUnknownAuth() {
	_, err := workspace.NewManagerWith(&config.Config{AgentSCMAuth: "token"}, log.Noop(), stubRepoAccess{}, nil, &fakeRunner{})
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), `AGENT_SCM_AUTH must be "ssh" or "app", got "token"`)
}

func (s *WorkspaceSuite) TestNewManagerDefaultsToSSH() {
	m, err := workspace.NewManager(&config.Config{AgentWorkDir: s.workDir}, log.Noop(), stubRepoAccess{}, nil)
	require.NoError(s.T(), err)
	_, err = m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "AGENT_SCM_SSH_KEY_PATH")
}

func (s *WorkspaceSuite) TestProvisionWithTokenClonesOverHTTPS() {
	const token = "ghs_secret_token"
	tokens := &stubTokens{token: token}
	runner := successfulCloneRunner(s.T())
	logger := &recordingLogger{Logger: log.Noop()}
	m, err := workspace.NewManagerWith(s.appCfg(), logger, stubRepoAccess{}, tokens, runner)
	require.NoError(s.T(), err)

	path, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.NoError(s.T(), err)
	require.DirExists(s.T(), path)
	require.Equal(s.T(), [][2]string{{"job-1", "agent-dev"}}, tokens.calls)

	wantHeader := "GIT_CONFIG_VALUE_0=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	var gitCmds []string
	for _, c := range runner.calls {
		require.NotEqual(s.T(), "ssh-keyscan", c.Name, "token mode must not scan SSH host keys")
		gitCmds = append(gitCmds, strings.Join(c.Args, " "))
		require.Equal(s.T(), []string{
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			wantHeader,
		}, c.Env)
		require.NotContains(s.T(), strings.Join(c.Args, " "), token, "token must not be on the command line")
	}
	require.Equal(s.T(), []string{
		"init",
		"remote add origin https://github.com/acme/infra.git",
		"fetch --depth 1 origin abc123",
		"checkout abc123",
	}, gitCmds)
	require.NoFileExists(s.T(), filepath.Join(path, "known_hosts"))
	for _, line := range logger.lines {
		require.NotContains(s.T(), line, token, "token must not be logged")
	}
	require.Contains(s.T(), strings.Join(logger.lines, "\n"), "Cloning with an orchestrator-issued token")
}

func (s *WorkspaceSuite) TestProvisionWithTokenNeedsNoSSHKey() {
	m := newManager(s.T(), s.appCfg(), &stubTokens{token: "t"}, successfulCloneRunner(s.T()))
	_, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.NoError(s.T(), err)
}

func (s *WorkspaceSuite) TestProvisionWithTokenFailureRemovesWorkspace() {
	tokens := &stubTokens{err: errors.New("orchestrator cannot issue clone tokens")}
	runner := &fakeRunner{}
	m := newManager(s.T(), s.appCfg(), tokens, runner)

	_, err := m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to obtain clone token for repository acme/infra")
	require.Contains(s.T(), err.Error(), "orchestrator cannot issue clone tokens")
	require.Empty(s.T(), runner.calls, "no git command may run without a token")

	entries, err := os.ReadDir(s.workDir)
	require.NoError(s.T(), err)
	require.Empty(s.T(), entries)
}

type badURLRepoAccess struct{ stubRepoAccess }

func (badURLRepoAccess) HTTPSCloneURL(string) string { return "://bad" }

func (s *WorkspaceSuite) TestProvisionWithTokenRejectsBadCloneURL() {
	m, err := workspace.NewManagerWith(s.appCfg(), log.Noop(), badURLRepoAccess{}, &stubTokens{token: "t"}, &fakeRunner{})
	require.NoError(s.T(), err)
	_, err = m.ProvisionWorkspace(context.Background(), "job-1", "acme/infra", "abc123", "stg")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to parse clone URL")
}
